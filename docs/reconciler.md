# Reconciler

Cron `*/1 * * * *`. Implementation in `internal/reconciler/`. Polls non-terminal `forwarded_request` rows and emits status events. The reconciler is the single source of truth for status transitions after the initial dispatch.

## Tick budget

| Setting | Value | Why |
| --- | --- | --- |
| `tickTimeout` | 45s | Cron fires every 60s; capping below the cron prevents queue-up |
| `perRowTimeout` | 10s | 200 rows × 1s upstream calls + slack fits comfortably inside the tick |
| Max rows per tick | 200 | `Store.ListNonTerminal(ctx, 200)` |
| Overlap guard | `tickMu.TryLock()` | If a previous tick is still running, the new one no-ops |

The SDK scheduler is generally serial, but clock skew + a slow upstream can occasionally fire two ticks back-to-back. The TryLock skip writes a `Skipped: true` status snapshot to the admin UI rather than queuing extra work behind a tick that's stuck on a hung upstream.

If the per-row loop blows the tick budget mid-pass, the loop **breaks** rather than continuing — finishing rows with `context deadline exceeded` would record that as the per-row error_text everywhere we didn't get to, polluting the error_text and dedup map. The next tick picks up from the oldest stale row (`ORDER BY last_polled ASC`).

## Per-row routing

`external_id` prefix decides the backend:

- `nzbget:<NZBID>` → `pollNZBGet` → `internal/nzbget.Client.Lookup`
- anything else → `audiobookbay.Client.GetDownload` → embedded / qBittorrent / scrape_only

Rows with empty `external_id` are skipped entirely (the consumer was never able to dispatch them; only an admin **Retry** or a re-emitted event can move them).

## Status translation

Internal status names ↔ upstream vocabulary.

**AudiobookBay (`translateStatus`):**

| Upstream | Plugin |
| --- | --- |
| `queued` | `queued` |
| `magnet_ready` | `acknowledged` (used by `scrape_only`) |
| `downloading` | `downloading` |
| `imported`, `completed` | `imported` |
| `failed`, `error` | `failed` |
| anything else | `""` (no transition — hold current status) |

The empty-string return is important. Previously this branch returned `acknowledged`, which regressed in-flight rows from `downloading` back to `acknowledged` and published a status-changed event on every poll. The current behaviour: keep the row at its current status, stamp `last_polled`, clear `error_text`.

**NZBGet (`translateNZBGetState` + `mapQueueStatus` + `mapHistoryStatus`):**

| Upstream | Plugin |
| --- | --- |
| `QUEUED`, `FETCHING` | `queued` |
| `DOWNLOADING`, `PAUSED`, `PP_QUEUED`, `LOADING_PARS`, `VERIFYING_*`, `REPAIRING`, `UNPACKING`, `MOVING`, `EXECUTING_SCRIPT` | `downloading` |
| `SUCCESS/*` (history) | `imported` |
| anything else in history | `failed` |
| not in queue, not in history | `unknown` → `failed` (terminal; user has nothing actionable to see) |

`PAUSED` is treated as `downloading` because the user's POV is "still in flight" — pausing is an NZBGet-side operation, not a request-level state.

## Dedup behaviour

`Reconciler.lastError` is a `map[requestID]string` of the last per-row error_text written. Before persisting a per-row error, the reconciler checks `recordErrorChanged`:

- If the new error matches the previous one for that row, the UPDATE is **skipped entirely** (no DB write, no log line, no churn).
- If different (or no previous error), the new error is persisted and the map is updated.
- On a successful poll, `clearError(requestID)` removes the entry so the next failure writes one fresh error and then dedups again.

Why this matters: AudiobookBay outages can last days. Without dedup, every minute the reconciler would rewrite identical `error_text` rows on every non-terminal row, generating database churn and log spam proportional to (outage duration × active rows). With dedup, the first tick of the outage writes the error and then the row sits quietly until the upstream recovers.

