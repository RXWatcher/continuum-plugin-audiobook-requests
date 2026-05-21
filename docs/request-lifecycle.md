# Request lifecycle

The state machine lives in `internal/consumer` (event handler) and `internal/reconciler` (poll loop). The persisted row is `forwarded_request` (one row per `request_id`, request_id is the primary key).

## States

| Status | Set by | Meaning | Terminal? |
| --- | --- | --- | --- |
| `submitted` | consumer (first write, before any network call) | row exists, no `external_id` yet | no |
| `acknowledged` | consumer (after a successful dispatch) | downloader has the magnet/NZB, awaiting first poll | no |
| `queued` | consumer / reconciler | downloader returned `queued` for the job | no |
| `downloading` | reconciler | downloader reports active progress | no |
| `imported` | reconciler | downloader reports completion/import | yes |
| `failed` | consumer / reconciler / admin | terminal failure (no search hit, downloader error, manual mark-failed, malformed external_id) | yes |

`acknowledged` is the bridge state: the consumer used it as the post-dispatch status; the reconciler will move it forward once the downloader reports `queued`/`downloading`. `Retry` in the admin UI resets a row to `acknowledged` so the reconciler picks it up.

## Walkthrough

1. **Event arrives.** `plugin.continuum.audiobooks.request_submitted` lands at the event consumer. The handler filters on `target_plugin_id == continuum.audiobook-requests`; foreign events are acked and dropped. Events missing both `title` and `source_id` are recorded as `failed` with `title or source_id required for AudiobookBay request`, emit `request_failed`, and ack.

2. **First DB write (mandatory).** The consumer writes a `submitted` row before any network call. The handler nacks the event if this write fails — there is **no untracked work**, ever. The host will redeliver; the upsert is idempotent and the terminal guard makes the redelivery safe.

3. **Source selection.**
   - If `abook` is fully configured (creds + NZBGet client + nzbking client) **and** the event didn't pin a `source_id`, try abook first. A direct `source_id` always routes to AudiobookBay because it's an AudiobookBay-internal slug.
   - On any abook hard failure (login, search, topic fetch, nzbking lookup, NZBGet append) the consumer logs a warning and falls through to AudiobookBay. abook is opportunistic, not gating.
   - The abook path persists a `source_id=abook:<topic_id>` and an `external_id=nzbget:<NZBID>`. Both prefixes are load-bearing; see [`reconciler.md`](reconciler.md) for the prefix-based routing.

4. **Dispatch outcome → second DB write.**
   - Success: upsert with the resolved `external_id`, `info_hash`, `magnet_uri` / NZBGet id, score and reason. Status = `acknowledged` (or whatever the downloader returned).
   - Failure: upsert `status=failed`, `error_text=<upstream error>`, publish `request_failed`.
   - If the upsert itself fails after a successful dispatch, the handler **nacks** — losing the `external_id` would orphan the torrent/NZB and the reconciler would skip the row forever (it requires a non-empty `external_id`). Re-dispatching on redelivery is idempotent (qBittorrent / embedded torrent / NZBGet all de-dupe by hash or NZBID).

5. **Reconciler poll.** Every minute the scheduler fires `reconciler.Tick`. It pulls up to 200 non-terminal rows ordered by `last_polled ASC` so the oldest stale rows go first. For each row it picks a backend by `external_id` prefix:
   - prefix `nzbget:` → `nzbget.Lookup(nzbID)` (queue then history)
   - anything else → `audiobookbay.GetDownload(hash)` (qBittorrent / embedded / scrape_only)

6. **Status transitions.** Translated upstream states are persisted via `MarkPolled`, which **also clears `error_text`** (a previous transient error would otherwise stick forever — `UpsertForwardedRequest` uses `COALESCE` and can't unset). Terminal transitions publish:
   - `request_fulfilled` for `imported`
   - `request_failed` for `failed`
   - `request_status_changed` for any other transition

## Terminal guard

`UpsertForwardedRequest` and `MarkPolled` both refuse to move a row out of `imported`/`failed`:

```sql
status = CASE
  WHEN forwarded_request.status IN ('imported','failed') THEN forwarded_request.status
  ELSE EXCLUDED.status
END
```

This is what makes at-least-once event delivery safe. A duplicate `request_submitted` for an already-`imported` request will not resurrect it. The only way out of a terminal state is the admin **Retry** button, which resets to `acknowledged` (and only succeeds if `external_id` is non-empty).

## Mandatory writes vs ack/nack table

| Failure point | Handler decision | Why |
| --- | --- | --- |
| Initial `submitted` upsert fails | nack | redelivery picks it up; no orphaned downloader work |
| Empty query AND empty source_id | ack (record `failed` + publish) | not retryable — host shouldn't redeliver |
| Search/Resolve upstream error | ack (record `failed` + publish) | retryable via admin UI, not via redelivery |
| Post-dispatch upsert fails | nack | losing `external_id` = permanent orphan |
| Reconciler row UPDATE fails | logged, tick continues | next tick retries; budget protected |

## Reading the row

```sql
SELECT request_id, status, external_id, search_query, selected_title,
       selected_score, error_text, last_polled, updated_at
FROM audiobook_requests.forwarded_request
WHERE request_id = $1;
```

Useful filters:

```sql
-- Rows that arrived but never got a downstream id (consumer never got
-- past the search).
SELECT * FROM audiobook_requests.forwarded_request
WHERE status NOT IN ('imported','failed')
  AND COALESCE(external_id,'') = '';

-- Rows stuck non-terminal for more than 24h (matches the admin
-- "Stuck requests" tile).
SELECT * FROM audiobook_requests.forwarded_request
WHERE status NOT IN ('imported','failed')
  AND (last_polled < now() - interval '24 hours'
       OR (last_polled IS NULL AND created_at < now() - interval '24 hours'));

-- Was this row dispatched via abook or AudiobookBay?
SELECT request_id,
       CASE
         WHEN source_id  LIKE 'abook:%'  THEN 'abook'
         WHEN external_id LIKE 'nzbget:%' THEN 'abook'
         ELSE 'audiobookbay'
       END AS pipeline
FROM audiobook_requests.forwarded_request
WHERE request_id = $1;
```
