package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/RXWatcher/continuum-plugin-audiobook-requests/internal/store"
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
	_ = s.UpsertForwardedRequest(ctx, store.ForwardedRequest{
		RequestID: "req-1", Status: "acknowledged", ExternalID: "job-42",
		SearchQuery: "project hail mary", SelectedTitle: "Project Hail Mary",
		DetailURL: "https://abb/audio-books/project-hail-mary/", InfoHash: "abc",
		MagnetURI: "magnet:?xt=urn:btih:abc", SelectedScore: 220,
		SelectedScoreReason: "exact title match", UpdatedAt: time.Now(),
	})
	got, _ := s.GetForwardedRequest(ctx, "req-1")
	if got.Status != "acknowledged" || got.ExternalID != "job-42" {
		t.Errorf("got %+v", got)
	}
	if got.SelectedTitle != "Project Hail Mary" || got.SelectedScore != 220 || got.InfoHash != "abc" {
		t.Errorf("metadata = %+v", got)
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

// Event delivery is at-least-once: a duplicate/late/replayed
// request_submitted (status submitted/acknowledged/queued) must not resurrect
// a row that already reached a terminal state (imported or failed).
func TestUpsertForwardedRequest_TerminalGuard(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, terminal := range []string{"imported", "failed"} {
		id := "term-" + terminal
		if err := s.UpsertForwardedRequest(ctx, store.ForwardedRequest{
			RequestID: id, Status: terminal, ExternalID: "h1", UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("seed %s: %v", terminal, err)
		}
		if err := s.UpsertForwardedRequest(ctx, store.ForwardedRequest{
			RequestID: id, Status: "submitted", UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("replay %s: %v", terminal, err)
		}
		if got, _ := s.GetForwardedRequest(ctx, id); got.Status != terminal {
			t.Errorf("%s row resurrected to %q", terminal, got.Status)
		}
	}
	// A non-terminal row must still advance.
	_ = s.UpsertForwardedRequest(ctx, store.ForwardedRequest{RequestID: "live", Status: "queued", UpdatedAt: time.Now()})
	_ = s.UpsertForwardedRequest(ctx, store.ForwardedRequest{RequestID: "live", Status: "downloading", UpdatedAt: time.Now()})
	if got, _ := s.GetForwardedRequest(ctx, "live"); got.Status != "downloading" {
		t.Errorf("non-terminal row should advance; got %q", got.Status)
	}
}

// Never-polled rows all share the epoch last_polled sentinel; without a
// tiebreaker their order is undefined, so under LIMIT the same subset can be
// returned every tick while the rest starve. Order must be deterministic.
func TestListNonTerminal_DeterministicTiebreak(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"req-3", "req-2", "req-1"} {
		_ = s.UpsertForwardedRequest(ctx, store.ForwardedRequest{
			RequestID: id, Status: "submitted", UpdatedAt: time.Now(),
		})
	}
	rows, err := s.ListNonTerminal(ctx, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"req-1", "req-2", "req-3"}
	for i := range want {
		if rows[i].RequestID != want[i] {
			t.Fatalf("order = %v..., want %v (no deterministic tiebreaker)", rows, want)
		}
	}
}

// Unsubmitted means "no external_id yet AND still in flight". A failed row
// with no external_id must count as failed only, not also as unsubmitted.
func TestRequestStats_UnsubmittedExcludesTerminal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertForwardedRequest(ctx, store.ForwardedRequest{
		RequestID: "pending", Status: "submitted", UpdatedAt: time.Now(),
	})
	_ = s.UpsertForwardedRequest(ctx, store.ForwardedRequest{
		RequestID: "dead", Status: "failed", ErrorText: "boom", UpdatedAt: time.Now(),
	})
	stats, err := s.RequestStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Unsubmitted != 1 {
		t.Errorf("Unsubmitted = %d, want 1 (failed row must not be counted)", stats.Unsubmitted)
	}
	if stats.Failed != 1 {
		t.Errorf("Failed = %d, want 1", stats.Failed)
	}
}
