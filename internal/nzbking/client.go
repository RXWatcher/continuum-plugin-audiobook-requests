// Package nzbking scrapes nzbking.com search results to turn an abook
// "search code" into an NZB download URL.
//
// Port of the searchNzbking() function in
// /opt/librarymanager/scripts/abook-search.ts. Same brittleness caveat
// applies — the page shape can change at any time; fixtures pin the
// parser so a future shape change shows up as a test failure rather than
// silent "no results" in the queue.
package nzbking

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultTimeout  = 15 * time.Second
	maxResponseSize = 8 << 20
	baseURL         = "https://nzbking.com"
)

// Result is one NZB row scraped from a nzbking search page.
type Result struct {
	Subject           string
	NZBURL            string
	DetailsURL        string
	Poster            string
	Group             string
	Age               string
	Size              string
	Parts             string
	PasswordProtected bool
}

// Client is a tiny wrapper around an http.Client. nzbking has no auth and
// no per-account state, so one Client serves all callers.
type Client struct {
	hc *http.Client
}

func New() *Client {
	return &Client{hc: &http.Client{Timeout: defaultTimeout}}
}

// Search returns the result rows for a free-text query. Caller is expected
// to pass the abook "search code" verbatim (or with its abook.link prefix
// stripped — the .ts script does the latter).
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	if query == "" {
		return nil, fmt.Errorf("nzbking: query is required")
	}
	u := baseURL + "/search/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nzbking: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("nzbking %d", resp.StatusCode)
	}
	return parseResults(string(body)), nil
}

// parseResults walks the search results page and pulls one Result per
// checkbox-prefixed row. The HTML grammar nzbking emits is:
//   <input type="checkbox" ...> SUBJECT <a href="/nzb:HASH/">NZB</a>
//      <a href="/details:HASH/">Details</a>
//      parts: N/N  size: N MB  <a href="/poster/...">poster</a>  a.b.GROUP  Nd
//   <input type="checkbox" ...> ... (next row)
// We use checkbox boundaries as the row separator so PAS PROTECTED tags
// can't leak across rows.
var (
	// rowMarker locates each checkbox boundary. Go's RE2 has no lookahead,
	// so we collect the boundary indices and slice the body manually
	// rather than using a single regex with an alternation terminator
	// (which would consume the next boundary into match N and skip every
	// other row).
	rowMarker    = regexp.MustCompile(`<input[^>]*type="checkbox"[^>]*>`)
	nzbHrefRe    = regexp.MustCompile(`href="(/nzb:[a-f0-9]+/)"`)
	detailsRe    = regexp.MustCompile(`href="(/details:[a-f0-9]+/)"`)
	partsRe      = regexp.MustCompile(`parts:\s*([\d,]+/[\d,]+)`)
	sizeRe       = regexp.MustCompile(`(?i)size:\s*([\d.]+\s*[KMGT]B)`)
	posterRe     = regexp.MustCompile(`href="/poster/[^"]*">([^<]+)`)
	groupRe      = regexp.MustCompile(`(a\.b\.[^<\s]+)`)
	ageRe        = regexp.MustCompile(`(\d+d)\b`)
	subjectStrip = regexp.MustCompile(`<[^>]+>`)
)

func parseResults(body string) []Result {
	// Find every checkbox boundary; each row is the slice from one boundary
	// to the next (or to the trailing "Query time:" footer / end-of-body).
	idxs := rowMarker.FindAllStringIndex(body, -1)
	if len(idxs) == 0 {
		return nil
	}
	// Tail bound — prefer the query-time footer when present so we don't
	// scoop unrelated trailing markup into the last row.
	tail := len(body)
	if qt := strings.Index(body, "Query time:"); qt >= 0 {
		tail = qt
	}
	var out []Result
	for i, span := range idxs {
		start := span[1]
		end := tail
		if i+1 < len(idxs) {
			end = idxs[i+1][0]
		}
		if start >= end {
			continue
		}
		row := body[start:end]
		nzb := nzbHrefRe.FindStringSubmatch(row)
		if len(nzb) != 2 {
			continue
		}
		// Subject is the text that appears before the first NZB anchor.
		subject := ""
		if idx := strings.Index(row, `<a`); idx >= 0 {
			subject = collapseWhitespace(subjectStrip.ReplaceAllString(row[:idx], ""))
		}
		r := Result{
			Subject:           subject,
			NZBURL:            baseURL + nzb[1],
			PasswordProtected: strings.Contains(row, "PASSWORD PROTECTED"),
		}
		if d := detailsRe.FindStringSubmatch(row); len(d) == 2 {
			r.DetailsURL = baseURL + d[1]
		}
		if p := partsRe.FindStringSubmatch(row); len(p) == 2 {
			r.Parts = p[1]
		}
		if s := sizeRe.FindStringSubmatch(row); len(s) == 2 {
			r.Size = s[1]
		}
		if p := posterRe.FindStringSubmatch(row); len(p) == 2 {
			r.Poster = strings.TrimSpace(p[1])
		}
		if g := groupRe.FindStringSubmatch(row); len(g) == 2 {
			r.Group = g[1]
		}
		if a := ageRe.FindStringSubmatch(row); len(a) == 2 {
			r.Age = a[1]
		}
		out = append(out, r)
	}
	return out
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
