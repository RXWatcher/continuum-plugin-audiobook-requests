package audiobookbay

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/embedded"
	"golang.org/x/net/html"
)

const (
	defaultTimeout  = 30 * time.Second
	maxResponseSize = 8 << 20
	// errBodySnippet caps how much of an upstream error body is inlined into
	// an error string. The body can be up to maxResponseSize and the error
	// propagates into logs and request error_text.
	errBodySnippet = 512
)

func truncForError(b []byte) string {
	if len(b) <= errBodySnippet {
		return string(b)
	}
	return string(b[:errBodySnippet]) + "…(truncated)"
}

var (
	detailHrefRe = regexp.MustCompile(`(?i)^/(audio-books|abss)/[^"'#?]+/?`)
	infoHashRe   = regexp.MustCompile(`(?is)<td>\s*Info Hash:\s*</td>\s*<td>\s*([0-9a-f]{40})\s*</td>`)
	hashRe       = regexp.MustCompile(`(?i)\b[0-9a-f]{40}\b`)
	magnetRe     = regexp.MustCompile(`(?i)magnet:\?xt=urn:btih:[^"' <]+`)
	trackerRe    = regexp.MustCompile(`(?i)\b(?:udp|https?)://[^\s<"]+`)
)

// maxTrackerScan caps how many URL-ish substrings extractTrackers pulls from a
// (hostile, up to 8 MiB) scraped page; maxTrackers caps what it keeps.
const (
	maxTrackerScan = 1024
	maxTrackers    = 64
)

type Client struct {
	cfg      Config
	hc       *http.Client
	qbt      *QBitClient
	embedded *embedded.Manager
}

func NewClient(cfg Config, embeddedManager *embedded.Manager) *Client {
	if cfg.SearchLimit <= 0 {
		cfg.SearchLimit = 10
	}
	if cfg.DownloadMode == "" {
		if cfg.QBitURL != "" {
			cfg.DownloadMode = "qbittorrent"
		} else {
			cfg.DownloadMode = "scrape_only"
		}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Client{
		cfg:      cfg,
		hc:       &http.Client{Timeout: defaultTimeout},
		qbt:      NewQBitClient(cfg.QBitURL, cfg.QBitUsername, cfg.QBitPassword),
		embedded: embeddedManager,
	}
}

func (c *Client) BaseURL() string { return c.cfg.BaseURL }

func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/?s=foundation", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("audiobookbay returned %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) QBitConfigured() bool { return c.cfg.QBitURL != "" }

func (c *Client) EmbeddedConfigured() bool {
	return c.cfg.DownloadMode == "embedded" && c.embedded != nil
}

func (c *Client) DownloadMode() string { return c.cfg.DownloadMode }

func (c *Client) PingQBit(ctx context.Context) error {
	if c.cfg.QBitURL == "" {
		return nil
	}
	return c.qbt.Login(ctx)
}

func (c *Client) ExternalSearch(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	if limit <= 0 || limit > c.cfg.SearchLimit {
		limit = c.cfg.SearchLimit
	}
	var hits []SearchHit
	seen := map[string]bool{}
	for page := 1; page <= limit && len(hits) < limit; page++ {
		u := c.cfg.BaseURL + fmt.Sprintf("/page/%d/?s=%s", page, strings.ReplaceAll(url.QueryEscape(strings.ToLower(query)), "%20", "+"))
		body, err := c.get(ctx, u)
		if err != nil {
			if page == 1 {
				return nil, err
			}
			break
		}
		pageHits, err := c.parseSearch(body, limit-len(hits))
		if err != nil {
			return nil, err
		}
		if len(pageHits) == 0 {
			break
		}
		for _, hit := range pageHits {
			if seen[hit.DetailURL] {
				continue
			}
			seen[hit.DetailURL] = true
			hits = append(hits, hit)
			if len(hits) >= limit {
				break
			}
		}
	}
	for i := range hits {
		if hits[i].InfoHash != "" || hits[i].MagnetURI != "" {
			continue
		}
		detail, err := c.Resolve(ctx, hits[i].SourceID)
		if err == nil {
			hits[i].InfoHash = detail.InfoHash
			hits[i].MagnetURI = detail.MagnetURI
			if detail.Title != "" {
				hits[i].Title = detail.Title
			}
		}
	}
	scoreHits(hits, query)
	return hits, nil
}

