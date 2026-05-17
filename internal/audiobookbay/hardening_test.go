package audiobookbay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
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
