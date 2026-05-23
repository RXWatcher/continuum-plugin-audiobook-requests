# Audiobook Requests for Silo

`silo.audiobook-requests` is a request provider for the Silo Audiobooks portal. It listens for `request_submitted` events from `silo.audiobooks`, searches AudiobookBay (and optionally abook.link), resolves the best result to a magnet or NZB, and hands it to the configured downloader so the request can be reconciled to completion.

The plugin is a request provider only — not a library backend. It does not expose shelves, streams, playback sessions, or Audiobookshelf-compatible routes. Install it beside `silo.audiobooks` when you want incoming audiobook requests fulfilled from public AudiobookBay scrape results, or from a private abook.link account routed through NZBGet.

Use this plugin only with content you are legally allowed to access.

## Category

Lives under **Books / Audiobooks** in the admin sidebar.

## Capabilities

| Type | ID | Purpose |
| --- | --- | --- |
| `http_routes.v1` | `backend` | Admin API for diagnostics, test search, request queue inspection, retry/mark-failed actions, abook + NZBGet test buttons, and reconciler status. Mounted at `/api/v1/*` (authenticated) plus an admin SPA at `/admin`. |
| `event_consumer.v1` | `request_handler` | Subscribes to `plugin.silo.audiobooks.request_submitted` and forwards the request to abook (when configured) or AudiobookBay. |
| `audiobook_backend.v1` | `default` | Declares this plugin as an audiobook `request_provider`. `supports_requests=true`, `supports_catalog=false`, `supports_auto_monitoring=false`. |
| `scheduled_task.v1` | `reconciler` | Cron `*/1 * * * *`. Polls non-terminal forwarded requests against AudiobookBay (torrent) or NZBGet (Usenet) and emits status events. |

## Dependencies

