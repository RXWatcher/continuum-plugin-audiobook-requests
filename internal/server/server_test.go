package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/server"
)

func TestHealthOK(t *testing.T) {
	h := server.New(server.Deps{})
	r := httptest.NewRequest("GET", "/api/v1/health", nil)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["ok"] != true {
		t.Errorf("ok = %v", body["ok"])
	}
}

func TestCapabilitiesAudiobookRequestProvider(t *testing.T) {
	h := server.New(server.Deps{})
	r := httptest.NewRequest("GET", "/api/v1/capabilities", nil)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	var body struct {
		Formats               []string `json:"formats"`
		Features              []string `json:"features"`
		SupportsRangeRequests bool     `json:"supports_range_requests"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Formats) != 7 {
		t.Errorf("formats len = %d (%v)", len(body.Formats), body.Formats)
	}
	for _, f := range body.Features {
		if f == "auto_monitoring" {
			t.Errorf("auto_monitoring should not appear: %v", body.Features)
		}
	}
	if body.SupportsRangeRequests {
		t.Error("AudiobookBay doesn't support Range")
	}
}
