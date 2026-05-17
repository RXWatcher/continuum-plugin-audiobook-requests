# AudiobookBay Requests for Continuum

`continuum.audiobookbay-requests` is a request provider for the Continuum
Audiobooks portal. It searches AudiobookBay, resolves a selected result to an
info hash or magnet URI, and hands the request to either scrape-only mode,
qBittorrent, or the plugin's experimental embedded torrent downloader.

This plugin is not an audiobook library or playback backend. It does not expose
owned shelves, streams, playback sessions, or Audiobookshelf-compatible routes.
Install it beside `continuum.audiobooks` when you want Audiobooks requests to
be fulfilled from AudiobookBay search results.

Use this plugin only with content you are legally allowed to access. The plugin
does not bypass private accounts; the current scraper flow uses public
AudiobookBay pages and does not require signing in.

## Detailed Operations Docs

- [Setup, debugging, and communication flows](docs/setup-debug-flows.md)

## Features

- Listens for `plugin.continuum.audiobooks.request_submitted` events.
- Searches AudiobookBay by title and author, or resolves a supplied detail URL.
- Scores candidates by title match, token coverage, resolved magnet/info hash,
  and seed count when the page exposes it.
- Builds magnet URIs from AudiobookBay `Info Hash` pages when no magnet is
  embedded in the page.
- Supports `scrape_only`, `qbittorrent`, and `embedded` download modes.
- Publishes `queued`, `downloading`, `imported`, and `failed` status updates
  back to the Audiobooks portal.
- Provides authenticated diagnostics, test search, external search, and request
  snapshot endpoints under `/api/v1/*`.

## Download Modes

| Mode | Behavior |
|---|---|
| `scrape_only` | Resolves the best AudiobookBay result and returns a magnet-ready acknowledgement. No download client is used. |
| `qbittorrent` | Adds the magnet to the configured qBittorrent Web API and reconciles status from qBittorrent. |
| `embedded` | Uses `github.com/anacrolix/torrent` in-process. Uploading and seeding are disabled, and completed torrents are dropped from the client. This mode is experimental. |

## Configuration

| Key | Required | Description |
|---|---|---|
| `database_url` | yes | Postgres DSN for the dedicated `audiobookbay_requests` schema. |
| `base_url` | yes | AudiobookBay base URL, for example `https://audiobookbay.lu`. |
| `download_mode` | no | `scrape_only`, `qbittorrent`, or `embedded`. Defaults to qBittorrent when a qBittorrent URL is set, otherwise scrape-only. |
| `qbittorrent_url` | no | qBittorrent Web API base URL. Required for qBittorrent mode. |
| `qbittorrent_username` | no | qBittorrent username, if Web UI auth is enabled. |
| `qbittorrent_password` | no | qBittorrent password, if Web UI auth is enabled. |
| `qbittorrent_category` | no | Optional category assigned to new torrents. |
| `qbittorrent_save_path` | no | Optional save path sent to qBittorrent. |
| `embedded_download_dir` | conditional | Required when `download_mode=embedded`. |
| `embedded_listen_port` | no | BitTorrent listen port for embedded mode. Empty or `0` chooses a random port. |
| `trackers` | no | JSON array of fallback trackers for info-hash-only pages. |
| `search_limit` | no | Maximum AudiobookBay results to inspect. Defaults to 10. |

Example DSN:

```text
postgres://plugin_audiobookbay_requests:password@postgres:5432/continuum?search_path=audiobookbay_requests&sslmode=disable
```

## Database Setup

```sql
CREATE ROLE plugin_audiobookbay_requests WITH LOGIN PASSWORD '<chosen>';
CREATE SCHEMA audiobookbay_requests AUTHORIZATION plugin_audiobookbay_requests;
GRANT CONNECT ON DATABASE continuum TO plugin_audiobookbay_requests;
```

The plugin runs its own migrations in the configured schema.

## Events And Status

Inbound:

- `plugin.continuum.audiobooks.request_submitted`

Outbound suffixes:

- `request_acknowledged`
- `request_status_changed`
- `request_fulfilled`
- `request_failed`

When qBittorrent or embedded mode accepts a magnet but has not yet made
progress, the acknowledgement includes `status: queued`. Later reconciler ticks
publish `queued`, `downloading`, `imported`, or `failed` as the provider state
changes.

## Operations

- `GET /api/v1/admin/diagnostics` checks AudiobookBay reachability, Postgres,
  qBittorrent, embedded mode, and recent forwarded requests.
- `GET /api/v1/search/external` lets the Audiobooks portal preview external
  AudiobookBay matches.
- Live AudiobookBay scrape coverage is available with
  `LIVE_AUDIOBOOKBAY_SMOKE=1 go test ./internal/audiobookbay -run TestLiveAudiobookBayPublicDomainResolve -count=1 -v`.
- The embedded torrent live smoke test uses a legal public sample and is gated
  behind `LIVE_EMBEDDED_TORRENT_SMOKE=1`.

## Build And Test

```bash
go test ./...
go build -buildvcs=false -o continuum-plugin-audiobookbay-requests ./cmd/continuum-plugin-audiobookbay-requests
```
