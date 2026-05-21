# abook.link and the NZBGet handoff

abook.link is a Simple Machines Forum (SMF) instance hosting audiobook NZB posts. The plugin scrapes it the same way the original `/opt/librarymanager/scripts/abook-search.ts` script did, then resolves the per-topic Usenet search code through `nzbking.com` and appends the resulting NZB URL to NZBGet.

## Pipeline at a glance

```
request_submitted (title, author)
  └── consumer.tryAbook (only when abook+NZBGet trio is wired)
        ├── ensureAbookSession  ── relogin if no cookies
        ├── abook.Search        ── one re-login retry on failure
        ├── pickBestAbookHit    ── token-overlap heuristic
        ├── abook.FetchTopic    ── auto-clicks "Thanks" if SMF gates the body
        ├── stripSearchPrefix   ── normalises "abook.link - " / "PW - " noise
        ├── nzbking.Search      ── most-recent NZB candidate wins
        └── nzbget.Append       ── stores nzbget:<NZBID> as external_id
```

The full chain is bounded by `abookSearchTimeout = 25s` (in `internal/consumer/abook.go`). Each HTTP step uses its own derived context so a single slow call doesn't burn the whole budget — but the outer cap is hard.

## Credentials and cookie persistence

`abook_email` / `abook_password` are mandatory to use abook (the consumer's `abookEnabled()` requires both). They're stored in `app_config.data` and the password is masked everywhere the admin UI renders the config back.

**Cookie persistence flow:**

1. On first event with no cookies, `ensureAbookSession` falls through to `relogin`.
2. `relogin` calls `abook.Client.Login` (SMF dance: GET `/index.php?action=login` for the rotating CSRF nonce, POST `/index.php?action=login2`, verify by hitting `/index.php` and looking for the `Welcome, <strong>Guest</strong>` marker).
3. On success, `consumer.CookieStore.SaveAbookCookie` writes the serialised cookie header to `app_config.data.abook_cookie`.
4. On next plugin restart, `main.go` calls `abook.Client.SetCookieHeader(cfg.AbookCookie)` to re-hydrate the jar before any event lands.

The cookie is **never exposed via the admin GET config endpoint** — `handleGetConfig` strips it (along with all passwords) before serialising. The admin **Test abook login** button mints a fresh cookie and persists it if the form's email/password match the stored values; the response returns only `cookieLength` and `cookiePersisted`, never the cookie itself.

## Session expiry

Symptoms:
- `abook search: ...` then `abook search (after re-login): ...` paired errors in the plugin logs.
- A burst of fallback-to-AudiobookBay log lines (abook is opportunistic; on hard failure the consumer logs a warning and falls through).
- Admin **Test abook login** button works, but real requests still fall back.

What happens internally:
- SMF expires the `PHPSESSID` (varies per install; abook's session length is bounded by SMF's `cookielength`, which the client sets to `-1` = forever, but the host may evict the session anyway).
- `abook.Search` returns either zero hits (logged-out shell) or an HTTP error.
- The consumer triggers a single re-login retry. If that also fails, the row falls back to AudiobookBay — abook is never the gating source.

What an operator should do:
- Open the admin UI, hit **Test abook login**. If it succeeds, the new cookie is persisted and the next event will use it.
- If **Test abook login** fails with `credentials rejected` or `session did not upgrade`, the password actually changed on abook.link. Update `abook_password` and save.
- If it fails with `SMF login form shape changed (no CSRF nonce found)`, the forum changed its HTML and `extractCSRFNonce` no longer matches. Check `internal/abook/client.go` against the live `/index.php?action=login` page; the regex pinned in `smfHiddenFieldRe` is the failure point.

## Topic gating ("Thanks")

abook hides the Usenet code/password behind an SMF mod that says `You must thank this post to see the content`. `FetchTopic` detects this, makes the thank request (URL extracted from `action=thank` links on the page), and refetches the topic. The thank-page response is discarded — SMF just flips an internal per-user-per-topic flag.

If the consumer logs `fetch topic N: ...` or `refetch topic N after thank: ...` consistently, either the thank action URL pattern changed, or the abook account doesn't have permission to thank posts. Try thanking a post manually in the abook UI with the same account.

## nzbking resolution

`internal/nzbking` takes the search code lifted from the topic and calls `nzbking.com` to find an NZB URL. It returns hits in most-recent-first order; the plugin trusts that ordering and picks `nzbResults[0]`.

If nzbking is down or returns zero hits, the consumer logs `nzbking lookup: ...` or `(false, nil)` from `tryAbook` and falls back to AudiobookBay.

## NZBGet append

`nzbget.Append` posts a JSON-RPC `append` with:
- `NZBFilename` = `<sanitized-title>.nzb` (`sanitizeNZBName` strips non-`\w.-` chars and trims).
- `Content` = the nzbking-resolved URL (NZBGet fetches the NZB itself).
- `Category` = configured `nzbget_category` (defaults to `"audiobooks"` in embedded mode).
- `PPParameters` = `[{Name: "*Unpack:Password", Value: <topic-password>}]` when abook exposed one.

The returned integer NZBID is stored as `external_id = "nzbget:<NZBID>"`. NZBID `0` means NZBGet's DupeMode rejected the append (usually because the same NZB is already queued). The plugin treats this as an append failure; the operator should check NZBGet's queue/history manually for the previous job.

**Auth:** the plugin embeds `user:pass@` directly in the JSON-RPC URL. NZBGet returns 401 if the password is wrong; the client surfaces that as `nzbget <method>: unauthorized (check NZBGet user/password)`. Use the admin **Test NZBGet** button to verify (`version` RPC).

## Persisted state

After a successful abook dispatch the row looks like:

```
status                : acknowledged
external_id           : nzbget:42
source_id             : abook:12345           (topic_id)
search_query          : "Foundation Isaac Asimov"
selected_title        : "Foundation - Isaac Asimov - 1951"
detail_url            : https://abook.link/book/index.php?topic=12345.0
selected_score_reason : "abook+nzbget: subject... | size..."
```

The `classifySource` helper in `internal/server/server.go` uses both prefixes (`source_id LIKE 'abook:%'` OR `external_id LIKE 'nzbget:%'`) to tag the row "abook" in the admin queue table.