func (c *Client) Resolve(ctx context.Context, sourceID string) (SearchHit, error) {
	detailURL := sourceID
	if strings.HasPrefix(sourceID, "/") {
		detailURL = c.cfg.BaseURL + sourceID
	}
	// SSRF guard: source_id comes from the request_submitted payload
	// (attacker-influenced). The resolved URL must stay within the
	// operator-configured AudiobookBay base; an absolute source_id like
	// http://169.254.169.254/ or http://internal-host/ would otherwise be
	// fetched verbatim.
	if detailURL != c.cfg.BaseURL && !strings.HasPrefix(detailURL, c.cfg.BaseURL+"/") {
		return SearchHit{}, fmt.Errorf("source_id outside AudiobookBay base")
	}
	body, err := c.get(ctx, detailURL)
	if err != nil {
		return SearchHit{}, err
	}
	title := firstText(body, "h1")
	magnet := ""
	if m := magnetRe.FindString(body); m != "" {
		magnet = htmlUnescape(m)
	}
	infoHash := ""
	if match := infoHashRe.FindStringSubmatch(body); len(match) == 2 {
		infoHash = strings.ToLower(match[1])
	}
	trackers := extractTrackers(body)
	if magnet == "" && infoHash != "" {
		magnet = c.magnetFromHash(infoHash, title, trackers)
	}
	if infoHash == "" && strings.HasPrefix(strings.ToLower(magnet), "magnet:") {
		infoHash = infoHashFromMagnet(magnet)
	}
	if magnet == "" {
		return SearchHit{}, fmt.Errorf("no magnet or info hash found on %s", detailURL)
	}
	return SearchHit{
		SourceID:  detailURL,
		Source:    "audiobookbay",
		Title:     strings.TrimSpace(title),
		DetailURL: detailURL,
		InfoHash:  infoHash,
		MagnetURI: magnet,
	}, nil
}

func (c *Client) StartDownload(ctx context.Context, sourceID, query string) (DownloadResponse, error) {
	var hit SearchHit
	var err error
	if sourceID != "" {
		hit, err = c.Resolve(ctx, sourceID)
	} else {
		results, searchErr := c.ExternalSearch(ctx, query, 5)
		if searchErr != nil {
			return DownloadResponse{}, searchErr
		}
		if len(results) == 0 {
			return DownloadResponse{}, fmt.Errorf("no AudiobookBay result for %q", query)
		}
		hit = results[0]
		if hit.MagnetURI == "" {
			hit, err = c.Resolve(ctx, hit.SourceID)
			hit.Score, hit.Reason = scoreHit(hit, query)
		}
	}
	if err != nil {
		return DownloadResponse{}, err
	}
	if hit.MagnetURI == "" {
		return DownloadResponse{}, fmt.Errorf("selected AudiobookBay result has no magnet")
	}
	id := hit.InfoHash
	if id == "" {
		id = fallbackID(hit.MagnetURI)
	}
	savePath := c.savePathFor(hit.Title)
	switch c.cfg.DownloadMode {
	case "qbittorrent":
		if err := c.qbt.AddMagnet(ctx, hit.MagnetURI, c.cfg.Category, savePath); err != nil {
			return DownloadResponse{}, err
		}
		return downloadResponse(id, "queued", hit), nil
	case "embedded":
		if c.embedded == nil {
			return DownloadResponse{}, fmt.Errorf("embedded downloader not configured")
		}
		st, err := c.embedded.Add(ctx, id, hit.MagnetURI, hit.Title)
		if err != nil {
			return DownloadResponse{}, err
		}
		resp := downloadResponse(id, st.Status, hit)
		resp.Progress = st.Progress
		return resp, nil
	default:
		return downloadResponse(id, "magnet_ready", hit), nil
	}
}

