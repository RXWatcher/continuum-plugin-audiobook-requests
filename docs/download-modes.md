# Download modes

Two independent mode knobs, one per source:

- `audiobookbay_download_mode` — what to do with the magnet/info-hash AudiobookBay produces.
- `abook_download_mode` — what to do with the NZB URL the abook → nzbking chain produces.

The legacy single `download_mode` is still honoured for upgraded installs; the runtime maps it to the per-source values via `EffectiveAudiobookBayMode()` and `EffectiveAbookMode()` (`internal/runtime/runtime.go`).

## AudiobookBay modes

### `scrape_only`

- **Needs:** `base_url`.
- **What happens:** consumer resolves the detail page to a magnet and stops. `external_id` is the info hash; `magnet_uri` is stored. Status set to `acknowledged`.
- **Reconciler behaviour:** `GetDownload` returns `magnet_ready` which translates to `acknowledged` — the row sits in that state until manually retried or marked failed. There is no progression.
- **When to pick it:** to validate that the scraper works (search + detail + info-hash extraction) without committing to a downloader. Also useful as a guarded default while changing config.

### `qbittorrent`

- **Needs:** `qbittorrent_url` (required). `qbittorrent_username`/`password` if Web UI auth is enabled. `qbittorrent_category` and `qbittorrent_save_path` optional but recommended (the plugin builds `<save_path>/<sanitized-title>/` so concurrent imports don't collide).
- **What happens:** `audiobookbay.QBitClient.AddMagnet` POSTs to `/api/v2/torrents/add`. The plugin doesn't read torrents back via the API on dispatch — it trusts the upload, stores the info hash, and lets the reconciler poll status from `/api/v2/torrents/info?hashes=<hash>`.
- **Reconciler behaviour:** progress < 99.9% → `downloading`, ≥ 99.9% → `imported`, state containing "error" or "missing" → `failed`. Empty hash result → `queued`.
- **Gotchas:**
  - qBittorrent's CSRF/session model means `Login` is called on first contact and the session cookie is reused. A long-running qBittorrent restart will eventually 403; the next dispatch re-authenticates.
  - HTTP is allowed (`validateOriginURL` passes `allowHTTP=true`) so an in-cluster qBittorrent on a private network is fine.
  - If the category doesn't exist in qBittorrent it's auto-created with no save path overrides — set `qbittorrent_save_path` explicitly.

### `embedded`

- **Needs:** `embedded_download_dir`. Optional: `embedded_listen_port` (`0` = random), `embedded_max_concurrent` (default 8, max 64).
- **What happens:** `internal/embedded` wraps `github.com/anacrolix/torrent`. Seeding and uploads are disabled by design; completed torrents are dropped from the client.
- **Pre-flight at Add time:**
  - `defaultMaxConcurrent` cap → `ErrAtCapacity` when reached. The row records "embedded downloader at concurrency cap"; admin **Retry** is the way to recycle it once a slot frees.
  - `minFreeBytes = 2 GiB` floor → `ErrInsufficientDisk`. Tune by freeing the disk; the threshold is constant.
  - `metadataTimeout = 3 min` for magnet metadata. Peerless magnets fail with a timeout rather than leaking the `*torrent.Torrent` forever.
- **Restart semantics:** on Configure, `main.go` iterates non-terminal rows and calls `RestoreDownload(hash, magnet, title)` so in-flight torrents resume from their `magnet_uri`. A row with a non-terminal status but an empty `magnet_uri` will **not** restore (this only matters if the dispatch failed before persisting the magnet; that combination shouldn't exist given the post-dispatch nack rule, but it's the failure case to look for).
- **When to avoid:** production. The admin UI shows a red warning panel when embedded is selected. It writes to disk, opens peer connections, and contributes to your conntrack table. qBittorrent is the recommended handoff.

## abook modes

### `scrape_only` (abook)

- **Behaviour:** abook is treated as **not configured** — the consumer skips it entirely. The plugin only ever uses AudiobookBay even if `abook_email`/`abook_password` are set. This is intentional: abook posts cannot be consumed without NZBGet to fetch the resolved NZB.
- **Implication:** to disable abook without deleting the creds, switch `abook_download_mode` to `scrape_only`.

### `external_nzbget`

- **Needs:** `abook_email`, `abook_password`, `nzbget_url`. `nzbget_username`/`password` if NZBGet auth is on.
- **What happens:** the consumer drives the abook → nzbking → NZBGet append chain. The resulting `NZBID` is stored as `external_id=nzbget:<id>`.
- **Reconciler behaviour:** routed to `nzbget.Lookup`. State mapping is in `internal/nzbget/client.go`:
  - `listgroups` first (live queue), then `history`.
  - `SUCCESS/*` history → `imported`. Any other history status → `failed`.
  - Both lists miss → `state="unknown"` → translated to `failed` (NZBGet has dropped the job from view, no recovery path).
- **Gotchas:**
  - The plugin sends the unpack password via `PPParameters` (`*Unpack:Password`). NZBGet 21+ honours this; older versions don't and will fail extraction. Embedded mode ships NZBGet 26.1.
  - DupeMode is `SCORE` with score 0 — NZBGet may silently dedupe a re-append against an existing job and return id `0`, which the client rejects with `server returned id 0 (likely DupeMode rejection)`. If you see this, the previous append succeeded; the existing job ID is the one to poll.

### `embedded_nzbget`

- **Needs (validated up-front):** `usenet_host`, `usenet_port`, `usenet_username`, `usenet_password`, `embedded_download_dir`.
- **Auto-set fields:** `nzbget_url`, `nzbget_username`, `nzbget_password` are overwritten with the supervised daemon's loopback URL and generated credentials on every Configure. Operator-supplied values are ignored. `nzbget_category` defaults to `audiobooks` if blank.
- **What happens:** see [`embedded-nzbget.md`](embedded-nzbget.md) for the full supervisor flow. From the consumer's perspective this is identical to `external_nzbget` — it just talks to `127.0.0.1:<random-port>` instead of an operator-configured URL.
- **Failure mode at startup:** if `embeddednzbget.New` or `Start` fails, `main.go` **logs a warning and falls back to the operator-supplied external NZBGet config** (if any). If neither is workable, `cfg.AbookConfigured()` returns false and the consumer skips abook for every event.

## Validation gates

`internal/runtime.ValidateAppConfig` enforces these at save time. Save failures surface in the admin UI as a red message under the Save button.

| Rule | Triggered when |
| --- | --- |
| `download_mode` must be one of the five valid values | upgrading from an older install with a garbage value |
| `audiobookbay_download_mode` must be `scrape_only`/`qbittorrent`/`embedded` | preventing a Usenet mode from leaking onto the torrent side |
| `abook_download_mode` must be `scrape_only`/`external_nzbget`/`embedded_nzbget` | preventing a torrent mode from leaking onto abook |
| `qbittorrent_url` required when `audiobookbay_download_mode=qbittorrent` | misconfiguration safety net |
| `embedded_download_dir` required when `audiobookbay_download_mode=embedded` OR `abook_download_mode=embedded_nzbget` | both modes write under this dir |
| `usenet_*` required when `abook_download_mode=embedded_nzbget` | daemon refuses to start without `Server1.*` block |
| `abook_email`/`abook_password` must both be set or both empty | partial creds are a silent foot-gun |
| `nzbget_username`/`nzbget_password` must both be set or both empty | same |
| `base_url`, `qbittorrent_url`, `nzbget_url`, `abook_base_url` parsed as origin URLs (no creds/query/fragment, HTTPS unless localhost) | SSRF guard |
| Each tracker is `udp`/`http`/`https` scheme with a host and no creds | magnet builder safety |
| `search_limit` in `[0, 100]`; `embedded_listen_port` in `[0, 65535]`; `embedded_max_concurrent` in `[0, 64]`; `usenet_port` in `[0, 65535]`; `usenet_connections` in `[0, 64]` | sanity ranges |

## Switching modes safely

The runtime Configure callback rebuilds the AudiobookBay client, abook+nzbking+nzbget trio, embedded torrent manager, and embedded NZBGet supervisor on every Save. The old instances are swapped out **after** the new ones are constructed, so there is no window during which a request would see "downloader unavailable" — except for `embedded_nzbget`, where the supervised daemon is restarted and the new daemon is reachable before the old one is signalled.

In-flight rows survive a mode change:
- A row dispatched via qBittorrent stays in qBittorrent — it isn't migrated.
- An abook row keeps its `nzbget:` external_id and continues polling NZBGet. If you remove NZBGet config entirely, the reconciler records `nzbget not configured` per row (deduped, so it's a single update per row, not a per-tick storm) and stops trying. Re-add NZBGet config to resume polling those rows.
- An embedded torrent row whose magnet is preserved will be restored on next plugin start. Switching to qBittorrent without first letting embedded finish those torrents leaves them orphaned in the embedded download dir.
