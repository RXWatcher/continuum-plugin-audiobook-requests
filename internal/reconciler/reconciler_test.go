package reconciler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/audiobookbay"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/reconciler"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/store"
)

type fakePub struct {
	mu   sync.Mutex
	pubs []struct {
		Name    string
		Payload map[string]any
	}
}

func (f *fakePub) Publish(_ context.Context, name string, payload map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pubs = append(f.pubs, struct {
		Name    string
		Payload map[string]any
	}{name, payload})
}

func newReconcilerForTest(t *testing.T, torrentResp string) (*reconciler.Reconciler, *fakePub, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	pub := &fakePub{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/info":
			_, _ = w.Write([]byte(torrentResp))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(up.Close)
	ebk := audiobookbay.NewClient(audiobookbay.Config{BaseURL: "https://abb.example", QBitURL: up.URL}, nil)
	r := reconciler.New(reconciler.Deps{Store: st, Pub: pub, ABB: ebk})
	return r, pub, st
}

func TestReconciler_StatusChange(t *testing.T) {
	r, pub, st := newReconcilerForTest(t, `[{"hash":"job-1","name":"Book","state":"downloading","progress":0.5}]`)
	_ = st.UpsertForwardedRequest(context.Background(), store.ForwardedRequest{
		RequestID: "req-1", ExternalID: "job-1", Status: "acknowledged",
		LastPolled: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
	})
	_ = r.Tick(context.Background())
	if len(pub.pubs) != 1 || pub.pubs[0].Name != "request_status_changed" {
		t.Errorf("got pubs = %v", pub.pubs)
	}
}

func TestReconciler_Imported(t *testing.T) {
	r, pub, st := newReconcilerForTest(t, `[{"hash":"job-1","name":"Book","state":"uploading","progress":1}]`)
	_ = st.UpsertForwardedRequest(context.Background(), store.ForwardedRequest{
		RequestID: "req-1", ExternalID: "job-1", Status: "downloading", UpdatedAt: time.Now(),
	})
	_ = r.Tick(context.Background())
	if len(pub.pubs) != 1 || pub.pubs[0].Name != "request_fulfilled" {
		t.Errorf("got pubs = %v", pub.pubs)
	}
	if pub.pubs[0].Payload["fulfilled_book_id"] != "job-1" {
		t.Errorf("fulfilled_book_id = %v", pub.pubs[0].Payload["fulfilled_book_id"])
	}
}

func TestReconciler_Failed(t *testing.T) {
	r, pub, st := newReconcilerForTest(t, fmt.Sprintf(`[{"hash":"job-1","name":"Book","state":"error","progress":%v}]`, 0.1))
	_ = st.UpsertForwardedRequest(context.Background(), store.ForwardedRequest{
		RequestID: "req-1", ExternalID: "job-1", Status: "downloading", UpdatedAt: time.Now(),
	})
	_ = r.Tick(context.Background())
	if len(pub.pubs) != 1 || pub.pubs[0].Name != "request_failed" {
		t.Errorf("got pubs = %v", pub.pubs)
	}
}
