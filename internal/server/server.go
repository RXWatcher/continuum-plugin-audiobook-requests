// Package server constructs the chi-based HTTP handler.
package server

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/audiobookbay"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/catalog"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/runtime"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/store"
)

type Deps struct {
	AudiobookBayClient *audiobookbay.Client
	Store              *store.Store
	Config             runtime.Config
}

type Server struct {
	deps Deps
}

func New(d Deps) *Server { return &Server{deps: d} }

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Get("/admin", s.handleAdminHome)
	r.Get("/admin/", s.handleAdminHome)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.handleHealth)
		r.Get("/capabilities", s.handleCapabilities)
		r.Get("/admin/diagnostics", s.handleDiagnostics)
		r.Get("/admin/test-search", s.handleTestSearch)
		if s.deps.AudiobookBayClient != nil {
			catalog.NewHandler(s.deps.AudiobookBayClient).Mount(r)
		}
	})
	return r
}

func (s *Server) handleAdminHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html>
<html lang="en" data-theme="` + adminTheme(r) + `">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>AudiobookBay Requests</title><style>` + adminThemeCSS() + `</style></head>
<body>
<p><a href="/admin/plugins">&larr; Back to plugins</a></p>
<h1>AudiobookBay Requests</h1>
<p>Audiobook request provider with AudiobookBay search and download reconciliation.</p>
<ul>
<li><a href="./api/v1/admin/diagnostics">Diagnostics</a></li>
<li><a href="./api/v1/admin/test-search">Test search</a></li>
</ul>
</body></html>`))
}

func adminTheme(r *http.Request) string {
	theme := r.Header.Get("X-Continuum-Theme")
	if theme == "" {
		theme = r.URL.Query().Get("theme")
	}
	if theme == "" {
		theme = "default"
	}
	return html.EscapeString(theme)
}

func adminThemeCSS() string {
	return `:root{--bg:#141417;--fg:#e8e8ec;--link:#93c5fd;--panel:#1c1c20;--border:#28282e}[data-theme="cinema-light"]{--bg:#f7f3ed;--fg:#201c18;--link:#9a3412;--panel:#fffaf3;--border:#ded1c0}[data-theme="cobalt-studio"]{--bg:#101623;--fg:#eef4ff;--link:#60a5fa;--panel:#172033;--border:#2d3f61}[data-theme="oxblood-noir"]{--bg:#170b10;--fg:#fff1f4;--link:#fb7185;--panel:#241018;--border:#4a2230}[data-theme="evergreen-studio"]{--bg:#0d1712;--fg:#ecfdf3;--link:#6ee7b7;--panel:#14241b;--border:#2b4b39}body{font-family:system-ui,sans-serif;margin:32px;line-height:1.5;background:var(--bg);color:var(--fg)}a{color:var(--link);text-decoration:none}li{margin:6px 0}ul{border:1px solid var(--border);background:var(--panel);border-radius:8px;padding:16px 16px 16px 34px;max-width:520px}`
}

func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	upstreamOK := false
	upstreamMessage := "not configured"
	qbitOK := false
	qbitMessage := "not configured"
	if s.deps.AudiobookBayClient != nil {
		if err := s.deps.AudiobookBayClient.Ping(ctx); err != nil {
			upstreamMessage = err.Error()
		} else {
			upstreamOK = true
			upstreamMessage = "AudiobookBay reachable"
		}
		if s.deps.AudiobookBayClient.QBitConfigured() {
			if err := s.deps.AudiobookBayClient.PingQBit(ctx); err != nil {
				qbitMessage = err.Error()
			} else {
				qbitOK = true
				qbitMessage = "qBittorrent reachable"
			}
		}
	}
	dbOK := false
	dbMessage := "not configured"
	var stats any = map[string]any{}
	var recent any = []any{}
	downloadMode := ""
	if s.deps.AudiobookBayClient != nil {
		downloadMode = s.deps.AudiobookBayClient.DownloadMode()
	}
	if s.deps.Store != nil {
		if err := s.deps.Store.Pool().Ping(ctx); err != nil {
			dbMessage = err.Error()
		} else {
			dbOK = true
			dbMessage = "database reachable"
		}
		if requestStats, err := s.deps.Store.RequestStats(ctx); err == nil {
			stats = requestStats
		}
		if recentRows, err := s.deps.Store.ListRecent(ctx, 10); err == nil {
			recent = recentRows
		}
	}
	writeJSON(w, 200, map[string]any{
		"plugin_id":     "continuum.audiobookbay-requests",
		"role":          "request_provider",
		"configured":    s.deps.Config.Configured(),
		"base_url":      s.deps.Config.BaseURL,
		"download_mode": downloadMode,
		"features":      []string{"external_search", "request_snapshot", "admin_diagnostics", "provider_test_search"},
		"upstream": map[string]any{
			"ok":      upstreamOK,
			"message": upstreamMessage,
		},
		"qbittorrent": map[string]any{
			"configured": s.deps.AudiobookBayClient != nil && s.deps.AudiobookBayClient.QBitConfigured(),
			"ok":         qbitOK,
			"message":    qbitMessage,
		},
		"embedded": map[string]any{
			"configured":   s.deps.AudiobookBayClient != nil && s.deps.AudiobookBayClient.EmbeddedConfigured(),
			"download_dir": s.deps.Config.EmbeddedDownloadDir,
		},
		"database": map[string]any{
			"ok":      dbOK,
			"message": dbMessage,
		},
		"requests":        stats,
		"recent_requests": recent,
	})
}

func (s *Server) handleTestSearch(w http.ResponseWriter, r *http.Request) {
	if s.deps.AudiobookBayClient == nil {
		writeJSON(w, 503, map[string]any{"ok": false, "message": "not configured"})
		return
	}
	query := r.URL.Query().Get("q")
	if query == "" {
		query = "foundation"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	hits, err := s.deps.AudiobookBayClient.ExternalSearch(ctx, query, 5)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "message": err.Error(), "items": []any{}})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "search completed", "items": hits})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
