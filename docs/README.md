# Operator docs

The plugin [README](../README.md) covers what `continuum.audiobook-requests` is, its capabilities, configuration keys, and event surface. These docs are for the operator on call when something stops working.

Index:

- [`request-lifecycle.md`](request-lifecycle.md) — every state a request passes through, which row writes are mandatory, where nacks happen, and how the terminal guard keeps replays idempotent.
- [`download-modes.md`](download-modes.md) — what each `audiobookbay_download_mode` / `abook_download_mode` actually needs to start, the validation gates, and which knobs change behaviour at runtime.
- [`abook-and-nzbget.md`](abook-and-nzbget.md) — abook.link login dance, SMF cookie persistence, what session expiry looks like, and the abook → nzbking → NZBGet handoff.
- [`embedded-nzbget.md`](embedded-nzbget.md) — where the supervised daemon lives on disk, its generated credentials, restart semantics, and how to tell whether it's healthy.
- [`reconciler.md`](reconciler.md) — tick budget, dedup behaviour, 429 backoff, how multi-day outages stay quiet, and what each status transition publishes.
- [`debugging.md`](debugging.md) — failure patterns per mode with the signal that distinguishes them, the admin endpoints to reach for first, and SQL queries for direct database triage.

Quick map of which doc to open for a given symptom:

| Symptom | Start here |
| --- | --- |
| Requests stuck in `submitted` forever | [`request-lifecycle.md`](request-lifecycle.md) |
| Search returns `0` results but the site has matches | [`debugging.md`](debugging.md) (block detection) |
| `429` errors in the reconciler status | [`reconciler.md`](reconciler.md) (backoff window) |
| abook login worked once, then everything fails | [`abook-and-nzbget.md`](abook-and-nzbget.md) (cookie expiry) |
| NZBGet test button red after restart | [`embedded-nzbget.md`](embedded-nzbget.md) (supervisor lifecycle) |
| `nzbget not configured` in error_text on rows that used to download | [`reconciler.md`](reconciler.md) (config change after dispatch) |
| Same error text repeated on every row, identical timestamp gaps | [`reconciler.md`](reconciler.md) (dedup window) |
