package consumer

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RXWatcher/continuum-plugin-audiobook-requests/internal/abook"
	"github.com/RXWatcher/continuum-plugin-audiobook-requests/internal/nzbget"
	"github.com/RXWatcher/continuum-plugin-audiobook-requests/internal/store"
)

// abookSearchTimeout caps every abook HTTP step (login, search, topic
// fetch, nzbking search, NZBGet append). Each step gets its own derived
// context so a single hung call doesn't burn the whole budget — but the
// outer cap keeps the consumer goroutine from being pinned indefinitely
// if every step is slow.
const abookSearchTimeout = 25 * time.Second

// tryAbook runs the full abook → nzbking → NZBGet pipeline for a single
// user request. Returns (true, nil) if it dispatched to NZBGet and the
// caller should treat the event as fully handled. (false, nil) means
// abook produced no usable result and the caller should fall back to
// AudiobookBay. A non-nil error means a transient failure the caller
// should log + fall back on.
func (h *Handler) tryAbook(ctx context.Context, d *Deps, requestID, query string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, abookSearchTimeout)
	defer cancel()

	if err := h.ensureAbookSession(ctx, d); err != nil {
		return false, fmt.Errorf("abook login: %w", err)
	}

	hits, err := d.Abook.Search(ctx, query)
	if err != nil {
		// One re-login retry on the assumption a fresh cookie fixes it
		// (cookie expiry surfaces as a 200 with a logged-out shell which
		// Search() parses as zero rows OR as a fetch error; either way a
		// fresh login is worth a single try).
		if rerr := h.relogin(ctx, d); rerr != nil {
			return false, fmt.Errorf("abook search: %w; re-login also failed: %v", err, rerr)
		}
		hits, err = d.Abook.Search(ctx, query)
		if err != nil {
			return false, fmt.Errorf("abook search (after re-login): %w", err)
		}
	}
	if len(hits) == 0 {
		return false, nil
	}

	chosen := pickBestAbookHit(hits, query)
	tc, err := d.Abook.FetchTopic(ctx, chosen.TopicID)
	if err != nil {
		return false, fmt.Errorf("abook topic %d: %w", chosen.TopicID, err)
	}
	if tc.Search == "" {
		// Topic exists but doesn't expose a Usenet search code (some
		// boards post direct links instead). Skip; AudiobookBay may
		// still produce a torrent.
		return false, nil
	}

	nzbResults, err := d.Nzbking.Search(ctx, stripSearchPrefix(tc.Search))
	if err != nil {
		return false, fmt.Errorf("nzbking lookup: %w", err)
	}
	if len(nzbResults) == 0 {
		return false, nil
	}
	bestNZB := nzbResults[0] // nzbking returns most-recent first; trusting that ordering matches the .ts reference

	nzbName := sanitizeNZBName(chosen.Title)
	nzbID, err := d.Nzbget.Append(ctx, nzbget.AppendOptions{
		URL:            bestNZB.NZBURL,
		Name:           nzbName,
		Category:       d.NZBGetCategory,
		UnpackPassword: tc.Password,
	})
	if err != nil {
		return false, fmt.Errorf("nzbget append: %w", err)
	}

	externalID := NZBExternalIDPrefix + strconv.Itoa(nzbID)
	row := store.ForwardedRequest{
		RequestID:           requestID,
		ExternalID:          externalID,
		Status:              "acknowledged",
		SourceID:            fmt.Sprintf("abook:%d", chosen.TopicID),
		SearchQuery:         query,
		SelectedTitle:       chosen.Title,
		DetailURL:           chosen.URL,
		SelectedScoreReason: fmt.Sprintf("abook+nzbget: %s | %s", bestNZB.Subject, bestNZB.Size),
		UpdatedAt:           time.Now(),
	}
	if err := d.Store.UpsertForwardedRequest(ctx, row); err != nil {
		// Persisting the NZBGet id is mandatory: without it the
		// reconciler can't poll, and the download is invisible.
		// Surface the dispatch ID in the error so an operator can
		// manually fix the row if the failure path keeps recurring.
		return true, fmt.Errorf("persist abook row (nzbget id %d already queued): %w", nzbID, err)
	}

	d.Pub.Publish(ctx, "request_acknowledged", map[string]any{
		"request_id": requestID, "requestId": requestID,
		"external_id": externalID, "provider_plugin_id": d.PluginID,
		"selected_title": chosen.Title, "detail_url": chosen.URL,
		"score": chosen.TopicID, "reason": row.SelectedScoreReason,
		"status": "acknowledged",
	})
	return true, nil
}

// ensureAbookSession warms the cookie jar from the stored cookie (cheap)
// or performs a fresh login if the stored value is empty. Caller is
// responsible for invoking relogin() on suspected expiry.
func (h *Handler) ensureAbookSession(ctx context.Context, d *Deps) error {
	// Cheap fast-path: if the jar already has cookies (e.g. from a prior
	// event in the same Configure lifecycle) skip the verification probe.
	if d.Abook.CookieHeader() != "" {
		return nil
	}
	return h.relogin(ctx, d)
}

// relogin runs the SMF login dance and (best-effort) persists the new
// cookie via the CookieStore so the next plugin restart skips a login.
func (h *Handler) relogin(ctx context.Context, d *Deps) error {
	if err := d.Abook.Login(ctx, d.AbookEmail, d.AbookPassword); err != nil {
		return err
	}
	if d.Cookies != nil {
		if err := d.Cookies.SaveAbookCookie(ctx, d.Abook.CookieHeader()); err != nil {
			h.logger.Warn("save abook cookie", "err", err)
		}
	}
	return nil
}

// pickBestAbookHit picks the abook search result whose title shares the
// most tokens with the query. With no further signal (no seeders, no
// dates we can fully trust) this is a deliberately simple heuristic;
// good-enough for an authenticated forum where there are usually 1-5
// results per query.
func pickBestAbookHit(hits []abook.SearchResult, query string) abook.SearchResult {
	if len(hits) == 1 {
		return hits[0]
	}
	qTokens := tokenSet(strings.ToLower(query))
	bestIdx, bestScore := 0, -1
	for i, h := range hits {
		score := 0
		for tok := range tokenSet(strings.ToLower(h.Title)) {
			if qTokens[tok] {
				score++
			}
		}
		if score > bestScore {
			bestIdx, bestScore = i, score
		}
	}
	return hits[bestIdx]
}

func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(f) > 1 {
			out[f] = true
		}
	}
	return out
}

// stripSearchPrefix mirrors the .ts reference: abook posts frequently
// prefix the actual unique Usenet search term with "abook.link - " /
// "abook.ws-" / "PW - " and similar; stripping these gives nzbking a
// cleaner query that hits more results.
var searchPrefixRe = regexp.MustCompile(`(?i)^(?:abook\.(?:link|ws)\s*-\s*|PW\s*-\s*)?(.+)$`)

func stripSearchPrefix(s string) string {
	m := searchPrefixRe.FindStringSubmatch(strings.TrimSpace(s))
	if len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return s
}

// sanitizeNZBName produces a filesystem-safe NZB filename body. NZBGet
// uses this for the queued job's display name and the on-disk file; bad
// characters can confuse downstream post-processing scripts.
func sanitizeNZBName(title string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		t = "audiobook-request"
	}
	bad := regexp.MustCompile(`[^\w.\-]+`)
	t = bad.ReplaceAllString(t, "_")
	if len(t) > 120 {
		t = t[:120]
	}
	return strings.Trim(t, "._-")
}

// Sentinel for tests that want to assert the abook path was tried.
var ErrAbookEmpty = errors.New("abook returned no hits")