func (c *Client) GetDownload(ctx context.Context, hash string) (DownloadResponse, error) {
	switch c.cfg.DownloadMode {
	case "qbittorrent":
	case "embedded":
		if c.embedded == nil {
			return DownloadResponse{}, fmt.Errorf("embedded downloader not configured")
		}
		st, err := c.embedded.Status(ctx, hash)
		if err != nil {
			return DownloadResponse{}, err
		}
		return DownloadResponse{ID: hash, Status: st.Status, Title: st.Title, Progress: st.Progress}, nil
	default:
		return DownloadResponse{ID: hash, Status: "magnet_ready"}, nil
	}
	t, err := c.qbt.Torrent(ctx, hash)
	if err != nil {
		return DownloadResponse{}, err
	}
	status := "downloading"
	if t.Hash == "" {
		status = "queued"
	} else if t.Progress >= 0.999 {
		status = "imported"
	} else if strings.Contains(strings.ToLower(t.State), "error") || strings.Contains(strings.ToLower(t.State), "missing") {
		status = "failed"
	}
	return DownloadResponse{ID: hash, Status: status, Title: t.Name, Progress: int(t.Progress * 100)}, nil
}

func (c *Client) RestoreDownload(ctx context.Context, hash, magnet, title string) error {
	if c.cfg.DownloadMode != "embedded" || c.embedded == nil || magnet == "" {
		return nil
	}
	_, err := c.embedded.Add(ctx, hash, magnet, title)
	return err
}

func (c *Client) get(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("audiobookbay %d: %s", resp.StatusCode, truncForError(body))
	}
	return string(body), nil
}

func (c *Client) parseSearch(body string, limit int) ([]SearchHit, error) {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var hits []SearchHit
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(hits) >= limit {
			return
		}
		if n.Type == html.ElementNode && n.Data == "a" {
			if href := attr(n, "href"); href != "" {
				u, ok := c.normalizeDetailURL(href)
				if ok && !seen[u] {
					seen[u] = true
					hits = append(hits, SearchHit{
						SourceID:  u,
						Source:    "audiobookbay",
						Title:     strings.TrimSpace(nodeText(n)),
						DetailURL: u,
					})
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	sort.SliceStable(hits, func(i, j int) bool {
		return len(hits[i].Title) > len(hits[j].Title)
	})
	return hits, nil
}

func scoreHits(hits []SearchHit, query string) {
	for i := range hits {
		hits[i].Score, hits[i].Reason = scoreHit(hits[i], query)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].Seeders > hits[j].Seeders
		}
		return hits[i].Score > hits[j].Score
	})
}

func scoreHit(hit SearchHit, query string) (int, string) {
	title := normalizeText(hit.Title)
	q := normalizeText(query)
	if q == "" {
		return hit.Seeders, "source_id supplied"
	}
	score := hit.Seeders
	reasons := []string{}
	if title == q {
		score += 120
		reasons = append(reasons, "exact title match")
	} else if strings.Contains(title, q) {
		score += 80
		reasons = append(reasons, "title contains query")
	}
	qTokens := tokenSet(q)
	titleTokens := tokenSet(title)
	matches := 0
	for token := range qTokens {
		if titleTokens[token] {
			matches++
		}
	}
	if len(qTokens) > 0 {
		coverage := matches * 100 / len(qTokens)
		score += coverage
		reasons = append(reasons, fmt.Sprintf("%d%% query token coverage", coverage))
	}
	if hit.InfoHash != "" || hit.MagnetURI != "" {
		score += 20
		reasons = append(reasons, "magnet resolved")
	}
	if hit.Seeders > 0 {
		reasons = append(reasons, fmt.Sprintf("%d seeders", hit.Seeders))
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "fallback result order")
	}
	return score, strings.Join(reasons, ", ")
}

