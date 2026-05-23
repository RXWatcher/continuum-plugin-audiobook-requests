package reconciler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/RXWatcher/silo-plugin-audiobook-requests/internal/consumer"
	"github.com/RXWatcher/silo-plugin-audiobook-requests/internal/store"
)

// pollNZBGet handles one forwarded_request whose external_id is an
// NZBGet NZBID (encoded as "nzbget:<int>"). Same shape as the existing
// AudiobookBay per-row branch: poll → translate → MarkPolled → publish
// the right downstream event, with the same error-dedup behaviour so a
// dead NZBGet doesn't spam DB writes.
func (r *Reconciler) pollNZBGet(ctx context.Context, row store.ForwardedRequest) error {
	nzbID, ok := parseNZBExternalID(row.ExternalID)
	if !ok {
		// Malformed prefix; mark the row failed once and stop polling
		// it on every tick.
		reason := "malformed nzbget external_id: " + row.ExternalID
		if r.recordErrorChanged(row.RequestID, reason) {
			_ = r.deps.Store.UpsertForwardedRequest(ctx, store.ForwardedRequest{
				RequestID: row.RequestID, ExternalID: row.ExternalID,
				Status: "failed", ErrorText: reason, UpdatedAt: time.Now(),
			})
			r.deps.Pub.Publish(ctx, "request_failed", map[string]any{
				"request_id": row.RequestID, "requestId": row.RequestID,
				"provider_plugin_id": r.deps.PluginID,
				"reason":             reason,
			})
		}
		return nil
	}

	rowCtx, rowCancel := context.WithTimeout(ctx, perRowTimeout)
	defer rowCancel()
	snap, err := r.deps.Nzbget.Lookup(rowCtx, nzbID)
	if err != nil {
		if !r.recordErrorChanged(row.RequestID, err.Error()) {
			return nil
		}
		_ = r.deps.Store.UpsertForwardedRequest(ctx, store.ForwardedRequest{
			RequestID: row.RequestID, ExternalID: row.ExternalID,
			Status: row.Status, LastPolled: time.Now(),
			ErrorText: err.Error(), UpdatedAt: time.Now(),
		})
		return nil
	}
	r.clearError(row.RequestID)

	newStatus := translateNZBGetState(snap.State)
	if newStatus == "" || newStatus == row.Status {
		// Either an unmapped state (treat as "no transition") or a
		// confirmation of the current state. Either way, the poll
		// succeeded; stamp last_polled + clear any sticky error.
		if uerr := r.deps.Store.MarkPolled(ctx, row.RequestID, row.ExternalID, row.Status, time.Now()); uerr != nil {
			return fmt.Errorf("nzbget mark polled (no transition): %w", uerr)
		}
		return nil
	}
	if uerr := r.deps.Store.MarkPolled(ctx, row.RequestID, row.ExternalID, newStatus, time.Now()); uerr != nil {
		return fmt.Errorf("nzbget mark polled (status change): %w", uerr)
	}
	switch newStatus {
	case "imported":
		r.deps.Pub.Publish(ctx, "request_fulfilled", map[string]any{
			"request_id": row.RequestID, "requestId": row.RequestID,
			"external_id":        row.ExternalID,
			"fulfilled_book_id":  strconv.Itoa(nzbID),
			"provider_plugin_id": r.deps.PluginID,
		})
	case "failed":
		r.deps.Pub.Publish(ctx, "request_failed", map[string]any{
			"request_id": row.RequestID, "requestId": row.RequestID,
			"external_id":        row.ExternalID,
			"provider_plugin_id": r.deps.PluginID,
			"reason":             "nzbget reported failure",
		})
	default:
		r.deps.Pub.Publish(ctx, "request_status_changed", map[string]any{
			"request_id": row.RequestID, "requestId": row.RequestID,
			"external_id":        row.ExternalID,
			"provider_plugin_id": r.deps.PluginID,
			"status":             newStatus,
		})
	}
	return nil
}

// parseNZBExternalID extracts the integer NZBID from the "nzbget:<id>"
// envelope the consumer stored. Returns (0, false) for any malformed
// value so the caller can mark the row failed once instead of looping.
func parseNZBExternalID(s string) (int, bool) {
	if !strings.HasPrefix(s, consumer.NZBExternalIDPrefix) {
		return 0, false
	}
	id, err := strconv.Atoi(strings.TrimPrefix(s, consumer.NZBExternalIDPrefix))
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// translateNZBGetState maps the NZBGet state vocabulary into the
// plugin's queue states. "unknown" (job dropped from both queue + history)
// becomes "failed" because the user can't see anything actionable in
// NZBGet anymore — better to surface as a terminal failure than poll
// forever.
func translateNZBGetState(state string) string {
	switch state {
	case "queued":
		return "queued"
	case "downloading":
		return "downloading"
	case "imported":
		return "imported"
	case "failed":
		return "failed"
	case "unknown":
		return "failed"
	}
	return ""
}
