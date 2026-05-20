// Package server constructs the chi-based HTTP handler.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/audiobookbay"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/catalog"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/reconciler"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/runtime"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/store"
)

// stuckThreshold flags non-terminal rows with no progress in the trailing
// window so the admin UI can highlight them. Tuned to the 1-min cron — older
// than this is stalled, not slow.
const stuckThreshold = 24 * time.Hour

type Deps struct {
	AudiobookBayClient *audiobookbay.Client
	Store              *store.Store
	Reconciler         *reconciler.Reconciler // nil before Configure runs
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
		r.Get("/admin/requests", s.handleListRequests)
		r.Get("/admin/requests/stuck", s.handleListStuckRequests)
		r.Post("/admin/requests/{id}/retry", s.handleRetryRequest)
		r.Post("/admin/requests/{id}/mark-failed", s.handleMarkFailedRequest)
		r.Get("/admin/reconciler/status", s.handleReconcilerStatus)
		r.Post("/admin/reconciler/run", s.handleReconcilerRun)
		if s.deps.AudiobookBayClient != nil {
			catalog.NewHandler(s.deps.AudiobookBayClient).Mount(r)
		}
	})
	return r
}

// requestRow is the JSON wire shape for the admin queue table. Includes
// scraper-specific fields (selected_title, info_hash) so operators can spot
// why a particular pick was made.
type requestRow struct {
	RequestID     string `json:"requestId"`
	ExternalID    string `json:"externalId,omitempty"`
	Status        string `json:"status"`
	SearchQuery   string `json:"searchQuery,omitempty"`
	SelectedTitle string `json:"selectedTitle,omitempty"`
	InfoHash      string `json:"infoHash,omitempty"`
	SelectedScore int    `json:"selectedScore,omitempty"`
	ErrorText     string `json:"errorText,omitempty"`
	LastPolledAt  string `json:"lastPolledAt,omitempty"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
	StuckDuration string `json:"stuckDuration,omitempty"`
}

func toRequestRow(r store.ForwardedRequest) requestRow {
	out := requestRow{
		RequestID:     r.RequestID,
		ExternalID:    r.ExternalID,
		Status:        r.Status,
		SearchQuery:   r.SearchQuery,
		SelectedTitle: r.SelectedTitle,
		InfoHash:      r.InfoHash,
		SelectedScore: r.SelectedScore,
		ErrorText:     r.ErrorText,
		CreatedAt:     r.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:     r.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if !r.LastPolled.IsZero() && r.LastPolled.Year() > 1 {
		out.LastPolledAt = r.LastPolled.UTC().Format(time.RFC3339)
	}
	return out
}

func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "store not configured"})
		return
	}
	q := r.URL.Query()
	limit := atoiOr(q.Get("limit"), 50)
	page := atoiOr(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	rows, total, err := s.deps.Store.ListRequests(r.Context(), q.Get("status"), q.Get("q"), limit, (page-1)*limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]requestRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, toRequestRow(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": out, "total": total, "limit": limit, "page": page})
}

func (s *Server) handleListStuckRequests(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "store not configured"})
		return
	}
	rows, err := s.deps.Store.ListStuck(r.Context(), stuckThreshold, 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	now := time.Now()
	out := make([]requestRow, 0, len(rows))
	for _, row := range rows {
		rr := toRequestRow(row)
		ref := row.LastPolled
		if ref.IsZero() || ref.Year() <= 1 {
			ref = row.CreatedAt
		}
		rr.StuckDuration = now.Sub(ref).Round(time.Minute).String()
		out = append(out, rr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": out, "thresholdHours": int(stuckThreshold / time.Hour)})
}

func (s *Server) handleRetryRequest(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "store not configured"})
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.deps.Store.RetryRequest(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "request not found or has no upstream id"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMarkFailedRequest(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "store not configured"})
		return
	}
	id := chi.URLParam(r, "id")
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.deps.Store.MarkFailedManually(r.Context(), id, body.Reason); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "request not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleReconcilerStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.Reconciler == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "reason": "plugin not configured"})
		return
	}
	st := s.deps.Reconciler.LastStatus()
	resp := map[string]any{"available": true, "rowsProcessed": st.RowsProcessed, "skipped": st.Skipped}
	if !st.LastRunAt.IsZero() {
		resp["lastRunAt"] = st.LastRunAt.UTC().Format(time.RFC3339)
		resp["lastDurationMs"] = st.LastDuration.Milliseconds()
	}
	if st.LastError != "" {
		resp["lastError"] = st.LastError
	}
	if s.deps.Store != nil {
		stuck, err := s.deps.Store.ListStuck(r.Context(), stuckThreshold, 200)
		if err == nil {
			resp["stuckCount"] = len(stuck)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleReconcilerRun(w http.ResponseWriter, r *http.Request) {
	if s.deps.Reconciler == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "reconciler not configured"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.deps.Reconciler.Tick(ctx); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
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
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Audiobook Requests</title><style>` + adminThemeCSS() + `</style></head>
<body>
<main class="shell">
<a class="back" href="/admin/plugins">&larr; Plugins</a>
<header><p class="eyebrow">Audiobook request provider</p><h1>Audiobook Requests</h1><p>Search, magnet resolution, download handoff, and request reconciliation for the Audiobooks portal.</p></header>
<nav class="tabs" aria-label="AudiobookBay admin sections">
<button class="tab active" data-tab-target="readiness" type="button">Readiness</button>
<button class="tab" data-tab-target="config" type="button">Config</button>
<button class="tab" data-tab-target="search-lab" type="button">Search lab</button>
<button class="tab" data-tab-target="download-queue" type="button">Download queue</button>
<button class="tab" data-tab-target="guardrails" type="button">Guardrails</button>
</nav>
<section class="tab-panel active" id="readiness">
<article class="panel"><div class="panel-head"><div><h2>Setup status</h2><p class="muted">Confirms database, upstream mirror, and download mode before requests are routed here.</p></div><span id="ready-badge" class="badge">Loading</span></div><div id="status" class="cards muted">Loading diagnostics...</div></article>
<article class="panel"><div class="panel-head"><div><h2>Reconciler</h2><p class="muted">Polls non-terminal requests every minute. Use Run now to trigger an unscheduled poll after fixing the mirror or qBittorrent.</p></div><button id="reconcile-now" type="button">Run now</button></div><div id="reconciler-status" class="cards muted">Loading reconciler status...</div></article>
</section>
<section class="tab-panel" id="config">
<div id="embedded-warning" class="panel" style="display:none;border-color:var(--bad);background:rgba(251,113,133,0.08)">
<div class="panel-head"><div><h2 style="color:var(--bad)">Embedded download mode is selected</h2><p class="muted">The plugin process is running the torrent client itself. It will write to <code id="embedded-warn-dir">—</code>, listen on a peer port, hold socket and connection-tracking state, and use disk and bandwidth in proportion to active downloads. qBittorrent mode is the recommended production handoff. Embedded is best for development or single-user installs.</p></div></div>
<div id="embedded-warning-status" class="muted" style="font-size:12px">—</div>
</div>
<article class="panel"><div class="panel-head"><div><h2>Plugin config</h2><p class="muted">AudiobookBay mirror, download mode, qBittorrent, embedded torrent, tracker, and search settings live in this plugin database.</p></div><span id="config-state" class="badge">Loading</span></div><form id="config-form" class="config-grid"><label>Base URL<input id="cfg-base-url" placeholder="https://audiobookbay.lu"></label><label>Download mode<input id="cfg-download-mode" placeholder="scrape_only"></label><label>qBittorrent URL<input id="cfg-qbit-url" placeholder="http://qbittorrent:8080"></label><label>qBittorrent username<input id="cfg-qbit-user"></label><label>qBittorrent password<input id="cfg-qbit-pass" type="password" placeholder="Leave blank to keep current password"></label><label>qBittorrent category<input id="cfg-qbit-category"></label><label>qBittorrent save path<input id="cfg-qbit-save-path"></label><label>Embedded download dir<input id="cfg-embedded-dir"></label><label>Embedded listen port<input id="cfg-embedded-port" type="number" min="0" max="65535"></label><label>Embedded max concurrent torrents<input id="cfg-embedded-max" type="number" min="0" max="64" placeholder="0 = default (8)"></label><label>Search limit<input id="cfg-search-limit" type="number" min="1" max="100"></label><label class="span-all">Fallback trackers JSON<textarea id="cfg-trackers" rows="5" placeholder='["udp://tracker.opentrackr.org:1337/announce"]'></textarea></label><button type="submit">Save config</button></form><pre id="config-output" class="output">Loading config...</pre></article>
</section>
<section class="tab-panel" id="search-lab">
<article class="panel"><div class="panel-head"><div><h2>Provider test</h2><p class="muted">Run a query, inspect candidates, and verify magnet or info-hash readiness before switching user traffic.</p></div></div><form id="search-form" class="row"><input id="q" value="foundation" placeholder="Search title or author" aria-label="Search query"><button type="submit">Test search</button></form><pre id="search-output" class="output">No test run yet.</pre><div class="triage-grid"><div><h3>Score explanation</h3><p>Search results are ranked by AudiobookBay parser quality, info-hash availability, and title relevance.</p></div><div><h3>Magnet readiness</h3><p>Entries with a magnet or info hash can skip a second resolution hop and hand off faster to the downloader.</p></div><div><h3>Mirror health</h3><p>A single failed search often means the mirror changed shape, blocked traffic, or served a captcha.</p></div></div></article>
</section>
<section class="tab-panel" id="download-queue">
<article class="panel">
<div class="panel-head"><div><h2>Download queue</h2><p class="muted">Per-request status with retry and force-fail actions. Aggregate stats live on the Readiness tab.</p></div></div>
<div class="row" style="grid-template-columns:auto auto minmax(0,1fr) auto auto;gap:8px;align-items:center;margin-top:8px">
<label class="muted" style="font-size:12px">Status <select id="queue-status"><option value="">all</option><option>submitted</option><option value="acknowledged">acknowledged</option><option>queued</option><option>downloading</option><option>imported</option><option>failed</option></select></label>
<input id="queue-search" placeholder="Search title / id / hash" aria-label="Search">
<button id="queue-refresh" type="button">Refresh</button>
<span id="queue-page" class="muted" style="font-size:12px;text-align:right">—</span>
</div>
<div id="queue-table" class="output" style="margin-top:12px;max-height:520px;padding:0">Loading queue…</div>
</article>
<article class="panel" id="stuck-panel" style="display:none">
<div class="panel-head"><div><h2 style="color:var(--bad)">Stuck requests</h2><p class="muted">Non-terminal requests with no progress in 24h. Likely mirror block, captcha, dead seeders, or qBittorrent disconnect.</p></div></div>
<div id="stuck-table" class="output" style="padding:0">—</div>
</article>
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
const statusEl=document.getElementById("status"), output=document.getElementById("search-output"), configOutput=document.getElementById("config-output"), configState=document.getElementById("config-state"), reconcilerStatusEl=document.getElementById("reconciler-status"), queueTableEl=document.getElementById("queue-table"), queuePageEl=document.getElementById("queue-page"), stuckPanelEl=document.getElementById("stuck-panel"), stuckTableEl=document.getElementById("stuck-table");
const url=new URL(location.href);
const hostToken=url.searchParams.get("token")||"";
if(url.searchParams.has("token")){url.searchParams.delete("token");history.replaceState(null,"",url.toString());}
let queueState={status:"",q:"",page:1,limit:25};
function headers(){return hostToken?{Authorization:"Bearer "+hostToken}:{}}
function el(tag,attrs,text){const e=document.createElement(tag);if(attrs){for(const k in attrs){if(k==="class")e.className=attrs[k];else if(k==="dataset"){for(const d in attrs.dataset)e.dataset[d]=attrs.dataset[d];}else if(k==="disabled"){if(attrs[k])e.disabled=true;}else e.setAttribute(k,attrs[k]);}}if(text!==undefined&&text!==null)e.textContent=String(text);return e;}
function clear(node){while(node.firstChild)node.removeChild(node.firstChild);}
function diag(title,value,tone){const wrap=el("div",{class:"diag"});wrap.appendChild(el("strong",null,title));const sp=el("span",null,value??"—");if(tone)sp.classList.add(tone);wrap.appendChild(sp);return wrap;}
function diagBadge(ok,title,detail){const wrap=el("div",{class:"diag"});wrap.appendChild(el("span",{class:"badge "+(ok?"ok":"bad")},ok?"OK":"Needs attention"));wrap.appendChild(el("strong",null,title));wrap.appendChild(el("span",null,detail||""));return wrap;}
function relAge(iso){if(!iso)return "—";const ms=Date.now()-new Date(iso).getTime();if(!isFinite(ms)||ms<0)return "—";const s=Math.floor(ms/1000);if(s<60)return s+"s";const m=Math.floor(s/60);if(m<60)return m+"m";const h=Math.floor(m/60);if(h<24)return h+"h";const d=Math.floor(h/24);if(d<30)return d+"d";const mo=Math.floor(d/30);return mo<12?mo+"mo":Math.floor(mo/12)+"y";}
function statusTone(s){if(s==="imported")return "ok";if(s==="failed")return "bad";return "";}
function activateTab(id){document.querySelectorAll(".tab").forEach(b=>b.classList.toggle("active",b.dataset.tabTarget===id));document.querySelectorAll(".tab-panel").forEach(p=>p.classList.toggle("active",p.id===id));if(id==="download-queue"){loadQueue();}}
document.querySelectorAll(".tab").forEach(b=>b.addEventListener("click",()=>activateTab(b.dataset.tabTarget)))
async function loadConfig(){try{const r=await fetch("./api/v1/admin/config",{headers:headers()});const d=await r.json();if(!r.ok)throw new Error(d.error||r.statusText);document.getElementById("cfg-base-url").value=d.base_url||"";document.getElementById("cfg-download-mode").value=d.download_mode||"scrape_only";document.getElementById("cfg-qbit-url").value=d.qbittorrent_url||"";document.getElementById("cfg-qbit-user").value=d.qbittorrent_username||"";document.getElementById("cfg-qbit-category").value=d.qbittorrent_category||"";document.getElementById("cfg-qbit-save-path").value=d.qbittorrent_save_path||"";document.getElementById("cfg-embedded-dir").value=d.embedded_download_dir||"";document.getElementById("cfg-embedded-port").value=d.embedded_listen_port||0;document.getElementById("cfg-embedded-max").value=d.embedded_max_concurrent||0;document.getElementById("cfg-search-limit").value=d.search_limit||10;document.getElementById("cfg-trackers").value=JSON.stringify(d.trackers||[],null,2);configState.textContent="Loaded";configOutput.textContent=JSON.stringify(d,null,2);renderEmbeddedWarning(d);}catch(e){configState.textContent="Unavailable";configOutput.textContent=String(e)}}
function renderEmbeddedWarning(cfg){const warn=document.getElementById("embedded-warning");if((cfg.download_mode||"")!=="embedded"){warn.style.display="none";return;}warn.style.display="";document.getElementById("embedded-warn-dir").textContent=cfg.embedded_download_dir||"(not set)";document.getElementById("embedded-warning-status").textContent="Concurrent cap: "+(cfg.embedded_max_concurrent>0?cfg.embedded_max_concurrent:"8 (default)");}
async function loadDiagnostics(){try{const r=await fetch("./api/v1/admin/diagnostics",{headers:headers()});const d=await r.json();const ready=d.configured&&d.database?.ok&&d.upstream?.ok;document.getElementById("ready-badge").textContent=ready?"Ready":"Needs attention";const rs=d.requests||{};clear(statusEl);statusEl.appendChild(diagBadge(d.configured,"Configured","base_url, db, and download mode applied"));statusEl.appendChild(diagBadge(d.database?.ok,"Database",d.database?.message));statusEl.appendChild(diagBadge(d.upstream?.ok,"AudiobookBay",d.upstream?.message));statusEl.appendChild(diagBadge(!d.qbittorrent?.configured||d.qbittorrent?.ok,"qBittorrent",d.qbittorrent?.message));statusEl.appendChild(diag("Mode",d.download_mode||"not set"));statusEl.appendChild(diag("Requests",(rs.total||0)+" total · "+(rs.active||0)+" active · "+(rs.failed||0)+" failed · "+(rs.imported||0)+" imported"));}catch(e){clear(statusEl);statusEl.appendChild(el("span",{class:"bad"},String(e)));}}
async function loadReconciler(){try{const r=await fetch("./api/v1/admin/reconciler/status",{headers:headers()});const d=await r.json();clear(reconcilerStatusEl);if(!d.available){reconcilerStatusEl.appendChild(diag("Reconciler",d.reason||"not available"));return;}reconcilerStatusEl.appendChild(diag("Last run",d.lastRunAt?(relAge(d.lastRunAt)+" ago"+(d.lastDurationMs?(" · "+d.lastDurationMs+"ms"):"")):"never"));reconcilerStatusEl.appendChild(diag("Rows processed",d.rowsProcessed||0));reconcilerStatusEl.appendChild(diag("Stuck (>24h)",d.stuckCount||0,(d.stuckCount||0)>0?"bad":""));if(d.lastError){reconcilerStatusEl.appendChild(diag("Last error",d.lastError,"bad"));}if((d.stuckCount||0)>0){loadStuck();}else{stuckPanelEl.style.display="none";}}catch(e){clear(reconcilerStatusEl);reconcilerStatusEl.appendChild(el("span",{class:"bad"},String(e)));}}
async function loadStuck(){try{const r=await fetch("./api/v1/admin/requests/stuck",{headers:headers()});const d=await r.json();if(!d.rows||d.rows.length===0){stuckPanelEl.style.display="none";return;}stuckPanelEl.style.display="";renderRequestTable(stuckTableEl,d.rows,{stuckMode:true});}catch(e){clear(stuckTableEl);stuckTableEl.appendChild(el("span",{class:"bad"},String(e)));}}
document.getElementById("reconcile-now").addEventListener("click",async()=>{const btn=document.getElementById("reconcile-now");btn.disabled=true;btn.textContent="Running…";try{const r=await fetch("./api/v1/admin/reconciler/run",{method:"POST",headers:headers()});const d=await r.json();btn.textContent=d.ok?"Done":(d.error?"Error":"Done");setTimeout(()=>{btn.textContent="Run now";btn.disabled=false;loadReconciler();loadQueue();},800);}catch(e){btn.textContent="Error";setTimeout(()=>{btn.textContent="Run now";btn.disabled=false;},1500);}});
function renderRequestTable(host,rows,opts){opts=opts||{};clear(host);if(!rows.length){host.appendChild(el("div",{class:"muted",style:"padding:14px"},"No matching requests."));return;}const tbl=el("table",{class:"qtable"});const thead=el("thead");const trh=el("tr");["Request","Status","Title","Last polled","Age"].forEach(h=>trh.appendChild(el("th",null,h)));if(opts.stuckMode)trh.appendChild(el("th",null,"Stuck for"));trh.appendChild(el("th",null,"Error"));trh.appendChild(el("th",{style:"width:140px"},""));thead.appendChild(trh);tbl.appendChild(thead);const tbody=el("tbody");rows.forEach(r=>{const tr=el("tr");const tdId=el("td");tdId.appendChild(el("code",null,r.requestId));if(r.externalId)tdId.appendChild(el("div",{class:"muted",style:"font-size:11px"},"ext: "+r.externalId));if(r.infoHash)tdId.appendChild(el("div",{class:"muted",style:"font-size:11px"},"hash: "+r.infoHash.slice(0,12)+"…"));tr.appendChild(tdId);tr.appendChild(el("td",null)).appendChild(el("span",{class:"badge "+statusTone(r.status)},r.status));const tdTitle=el("td",{style:"max-width:260px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap"},r.selectedTitle||r.searchQuery||"—");if(r.selectedTitle||r.searchQuery)tdTitle.title=(r.selectedTitle||"")+(r.searchQuery?(" — query: "+r.searchQuery):"");tr.appendChild(tdTitle);tr.appendChild(el("td",{class:"muted",style:"font-size:12px"},r.lastPolledAt?(relAge(r.lastPolledAt)+" ago"):"never"));tr.appendChild(el("td",{class:"muted",style:"font-size:12px"},relAge(r.createdAt)));if(opts.stuckMode)tr.appendChild(el("td",{class:"bad",style:"font-size:12px"},r.stuckDuration||"—"));const tdErr=el("td",{class:"muted",style:"font-size:12px;max-width:260px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap"},r.errorText||"—");if(r.errorText)tdErr.title=r.errorText;tr.appendChild(tdErr);const tdAct=el("td",{style:"text-align:right;white-space:nowrap"});const terminal=(r.status==="imported"||r.status==="failed");if(r.externalId&&r.status!=="imported"){tdAct.appendChild(el("button",{dataset:{act:"retry",id:r.requestId},type:"button"},"Retry"));tdAct.appendChild(document.createTextNode(" "));}if(!terminal){tdAct.appendChild(el("button",{class:"danger",dataset:{act:"fail",id:r.requestId},type:"button"},"Fail"));}tr.appendChild(tdAct);tbody.appendChild(tr);});tbl.appendChild(tbody);host.appendChild(tbl);}
async function loadQueue(){clear(queueTableEl);queueTableEl.appendChild(el("div",{class:"muted",style:"padding:14px"},"Loading…"));try{const params=new URLSearchParams({status:queueState.status,q:queueState.q,limit:String(queueState.limit),page:String(queueState.page)});const r=await fetch("./api/v1/admin/requests?"+params.toString(),{headers:headers()});const d=await r.json();if(!r.ok)throw new Error(d.error||r.statusText);renderRequestTable(queueTableEl,d.rows||[],{});const total=d.total||0,page=d.page||1,limit=d.limit||queueState.limit,pages=Math.max(1,Math.ceil(total/limit));clear(queuePageEl);queuePageEl.appendChild(document.createTextNode(total+" total"));if(pages>1){queuePageEl.appendChild(document.createTextNode(" · page "+page+"/"+pages+" "));const prev=el("button",{type:"button",disabled:page<=1},"Prev");prev.addEventListener("click",()=>{queueState.page=Math.max(1,queueState.page-1);loadQueue();});const next=el("button",{type:"button",disabled:page>=pages},"Next");next.addEventListener("click",()=>{queueState.page=queueState.page+1;loadQueue();});queuePageEl.appendChild(prev);queuePageEl.appendChild(document.createTextNode(" "));queuePageEl.appendChild(next);}}catch(e){clear(queueTableEl);queueTableEl.appendChild(el("div",{class:"bad",style:"padding:14px"},String(e.message||e)));queuePageEl.textContent="—";}}
queueTableEl.addEventListener("click",async e=>{const t=e.target;if(!(t instanceof HTMLButtonElement))return;const id=t.dataset.id;const act=t.dataset.act;if(!id||!act)return;t.disabled=true;const orig=t.textContent;t.textContent="…";try{if(act==="retry"){const r=await fetch("./api/v1/admin/requests/"+encodeURIComponent(id)+"/retry",{method:"POST",headers:headers()});if(!r.ok){const d=await r.json().catch(()=>({}));throw new Error(d.error||r.statusText);}}else if(act==="fail"){if(!confirm("Mark this request failed? This is terminal; users see it as failed in the portal."))return;const reason=prompt("Reason (optional):","manual force-fail")||"";const r=await fetch("./api/v1/admin/requests/"+encodeURIComponent(id)+"/mark-failed",{method:"POST",headers:{...headers(),"Content-Type":"application/json"},body:JSON.stringify({reason})});if(!r.ok){const d=await r.json().catch(()=>({}));throw new Error(d.error||r.statusText);}}await loadQueue();}catch(err){alert(String(err.message||err));}finally{t.disabled=false;t.textContent=orig;}});
document.getElementById("queue-status").addEventListener("change",e=>{queueState.status=e.target.value;queueState.page=1;loadQueue();});
let searchTimer;
document.getElementById("queue-search").addEventListener("input",e=>{clearTimeout(searchTimer);searchTimer=setTimeout(()=>{queueState.q=e.target.value.trim();queueState.page=1;loadQueue();},300);});
document.getElementById("queue-refresh").addEventListener("click",loadQueue);
document.getElementById("config-form").addEventListener("submit",async e=>{e.preventDefault();configState.textContent="Saving";try{const body={base_url:document.getElementById("cfg-base-url").value.trim(),download_mode:document.getElementById("cfg-download-mode").value.trim()||"scrape_only",qbittorrent_url:document.getElementById("cfg-qbit-url").value.trim(),qbittorrent_username:document.getElementById("cfg-qbit-user").value,qbittorrent_password:document.getElementById("cfg-qbit-pass").value,qbittorrent_category:document.getElementById("cfg-qbit-category").value,qbittorrent_save_path:document.getElementById("cfg-qbit-save-path").value,embedded_download_dir:document.getElementById("cfg-embedded-dir").value,embedded_listen_port:Number(document.getElementById("cfg-embedded-port").value||0),embedded_max_concurrent:Number(document.getElementById("cfg-embedded-max").value||0),search_limit:Number(document.getElementById("cfg-search-limit").value||10),trackers:JSON.parse(document.getElementById("cfg-trackers").value||"[]")};const r=await fetch("./api/v1/admin/config",{method:"PATCH",headers:{...headers(),"Content-Type":"application/json"},body:JSON.stringify(body)});const d=await r.json();if(!r.ok)throw new Error(d.error||r.statusText);document.getElementById("cfg-qbit-pass").value="";configState.textContent="Saved";configOutput.textContent=JSON.stringify(d,null,2);await loadConfig()}catch(err){configState.textContent="Error";configOutput.textContent=String(err)}})
document.getElementById("search-form").addEventListener("submit",async e=>{e.preventDefault();output.textContent="Searching...";try{const q=encodeURIComponent(document.getElementById("q").value||"foundation");const r=await fetch("./api/v1/admin/test-search?q="+q,{headers:headers()});output.textContent=JSON.stringify(await r.json(),null,2)}catch(err){output.textContent=String(err)}})
loadDiagnostics();loadConfig();loadReconciler();
setInterval(loadReconciler,30000);
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
	return `:root{--bg:#141417;--fg:#e8e8ec;--muted:#a1a1aa;--link:#93c5fd;--panel:#1c1c20;--border:#28282e;--ok:#22c55e;--bad:#fb7185;--input:#101014}[data-theme="cinema-light"]{--bg:#f7f3ed;--fg:#201c18;--muted:#756b60;--link:#9a3412;--panel:#fffaf3;--border:#ded1c0;--input:#fff}[data-theme="cobalt-studio"]{--bg:#101623;--fg:#eef4ff;--muted:#afc2e2;--link:#60a5fa;--panel:#172033;--border:#2d3f61;--input:#0d1422}[data-theme="oxblood-noir"]{--bg:#170b10;--fg:#fff1f4;--muted:#f0a6b7;--link:#fb7185;--panel:#241018;--border:#4a2230;--input:#12070b}[data-theme="evergreen-studio"]{--bg:#0d1712;--fg:#ecfdf3;--muted:#9bd6b4;--link:#6ee7b7;--panel:#14241b;--border:#2b4b39;--input:#08110d}*{box-sizing:border-box}body{font-family:system-ui,sans-serif;margin:0;line-height:1.5;background:var(--bg);color:var(--fg)}.shell{max-width:1120px;margin:0 auto;padding:28px}.back{display:inline-flex;margin-bottom:12px;color:var(--link);text-decoration:none}.eyebrow{color:var(--muted);text-transform:uppercase;font-size:12px;letter-spacing:.08em}h1{margin:.2rem 0}h2{font-size:16px;margin:0}.tabs{display:flex;gap:8px;flex-wrap:wrap;margin:18px 0}.tab{background:transparent;color:var(--fg);border:1px solid var(--border)}.tab.active{background:var(--link);color:#08111f}.tab-panel{display:none}.tab-panel.active{display:block}.grid,.triage-grid,.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:16px}.panel{border:1px solid var(--border);background:var(--panel);border-radius:8px;padding:16px;margin-top:16px}.panel-head{display:flex;align-items:flex-start;justify-content:space-between;gap:16px}.triage-grid h3{font-size:14px;margin:.2rem 0}.triage-grid p{color:var(--muted);margin:.25rem 0}.stack>*+*{margin-top:8px}.row{display:grid;grid-template-columns:minmax(0,1fr) auto}.config-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px;margin-top:12px}.config-grid label{display:grid;gap:6px;color:var(--muted);font-size:13px}.config-grid .span-all{grid-column:1/-1}.diag{display:grid;gap:4px;border:1px solid var(--border);border-radius:6px;background:var(--input);padding:12px}.diag strong{color:var(--fg)}.diag span{color:var(--muted);font-size:12px}textarea,input{min-width:0;background:var(--input);color:var(--fg);border:1px solid var(--border);border-radius:6px;padding:9px}textarea{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;resize:vertical}button{background:var(--link);border:0;border-radius:6px;padding:9px 12px;color:#08111f;font-weight:700;cursor:pointer}.badge{display:inline-block;border:1px solid var(--border);border-radius:999px;padding:2px 8px;margin-right:6px;font-size:12px;white-space:nowrap}.ok{color:var(--ok)}.bad{color:var(--bad)}.muted{color:var(--muted)}.output{overflow:auto;max-height:340px;background:var(--input);border:1px solid var(--border);border-radius:6px;padding:10px;color:var(--fg)}code{color:var(--link)}.qtable{width:100%;border-collapse:collapse;font-size:13px}.qtable th{text-align:left;padding:8px 10px;border-bottom:1px solid var(--border);color:var(--muted);font-weight:600;font-size:11px;text-transform:uppercase;letter-spacing:.04em;position:sticky;top:0;background:var(--panel)}.qtable td{padding:8px 10px;border-bottom:1px solid var(--border);vertical-align:top}.qtable tr:last-child td{border-bottom:0}.qtable tr:hover{background:rgba(255,255,255,0.02)}button.danger{background:var(--bad);color:#0b0508}@media(max-width:760px){.row,.panel-head,.config-grid{grid-template-columns:1fr;display:grid}}`
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
		"plugin_id":     "continuum.audiobook-requests",
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
