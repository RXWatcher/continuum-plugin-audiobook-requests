package consumer_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginproto/continuum/plugin/v1"

	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/audiobookbay"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/consumer"
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

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	return s
}

func newConsumerForTest(t *testing.T, upstream *httptest.Server) (*consumer.Handler, *fakePub, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	pub := &fakePub{}
	qbt := fakeQBit(t)
	ebk := audiobookbay.NewClient(audiobookbay.Config{BaseURL: upstream.URL, QBitURL: qbt.URL}, nil)
	deps := &consumer.Deps{
		Store: st, Pub: pub, ABB: ebk,
		PluginID: "continuum.audiobookbay-requests",
	}
	h := consumer.New(func() *consumer.Deps { return deps }, nil)
	return h, pub, st
}

func TestConsumer_HappyPath_EmitsAcknowledged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page/1/":
			_, _ = w.Write([]byte(`<a href="/audio-books/book/">Book</a>`))
		case "/audio-books/book/":
			_, _ = w.Write([]byte(`<h1>Book</h1><table><tr><td>Info Hash:</td><td>0123456789abcdef0123456789abcdef01234567</td></tr></table>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer up.Close()
	h, pub, st := newConsumerForTest(t, up)
	_, _ = h.HandleEvent(context.Background(), &pluginv1.HandleEventRequest{
		EventName: "plugin.continuum.audiobooks.request_submitted",
		Payload: mustStruct(t, map[string]any{
			"request_id":       "r-1",
			"target_plugin_id": "continuum.audiobookbay-requests",
			"title":            "Book",
		}),
	})
	if len(pub.pubs) != 1 || pub.pubs[0].Name != "request_acknowledged" {
		t.Errorf("emitted = %+v", pub.pubs)
	}
	row, _ := st.GetForwardedRequest(context.Background(), "r-1")
	if row.Status != "acknowledged" || row.ExternalID != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("row = %+v", row)
	}
}

func TestConsumer_MissingQuery_EmitsFailed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be called")
	}))
	defer up.Close()
	h, pub, st := newConsumerForTest(t, up)
	_, _ = h.HandleEvent(context.Background(), &pluginv1.HandleEventRequest{
		EventName: "plugin.continuum.audiobooks.request_submitted",
		Payload: mustStruct(t, map[string]any{
			"request_id":       "r-2",
			"target_plugin_id": "continuum.audiobookbay-requests",
		}),
	})
	if len(pub.pubs) != 1 || pub.pubs[0].Name != "request_failed" {
		t.Errorf("emitted = %+v", pub.pubs)
	}
	row, _ := st.GetForwardedRequest(context.Background(), "r-2")
	if row.Status != "failed" {
		t.Errorf("row = %+v", row)
	}
}

func TestConsumer_SkipsTargetMismatch(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be called")
	}))
	defer up.Close()
	h, pub, _ := newConsumerForTest(t, up)
	_, _ = h.HandleEvent(context.Background(), &pluginv1.HandleEventRequest{
		EventName: "plugin.continuum.audiobooks.request_submitted",
		Payload: mustStruct(t, map[string]any{
			"request_id":       "r-3",
			"target_plugin_id": "continuum.other-audiobook-provider",
			"title":            "X",
		}),
	})
	if len(pub.pubs) != 0 {
		t.Errorf("pubs = %+v", pub.pubs)
	}
}

func fakeQBit(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/add":
			_, _ = w.Write([]byte("Ok."))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}
