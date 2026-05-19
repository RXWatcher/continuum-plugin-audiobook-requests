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
		r.Get("/admin/config", s.handleGetConfig)
		r.Patch("/admin/config", s.handleUpdateConfig)
		r.Get("/admin/test-search", s.handleTestSearch)
		if s.deps.AudiobookBayClient != nil {
			catalog.NewHandler(s.deps.AudiobookBayClient).Mount(r)
		}
	})
	return r
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "store not configured"})
		return
	}
	cfg, err := s.deps.Store.GetAppConfig(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	cfg.QBitPassword = ""
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "store not configured"})
		return
	}
	cur, err := s.deps.Store.GetAppConfig(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var next runtime.Config
	if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if next.QBitPassword == "" {
		next.QBitPassword = cur.QBitPassword
	}
	if err := s.deps.Store.UpdateAppConfig(r.Context(), next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
<nav class="tabs" aria-label="AudiobookBay admin sections">
<button class="tab active" data-tab-target="readiness" type="button">Readiness</button>
<button class="tab" data-tab-target="config" type="button">Config</button>
<button class="tab" data-tab-target="search-lab" type="button">Search lab</button>
<button class="tab" data-tab-target="download-queue" type="button">Download queue</button>
<button class="tab" data-tab-target="guardrails" type="button">Guardrails</button>
</nav>
<section class="tab-panel active" id="readiness">
<article class="panel"><div class="panel-head"><div><h2>Setup status</h2><p class="muted">Confirms database, upstream mirror, and download mode before requests are routed here.</p></div><span id="ready-badge" class="badge">Loading</span></div><div id="status" class="cards muted">Loading diagnostics...</div></article>
</section>
<section class="tab-panel" id="config">
<article class="panel"><div class="panel-head"><div><h2>Plugin config</h2><p class="muted">AudiobookBay mirror, download mode, qBittorrent, embedded torrent, tracker, and search settings live in this plugin database.</p></div><span id="config-state" class="badge">Loading</span></div><form id="config-form" class="config-grid"><label>Base URL<input id="cfg-base-url" placeholder="https://audiobookbay.lu"></label><label>Download mode<input id="cfg-download-mode" placeholder="scrape_only"></label><label>qBittorrent URL<input id="cfg-qbit-url" placeholder="http://qbittorrent:8080"></label><label>qBittorrent username<input id="cfg-qbit-user"></label><label>qBittorrent password<input id="cfg-qbit-pass" type="password" placeholder="Leave blank to keep current password"></label><label>qBittorrent category<input id="cfg-qbit-category"></label><label>qBittorrent save path<input id="cfg-qbit-save-path"></label><label>Embedded download dir<input id="cfg-embedded-dir"></label><label>Embedded listen port<input id="cfg-embedded-port" type="number" min="0" max="65535"></label><label>Search limit<input id="cfg-search-limit" type="number" min="1" max="100"></label><label class="span-all">Fallback trackers JSON<textarea id="cfg-trackers" rows="5" placeholder='["udp://tracker.opentrackr.org:1337/announce"]'></textarea></label><button type="submit">Save config</button></form><pre id="config-output" class="output">Loading config...</pre></article>
</section>
<section class="tab-panel" id="search-lab">
<article class="panel"><div class="panel-head"><div><h2>Provider test</h2><p class="muted">Run a query, inspect candidates, and verify magnet or info-hash readiness before switching user traffic.</p></div></div><form id="search-form" class="row"><input id="q" value="foundation" placeholder="Search title or author" aria-label="Search query"><button type="submit">Test search</button></form><pre id="search-output" class="output">No test run yet.</pre><div class="triage-grid"><div><h3>Score explanation</h3><p>Search results are ranked by AudiobookBay parser quality, info-hash availability, and title relevance.</p></div><div><h3>Magnet readiness</h3><p>Entries with a magnet or info hash can skip a second resolution hop and hand off faster to the downloader.</p></div><div><h3>Mirror health</h3><p>A single failed search often means the mirror changed shape, blocked traffic, or served a captcha.</p></div></div></article>
</section>
<section class="tab-panel" id="download-queue">
<article class="panel"><div class="panel-head"><div><h2>Download queue</h2><p class="muted">Watch qBittorrent or embedded jobs, save paths, and stalled progress before blaming the portal.</p></div></div><div id="queue-output" class="cards muted">Loading request snapshot...</div></article>
</section>
<section class="tab-panel" id="guardrails">
<article class="panel"><div class="panel-head"><div><h2>Download triage</h2><p class="muted"><strong>Policy guardrails</strong> keep the operational mode obvious: scrape-only, qBittorrent handoff, or embedded torrent mode.</p></div></div><div class="triage-grid">
<div><h3>Scraper failures</h3><p>Search failures usually mean the AudiobookBay mirror changed, blocked traffic, or returned a captcha. Test a known title before routing new requests here.</p></div>
<div><h3>qBittorrent queue</h3><p>If requests are queued but never download, verify qBittorrent reachability, category, save path, and that the selected torrent has peers.</p></div>
<div><h3>Embedded mode</h3><p>Embedded mode runs torrents in the plugin process. Watch disk, ports, and long-running jobs; prefer qBittorrent for production handoff.</p></div>
</div></section>
</section>
<section class="panel"><h2>Operations checklist</h2><ul><li>Configure <code>database_url</code>, <code>base_url</code>, and the intended download mode.</li><li>For qBittorrent mode, verify qBittorrent reachability before approving user requests.</li><li>Select this plugin as the Audiobooks request provider.</li><li>Use recent request status when diagnosing stalled downloads.</li></ul></section>
</main>
<script>
const statusEl=document.getElementById("status"), output=document.getElementById("search-output"), queueOutput=document.getElementById("queue-output"), configOutput=document.getElementById("config-output"), configState=document.getElementById("config-state");
const hostToken=new URLSearchParams(location.search).get("token")||"";
function headers(){return hostToken?{Authorization:"Bearer "+hostToken}:{}}
function badge(ok){return '<span class="badge '+(ok?'ok':'bad')+'">'+(ok?'OK':'Needs attention')+'</span>'}
function esc(v){return String(v??"").replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]))}
function activateTab(id){document.querySelectorAll(".tab").forEach(b=>b.classList.toggle("active",b.dataset.tabTarget===id));document.querySelectorAll(".tab-panel").forEach(p=>p.classList.toggle("active",p.id===id))}
document.querySelectorAll(".tab").forEach(b=>b.addEventListener("click",()=>activateTab(b.dataset.tabTarget)))
async function loadConfig(){try{const r=await fetch("./api/v1/admin/config",{headers:headers()});const d=await r.json();if(!r.ok)throw new Error(d.error||r.statusText);document.getElementById("cfg-base-url").value=d.base_url||"";document.getElementById("cfg-download-mode").value=d.download_mode||"scrape_only";document.getElementById("cfg-qbit-url").value=d.qbittorrent_url||"";document.getElementById("cfg-qbit-user").value=d.qbittorrent_username||"";document.getElementById("cfg-qbit-category").value=d.qbittorrent_category||"";document.getElementById("cfg-qbit-save-path").value=d.qbittorrent_save_path||"";document.getElementById("cfg-embedded-dir").value=d.embedded_download_dir||"";document.getElementById("cfg-embedded-port").value=d.embedded_listen_port||0;document.getElementById("cfg-search-limit").value=d.search_limit||10;document.getElementById("cfg-trackers").value=JSON.stringify(d.trackers||[],null,2);configState.textContent="Loaded";configOutput.textContent=JSON.stringify(d,null,2)}catch(e){configState.textContent="Unavailable";configOutput.textContent=String(e)}}
async function load(){try{const r=await fetch("./api/v1/admin/diagnostics",{headers:headers()});const d=await r.json();const ready=d.configured&&d.database?.ok&&d.upstream?.ok;document.getElementById("ready-badge").textContent=ready?"Ready":"Needs attention";statusEl.innerHTML='<div class="diag">'+badge(d.configured)+'<strong>Configured</strong><span>base_url, db, and download mode applied</span></div><div class="diag">'+badge(d.database?.ok)+'<strong>Database</strong><span>'+esc(d.database?.message)+'</span></div><div class="diag">'+badge(d.upstream?.ok)+'<strong>AudiobookBay</strong><span>'+esc(d.upstream?.message)+'</span></div><div class="diag">'+badge(!d.qbittorrent?.configured||d.qbittorrent?.ok)+'<strong>qBittorrent</strong><span>'+esc(d.qbittorrent?.message)+'</span></div><div class="diag">'+badge(d.embedded?.configured||!d.embedded?.configured)+'<strong>Embedded</strong><span>'+esc(d.embedded?.download_dir||"not configured")+'</span></div>';queueOutput.innerHTML='<div class="diag"><strong>Mode</strong><span>'+esc(d.download_mode||"not set")+'</span></div><div class="diag"><strong>Recent requests</strong><span>'+esc(JSON.stringify(d.recent_requests||[],null,2))+'</span></div><div class="diag"><strong>Request stats</strong><span>'+esc(JSON.stringify(d.requests||{},null,2))+'</span></div>'}catch(e){statusEl.textContent=String(e);queueOutput.textContent=String(e)}} 
document.getElementById("config-form").addEventListener("submit",async e=>{e.preventDefault();configState.textContent="Saving";try{const body={base_url:document.getElementById("cfg-base-url").value.trim(),download_mode:document.getElementById("cfg-download-mode").value.trim()||"scrape_only",qbittorrent_url:document.getElementById("cfg-qbit-url").value.trim(),qbittorrent_username:document.getElementById("cfg-qbit-user").value,qbittorrent_password:document.getElementById("cfg-qbit-pass").value,qbittorrent_category:document.getElementById("cfg-qbit-category").value,qbittorrent_save_path:document.getElementById("cfg-qbit-save-path").value,embedded_download_dir:document.getElementById("cfg-embedded-dir").value,embedded_listen_port:Number(document.getElementById("cfg-embedded-port").value||0),search_limit:Number(document.getElementById("cfg-search-limit").value||10),trackers:JSON.parse(document.getElementById("cfg-trackers").value||"[]")};const r=await fetch("./api/v1/admin/config",{method:"PATCH",headers:{...headers(),"Content-Type":"application/json"},body:JSON.stringify(body)});const d=await r.json();if(!r.ok)throw new Error(d.error||r.statusText);document.getElementById("cfg-qbit-pass").value="";configState.textContent="Saved";configOutput.textContent=JSON.stringify(d,null,2);await loadConfig()}catch(err){configState.textContent="Error";configOutput.textContent=String(err)}})
document.getElementById("search-form").addEventListener("submit",async e=>{e.preventDefault();output.textContent="Searching...";try{const q=encodeURIComponent(document.getElementById("q").value||"foundation");const r=await fetch("./api/v1/admin/test-search?q="+q,{headers:headers()});output.textContent=JSON.stringify(await r.json(),null,2)}catch(err){output.textContent=String(err)}})
load();loadConfig();
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
	return `:root{--bg:#141417;--fg:#e8e8ec;--muted:#a1a1aa;--link:#93c5fd;--panel:#1c1c20;--border:#28282e;--ok:#22c55e;--bad:#fb7185;--input:#101014}[data-theme="cinema-light"]{--bg:#f7f3ed;--fg:#201c18;--muted:#756b60;--link:#9a3412;--panel:#fffaf3;--border:#ded1c0;--input:#fff}[data-theme="cobalt-studio"]{--bg:#101623;--fg:#eef4ff;--muted:#afc2e2;--link:#60a5fa;--panel:#172033;--border:#2d3f61;--input:#0d1422}[data-theme="oxblood-noir"]{--bg:#170b10;--fg:#fff1f4;--muted:#f0a6b7;--link:#fb7185;--panel:#241018;--border:#4a2230;--input:#12070b}[data-theme="evergreen-studio"]{--bg:#0d1712;--fg:#ecfdf3;--muted:#9bd6b4;--link:#6ee7b7;--panel:#14241b;--border:#2b4b39;--input:#08110d}*{box-sizing:border-box}body{font-family:system-ui,sans-serif;margin:0;line-height:1.5;background:var(--bg);color:var(--fg)}.shell{max-width:1120px;margin:0 auto;padding:28px}.back{display:inline-flex;margin-bottom:12px;color:var(--link);text-decoration:none}.eyebrow{color:var(--muted);text-transform:uppercase;font-size:12px;letter-spacing:.08em}h1{margin:.2rem 0}h2{font-size:16px;margin:0}.tabs{display:flex;gap:8px;flex-wrap:wrap;margin:18px 0}.tab{background:transparent;color:var(--fg);border:1px solid var(--border)}.tab.active{background:var(--link);color:#08111f}.tab-panel{display:none}.tab-panel.active{display:block}.grid,.triage-grid,.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:16px}.panel{border:1px solid var(--border);background:var(--panel);border-radius:8px;padding:16px;margin-top:16px}.panel-head{display:flex;align-items:flex-start;justify-content:space-between;gap:16px}.triage-grid h3{font-size:14px;margin:.2rem 0}.triage-grid p{color:var(--muted);margin:.25rem 0}.stack>*+*{margin-top:8px}.row{display:grid;grid-template-columns:minmax(0,1fr) auto}.config-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px;margin-top:12px}.config-grid label{display:grid;gap:6px;color:var(--muted);font-size:13px}.config-grid .span-all{grid-column:1/-1}.diag{display:grid;gap:4px;border:1px solid var(--border);border-radius:6px;background:var(--input);padding:12px}.diag strong{color:var(--fg)}.diag span{color:var(--muted);font-size:12px}textarea,input{min-width:0;background:var(--input);color:var(--fg);border:1px solid var(--border);border-radius:6px;padding:9px}textarea{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;resize:vertical}button{background:var(--link);border:0;border-radius:6px;padding:9px 12px;color:#08111f;font-weight:700;cursor:pointer}.badge{display:inline-block;border:1px solid var(--border);border-radius:999px;padding:2px 8px;margin-right:6px;font-size:12px;white-space:nowrap}.ok{color:var(--ok)}.bad{color:var(--bad)}.muted{color:var(--muted)}.output{overflow:auto;max-height:340px;background:var(--input);border:1px solid var(--border);border-radius:6px;padding:10px;color:var(--fg)}code{color:var(--link)}@media(max-width:760px){.row,.panel-head,.config-grid{grid-template-columns:1fr;display:grid}}`
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
		"configured":    s.deps.Config.ProviderConfigured(),
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
