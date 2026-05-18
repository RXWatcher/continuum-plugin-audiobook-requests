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
<main class="shell">
<a class="back" href="/admin/plugins">&larr; Plugins</a>
<header><p class="eyebrow">Audiobook request provider</p><h1>AudiobookBay Requests</h1><p>Search, magnet resolution, download handoff, and request reconciliation for the Audiobooks portal.</p></header>
<section class="grid">
<article class="panel"><h2>Setup status</h2><div id="status" class="stack muted">Loading diagnostics...</div></article>
<article class="panel"><h2>Provider test</h2><form id="search-form" class="row"><input id="q" value="foundation" placeholder="Search title or author"><button type="submit">Test search</button></form><pre id="search-output" class="output">No test run yet.</pre></article>
</section>
<section class="panel"><h2>Operations checklist</h2><ul><li>Configure <code>database_url</code>, <code>base_url</code>, and the intended download mode.</li><li>For qBittorrent mode, verify qBittorrent reachability before approving user requests.</li><li>Select this plugin as the Audiobooks request provider.</li><li>Use recent request status when diagnosing stalled downloads.</li></ul></section>
</main>
<script>
const statusEl=document.getElementById("status"), output=document.getElementById("search-output");
const hostToken=new URLSearchParams(location.search).get("token")||"";
function headers(){return hostToken?{Authorization:"Bearer "+hostToken}:{}}
function badge(ok){return '<span class="badge '+(ok?'ok':'bad')+'">'+(ok?'OK':'Needs attention')+'</span>'}
function esc(v){return String(v??"").replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]))}
async function load(){try{const r=await fetch("./api/v1/admin/diagnostics",{headers:headers()});const d=await r.json();statusEl.innerHTML='<div>'+badge(d.configured)+' Configured</div><div>'+badge(d.database?.ok)+' Database: '+esc(d.database?.message)+'</div><div>'+badge(d.upstream?.ok)+' AudiobookBay: '+esc(d.upstream?.message)+'</div><div>'+badge(!d.qbittorrent?.configured||d.qbittorrent?.ok)+' qBittorrent: '+esc(d.qbittorrent?.message)+'</div><div class="muted">Mode: '+esc(d.download_mode||"not set")+'</div><pre class="output">'+esc(JSON.stringify({requests:d.requests,recent:d.recent_requests},null,2))+'</pre>'}catch(e){statusEl.textContent=String(e)}} 
document.getElementById("search-form").addEventListener("submit",async e=>{e.preventDefault();output.textContent="Searching...";try{const q=encodeURIComponent(document.getElementById("q").value||"foundation");const r=await fetch("./api/v1/admin/test-search?q="+q,{headers:headers()});output.textContent=JSON.stringify(await r.json(),null,2)}catch(err){output.textContent=String(err)}})
load();
</script>
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
	return `:root{--bg:#141417;--fg:#e8e8ec;--muted:#a1a1aa;--link:#93c5fd;--panel:#1c1c20;--border:#28282e;--ok:#22c55e;--bad:#fb7185;--input:#101014}[data-theme="cinema-light"]{--bg:#f7f3ed;--fg:#201c18;--muted:#756b60;--link:#9a3412;--panel:#fffaf3;--border:#ded1c0;--input:#fff}[data-theme="cobalt-studio"]{--bg:#101623;--fg:#eef4ff;--muted:#afc2e2;--link:#60a5fa;--panel:#172033;--border:#2d3f61;--input:#0d1422}[data-theme="oxblood-noir"]{--bg:#170b10;--fg:#fff1f4;--muted:#f0a6b7;--link:#fb7185;--panel:#241018;--border:#4a2230;--input:#12070b}[data-theme="evergreen-studio"]{--bg:#0d1712;--fg:#ecfdf3;--muted:#9bd6b4;--link:#6ee7b7;--panel:#14241b;--border:#2b4b39;--input:#08110d}body{font-family:system-ui,sans-serif;margin:0;line-height:1.5;background:var(--bg);color:var(--fg)}.shell{max-width:1120px;margin:0 auto;padding:28px}.back{color:var(--link);text-decoration:none}.eyebrow{color:var(--muted);text-transform:uppercase;font-size:12px;letter-spacing:.08em}h1{margin:.2rem 0}h2{font-size:16px}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:16px}.panel{border:1px solid var(--border);background:var(--panel);border-radius:8px;padding:16px;margin-top:16px}.stack>*+*{margin-top:8px}.row{display:flex;gap:8px}input{min-width:0;flex:1;background:var(--input);color:var(--fg);border:1px solid var(--border);border-radius:6px;padding:9px}button{background:var(--link);border:0;border-radius:6px;padding:9px 12px;color:#08111f;font-weight:700}.badge{display:inline-block;border:1px solid var(--border);border-radius:999px;padding:2px 8px;margin-right:6px;font-size:12px}.ok{color:var(--ok)}.bad{color:var(--bad)}.muted{color:var(--muted)}.output{overflow:auto;max-height:340px;background:var(--input);border:1px solid var(--border);border-radius:6px;padding:10px;color:var(--fg)}code{color:var(--link)}`
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
		writeJSON(w, 200, map[string]any{"ok": false, "message": "not configured", "items": []any{}})
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
