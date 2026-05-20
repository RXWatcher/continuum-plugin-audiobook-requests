package reconciler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/audiobookbay"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/reconciler"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/store"
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

func TestReconciler_QueuedStatusChange(t *testing.T) {
	r, pub, st := newReconcilerForTest(t, `[]`)
	_ = st.UpsertForwardedRequest(context.Background(), store.ForwardedRequest{
		RequestID: "req-1", ExternalID: "job-1", Status: "acknowledged", UpdatedAt: time.Now(),
	})
	_ = r.Tick(context.Background())
	if len(pub.pubs) != 1 || pub.pubs[0].Name != "request_status_changed" {
		t.Errorf("got pubs = %v", pub.pubs)
	}
	if pub.pubs[0].Payload["status"] != "queued" {
		t.Errorf("status = %v", pub.pubs[0].Payload["status"])
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

// A transient upstream failure sets error_text. Once polling succeeds again
// it must be cleared, otherwise it sticks forever and RequestStats.WithErrors
// over-counts permanently.
func TestReconciler_SuccessfulPoll_ClearsStickyError(t *testing.T) {
	r, _, st := newReconcilerForTest(t, `[{"hash":"job-1","name":"Book","state":"downloading","progress":0.5}]`)
	_ = st.UpsertForwardedRequest(context.Background(), store.ForwardedRequest{
		RequestID: "req-1", ExternalID: "job-1", Status: "downloading",
		ErrorText: "boom: upstream blip", LastPolled: time.Now().Add(-time.Hour),
		UpdatedAt: time.Now(),
	})
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	row, _ := st.GetForwardedRequest(context.Background(), "req-1")
	if row.ErrorText != "" {
		t.Errorf("error_text should be cleared after a successful poll; got %q", row.ErrorText)
	}
	stats, _ := st.RequestStats(context.Background())
	if stats.WithErrors != 0 {
		t.Errorf("WithErrors should be 0 after recovery; got %d", stats.WithErrors)
	}
}

// A cancelled context must short-circuit: no events, no DB writes.
func TestReconciler_CancelledContext_NoProcessing(t *testing.T) {
	r, pub, st := newReconcilerForTest(t, `[{"hash":"job-1","name":"Book","state":"uploading","progress":1}]`)
	_ = st.UpsertForwardedRequest(context.Background(), store.ForwardedRequest{
		RequestID: "req-1", ExternalID: "job-1", Status: "downloading", UpdatedAt: time.Now(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = r.Tick(ctx)
	if len(pub.pubs) != 0 {
		t.Errorf("cancelled context must not publish; got %v", pub.pubs)
	}
	row, _ := st.GetForwardedRequest(context.Background(), "req-1")
	if row.Status != "downloading" {
		t.Errorf("cancelled context must not write; status = %q", row.Status)
	}
}
