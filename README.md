# AudiobookBay Requests Plugin

`continuum.audiobookbay-requests` is a scraper-based Continuum audiobook
request provider. It listens for requests from `continuum.audiobooks`, searches
AudiobookBay, and resolves the selected result to a magnet. qBittorrent
enqueueing is optional.

It is intentionally not a presentation library source: it does not expose
shelves, catalog browsing, streaming, playback state, or owned library data.

## What It Does

- Receives `plugin.continuum.audiobooks.request_submitted` events.
- Searches AudiobookBay by submitted title and author.
- Scores candidates by title/query match, token coverage, resolved magnet, and seed count when present.
- Accepts an optional `source_id` containing an AudiobookBay detail URL.
- Supports three download modes: scrape-only magnet resolution, qBittorrent
  enqueueing, and experimental embedded torrent downloading.
- Persists the selected title, detail URL, info hash, magnet URI, score, and score reason.
- Tracks non-terminal embedded/qBittorrent jobs with a scheduled reconciler.
- Publishes request status events back to Continuum.
- Exposes authenticated diagnostics, test search, external search, and request
  snapshot endpoints under `/api/v1/*`.

## Capabilities

| Capability | ID | Purpose |
|---|---|---|
| `http_routes.v1` | `backend` | Authenticated `/api/v1/*` provider API. |
| `event_consumer.v1` | `request_handler` | Subscribes to `plugin.continuum.audiobooks.request_submitted`. |
| `audiobook_backend.v1` | `default` | Advertises a request-provider role to the Audiobooks portal. |
| `scheduled_task.v1` | `reconciler` | Polls qBittorrent status every minute. |

The `audiobook_backend.v1` metadata advertises:

- `audiobook_roles`: `request_provider`
- `supports_catalog`: `false`
- `supports_requests`: `true`
- `supports_auto_monitoring`: `false`

## Event Flow

1. A user submits an audiobook request in the Audiobooks portal.
2. `continuum.audiobooks` emits `plugin.continuum.audiobooks.request_submitted`.
3. This plugin ignores requests targeted at other providers.
4. The plugin resolves a magnet from AudiobookBay.
5. If qBittorrent or embedded mode is configured, the magnet is downloaded and reconciled until completion.
6. In scrape-only mode, the request is acknowledged once the magnet is ready.

Published event suffixes:

- `request_acknowledged`
- `request_status_changed`
- `request_fulfilled`
- `request_failed`

## Configuration

| Key | Required | Description |
|---|---|---|
| `database_url` | yes | Postgres DSN for the dedicated `audiobookbay_requests` schema. |
| `base_url` | yes | AudiobookBay base URL, no trailing slash. |
| `download_mode` | no | `scrape_only`, `qbittorrent`, or `embedded`. |
| `qbittorrent_url` | no | Optional qBittorrent Web API base URL. |
| `qbittorrent_username` | no | qBittorrent username, if authentication is enabled. |
| `qbittorrent_password` | no | qBittorrent password, if authentication is enabled. |
| `qbittorrent_category` | no | Category assigned to new torrents. |
| `qbittorrent_save_path` | no | Save path sent to qBittorrent. |
| `embedded_download_dir` | conditional | Required when `download_mode=embedded`. |
| `embedded_listen_port` | no | BitTorrent listen port for embedded mode; `0`/empty chooses a random port. |
| `trackers` | no | JSON array of fallback trackers for info-hash-only pages. |
| `search_limit` | no | Maximum AudiobookBay results to inspect; default 10. |

## Selection And Diagnostics

Automatic requests use a scored selection pass instead of blindly taking the
first result. The score favors exact title matches, query token coverage, pages
where the magnet/info hash resolves cleanly, and seed count when that value is
available from the page.

`GET /api/v1/admin/diagnostics` reports separate health for AudiobookBay,
qBittorrent, embedded mode, and Postgres. It also includes recent forwarded
requests with the selected result metadata so operators can audit why a result
was chosen.

## Embedded Torrent Mode

Set `download_mode=embedded` and configure `embedded_download_dir` to let the
plugin download magnets directly with `github.com/anacrolix/torrent`. Embedded
mode is intentionally conservative: uploads are disabled (`NoUpload`), seeding
is disabled, and completed torrents are dropped from the in-process client.

This mode is experimental. The plugin owns the torrent lifecycle, so operators
should monitor disk space and stalled downloads more closely than when using an
external client.

Example `database_url`:

```text
postgres://plugin_audiobookbay_requests:password@postgres:5432/continuum?search_path=audiobookbay_requests&sslmode=disable
```

## Database Setup

```sql
CREATE ROLE plugin_audiobookbay_requests WITH LOGIN PASSWORD '<chosen>';
CREATE SCHEMA audiobookbay_requests AUTHORIZATION plugin_audiobookbay_requests;
GRANT CONNECT ON DATABASE continuum TO plugin_audiobookbay_requests;
```

## Build And Test

```bash
go test ./...
go build -buildvcs=false -o continuum-plugin-audiobookbay-requests ./cmd/continuum-plugin-audiobookbay-requests
```

Store tests skip automatically when the default local Postgres test DSN is not
reachable. Set `TEST_DATABASE_URL` to run them against another database.
