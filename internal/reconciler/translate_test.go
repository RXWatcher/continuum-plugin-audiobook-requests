package reconciler

import "testing"

// translateStatus must map the known status enum and, crucially, return ""
// ("no transition / hold") for anything unknown — never the old default of
// "acknowledged", which regressed in-flight requests and spammed events.
func TestTranslateStatus(t *testing.T) {
	cases := map[string]string{
		"queued":       "queued",
		"magnet_ready": "acknowledged", // intentional: scrape-only mode
		"downloading":  "downloading",
		"imported":     "imported",
		"completed":    "imported",
		"failed":       "failed",
		"error":        "failed",
		"":             "", // hold
		"seeding":      "", // unknown embedded/qbit state -> hold
		"stalledDL":    "", // unknown -> hold (was "acknowledged")
	}
	for in, want := range cases {
		if got := translateStatus(in); got != want {
			t.Errorf("translateStatus(%q) = %q, want %q", in, got, want)
		}
	}
}
