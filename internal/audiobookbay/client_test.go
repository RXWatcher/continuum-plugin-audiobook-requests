package audiobookbay_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RXWatcher/silo-plugin-audiobook-requests/internal/audiobookbay"
)

// AudiobookBay is an untrusted scraped site. A broken/hostile error response
// must not have its (up to 8 MiB) body inlined whole into the error string
// that propagates into logs and request error_text.
func TestClient_TruncatesErrorBody(t *testing.T) {
	big := strings.Repeat("x", 60000)
	abb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(big))
	}))
	defer abb.Close()
	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: abb.URL}, nil)
	_, err := c.ExternalSearch(context.Background(), "weir", 5)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(err.Error()) > 1024 {
		t.Errorf("error not truncated: %d bytes", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status: %q", err.Error())
	}
}

// source_id comes from the request payload; Resolve must refuse an absolute
// URL outside the configured AudiobookBay base (SSRF), without fetching it.
func TestClient_Resolve_RejectsForeignSourceID(t *testing.T) {
	hit := false
	abb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer abb.Close()
	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: abb.URL}, nil)
	if _, err := c.Resolve(context.Background(), "http://169.254.169.254/latest/meta-data/"); err == nil {
		t.Fatal("foreign absolute source_id must be rejected")
	}
	if hit {
		t.Fatal("upstream/foreign host must not be contacted for a rejected source_id")
	}
	// A path-shaped source_id is still accepted (prefixed onto BaseURL).
	if _, err := c.Resolve(context.Background(), "/audio-books/x/"); err != nil && strings.Contains(err.Error(), "outside AudiobookBay base") {
		t.Fatalf("a relative path source_id must not be rejected by the SSRF guard: %v", err)
	}
}

func fakeQBit(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/add":
			if r.Method != http.MethodPost {
				t.Errorf("add method = %s", r.Method)
			}
			_ = r.ParseForm()
			if !strings.HasPrefix(r.Form.Get("urls"), "magnet:?") {
				t.Errorf("urls = %q", r.Form.Get("urls"))
			}
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			_, _ = w.Write([]byte(`[{"hash":"abcdef","name":"Book","state":"downloading","progress":0.5}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestClient_ExternalSearchParsesAudiobookBay(t *testing.T) {
	abb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page/1/":
			if r.URL.Query().Get("s") != "weir" {
				t.Errorf("search = %q", r.URL.Query().Get("s"))
			}
			_, _ = w.Write([]byte(`<a href="/audio-books/project-hail-mary/">Project Hail Mary</a>`))
		case "/audio-books/project-hail-mary/":
			_, _ = w.Write([]byte(`<h1>Project Hail Mary</h1><a href="magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&amp;dn=Project">Magnet</a>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer abb.Close()

	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: abb.URL}, nil)
	hits, err := c.ExternalSearch(context.Background(), "weir", 5)
	if err != nil {
		t.Fatalf("ExternalSearch: %v", err)
	}
	if len(hits) != 1 || hits[0].InfoHash != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestClient_StartDownloadSearchesAndAddsMagnet(t *testing.T) {
	qbt := fakeQBit(t)
	defer qbt.Close()
	abb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page/1/":
			_, _ = w.Write([]byte(`<a href="/audio-books/book/">Book</a>`))
		case "/audio-books/book/":
			_, _ = w.Write([]byte(`<h1>Book</h1><table><tr><td>Info Hash:</td><td>0123456789abcdef0123456789abcdef01234567</td></tr></table>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer abb.Close()

	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: abb.URL, QBitURL: qbt.URL}, nil)
	resp, err := c.StartDownload(context.Background(), "", "book")
	if err != nil {
		t.Fatalf("StartDownload: %v", err)
	}
	if resp.ID != "0123456789abcdef0123456789abcdef01234567" || resp.Status != "queued" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestClient_StartDownloadChoosesBestScoredResult(t *testing.T) {
	abb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page/1/":
			_, _ = w.Write([]byte(`
				<a href="/audio-books/random-book/">Random Book</a>
				<a href="/audio-books/project-hail-mary/">Project Hail Mary</a>
			`))
		case "/audio-books/random-book/":
			_, _ = w.Write([]byte(`<h1>Random Book</h1><table><tr><td>Info Hash:</td><td>1111111111111111111111111111111111111111</td></tr></table>`))
		case "/audio-books/project-hail-mary/":
			_, _ = w.Write([]byte(`<h1>Project Hail Mary</h1><table><tr><td>Info Hash:</td><td>2222222222222222222222222222222222222222</td></tr></table>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer abb.Close()

	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: abb.URL}, nil)
	resp, err := c.StartDownload(context.Background(), "", "Project Hail Mary")
	if err != nil {
		t.Fatalf("StartDownload: %v", err)
	}
	if resp.ID != "2222222222222222222222222222222222222222" {
		t.Fatalf("selected %+v", resp)
	}
	if resp.Score == 0 || !strings.Contains(resp.Reason, "exact title match") {
		t.Fatalf("score/reason = %d %q", resp.Score, resp.Reason)
	}
}

func TestClient_GetDownloadReadsQBitState(t *testing.T) {
	qbt := fakeQBit(t)
	defer qbt.Close()
	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: "https://abb.example", QBitURL: qbt.URL}, nil)
	resp, err := c.GetDownload(context.Background(), "abcdef")
	if err != nil {
		t.Fatalf("GetDownload: %v", err)
	}
	if resp.Status != "downloading" || resp.Progress != 50 {
		t.Fatalf("resp = %+v", resp)
	}
}
