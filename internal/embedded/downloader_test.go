package embedded_test

import (
	"context"
	"testing"

	"github.com/RXWatcher/silo-plugin-audiobook-requests/internal/embedded"
)

func TestManagerStartsAndReportsUnknownQueued(t *testing.T) {
	m, err := embedded.New(embedded.Config{DownloadDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	st, err := m.Status(context.Background(), "missing")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Status != "queued" {
		t.Fatalf("status = %+v", st)
	}
}

func TestManagerRequiresDownloadDir(t *testing.T) {
	if _, err := embedded.New(embedded.Config{}); err == nil {
		t.Fatal("expected error")
	}
}
