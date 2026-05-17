// Package consumer implements the event_consumer.v1 handler for
// request_submitted events. It accepts an optional source_id/detail URL; when
// absent it searches AudiobookBay from the submitted title and author.
package consumer

import (
	"context"
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
		return &pluginv1.HandleEventResponse{}, nil
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

	if err := d.Store.UpsertForwardedRequest(ctx, store.ForwardedRequest{
		RequestID: requestID, Status: "submitted", SourceID: sourceID,
		SearchQuery: query, UpdatedAt: time.Now(),
	}); err != nil {
		h.logger.Warn("upsert forwarded_request (initial)", "request_id", requestID, "err", err)
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
			h.logger.Warn("upsert forwarded_request (after StartDownload err)",
				"request_id", requestID, "upstream_err", err, "db_err", uerr)
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
		// Upstream already accepted; logging is enough — the reconciler will
		// heal the row on the next tick. Returning an error here would cause
		// the host to retry the event, duplicate-adding upstream.
		h.logger.Warn("upsert forwarded_request (after acknowledged)",
			"request_id", requestID, "external_id", resp.ID, "db_err", uerr)
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