func normalizeText(s string) string {
	s = strings.ToLower(s)
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}), " ")
}

func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, token := range strings.Fields(s) {
		if len(token) > 1 {
			out[token] = true
		}
	}
	return out
}

func downloadResponse(id, status string, hit SearchHit) DownloadResponse {
	return DownloadResponse{
		ID:        id,
		Status:    status,
		Magnet:    hit.MagnetURI,
		Title:     hit.Title,
		DetailURL: hit.DetailURL,
		InfoHash:  hit.InfoHash,
		Score:     hit.Score,
		Reason:    hit.Reason,
	}
}

func (c *Client) normalizeDetailURL(href string) (string, bool) {
	href = htmlUnescape(strings.TrimSpace(href))
	if href == "" {
		return "", false
	}
	if strings.HasPrefix(href, c.cfg.BaseURL) {
		u, err := url.Parse(href)
		if err == nil {
			href = u.EscapedPath()
		}
	}
	if !detailHrefRe.MatchString(href) {
		return "", false
	}
	return c.cfg.BaseURL + href, true
}

func (c *Client) magnetFromHash(infoHash, title string, pageTrackers []string) string {
	parts := []string{"xt=urn:btih:" + strings.ToLower(infoHash)}
	if title != "" {
		parts = append(parts, "dn="+url.QueryEscape(title))
	}
	trackers := pageTrackers
	if len(trackers) == 0 {
		trackers = c.cfg.Trackers
	}
	if len(trackers) == 0 {
		trackers = []string{
			"udp://tracker.openbittorrent.com:80",
			"udp://opentor.org:2710",
			"udp://tracker.ccc.de:80",
			"udp://tracker.blackunicorn.xyz:6969",
		}
	}
	for _, tr := range trackers {
		if strings.TrimSpace(tr) != "" {
			parts = append(parts, "tr="+url.QueryEscape(strings.TrimSpace(tr)))
		}
	}
	return "magnet:?" + strings.Join(parts, "&")
}

func (c *Client) savePathFor(title string) string {
	base := strings.TrimSpace(c.cfg.SavePath)
	if base == "" || strings.TrimSpace(title) == "" {
		return base
	}
	return path.Join(base, sanitizeTitle(title))
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(cur *html.Node) {
		if cur.Type == html.TextNode {
			b.WriteString(cur.Data)
			b.WriteByte(' ')
		}
		for child := cur.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func firstText(body, tag string) string {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return ""
	}
	var out string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if out != "" {
			return
		}
		if n.Type == html.ElementNode && n.Data == tag {
			out = nodeText(n)
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return out
}

func htmlUnescape(s string) string {
	return strings.NewReplacer("&amp;", "&", "&#038;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`).Replace(s)
}

func infoHashFromMagnet(magnet string) string {
	u, err := url.Parse(magnet)
	if err != nil {
		return ""
	}
	xt := u.Query().Get("xt")
	return strings.TrimPrefix(strings.ToLower(xt), "urn:btih:")
}

func fallbackID(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func extractTrackers(body string) []string {
	matches := trackerRe.FindAllString(body, maxTrackerScan)
	seen := map[string]bool{}
	out := make([]string, 0, maxTrackers)
	for _, tracker := range matches {
		tracker = strings.TrimRight(strings.TrimSpace(htmlUnescape(tracker)), "'\"),.;<")
		lower := strings.ToLower(tracker)
		if tracker == "" || seen[tracker] || (!strings.Contains(lower, "announce") && !strings.Contains(lower, "tracker")) {
			continue
		}
		seen[tracker] = true
		out = append(out, tracker)
		if len(out) >= maxTrackers {
			break
		}
	}
	return out
}

func sanitizeTitle(title string) string {
	title = strings.TrimSpace(title)
	return strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return -1
		default:
			return r
		}
	}, title)
}
