package catalog_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/audiobookbay"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/catalog"
)

func newRouter(c *audiobookbay.Client) *chi.Mux {
	r := chi.NewRouter()
	catalog.NewHandler(c).Mount(r)
	return r
}

func TestCatalogEndpointsReportNotImplemented(t *testing.T) {
	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: "https://abb.example"}, nil)
	r := newRouter(c)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/catalog", nil))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("code = %d", w.Code)
	}
}

func TestExternalSearchReturnsItems(t *testing.T) {
	abb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/page/1/" {
			_, _ = w.Write([]byte(`<a href="/audio-books/book/">Book</a>`))
			return
		}
		_, _ = w.Write([]byte(`<h1>Book</h1><a href="magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567">Magnet</a>`))
	}))
	defer abb.Close()
	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: abb.URL}, nil)
	r := newRouter(c)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/external_search", strings.NewReader(`{"q":"book"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var body map[string][]audiobookbay.SearchHit
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if len(body["items"]) != 1 {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestRequestSnapshot(t *testing.T) {
	qbt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			_, _ = w.Write([]byte(`[{"hash":"abc","name":"Book","state":"uploading","progress":1}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer qbt.Close()
	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: "https://abb.example", QBitURL: qbt.URL}, nil)
	r := newRouter(c)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/requests/abc", nil))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "imported" {
		t.Fatalf("body = %s", w.Body.String())
	}
}
