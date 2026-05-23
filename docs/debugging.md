# Debugging

Operator playbook for routine failures. Each section starts with the symptom and ends with the specific signal that confirms a diagnosis.

## First moves

When a user says "my request didn't come through":

1. Open `/admin` (the plugin's admin SPA, mounted at the plugin route). The **Readiness** tab shows database, AudiobookBay, qBittorrent, mode, and request stats.
2. Hit `GET /api/v1/admin/diagnostics` for the raw JSON if you want to script anything. It hits Postgres `Ping`, AudiobookBay `Ping`, qBittorrent `Login` (only if configured), and pulls `RequestStats`.
3. Go to the **Download queue** tab. Filter to the user's request, or search by title/info-hash. The error_text column is the first place to look.

## Symptom → diagnosis table

| Symptom | First check | Confirms |
| --- | --- | --- |
| Request stuck in `submitted` | `external_id` column in row | Empty → consumer never dispatched (search miss or upstream error). Non-empty → reconciler issue |
| `failed` with `title or source_id required for AudiobookBay request` | The originating event payload in `silo.audiobooks` | Audiobooks plugin sent an empty title and no source pin — not our bug |
| `failed` with `no AudiobookBay result for "..."` | Run the **Search lab** with the same query | If lab returns 0 hits with no error, search is genuinely empty. If lab errors with "audiobookbay blocked: ...", the mirror is challenging us |
| Search lab shows `audiobookbay blocked: cloudflare` / `hcaptcha` / `attention required` | `internal/audiobookbay.blockSignatures` for the full list | Mirror is serving a challenge. Either wait, change `base_url` to a different mirror, or route through a proxy. The block detection runs on HTTP 200 bodies too — see below |
| Search returns 0 hits but you know the title is on the site | curl the search URL manually: `curl "https://audiobookbay.lu/page/1/?s=<query>"` | If your curl works but the plugin doesn't, the user-agent or scraping rules are upstream's filter target |
| Row has `external_id` but never progresses | Reconciler `lastError` for that row | See [`reconciler.md`](reconciler.md) dedup section — error_text may be days stale |
| `LastError: backoff: upstream rate-limited` on reconciler status | `Retry-After` parking window | Wait it out. The whole reconciler is parked, not just the offending row |
| qBittorrent rows show as `queued` forever | qBittorrent UI for that hash | Likely no seeders. Embedded torrent metadataTimeout (3 min) doesn't apply to qBittorrent — it'll just sit there |
| Embedded rows stop with `embedded downloader at concurrency cap` | `embedded_max_concurrent` setting | Bump the cap (max 64) or hit **Retry** once jobs complete |
| Embedded rows stop with `embedded download dir is below minimum free space` | `df -h <embedded_download_dir>` | Free space below 2 GiB. Constant in source (`minFreeBytes`) |
| abook never appears in queue source column, only AudiobookBay | `cfg.AbookConfigured()` requirements | Need `abook_email`, `abook_password`, AND `abook_download_mode` ∈ `{external_nzbget, embedded_nzbget}`. `scrape_only` disables abook |
| **Test abook login** fails with `credentials rejected` | Try logging in to abook.link manually | Password actually changed |
| **Test abook login** fails with `SMF login form shape changed` | `internal/abook/client.go::smfHiddenFieldRe` | The forum changed HTML; regex needs updating |
| **Test NZBGet** fails with `unauthorized` | NZBGet auth settings | Password mismatch. In `embedded_nzbget` mode the daemon generates new creds on every restart; operator creds are ignored |
| **Test NZBGet** times out | Network reachability to NZBGet | For `embedded_nzbget`, the daemon is on `127.0.0.1:<random>` — if reachable from the plugin but not from your curl, that's expected (loopback-only) |
| `nzbget append: server returned id 0` | NZBGet history for the same NZB | DupeMode rejection; the prior append succeeded |
| All abook rows show `nzbget not configured` after a config change | `abook_download_mode` value | If you switched away from a Usenet mode, rows queued earlier still hold `nzbget:` external_ids the reconciler can't poll. Either re-enable NZBGet, or mark those rows failed manually |

## How block detection works

`internal/audiobookbay.DetectBlocked` scans the response body for any of:

```
hcaptcha, h-captcha, recaptcha, attention required,
checking your browser, please enable javascript, bot protection,
access denied, captcha, cloudflare
```

Order matters: more-specific phrases come first so the returned hint is the most useful one (e.g. `hcaptcha` rather than the generic `captcha`). The check runs on:

- HTTP 200 bodies (the **insidious case** — search would otherwise return 0 hits and look like "no matches")
- HTTP 403 bodies (canonical Cloudflare/WAF block)

A 403 with no signature still returns a `BlockedError` with the truncated body in the hint. Operator action: try a different mirror, or wait for the challenge to expire.

## Embedded download diagnostics

The admin UI's **Readiness** tab includes an embedded card if embedded mode is selected:

```json
"embedded": {
  "configured": true,
  "download_dir": "/data/audiobooks/embedded"
}
```

For deeper visibility, query the manager state via `internal/embedded`. The dir layout under `embedded_download_dir`:

- `<title>/` (sanitized) per torrent's save subdir
- `.nzbget/` (only if `embedded_nzbget` mode is also on)

Disk pressure: `embedded.DiskFree(dir)` returns `(free, total)` bytes. The 2 GiB floor is enforced **at Add time**, not continuously — a torrent that fills the disk mid-download will still fail with ENOSPC. Operator action: free space, then **Retry** the affected rows.

## Direct database triage

Schema name defaults to `audiobook_requests`. Substitute if you've namespaced it differently.

```sql
-- All non-terminal rows ordered by age (oldest first; matches reconciler order).
SELECT request_id, status, source_id, external_id, search_query,
       selected_title, error_text,
       last_polled, created_at,
       now() - GREATEST(COALESCE(last_polled, '-infinity'::timestamptz), created_at) AS age
FROM audiobook_requests.forwarded_request
WHERE status NOT IN ('imported','failed')
ORDER BY COALESCE(last_polled, '-infinity'::timestamptz) ASC;

-- Per-pipeline counts.
SELECT
  CASE
    WHEN external_id LIKE 'nzbget:%' OR source_id LIKE 'abook:%' THEN 'abook'
    ELSE 'audiobookbay'
  END AS pipeline,
  status,
  count(*)
FROM audiobook_requests.forwarded_request
GROUP BY 1, 2
ORDER BY 1, 2;

-- Top error texts to spot a systemic problem.
SELECT error_text, count(*) AS n,
       min(updated_at) AS first_seen,
       max(updated_at) AS last_seen
FROM audiobook_requests.forwarded_request
WHERE COALESCE(error_text,'') <> ''
GROUP BY error_text
ORDER BY n DESC
LIMIT 20;

-- Force-mark a row failed (the admin UI button calls this, but you can
-- also do it directly).
UPDATE audiobook_requests.forwarded_request
SET status = 'failed',
    error_text = COALESCE(error_text, '') || ' [manual fail by operator]',
    updated_at = now()
WHERE request_id = '<id>';

-- "Retry" equivalent: reset a row to acknowledged so the reconciler
-- picks it up next minute. The admin endpoint refuses rows without an
-- external_id; this raw SQL doesn't.
UPDATE audiobook_requests.forwarded_request
SET status = 'acknowledged',
    error_text = NULL,
    last_polled = NULL,
    updated_at = now()
WHERE request_id = '<id>'
  AND external_id IS NOT NULL
  AND external_id <> '';
```

## Common per-mode failure patterns

### `audiobookbay_download_mode=scrape_only`

- Rows stay at `acknowledged` indefinitely. This is **expected behaviour** — no downloader is configured. The reconciler will keep stamping `last_polled` (every minute, for hours) but the status never changes. If you don't want that activity, switch the mode to one that progresses, or mark the rows failed.

### `audiobookbay_download_mode=qbittorrent`

- **Login loop:** qBittorrent's session cookie expires; the client transparently re-authenticates. If you see repeated `qbittorrent: HTTP 403` in error_text, the password is wrong.
- **Wrong save_path:** qBittorrent silently uses its default if the path isn't writable. Check qBittorrent's own UI for the actual on-disk location.
- **Category not visible:** qBittorrent auto-creates the category on first use, but with no save-path override. Set `qbittorrent_save_path` explicitly.

### `audiobookbay_download_mode=embedded`

- **Restart orphans:** in-flight torrents are restored on Configure by iterating non-terminal rows and calling `RestoreDownload`. Rows without a stored `magnet_uri` are skipped (shouldn't happen given the post-dispatch nack rule, but check `magnet_uri` if a row sits at `acknowledged` after a restart).
- **Conntrack saturation:** each torrent opens many small connections. On heavily loaded hosts, dropping connections silently. Reduce `embedded_max_concurrent`.
- **Dead magnet:** metadata not retrieved within 3 minutes → the Add fails and the row gets the timeout error. Embedded mode has no second chance — admin **Retry** rebuilds from `magnet_uri` if it's still in the row.

### `abook_download_mode=external_nzbget`

- **Append succeeded but the row never goes `downloading`:** check NZBGet's queue UI. If the job is paused / scheduled / propagation-waiting, the plugin's mapping (see [`reconciler.md`](reconciler.md)) treats it as `downloading`. If the job dropped to history with a failure status, the plugin maps that to `failed` (`SUCCESS/*` is the **only** status that becomes `imported`).
- **Auth flapping:** `nzbget: unauthorized` after a config push usually means the operator changed the NZBGet password without updating the plugin's stored copy. Re-save the config.

### `abook_download_mode=embedded_nzbget`

- See [`embedded-nzbget.md`](embedded-nzbget.md). Most failures land in the plugin's log as `embedded nzbget ...` lines.
- If the daemon dies after running for a while (OOM, signalled by something else on the host), the next poll fails with a connection error and the row records it. The supervisor doesn't auto-restart between Configure calls — restart the plugin (which calls Configure) to bring the daemon back.

## When to escalate beyond docs

- `bundle SHA mismatch` on embedded NZBGet startup → the binary was tampered with or corrupted; reinstall.
- Repeated `SMF login form shape changed` → abook.link rewrote its login HTML; needs a code change in `internal/abook/client.go`.
- AudiobookBay returning a new block signature not in `blockSignatures` → searches will look empty rather than blocked; add the substring to the list.
- `pgxpool` errors that look like `too many connections` → another consumer is monopolising the database; the plugin's pool floor is 16, so a busy host running many plugins can exhaust Postgres' `max_connections`.