**Implication for triage:** when a row's error_text is days old but `last_polled` is current, that means the reconciler **did** poll, hit the same error, and skipped the rewrite. `updated_at` will only move forward on the first occurrence of the error and on successful polls. Use `last_polled` to confirm liveness, not `updated_at`.

## 429 backoff

AudiobookBay returns HTTP 429 with a `Retry-After` header when it's rate-limiting the plugin. The `RateLimitError` carries the parsed duration.

When the reconciler sees a 429 on any row in the current batch:
- `setBackoff(retryAfter)` is called. The window is clamped to `[60s, 10min]` (zero → 60s, parsed values capped at 10 min so a misbehaving upstream can't park us forever).
- The current tick **breaks out of the row loop**. Subsequent rows in the same batch would all 429 too.
- The first 429-row's error is persisted (deduped) so the operator can see why.
- Next tick: `backoffRemaining()` returns `> 0` → tick records `Skipped: true` with `lastError = "backoff: upstream rate-limited, Xs remaining"` and returns immediately. No upstream calls, no DB writes.
- When the window passes, normal polling resumes.

**NZBGet rows are unaffected by AudiobookBay backoff** — the backoff applies to the whole tick (the test happens before any row work), so NZBGet polls are also paused. This is intentional: a backed-off upstream that recovers needs the next minute of headroom to confirm before we start firing 200 calls at it.

The 403 block path (`BlockedError`) is **not** backoff-triggering. It records the per-row error and continues. The reasoning: 403 is a per-page Cloudflare/WAF response that may apply only to a specific URL pattern, while 429 is the upstream explicitly telling us to back off.

## What the reconciler publishes

Per row, exactly one of these per status transition:

| Transition | Event | Payload extras |
| --- | --- | --- |
| anything → `imported` | `request_fulfilled` | `fulfilled_book_id`, `external_id` |
| anything → `failed` | `request_failed` | `reason`, `external_id` |
| any other change | `request_status_changed` | `status`, `external_id` |
| no transition (poll succeeded) | nothing | `last_polled` / `error_text=NULL` updated only |

`request_id` and `requestId` are both included in every payload (the audiobooks plugin and host historically used either casing).

## LastStatus / admin UI

`Reconciler.LastStatus()` returns the most recent tick outcome:

```go
type Status struct {
    LastRunAt     time.Time
    LastDuration  time.Duration
    RowsProcessed int
    LastError     string
    Skipped       bool      // true if the tick was a no-op (overlap or backoff)
}
```

Surfaced at `GET /api/v1/admin/reconciler/status` and the **Readiness** tab in the admin UI. The UI also computes `stuckCount` by hitting `Store.ListStuck` (non-terminal rows with `last_polled < now() - 24h` OR `(last_polled IS NULL AND created_at < now() - 24h)`); the **Stuck requests** panel appears when that count is > 0.

`POST /api/v1/admin/reconciler/run` calls `Tick(ctx)` directly with a 60s context, ignoring the cron schedule. Useful for "I fixed the mirror, run now" — the overlap guard still applies, so this is a no-op if a scheduled tick is currently mid-run.

## When the reconciler stops making progress

| Symptom | Likely cause | Verification |
| --- | --- | --- |
| `LastError: backoff: ...` | AudiobookBay 429 active | Wait for the window; or visit the mirror manually to confirm the 429 |
| `Skipped: true` with no error | Previous tick still running | Should resolve next minute; if it persists, the upstream is hung — `journalctl` for stuck goroutines |
| `RowsProcessed: 0`, no error | No non-terminal rows | Check `ListNonTerminal` directly — request flow may have stalled at the consumer |
| All rows show `nzbget not configured` | Config change removed NZBGet after dispatch | Re-add NZBGet config; rows will resume polling next tick |
| All rows show `audiobookbay blocked: cloudflare` | Mirror is serving a challenge | Switch to a different `base_url` mirror; see `internal/audiobookbay.blockSignatures` for the detection list |
| `LastError: list non-terminal: ...` | Postgres unreachable | Verify `database_url`, check the pool's MaxConns |
