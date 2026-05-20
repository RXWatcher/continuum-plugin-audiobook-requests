package nzbget

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Append must send the NZBGet RPC envelope NZBGet expects and surface the
// returned NZBID. Captures the request body so we can also confirm the
// unpack-password PPParameter shape is right (NZBGet recipe: name
// "*Unpack:Password").
func TestAppend_SendsCorrectEnvelopeAndReturnsNZBID(t *testing.T) {
	var got struct {
		Method string `json:"method"`
		Params []any  `json:"params"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode RPC body: %v", err)
		}
		_, _ = w.Write([]byte(`{"result": 4242}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Append(context.Background(), AppendOptions{
		URL:            "https://nzbking.com/nzb:abc/",
		Name:           "Foundation.Isaac.Asimov",
		Category:       "audiobooks",
		UnpackPassword: "Per.Ardua.Ad.Astra",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != 4242 {
		t.Errorf("NZBID = %d, want 4242", id)
	}
	if got.Method != "append" {
		t.Errorf("method = %q, want append", got.Method)
	}
	// PPParameters is the 10th element (index 9).
	pp, ok := got.Params[9].([]any)
	if !ok || len(pp) != 1 {
		t.Fatalf("PPParameters = %v, want one entry", got.Params[9])
	}
	first := pp[0].(map[string]any)
	if first["Name"] != "*Unpack:Password" {
		t.Errorf("PP Name = %q, want *Unpack:Password", first["Name"])
	}
	if first["Value"] != "Per.Ardua.Ad.Astra" {
		t.Errorf("PP Value = %q", first["Value"])
	}
}

// HTTP 401 must surface as an actionable "unauthorized" message so the
// operator immediately knows to check NZBGet credentials.
func TestAppend_UnauthorizedSurfacesActionableError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "wrong", "creds")
	_, err := c.Append(context.Background(), AppendOptions{URL: "http://x/y"})
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("err = %v, want unauthorized", err)
	}
}

// listgroups hit → live download. The plugin maps state and progress.
func TestLookup_ActiveDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"listgroups"`) {
			_, _ = w.Write([]byte(`{"result": [
{"NZBID": 4242, "NZBName": "Foundation", "Status": "DOWNLOADING",
 "FileSizeMB": 256, "RemainingSizeMB": 64, "DownloadedSizeMB": 192}
]}`))
			return
		}
		t.Fatalf("unexpected RPC: %s", body)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "", "")
	s, err := c.Lookup(context.Background(), 4242)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != "downloading" {
		t.Errorf("state = %q, want downloading", s.State)
	}
	if s.Progress != 75 {
		t.Errorf("progress = %d, want 75", s.Progress)
	}
}

// listgroups misses → fall back to history; status SUCCESS/ALL → imported.
func TestLookup_HistorySuccess_MapsToImported(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls++
		switch {
		case strings.Contains(string(body), `"listgroups"`):
			_, _ = w.Write([]byte(`{"result": []}`))
		case strings.Contains(string(body), `"history"`):
			_, _ = w.Write([]byte(`{"result": [{"NZBID": 9, "Status": "SUCCESS/ALL"}]}`))
		}
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "", "")
	s, err := c.Lookup(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != "imported" {
		t.Errorf("state = %q, want imported", s.State)
	}
	if calls != 2 {
		t.Errorf("RPC calls = %d, want 2 (listgroups + history fallback)", calls)
	}
}

func TestLookup_HistoryFailure_MapsToFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"listgroups"`) {
			_, _ = w.Write([]byte(`{"result": []}`))
			return
		}
		_, _ = w.Write([]byte(`{"result": [{"NZBID": 9, "Status": "FAILURE/PAR"}]}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "", "")
	s, _ := c.Lookup(context.Background(), 9)
	if s.State != "failed" {
		t.Errorf("state = %q, want failed", s.State)
	}
}

func TestLookup_Missing_ReturnsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"result": []}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "", "")
	s, _ := c.Lookup(context.Background(), 42)
	if s.State != "unknown" {
		t.Errorf("state = %q, want unknown when neither queue nor history has the id", s.State)
	}
}
