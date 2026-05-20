package audiobookbay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A hostile/large scraped page must not yield an unbounded tracker slice.
func TestExtractTrackers_Bounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, ` udp://tracker%d.example.com:80/announce `, i)
	}
	got := extractTrackers(b.String())
	if len(got) > maxTrackers {
		t.Fatalf("extractTrackers returned %d, want <= %d", len(got), maxTrackers)
	}
	if len(got) == 0 {
		t.Fatal("expected some trackers extracted")
	}
}

// DetectBlocked recognises canonical Cloudflare / captcha / WAF signatures.
// Conservative wins: catching common patterns matters more than catching
// every variant — false positives turn legit empty searches into reconciler
// backoff, false negatives just look like "no results" until the operator
// runs a test search.
func TestDetectBlocked(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		match string // empty = expect no match
	}{
		{"clean html", `<html><body>No results</body></html>`, ""},
		{"cloudflare interstitial", `<title>Just a moment...</title><body>Checking your browser before accessing</body>`, "checking your browser"},
		{"hcaptcha gate", `<div class="h-captcha" data-sitekey="abc"></div>`, "h-captcha"},
		{"recaptcha gate", `<script src="//www.google.com/recaptcha/api.js"></script>`, "recaptcha"},
		{"attention required (CF 1020)", `<title>Attention Required! | Cloudflare</title>`, "attention required"},
		{"access denied", `<h1>Access Denied</h1>`, "access denied"},
		{"plain captcha word", `Please solve this captcha to continue`, "captcha"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectBlocked(tc.body)
			if got != tc.match {
				t.Fatalf("DetectBlocked = %q, want %q", got, tc.match)
			}
		})
	}
}

// get() must surface 429 as a typed RateLimitError so the reconciler can
// back off across ticks instead of hammering an already-mad upstream.
func TestGet_429_ReturnsRateLimitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer srv.Close()
	c := &Client{cfg: Config{BaseURL: srv.URL}, hc: srv.Client()}
	_, err := c.get(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error")
	}
	rl, ok := IsRateLimited(err)
	if !ok {
		t.Fatalf("err = %T %v, want *RateLimitError", err, err)
	}
	if rl.RetryAfter < 119*time.Second {
		t.Fatalf("RetryAfter = %s, want ~120s", rl.RetryAfter)
	}
}

// 200 + challenge HTML is the most insidious failure mode: the parser would
// return zero hits and look like "no results". get() must sniff and surface
// it as a typed BlockedError.
func TestGet_200_CloudflareInterstitial_ReturnsBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Just a moment...</title></head><body>Cloudflare is checking your browser before allowing access.</body></html>`))
	}))
	defer srv.Close()
	c := &Client{cfg: Config{BaseURL: srv.URL}, hc: srv.Client()}
	_, err := c.get(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected blocked error")
	}
	if _, ok := IsBlocked(err); !ok {
		t.Fatalf("err = %T %v, want *BlockedError", err, err)
	}
}

// 403 with bot-protection HTML must also surface as BlockedError so the
// reconciler error message points at the right cause (not just "403").
func TestGet_403_ChallengeHTML_ReturnsBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<title>Attention Required! | Cloudflare</title>`))
	}))
	defer srv.Close()
	c := &Client{cfg: Config{BaseURL: srv.URL}, hc: srv.Client()}
	_, err := c.get(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected blocked error")
	}
	if _, ok := IsBlocked(err); !ok {
		t.Fatalf("err = %T %v, want *BlockedError", err, err)
	}
}

// Login must be cached: repeated AddMagnet/Torrent calls (the reconciler does
// up to 200/min) must not re-authenticate every time.
func TestQBit_LoginDedupedAcrossCalls(t *testing.T) {
	var logins int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			atomic.AddInt32(&logins, 1)
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/add":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			_, _ = w.Write([]byte("[]"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := NewQBitClient(srv.URL, "u", "p")
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := c.Login(ctx); err != nil {
			t.Fatalf("login: %v", err)
		}
	}
	if err := c.AddMagnet(ctx, "magnet:?xt=urn:btih:abc", "", ""); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := c.Torrent(ctx, "abc"); err != nil {
		t.Fatalf("torrent: %v", err)
	}
	if n := atomic.LoadInt32(&logins); n != 1 {
		t.Fatalf("logged in %d times, want exactly 1 (session must be cached)", n)
	}
}