- **[`silo-plugin-audiobooks`](https://github.com/RXWatcher/silo-plugin-audiobooks)** — the portal that publishes `plugin.silo.audiobooks.request_submitted` events this plugin consumes and that observes the status events this plugin publishes back. Without it, there is no request source.
- **Postgres schema** — the plugin runs its own migrations in a dedicated schema (default `audiobook_requests`).
- **Silo host** ([`ContinuumApp/silo`](https://github.com/ContinuumApp/silo)) and the SDK ([`ContinuumApp/continuum-plugin-sdk`](https://github.com/ContinuumApp/continuum-plugin-sdk)).

Sibling request providers (install at most one as the active provider in the Audiobooks portal):

- `silo-plugin-audiobook-requests` (this) — AudiobookBay + abook.link.
- [`silo-plugin-bookwarehouse-audio`](https://github.com/RXWatcher/silo-plugin-bookwarehouse-audio) — alternate catalog/stream backend via BookWarehouse.

## External services

- **AudiobookBay** mirror at `base_url` — scraped over HTTPS for search and detail pages; the plugin pulls `Info Hash` values and on-page trackers and synthesises magnets when no magnet link is embedded.
- **qBittorrent** Web API — optional, used when AudiobookBay's download mode is `qbittorrent`.
- **Embedded BitTorrent client** (`github.com/anacrolix/torrent`, experimental) — optional, used when AudiobookBay's download mode is `embedded`. Seeding and uploads are disabled; completed torrents are dropped from the client.
- **abook.link** — optional second search source; requires an account.
- **nzbking.com** — resolves abook.link Usenet search codes to NZB URLs.
- **NZBGet** — required when abook is configured. Either an external NZBGet the operator points at (`abook_download_mode=external_nzbget`) or an in-process supervised NZBGet daemon (`abook_download_mode=embedded_nzbget`) that the plugin starts on a loopback port with generated credentials and a Usenet provider the operator supplies.

## Request lifecycle

1. `silo.audiobooks` publishes `plugin.silo.audiobooks.request_submitted` with a `request_id`, `target_plugin_id`, optional `source_id`, plus `title` and `author`.
2. The consumer persists a `submitted` row in `forwarded_requests` *before* any network call. If the row can't be written, the event is nacked so the host redelivers — never start untracked work.
3. If the abook + NZBGet pipeline is fully configured and the caller did not pin a specific `source_id`, the consumer tries abook first (`abook search → topic → nzbking → NZBGet Append`). On any hard failure it falls back to AudiobookBay; abook is opportunistic, not gating. The row's `external_id` is prefixed `nzbget:<NZBID>` so the reconciler routes its polls correctly.
4. Otherwise (or on abook miss), the consumer calls `audiobookbay.StartDownload`, which either resolves the supplied `source_id` or runs `ExternalSearch`, scores hits, picks the best, and dispatches per `audiobookbay_download_mode`:
   - `scrape_only` — return magnet metadata, no client.
   - `qbittorrent` — `POST /api/v2/torrents/add` against the configured qBittorrent Web API.
   - `embedded` — add the magnet to the in-process torrent client.
5. The consumer upserts the resolved `external_id` (info hash or `nzbget:` NZBID), magnet, detail URL, score and reason; on failure it marks the row `failed` and publishes `request_failed`. On success it publishes `request_acknowledged`.
6. The reconciler runs every minute, lists up to 200 non-terminal rows, and for each one calls either `audiobookbay.GetDownload` (torrent rows) or NZBGet history/queue (NZB rows). Status transitions are persisted and published as `request_status_changed`, `request_fulfilled`, or `request_failed`. A 429 from AudiobookBay parks the whole reconciler for `Retry-After`; identical per-row errors are deduped so a months-long outage doesn't rewrite every row every minute.
7. Embedded-mode downloads still in flight at startup are restored from the database (`ListNonTerminal` → `RestoreDownload`) so a restart doesn't orphan torrents.

## Configuration

Validation lives in `internal/runtime.ValidateAppConfig`; admin form fields are defined in `cmd/silo-plugin-audiobook-requests/manifest.json` and mutated through the admin SPA.

**Storage**

| Key | Required | Description |
| --- | --- | --- |
| `database_url` | yes | Postgres DSN for the dedicated `audiobook_requests` schema. Pool max-conns is forced to at least 16 to keep the search API and reconciler from starving each other. |

**AudiobookBay (torrent path)**

| Key | Required | Description |
| --- | --- | --- |
| `base_url` | yes | AudiobookBay mirror origin. Validated as an origin URL (no creds/query/fragment, HTTPS unless localhost). |
| `audiobookbay_download_mode` | no | `scrape_only`, `qbittorrent`, or `embedded`. Defaults to the legacy `download_mode` when set to one of those values, otherwise `scrape_only`. |
| `qbittorrent_url` | conditional | qBittorrent Web API base URL. Required when the effective AudiobookBay mode is `qbittorrent`. HTTP allowed. |
| `qbittorrent_username` / `qbittorrent_password` | no | Web UI creds, if auth is enabled. |
| `qbittorrent_category` / `qbittorrent_save_path` | no | Optional category and save path applied to new torrents. |
| `embedded_download_dir` | conditional | Required when the effective mode is `embedded` (and reused as the NZBGet working dir for `embedded_nzbget`). |
| `embedded_listen_port` | no | BitTorrent listen port. `0` picks a random free port. Range 0–65535. |
| `embedded_max_concurrent` | no | Cap on concurrent in-process torrents. `0` defers to the package default. Range 0–64. |
| `trackers` | no | JSON array of fallback trackers (UDP/HTTP/HTTPS) appended to magnets built from info-hash-only pages. |
| `search_limit` | no | Maximum AudiobookBay results to inspect across pages. Defaults to 10. Range 0–100. |

**abook.link (Usenet path)**

| Key | Required | Description |
| --- | --- | --- |
| `abook_base_url` | no | abook.link board base. |
| `abook_email` / `abook_password` | conditional | Account credentials. Must be set together. Required to enable the abook search source. |
| `abook_cookie` | no | Persisted SMF session cookie; rewritten on each successful login so restarts don't burn fresh logins. |
| `abook_download_mode` | no | `scrape_only`, `external_nzbget`, or `embedded_nzbget`. Defaults to `scrape_only` unless `download_mode` (legacy) sets one of the Usenet modes. The abook pipeline is only active when this resolves to `external_nzbget` or `embedded_nzbget`. |

**NZBGet handoff** (required when abook is configured)

| Key | Required | Description |
| --- | --- | --- |
| `nzbget_url` | conditional | NZBGet JSON-RPC base URL. Required for `abook_download_mode=external_nzbget`. Auto-set to `http://127.0.0.1:<port>` in `embedded_nzbget` mode. |
| `nzbget_username` / `nzbget_password` | no | Must be set together. Overridden in `embedded_nzbget` mode by the supervised daemon's generated credentials. |
| `nzbget_category` | no | Category for appended NZBs. Defaults to `audiobooks` in `embedded_nzbget` mode. |

**Embedded NZBGet supervisor** (required when `abook_download_mode=embedded_nzbget`)

| Key | Required | Description |
| --- | --- | --- |
| `usenet_host` / `usenet_port` | yes | News provider host and port (1–65535). |
| `usenet_ssl` | no | TLS flag for the provider. |
| `usenet_username` / `usenet_password` | yes | Provider credentials. |
| `usenet_connections` | no | Connection cap. Range 0–64 (0 = default 8). |

**Legacy**

`download_mode` (single global mode: `scrape_only`, `qbittorrent`, `embedded`, `external_nzbget`, `embedded_nzbget`) is preserved so existing installs continue to work; new deployments should set the per-source `audiobookbay_download_mode` and `abook_download_mode` instead.

## Event subscriptions

Consumes:

- `plugin.silo.audiobooks.request_submitted` — the only subscribed event; the handler ignores events whose `target_plugin_id` isn't `silo.audiobook-requests`.

Publishes (suffixes; the host namespaces them under this plugin's ID):

- `request_acknowledged` — emitted once the consumer has resolved a hit and persisted the `external_id`.
- `request_status_changed` — emitted by the reconciler on a non-terminal status transition.
- `request_fulfilled` — emitted when AudiobookBay or NZBGet reports the download as imported/completed.
- `request_failed` — emitted on search-resolution failure, missing query inputs, or an upstream terminal failure.

## Detailed docs

- [Setup, debug, and communication flows](docs/setup-debug-flows.md)

## Build and release

```bash
make build       # builds ./silo-plugin-audiobook-requests
make test        # go test ./...
```

CI builds linux-amd64 binaries on push to main via the reusable workflow in [RXWatcher/silo-plugin-repository](https://github.com/RXWatcher/silo-plugin-repository) and publishes them to the catalog at [`./binaries/`](https://github.com/RXWatcher/silo-plugin-repository/tree/main/binaries).
