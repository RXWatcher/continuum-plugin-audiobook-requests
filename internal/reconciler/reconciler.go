// Package reconciler periodically polls upstream AudiobookBay for status of
// non-terminal forwarded_request rows and emits status events.
package reconciler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/audiobookbay"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/store"
)

// tickTimeout caps a full Tick invocation. The scheduler fires this task on
// a 1-minute cron; capping below that prevents the next tick from arriving
// while we're still working and avoids starving other scheduled tasks if
// the upstream AudiobookBay hangs.
const tickTimeout = 45 * time.Second

// perRowTimeout caps each upstream lookup. We process up to 200 rows per
// tick; 1s per row × 200 + slack fits comfortably inside tickTimeout.
const perRowTimeout = 10 * time.Second

type Publisher interface {
	Publish(ctx context.Context, name string, payload map[string]any)
}

type Deps struct {
	Store    *store.Store
	Pub      Publisher
	ABB      *audiobookbay.Client
	PluginID string
}

type Reconciler struct {
	deps Deps

	// tickMu guards against overlapping Tick calls. If a previous Tick is
	// still running when the scheduler fires the next one, the new call
	// returns immediately instead of doubling up on upstream calls and DB
	// writes. The SDK scheduler is generally serial, but a slow upstream +
	// clock skew can occasionally trigger overlap.
	tickMu sync.Mutex
}

func New(d Deps) *Reconciler { return &Reconciler{deps: d} }

func (r *Reconciler) Tick(ctx context.Context) error {
	if !r.tickMu.TryLock() {
		// Previous tick still running. Drop this one rather than queuing
		// extra work behind it.
		return nil
	}
	defer r.tickMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, tickTimeout)
	defer cancel()

	rows, err := r.deps.Store.ListNonTerminal(ctx, 200)
	if err != nil {
		return err
	}
	// firstErr captures the first non-nil error from per-row work. We keep
	// processing remaining rows so one dead row doesn't starve the others,
	// but return the error at the end so the SDK records a failed tick and
	// operators can see why.
	var firstErr error
	for _, row := range rows {
		// Tick budget exhausted (or cancelled): stop now. Continuing would
		// make every remaining GetDownload/upsert fail with "context
		// deadline exceeded" and record that as a per-row upstream error
		// across all the rows we didn't get to.
		if ctx.Err() != nil {
			break
		}
		if row.ExternalID == "" {
			continue
		}
		rowCtx, rowCancel := context.WithTimeout(ctx, perRowTimeout)
		snap, err := r.deps.ABB.GetDownload(rowCtx, row.ExternalID)
		rowCancel()
		if err != nil {
			if uerr := r.deps.Store.UpsertForwardedRequest(ctx, store.ForwardedRequest{
				RequestID: row.RequestID, ExternalID: row.ExternalID,
				Status: row.Status, LastPolled: time.Now(),
				ErrorText: err.Error(), UpdatedAt: time.Now(),
			}); uerr != nil && firstErr == nil {
				firstErr = fmt.Errorf("upsert (after upstream err): %w", uerr)
			}
			continue
		}
		newStatus := translateStatus(snap.Status)
		// "" => unknown upstream status: hold the current status. Either way
		// the poll succeeded, so MarkPolled stamps last_polled and clears any
		// sticky error_text from a previous transient failure.
		if newStatus == "" || newStatus == row.Status {
			if uerr := r.deps.Store.MarkPolled(ctx, row.RequestID, row.ExternalID, row.Status, time.Now()); uerr != nil && firstErr == nil {
				firstErr = fmt.Errorf("mark polled (no transition): %w", uerr)
			}
			continue
		}
		if uerr := r.deps.Store.MarkPolled(ctx, row.RequestID, row.ExternalID, newStatus, time.Now()); uerr != nil && firstErr == nil {
			firstErr = fmt.Errorf("mark polled (status change): %w", uerr)
		}
		switch newStatus {
		case "imported":
			r.deps.Pub.Publish(ctx, "request_fulfilled", map[string]any{
				"request_id":         row.RequestID,
				"requestId":          row.RequestID,
				"external_id":        row.ExternalID,
				"fulfilled_book_id":  snap.ID,
				"provider_plugin_id": r.deps.PluginID,
			})
		case "failed":
			r.deps.Pub.Publish(ctx, "request_failed", map[string]any{
				"request_id":         row.RequestID,
				"requestId":          row.RequestID,
				"external_id":        row.ExternalID,
				"provider_plugin_id": r.deps.PluginID,
				"reason":             "upstream marked failed",
			})
		default:
			r.deps.Pub.Publish(ctx, "request_status_changed", map[string]any{
				"request_id":         row.RequestID,
				"requestId":          row.RequestID,
				"external_id":        row.ExternalID,
				"provider_plugin_id": r.deps.PluginID,
				"status":             newStatus,
			})
		}
	}
	return firstErr
}

func translateStatus(ebkStatus string) string {
	switch ebkStatus {
	case "queued":
		return "queued"
	case "magnet_ready":
		return "acknowledged"
	case "downloading":
		return "downloading"
	case "imported", "completed":
		return "imported"
	case "failed", "error":
		return "failed"
	}
	// Unknown/unmapped status (e.g. an embedded/qBittorrent state we don't
	// recognise): signal "no transition" so the caller holds the current
	// status. Previously this returned "acknowledged", which regressed an
	// in-flight request (e.g. downloading -> acknowledged) and spammed a
	// status-changed event on every poll. (magnet_ready -> acknowledged
	// above is an intentional mapping for scrape-only mode and is kept.)
	return ""
}
