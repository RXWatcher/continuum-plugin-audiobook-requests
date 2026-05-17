package audiobookbay_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/audiobookbay"
)

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

	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: abb.URL})
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
			_, _ = w.Write([]byte(`<h1>Book</h1>Info Hash: 0123456789abcdef0123456789abcdef01234567`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer abb.Close()

	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: abb.URL, QBitURL: qbt.URL})
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
			_, _ = w.Write([]byte(`<h1>Random Book</h1>Info Hash: 1111111111111111111111111111111111111111`))
		case "/audio-books/project-hail-mary/":
			_, _ = w.Write([]byte(`<h1>Project Hail Mary</h1>Info Hash: 2222222222222222222222222222222222222222`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer abb.Close()

	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: abb.URL})
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
	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: "https://abb.example", QBitURL: qbt.URL})
	resp, err := c.GetDownload(context.Background(), "abcdef")
	if err != nil {
		t.Fatalf("GetDownload: %v", err)
	}
	if resp.Status != "downloading" || resp.Progress != 50 {
		t.Fatalf("resp = %+v", resp)
	}
}
