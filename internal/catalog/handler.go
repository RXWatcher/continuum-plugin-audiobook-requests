package catalog

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/RXWatcher/silo-plugin-audiobook-requests/internal/audiobookbay"
)

type Handler struct {
	client *audiobookbay.Client
}

func NewHandler(c *audiobookbay.Client) *Handler { return &Handler{client: c} }

func (h *Handler) Mount(r chi.Router) {
	r.Get("/capabilities/provider", h.Capabilities())
	r.Post("/external_search", h.ExternalSearch())
	r.Get("/requests/{external_id}", h.RequestSnapshot())

	r.Get("/catalog", notCatalogProvider)
	r.Get("/catalog/search", notCatalogProvider)
	r.Get("/catalog/{id}", notCatalogProvider)
}

func (h *Handler) Capabilities() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"roles":             []string{"request_provider"},
			"supports_catalog":  false,
			"supports_requests": true,
			"source":            "audiobookbay",
		})
	}
}

func (h *Handler) ExternalSearch() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Q     string `json:"q"`
			Limit int    `json:"limit"`
		}
		// Bound the request body — it's a tiny JSON object; don't let the
		// decoder read an unbounded payload.
		_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body)
		if body.Q == "" {
			http.Error(w, "q required", http.StatusBadRequest)
			return
		}
		hits, err := h.client.ExternalSearch(r.Context(), body.Q, body.Limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"items": hits})
	}
}

func (h *Handler) RequestSnapshot() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		eid := chi.URLParam(r, "external_id")
		if eid == "" {
			http.Error(w, "external_id required", http.StatusBadRequest)
			return
		}
		snap, err := h.client.GetDownload(r.Context(), eid)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{
			"external_id": snap.ID,
			"status":      snap.Status,
			"title":       snap.Title,
			"detail_url":  snap.DetailURL,
			"info_hash":   snap.InfoHash,
			"score":       snap.Score,
			"reason":      snap.Reason,
			"progress":    snap.Progress,
		})
	}
}

func notCatalogProvider(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "Audiobook Requests is a request provider, not a catalog backend", http.StatusNotImplemented)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
