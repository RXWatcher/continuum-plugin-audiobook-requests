# Audiobook Requests Setup, Debugging, And Flows

Plugin ID: `continuum.audiobook-requests`
Version documented: `0.1.0`

## Purpose

audiobook request provider that scrapes public AudiobookBay pages, resolves magnets/info
hashes, and queues or reports downloads.

## Runtime Dependencies

- Continuum plugin host
- Postgres schema for this plugin
- continuum.audiobooks for user-facing requests
- Public AudiobookBay mirror/base URL
- qBittorrent Web API for qbittorrent mode, or writable download directory for embedded mode

## Setup Checklist

1. Create the plugin database role and schema.
2. Configure base_url and choose download_mode: scrape_only, qbittorrent, or embedded.
3. For qbittorrent mode, configure URL, credentials, category, and save path.
4. For embedded mode, configure embedded_download_dir and an optional listen port.
5. Install continuum.audiobooks and select this plugin as the request provider.
6. Use external search/diagnostics before enabling it for users.

## Configuration Reference

- `database_url`
- `base_url`
- `download_mode`
- `qbittorrent_url`
- `qbittorrent_username`
- `qbittorrent_password`
- `qbittorrent_category`
- `qbittorrent_save_path`
- `embedded_download_dir`
- `embedded_listen_port`
- `trackers`
- `search_limit`

Use the plugin manifest/admin form as the source of truth for field validation and defaults. Keep database credentials scoped to the plugin schema unless a plugin explicitly needs read access to Continuum core tables.

## Exposed Routes

- `* /api/v1/* [authenticated]`

## Capabilities

- `http_routes.v1 (backend) - Request-provider API for scored AudiobookBay search, magnet resolution, diagnostics, and optional qBittorrent enqueueing.`
- `event_consumer.v1 (request_handler) - Forwards audiobook request_submitted events to AudiobookBay search and the configured download mode.`
- `audiobook_backend.v1 (default) - Scraper-based audiobook request provider backed by AudiobookBay with optional qBittorrent enqueueing; not a presentation library source.`
- `scheduled_task.v1 (reconciler) - Polls embedded/qBittorrent download status for non-terminal audiobook requests.`

## Operational Flows

### Audiobook request

1. A user submits a request in continuum.audiobooks.
2. The portal emits plugin.continuum.audiobooks.request_submitted for this provider.
3. The plugin searches AudiobookBay, scores candidates, and resolves a magnet or info hash.
4. scrape_only returns queued/acknowledged metadata without downloading; qbittorrent adds the magnet to qBittorrent; embedded starts an in-process download.
5. The reconciler publishes queued, downloading, imported, or failed status back to the Audiobooks portal.

## How This Plugin Communicates

- Consumes request events from continuum.audiobooks.
- Scrapes public AudiobookBay pages and optionally talks to qBittorrent.
- Publishes request acknowledgement and status events for continuum.audiobooks.

## Debugging Runbook

- Start with /api/v1/admin/diagnostics to check Postgres, AudiobookBay reachability, and selected download mode.
- Use scrape_only mode to separate scraper problems from download-client problems.
- If qBittorrent does not queue, check Web API URL, CSRF/session auth, category permissions, and save path.
- If embedded mode stalls, check listen port/firewall, disk permissions, and tracker list.
- If users do not see queued status, verify continuum.audiobooks is consuming provider status events.

## Log And Health Checks

- Start with Continuum Admin -> Plugins and confirm the installation is enabled.
- Check the plugin process logs around startup for manifest loading, migration, and route registration.
- Check scheduled task logs when a workflow depends on polling or reconciliation.
- Confirm the plugin routes are reachable through Continuum using the access level shown above.
- For database-backed plugins, verify the configured role can connect, create/migrate tables in its schema, and read/write expected rows.

## Common Failure Patterns

- Wrong installation ID selected in a portal or router setting after reinstalling a plugin.
- Plugin database URL points at the public schema instead of the dedicated plugin schema.
- Reverse proxy forwards the SPA route but not `/api/*`, `/api/v1/*`, `/assets/*`, or provider-specific public routes.
- Network checks are run from the operator laptop instead of from the Continuum/plugin runtime network.
- Secrets are regenerated during restart, invalidating signed URLs, encrypted fields, or login state.

## Verification After Changes

1. Restart or reload the plugin installation.
2. Open the plugin route or admin page in Continuum.
3. Exercise the smallest workflow that crosses a plugin boundary.
4. Confirm both the source plugin and destination plugin record the same request/session/login identifier.
5. Leave the scheduled reconciler enough time to run, then confirm terminal state or a useful error.
