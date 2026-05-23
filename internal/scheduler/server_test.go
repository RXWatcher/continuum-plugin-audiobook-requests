package scheduler

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginproto/silo/plugin/v1"

	"github.com/RXWatcher/silo-plugin-audiobook-requests/internal/reconciler"
)

func TestTaskID(t *testing.T) {
	cases := map[string]string{
		"plugin:42:reconciler": "reconciler", // real host wire format
		"plugin:7:reconciler":  "reconciler",
		"reconciler":           "reconciler", // bare (host integration tests)
	}
	for in, want := range cases {
		if got := taskID(in); got != want {
			t.Errorf("taskID(%q) = %q, want %q", in, got, want)
		}
	}
}

// The host sends TaskKey="plugin:<installationID>:reconciler"; the old bare
// "!= reconciler" compare hit the no-op branch every tick so the reconciler
// scheduled task never ran. A prefixed reconciler key must be recognised:
// with deps still nil it must return the "not configured" error (proving it
// matched reconciler), not a silent success and not "unknown".
func TestRun_PrefixedKeyMatchesReconciler(t *testing.T) {
	s := New(func() *reconciler.Reconciler { return nil })
	_, err := s.Run(context.Background(),
		&pluginv1.RunScheduledTaskRequest{TaskKey: "plugin:42:reconciler"})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("prefixed reconciler key must reach the not-configured path; got err=%v", err)
	}
}

func TestRun_UnknownKeyErrors(t *testing.T) {
	s := New(func() *reconciler.Reconciler { return nil })
	if _, err := s.Run(context.Background(),
		&pluginv1.RunScheduledTaskRequest{TaskKey: "plugin:42:bogus"}); err == nil ||
		!strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown key must error; got err=%v", err)
	}
}
