package runtime

import (
	"context"
	"testing"

	pluginv1 "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func cfgReq(t *testing.T, kv map[string]any) *pluginv1.ConfigureRequest {
	t.Helper()
	entries := make([]*pluginv1.ConfigEntry, 0, len(kv))
	for k, v := range kv {
		s, err := structpb.NewStruct(map[string]any{"value": v})
		if err != nil {
			t.Fatalf("structpb: %v", err)
		}
		entries = append(entries, &pluginv1.ConfigEntry{Key: k, Value: s})
	}
	return &pluginv1.ConfigureRequest{Config: entries}
}

func TestConfigure_RejectsInsecureRemoteBaseURL(t *testing.T) {
	s := New(nil, func(Config) error { return nil })
	_, err := s.Configure(context.Background(), cfgReq(t, map[string]any{
		"database_url": "postgres://x",
		"base_url":     "http://audiobookbay.example",
	}))
	if err == nil {
		t.Fatal("expected base_url error")
	}
}

func TestConfigure_AllowsLocalHTTPBaseURL(t *testing.T) {
	var got Config
	s := New(nil, func(c Config) error {
		got = c
		return nil
	})
	_, err := s.Configure(context.Background(), cfgReq(t, map[string]any{
		"database_url": "postgres://x",
		"base_url":     "http://localhost:8080",
	}))
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if got.BaseURL != "http://localhost:8080" {
		t.Fatalf("BaseURL = %q", got.BaseURL)
	}
}

func TestConfigure_RejectsInvalidTracker(t *testing.T) {
	s := New(nil, func(Config) error { return nil })
	_, err := s.Configure(context.Background(), cfgReq(t, map[string]any{
		"database_url": "postgres://x",
		"base_url":     "https://audiobookbay.example",
		"trackers":     []any{"file:///etc/passwd"},
	}))
	if err == nil {
		t.Fatal("expected tracker scheme error")
	}
}

func TestConfigure_RejectsSearchLimitOutOfRange(t *testing.T) {
	s := New(nil, func(Config) error { return nil })
	_, err := s.Configure(context.Background(), cfgReq(t, map[string]any{
		"database_url": "postgres://x",
		"base_url":     "https://audiobookbay.example",
		"search_limit": float64(101),
	}))
	if err == nil {
		t.Fatal("expected search_limit error")
	}
}
