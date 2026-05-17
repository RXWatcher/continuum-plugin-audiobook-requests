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

	"golang.org/x/net/html"
)

const (
	defaultTimeout  = 30 * time.Second
	maxResponseSize = 8 << 20
)

var (
	detailHrefRe = regexp.MustCompile(`(?i)^/(audio-books|abss)/[^"'#?]+/?`)
	hashRe       = regexp.MustCompile(`(?i)\b[0-9a-f]{40}\b`)
	magnetRe     = regexp.MustCompile(`(?i)magnet:\?xt=urn:btih:[^"' <]+`)
)

type Client struct {
	cfg Config
	hc  *http.Client
	qbt *QBitClient
}

func NewClient(cfg Config) *Client {
	if cfg.SearchLimit <= 0 {
		cfg.SearchLimit = 10
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Client{
		cfg: cfg,
		hc:  &http.Client{Timeout: defaultTimeout},
		qbt: NewQBitClient(cfg.QBitURL, cfg.QBitUsername, cfg.QBitPassword),
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
	if m := hashRe.FindString(body); m != "" {
		infoHash = strings.ToLower(m)
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
	if c.cfg.QBitURL != "" {
		if err := c.qbt.AddMagnet(ctx, hit.MagnetURI, c.cfg.Category, savePath); err != nil {
			return DownloadResponse{}, err
		}
	} else {
		return downloadResponse(id, "magnet_ready", hit), nil
	}
	return downloadResponse(id, "queued", hit), nil
}

func (c *Client) GetDownload(ctx context.Context, hash string) (DownloadResponse, error) {
	if c.cfg.QBitURL == "" {
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

func (c *Client) get(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Continuum AudiobookBay Requests/0.1")
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
		return "", fmt.Errorf("audiobookbay %d: %s", resp.StatusCode, string(body))
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
	v := url.Values{}
	v.Set("xt", "urn:btih:"+strings.ToLower(infoHash))
	if title != "" {
		v.Set("dn", title)
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
			v.Add("tr", strings.TrimSpace(tr))
		}
	}
	return "magnet:?" + v.Encode()
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
	re := regexp.MustCompile(`(?i)\b(?:udp|https?)://[^\s<"]+`)
	matches := re.FindAllString(body, -1)
	seen := map[string]bool{}
	out := make([]string, 0, len(matches))
	for _, tracker := range matches {
		tracker = strings.TrimSpace(htmlUnescape(tracker))
		if tracker == "" || seen[tracker] {
			continue
		}
		seen[tracker] = true
		out = append(out, tracker)
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
