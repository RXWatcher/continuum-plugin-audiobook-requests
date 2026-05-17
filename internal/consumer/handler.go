// Package consumer implements the event_consumer.v1 handler for
// request_submitted events. It accepts an optional source_id/detail URL; when
// absent it searches AudiobookBay from the submitted title and author.
package consumer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginproto/continuum/plugin/v1"

	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/audiobookbay"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/store"
)

type Publisher interface {
	Publish(ctx context.Context, name string, payload map[string]any)
}

type Deps struct {
	Store    *store.Store
	Pub      Publisher
	ABB      *audiobookbay.Client
	PluginID string
}

type Handler struct {
	pluginv1.UnimplementedEventConsumerServer
	depsFn func() *Deps
	logger hclog.Logger
}

// New constructs the handler. logger may be nil; a null logger is used so
// the handler is safe to use in tests.
func New(depsFn func() *Deps, logger hclog.Logger) *Handler {
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	return &Handler{depsFn: depsFn, logger: logger}
}

func (h *Handler) HandleEvent(ctx context.Context, req *pluginv1.HandleEventRequest) (*pluginv1.HandleEventResponse, error) {
	if req.GetEventName() != "plugin.continuum.audiobooks.request_submitted" {
		return &pluginv1.HandleEventResponse{}, nil
	}
	if req.GetPayload() == nil {
		return &pluginv1.HandleEventResponse{}, nil
	}
	d := h.depsFn()
	if d == nil {
		// Capability servers serve before Configure runs. Nack so the host
		// redelivers once configured instead of acking and dropping the
		// request permanently.
		return nil, fmt.Errorf("plugin not configured yet")
	}
	p := req.GetPayload().AsMap()
	if target := targetPluginIDFromPayload(p); target != d.PluginID {
		return &pluginv1.HandleEventResponse{}, nil
	}
	requestID := requestIDFromPayload(p)
	if requestID == "" {
		return &pluginv1.HandleEventResponse{}, nil
	}
	sourceID, _ := p["source_id"].(string)
	title, _ := p["title"].(string)
	author, _ := p["author"].(string)
	query := strings.TrimSpace(title + " " + author)

	// Must persist before kicking off the torrent: if this row is lost the
	// reconciler never polls it and the request is permanently lost (worse,
	// the torrent would be orphaned in qBittorrent/embedded). Nack instead of
	// starting untracked work; the terminal guard in UpsertForwardedRequest
	// makes the inevitable redelivery idempotent.
	if err := d.Store.UpsertForwardedRequest(ctx, store.ForwardedRequest{
		RequestID: requestID, Status: "submitted", SourceID: sourceID,
		SearchQuery: query, UpdatedAt: time.Now(),
	}); err != nil {
		return nil, fmt.Errorf("persist submitted %s: %w", requestID, err)
	}

	if query == "" && sourceID == "" {
		reason := "title or source_id required for AudiobookBay request"
		if err := d.Store.UpsertForwardedRequest(ctx, store.ForwardedRequest{
			RequestID: requestID, Status: "failed", ErrorText: reason, UpdatedAt: time.Now(),
		}); err != nil {
			h.logger.Warn("upsert forwarded_request (missing request query)",
				"request_id", requestID, "err", err)
		}
		d.Pub.Publish(ctx, "request_failed", map[string]any{
			"request_id": requestID, "requestId": requestID,
			"provider_plugin_id": d.PluginID, "reason": reason,
		})
		return &pluginv1.HandleEventResponse{}, nil
	}

	resp, err := d.ABB.StartDownload(ctx, sourceID, query)
	if err != nil {
		if uerr := d.Store.UpsertForwardedRequest(ctx, store.ForwardedRequest{
			RequestID: requestID, Status: "failed", ErrorText: err.Error(), UpdatedAt: time.Now(),
		}); uerr != nil {
			// Couldn't even record the failure — nack so it's retried rather
			// than left stuck non-terminal (the row is still "submitted",
			// which the reconciler skips forever for want of an external_id).
			return nil, fmt.Errorf("persist failed %s: %w (upstream: %v)", requestID, uerr, err)
		}
		d.Pub.Publish(ctx, "request_failed", map[string]any{
			"request_id": requestID, "requestId": requestID,
			"provider_plugin_id": d.PluginID, "reason": err.Error(),
		})
		return &pluginv1.HandleEventResponse{}, nil
	}
	status := requestStatusFromDownload(resp.Status)
	if uerr := d.Store.UpsertForwardedRequest(ctx, store.ForwardedRequest{
		RequestID: requestID, ExternalID: resp.ID, Status: status,
		SourceID: sourceID, SearchQuery: query, SelectedTitle: resp.Title,
		DetailURL: resp.DetailURL, InfoHash: resp.InfoHash, MagnetURI: resp.Magnet,
		SelectedScore: resp.Score, SelectedScoreReason: resp.Reason, UpdatedAt: time.Now(),
	}); uerr != nil {
		// Must persist the external_id (info hash): without it the reconciler
		// skips this row forever (it requires a non-empty external_id), so the
		// torrent is never polled and the request hangs permanently. Nack; the
		// terminal guard makes redelivery idempotent. (Re-adding the same
		// magnet/hash to qBittorrent/embedded is idempotent — the accepted
		// tradeoff vs. a permanently lost request.)
		return nil, fmt.Errorf("persist acknowledged %s: %w", requestID, uerr)
	}
	d.Pub.Publish(ctx, "request_acknowledged", map[string]any{
		"request_id": requestID, "requestId": requestID,
		"external_id": resp.ID, "provider_plugin_id": d.PluginID,
		"selected_title": resp.Title, "detail_url": resp.DetailURL,
		"info_hash": resp.InfoHash, "score": resp.Score, "reason": resp.Reason,
		"status": status, "progress": resp.Progress,
	})
	return &pluginv1.HandleEventResponse{}, nil
}

func requestStatusFromDownload(status string) string {
	switch status {
	case "queued", "downloading", "imported", "failed":
		return status
	default:
		return "acknowledged"
	}
}

func targetPluginIDFromPayload(p map[string]any) string {
	for _, key := range []string{"target_plugin_id", "target_provider_plugin_id", "provider_plugin_id"} {
		if v, _ := p[key].(string); v != "" {
			return v
		}
	}
	return ""
}

func requestIDFromPayload(p map[string]any) string {
	if id, _ := p["request_id"].(string); id != "" {
		return id
	}
	id, _ := p["requestId"].(string)
	return id
}
