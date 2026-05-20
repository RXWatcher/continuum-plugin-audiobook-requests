package audiobookbay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadFixture is a thin helper so the tests below stay short. Reads the
// testdata file as a string; t.Fatal on missing/unreadable fixtures so
// fixture rot fails loudly rather than silently turning into "no hits".
func loadFixture(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("testdata", name)
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(body)
}

// parseSearch should pick up valid AudiobookBay detail links (/audio-books/
// and /abss/ paths) and skip category/home links. Pins behavior against a
// representative search page so a future selector change shows up here
// before showing up in production as "no results".
func TestParseSearch_PinsKnownVariants(t *testing.T) {
	body := loadFixture(t, "search_two_hits.html")
	c := &Client{cfg: Config{BaseURL: "https://example.audiobookbay.test"}}
	hits, err := c.parseSearch(body, 10)
	if err != nil {
		t.Fatalf("parseSearch: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("hits = %d, want 3 (two audio-books + one abss); got %+v", len(hits), hits)
	}
	want := map[string]bool{
		"https://example.audiobookbay.test/audio-books/foundation-isaac-asimov-unabridged/": true,
		"https://example.audiobookbay.test/audio-books/foundation-and-empire-isaac-asimov/": true,
		"https://example.audiobookbay.test/abss/sponsored-not-a-real-detail/":               true,
	}
	for _, h := range hits {
		if !want[h.SourceID] {
			t.Errorf("unexpected hit %q", h.SourceID)
		}
	}
	for _, h := range hits {
		if strings.Contains(h.SourceID, "/category/") {
			t.Errorf("category link leaked into hits: %s", h.SourceID)
		}
	}
}

// Resolve must surface the info hash from the canonical detail-table row.
// This is the most brittle selector in the scraper; pinning it in a test
// keeps a hash-cell HTML refactor from silently breaking magnet handoff.
func TestResolve_InfoHashOnlyDetailPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(loadFixture(t, "detail_info_hash_only.html")))
	}))
	defer srv.Close()
	c := &Client{cfg: Config{BaseURL: srv.URL, Trackers: []string{
		"udp://tracker.fallback.example:80/announce",
	}}, hc: srv.Client()}
	hit, err := c.Resolve(context.Background(), srv.URL+"/audio-books/foundation/")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if hit.InfoHash != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("InfoHash = %q, want 40-char a's", hit.InfoHash)
	}
	if !strings.Contains(hit.MagnetURI, "urn:btih:") {
		t.Errorf("MagnetURI = %q, want magnet built from info hash + trackers", hit.MagnetURI)
	}
}

// Resolve must prefer an explicit magnet over building one from a hash.
func TestResolve_MagnetPresentDetailPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(loadFixture(t, "detail_magnet_present.html")))
	}))
	defer srv.Close()
	c := &Client{cfg: Config{BaseURL: srv.URL}, hc: srv.Client()}
	hit, err := c.Resolve(context.Background(), srv.URL+"/audio-books/foundation-and-empire/")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.HasPrefix(hit.MagnetURI, "magnet:?xt=urn:btih:bbbbbbbb") {
		t.Errorf("MagnetURI = %q, want detail-page magnet", hit.MagnetURI)
	}
}

// A detail page with neither magnet nor info hash must surface a clear
// error rather than panic or pick up unrelated 40-hex strings from the
// page. Reconciler will mark the request failed with this reason.
func TestResolve_NoHashNoMagnet_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(loadFixture(t, "detail_no_hash_no_magnet.html")))
	}))
	defer srv.Close()
	c := &Client{cfg: Config{BaseURL: srv.URL}, hc: srv.Client()}
	_, err := c.Resolve(context.Background(), srv.URL+"/audio-books/anything/")
	if err == nil {
		t.Fatal("expected error on detail page with neither magnet nor info hash")
	}
	if !strings.Contains(err.Error(), "magnet") && !strings.Contains(err.Error(), "info hash") {
		t.Errorf("error should mention missing magnet/info hash, got: %v", err)
	}
}

// A 200 response that's actually a Cloudflare interstitial must be caught
// by DetectBlocked so it doesn't reach parseSearch (which would return
// zero hits and look like "no results"). Pinned against a realistic
// challenge fixture so a tweak to the signature list doesn't lose this.
func TestGet_CloudflareFixture_ReturnsBlocked(t *testing.T) {
	body := loadFixture(t, "captcha_interstitial.html")
	if DetectBlocked(body) == "" {
		t.Fatal("DetectBlocked failed to spot the captcha_interstitial.html fixture")
	}
}
