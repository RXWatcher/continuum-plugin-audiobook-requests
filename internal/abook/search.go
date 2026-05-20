package abook

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// SearchResult is one forum topic returned by the abook search tool.
type SearchResult struct {
	TopicID int
	Title   string
	Poster  string
	Board   string
	Date    string
	URL     string
}

// abookSearchResultRe matches a result row from
// /book/tools/search_abook.php?search=... — the same structure the .ts
// reference script targets. The shape is brittle (one custom HTML row per
// hit) so it's pinned in tests against a fixture.
var abookSearchResultRe = regexp.MustCompile(`<a\s+href='[^']*\?topic=(\d+)&r=\d+'[^>]*>([^<]+)</a><br/>\s*([^-]+?)\s*-\s*<span class='boardname'>([^<]+)</span>\s*-\s*<span class='posttime'>([^<]+)</span>`)

// Search runs the forum search tool and returns the parsed result rows.
// On a logged-out session most queries return zero rows; the Client should
// be logged in via Login() first.
func (c *Client) Search(ctx context.Context, query string) ([]SearchResult, error) {
	if query == "" {
		return nil, fmt.Errorf("abook: search query is required")
	}
	u := c.baseURL + "/tools/search_abook.php?search=" + url.QueryEscape(query)
	body, err := c.get(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("abook search: %w", err)
	}
	matches := abookSearchResultRe.FindAllStringSubmatch(body, -1)
	out := make([]SearchResult, 0, len(matches))
	for _, m := range matches {
		topicID, _ := strconv.Atoi(m[1])
		out = append(out, SearchResult{
			TopicID: topicID,
			Title:   decode(m[2]),
			Poster:  decode(m[3]),
			Board:   decode(m[4]),
			Date:    strings.TrimSpace(m[5]),
			URL:     c.baseURL + "/index.php?topic=" + m[1] + ".0",
		})
	}
	return out, nil
}

// TopicContent is the per-topic metadata that the script's
// extractTopicContent() pulled — most importantly the Usenet "search code"
// and unpack password that nzbking + NZBGet need.
type TopicContent struct {
	Search   string
	Password string
	Author   string
	Narrator string
	Year     string
	FileType string
	Duration string
	Size     string
}

// hiddenContentLabel is the SMF gate that says "hit the Thanks button
// before I'll render the body". The script auto-clicks Thanks once and
// retries; we mirror that.
const hiddenContentLabel = "You must thank this post to see the content"

var (
	thankActionHrefRe = regexp.MustCompile(`href="([^"]*action=thank[^"]*)"`)
	bbcCodeBlockRe    = regexp.MustCompile(`(?s)<code class="bbc_code">(.*?)</code>`)
	hiddenBlockRe     = regexp.MustCompile(`(?s)<h6>Hidden content:</h6>(.*?)(?:<div class="moderatorbar"|<div id="quickreplybox"|$)`)
)

// FetchTopic loads a topic page (auto-thanking if needed) and extracts the
// metadata + hidden Usenet code/password. Caller must have an authenticated
// Client.
func (c *Client) FetchTopic(ctx context.Context, topicID int) (TopicContent, error) {
	if topicID <= 0 {
		return TopicContent{}, fmt.Errorf("abook: topic id is required")
	}
	topicURL := fmt.Sprintf("%s/index.php?topic=%d.0", c.baseURL, topicID)
	body, err := c.get(ctx, topicURL)
	if err != nil {
		return TopicContent{}, fmt.Errorf("fetch topic %d: %w", topicID, err)
	}

	if strings.Contains(body, hiddenContentLabel) {
		thanks := thankActionHrefRe.FindStringSubmatch(body)
		if len(thanks) == 2 {
			thanksURL := decode(thanks[1])
			if !strings.HasPrefix(thanksURL, "http") {
				thanksURL = c.baseURL + "/" + strings.TrimLeft(thanksURL, "/")
			}
			// We don't care about the thank-page response body; SMF just
			// flips a per-user-per-topic flag and the next render unlocks.
			_, _ = c.get(ctx, thanksURL)
			body, err = c.get(ctx, topicURL)
			if err != nil {
				return TopicContent{}, fmt.Errorf("refetch topic %d after thank: %w", topicID, err)
			}
		}
	}

	content := TopicContent{
		Author:   firstLabeled(body, "Author"),
		Narrator: firstLabeled(body, "Read By", "Narrator", "Narrated by"),
		Year:     firstLabeled(body, "Copyright"),
		FileType: firstLabeled(body, "File Type"),
		Duration: firstLabeled(body, "Total Duration"),
		Size:     firstLabeled(body, "Total Size"),
	}

	if hidden := hiddenBlockRe.FindStringSubmatch(body); len(hidden) == 2 {
		block := hidden[1]
		codes := bbcCodeBlockRe.FindAllStringSubmatch(block, -1)
		if len(codes) >= 1 {
			content.Search = decode(codes[0][1])
		}
		if len(codes) >= 2 && strings.Contains(block, "Password") {
			content.Password = decode(codes[1][1])
		}
	}
	return content, nil
}

// firstLabeled finds the first occurrence of "<Label>:" in body and returns
// the inline text following it (after closing markup), trimmed. Mirrors the
// script's "label: <value>" regex with a tolerant-of-markup pattern.
func firstLabeled(body string, labels ...string) string {
	for _, label := range labels {
		re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(label) + `:\s*(?:</b>|</span>)?\s*([^\n<]+)`)
		m := re.FindStringSubmatch(body)
		if len(m) == 2 {
			v := decode(m[1])
			v = collapseWhitespace(v)
			if v != "" {
				return v
			}
		}
	}
	return ""
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// decode is a small HTML-entity decoder covering the entities the abook
// pages actually emit. Full-blown html.UnescapeString would also work but
// pulls in golang.org/x/net just for entity decode; this is enough.
func decode(s string) string {
	r := strings.NewReplacer(
		"&nbsp;", " ",
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#039;", "'",
		"&rsquo;", "'",
		"&lsquo;", "'",
		"&rdquo;", `"`,
		"&ldquo;", `"`,
		"&mdash;", "—",
		"&ndash;", "–",
		"&hellip;", "…",
	)
	return strings.TrimSpace(r.Replace(s))
}
