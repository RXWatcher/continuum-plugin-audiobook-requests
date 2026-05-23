// Package reconciler periodically polls upstream AudiobookBay for status of
// non-terminal forwarded_request rows and emits status events.
package reconciler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/RXWatcher/silo-plugin-audiobook-requests/internal/audiobookbay"
	"github.com/RXWatcher/silo-plugin-audiobook-requests/internal/consumer"
	"github.com/RXWatcher/silo-plugin-audiobook-requests/internal/nzbget"
	"github.com/RXWatcher/silo-plugin-audiobook-requests/internal/store"
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
	Nzbget   *nzbget.Client // nil disables the NZBGet poll branch
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

	statusMu sync.RWMutex
	status   Status

	// lastError dedupes consecutive identical per-row errors so a months-
	// long upstream outage doesn't write the same error_text + log line to
	// every row every minute. Cleared on success.
	lastErrorMu sync.Mutex
	lastError   map[string]string

	// backoffUntil parks the entire reconciler after a 429 from upstream.
	// Until the window passes we skip the work (no per-row calls, no DB
	// updates) so a backed-off mirror isn't hammered into staying mad.
	backoffMu    sync.Mutex
	backoffUntil time.Time
}

// Status snapshots the most recent Tick outcome for the admin UI.
type Status struct {
	LastRunAt     time.Time
	LastDuration  time.Duration
	RowsProcessed int
	LastError     string
	Skipped       bool
}

func New(d Deps) *Reconciler {
	return &Reconciler{deps: d, lastError: map[string]string{}}
}

// LastStatus returns the most recent Tick outcome. Zero value before the
// first Tick fires.
func (r *Reconciler) LastStatus() Status {
	r.statusMu.RLock()
	defer r.statusMu.RUnlock()
	return r.status
}

func (r *Reconciler) setStatus(s Status) {
	r.statusMu.Lock()
	r.status = s
	r.statusMu.Unlock()
}

func (r *Reconciler) recordErrorChanged(requestID, errText string) bool {
	r.lastErrorMu.Lock()
	defer r.lastErrorMu.Unlock()
	if prev, ok := r.lastError[requestID]; ok && prev == errText {
		return false
	}
	r.lastError[requestID] = errText
	return true
}

func (r *Reconciler) clearError(requestID string) {
	r.lastErrorMu.Lock()
	delete(r.lastError, requestID)
	r.lastErrorMu.Unlock()
}

func (r *Reconciler) backoffRemaining() time.Duration {
	r.backoffMu.Lock()
	defer r.backoffMu.Unlock()
	if r.backoffUntil.IsZero() {
		return 0
	}
	d := time.Until(r.backoffUntil)
	if d <= 0 {
		r.backoffUntil = time.Time{}
		return 0
	}
	return d
}

func (r *Reconciler) setBackoff(d time.Duration) {
	if d <= 0 {
		d = 60 * time.Second
	}
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	r.backoffMu.Lock()
	r.backoffUntil = time.Now().Add(d)
	r.backoffMu.Unlock()
}

func (r *Reconciler) Tick(ctx context.Context) error {
	if !r.tickMu.TryLock() {
		// Previous tick still running. Drop this one rather than queuing
		// extra work behind it.
		r.setStatus(Status{LastRunAt: time.Now(), Skipped: true})
		return nil
	}
	defer r.tickMu.Unlock()

	if remain := r.backoffRemaining(); remain > 0 {
		r.setStatus(Status{
			LastRunAt:    time.Now(),
			LastDuration: 0,
			Skipped:      true,
			LastError:    fmt.Sprintf("backoff: upstream rate-limited, %s remaining", remain.Round(time.Second)),
		})
		return nil
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, tickTimeout)
	defer cancel()

	rows, err := r.deps.Store.ListNonTerminal(ctx, 200)
	if err != nil {
		r.setStatus(Status{LastRunAt: time.Now(), LastDuration: time.Since(start), LastError: err.Error()})
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
		// Rows handed to the abook+NZBGet pipeline carry a typed prefix
		// on external_id; route them through the NZBGet poll branch
		// instead of asking AudiobookBay (which has no concept of that
		// id). NZBGet rows reuse the same error-dedup + status-publish
		// machinery below — see pollNZBGet.
		if strings.HasPrefix(row.ExternalID, consumer.NZBExternalIDPrefix) {
			if r.deps.Nzbget == nil {
				// Plugin lost its NZBGet config since the row was
				// queued; record a clear error and skip rather than
				// flooding logs with "nil client" panics.
				if r.recordErrorChanged(row.RequestID, "nzbget not configured") {
					_ = r.deps.Store.UpsertForwardedRequest(ctx, store.ForwardedRequest{
						RequestID: row.RequestID, ExternalID: row.ExternalID,
						Status: row.Status, LastPolled: time.Now(),
						ErrorText: "nzbget not configured", UpdatedAt: time.Now(),
					})
				}
				continue
			}
			if perr := r.pollNZBGet(ctx, row); perr != nil && firstErr == nil {
				firstErr = perr
			}
			continue
		}
		rowCtx, rowCancel := context.WithTimeout(ctx, perRowTimeout)
		snap, err := r.deps.ABB.GetDownload(rowCtx, row.ExternalID)
		rowCancel()
		if err != nil {
			// 429 from the mirror: park the whole reconciler for the
			// Retry-After window and stop processing this batch. Further
			// rows would all 429 too.
			if rl, ok := audiobookbay.IsRateLimited(err); ok {
				r.setBackoff(rl.RetryAfter)
				if r.recordErrorChanged(row.RequestID, err.Error()) {
					_ = r.deps.Store.UpsertForwardedRequest(ctx, store.ForwardedRequest{
						RequestID: row.RequestID, ExternalID: row.ExternalID,
						Status: row.Status, LastPolled: time.Now(),
						ErrorText: err.Error(), UpdatedAt: time.Now(),
					})
				}
				if firstErr == nil {
					firstErr = err
				}
				break
			}
			// Dedupe: skip the UPDATE when the same row keeps returning the
			// same error_text across ticks. last_polled still moves forward
			// via the upsert the first time the error appears (or changes).
			if !r.recordErrorChanged(row.RequestID, err.Error()) {
				continue
			}
			if uerr := r.deps.Store.UpsertForwardedRequest(ctx, store.ForwardedRequest{
				RequestID: row.RequestID, ExternalID: row.ExternalID,
				Status: row.Status, LastPolled: time.Now(),
				ErrorText: err.Error(), UpdatedAt: time.Now(),
			}); uerr != nil && firstErr == nil {
				firstErr = fmt.Errorf("upsert (after upstream err): %w", uerr)
			}
			continue
		}
		r.clearError(row.RequestID)
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
			r.clearError(row.RequestID)
			r.deps.Pub.Publish(ctx, "request_fulfilled", map[string]any{
				"request_id":         row.RequestID,
				"requestId":          row.RequestID,
				"external_id":        row.ExternalID,
				"fulfilled_book_id":  snap.ID,
				"provider_plugin_id": r.deps.PluginID,
			})
		case "failed":
			r.clearError(row.RequestID)
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
	s := Status{
		LastRunAt:     time.Now(),
		LastDuration:  time.Since(start),
		RowsProcessed: len(rows),
	}
	if firstErr != nil {
		s.LastError = firstErr.Error()
	}
	r.setStatus(s)
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
