package embedded_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/embedded"
)

const bigBuckBunnyMagnet = "magnet:?xt=urn:btih:dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c&dn=Big+Buck+Bunny&tr=udp%3A%2F%2Fexplodie.org%3A6969&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337&xs=https%3A%2F%2Fwebtorrent.io%2Ftorrents%2Fbig-buck-bunny.torrent"

func TestLiveEmbeddedTorrentSmoke(t *testing.T) {
	if os.Getenv("LIVE_EMBEDDED_TORRENT_SMOKE") != "1" {
		t.Skip("set LIVE_EMBEDDED_TORRENT_SMOKE=1 to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	m, err := embedded.New(embedded.Config{DownloadDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	st, err := m.Add(ctx, "dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c", bigBuckBunnyMagnet, "Big Buck Bunny")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	for st.Status == "queued" && ctx.Err() == nil {
		time.Sleep(2 * time.Second)
		st, err = m.Status(ctx, st.ID)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
	}
	if ctx.Err() != nil {
		t.Fatalf("timed out waiting for metadata/progress; last status=%+v", st)
	}
	t.Logf("embedded torrent smoke status: %+v", st)
}
