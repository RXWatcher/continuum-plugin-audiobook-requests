package audiobookbay_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/audiobookbay"
)

const publicDomainABBDetailURL = "https://audiobookbay.lu/abss/othe-sandman-his-farm-stories-william-john-hopkins/"

func TestLiveAudiobookBayPublicDomainResolve(t *testing.T) {
	if os.Getenv("LIVE_AUDIOBOOKBAY_SMOKE") != "1" {
		t.Skip("set LIVE_AUDIOBOOKBAY_SMOKE=1 to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := audiobookbay.NewClient(audiobookbay.Config{BaseURL: "https://audiobookbay.lu"}, nil)
	hit, err := c.Resolve(ctx, publicDomainABBDetailURL)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if hit.InfoHash != "1793458b18ff36bacb08047a86054787628dca74" {
		t.Fatalf("info hash = %q", hit.InfoHash)
	}
	if !strings.HasPrefix(hit.MagnetURI, "magnet:?xt=urn:btih:1793458b18ff36bacb08047a86054787628dca74") {
		t.Fatalf("magnet = %q", hit.MagnetURI)
	}
	t.Logf("resolved public-domain AudiobookBay result: title=%q hash=%s", hit.Title, hit.InfoHash)
}
