package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/store"
)

func TestUpsertForwardedRequest_NewRow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertForwardedRequest(ctx, store.ForwardedRequest{
		RequestID: "req-1", Status: "submitted", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetForwardedRequest(ctx, "req-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "submitted" {
		t.Errorf("got %+v", got)
	}
}

func TestUpsertForwardedRequest_UpdatesExisting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertForwardedRequest(ctx, store.ForwardedRequest{RequestID: "req-1", Status: "submitted", UpdatedAt: time.Now()})
	_ = s.UpsertForwardedRequest(ctx, store.ForwardedRequest{RequestID: "req-1", Status: "acknowledged", ExternalID: "job-42", UpdatedAt: time.Now()})
	got, _ := s.GetForwardedRequest(ctx, "req-1")
	if got.Status != "acknowledged" || got.ExternalID != "job-42" {
		t.Errorf("got %+v", got)
	}
}

func TestListNonTerminal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertForwardedRequest(ctx, store.ForwardedRequest{RequestID: "a", Status: "downloading", UpdatedAt: time.Now()})
	_ = s.UpsertForwardedRequest(ctx, store.ForwardedRequest{RequestID: "b", Status: "imported", UpdatedAt: time.Now()})
	_ = s.UpsertForwardedRequest(ctx, store.ForwardedRequest{RequestID: "c", Status: "failed", UpdatedAt: time.Now()})
	rows, err := s.ListNonTerminal(ctx, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].RequestID != "a" {
		t.Errorf("non-terminal = %+v", rows)
	}
}
