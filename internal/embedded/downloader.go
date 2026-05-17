package embedded

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
)

// metadataTimeout bounds how long we wait for a magnet to obtain its info
// (peers/seeds). A dead/peerless magnet's GotInfo() never fires; without this
// the waiter goroutine and the *torrent.Torrent (conns, piece state) would
// leak forever and the request would poll "downloading" indefinitely.
const metadataTimeout = 3 * time.Minute

type Config struct {
	DownloadDir string
	ListenPort  int
}

type Status struct {
	ID       string
	Title    string
	Status   string
	Progress int
}

type Manager struct {
	client *torrent.Client

	mu        sync.Mutex
	jobs      map[string]*torrent.Torrent
	failed    map[string]string // id -> reason; lets Status report a terminal state
	closeOnce sync.Once
	closed    chan struct{}
}

func New(cfg Config) (*Manager, error) {
	if cfg.DownloadDir == "" {
		return nil, fmt.Errorf("embedded_download_dir is required")
	}
	if err := os.MkdirAll(cfg.DownloadDir, 0o755); err != nil {
		return nil, fmt.Errorf("create embedded download dir: %w", err)
	}
	tc := torrent.NewDefaultClientConfig()
	tc.DataDir = cfg.DownloadDir
	tc.NoUpload = true
	tc.DisableAggressiveUpload = true
	tc.Seed = false
	tc.DropMutuallyCompletePeers = true
	tc.Slogger = slog.New(slog.DiscardHandler)
	if cfg.ListenPort > 0 {
		tc.SetListenAddr(fmt.Sprintf(":%d", cfg.ListenPort))
	} else {
		tc.SetListenAddr(":0")
	}
	client, err := torrent.NewClient(tc)
	if err != nil {
		return nil, fmt.Errorf("create embedded torrent client: %w", err)
	}
	return &Manager{
		client: client,
		jobs:   map[string]*torrent.Torrent{},
		failed: map[string]string{},
		closed: make(chan struct{}),
	}, nil
}

func (m *Manager) Close() {
	if m == nil || m.client == nil {
		return
	}
	m.closeOnce.Do(func() { close(m.closed) }) // unblock metadata waiters
	_ = m.client.Close()
}

func (m *Manager) Add(ctx context.Context, id, magnet, title string) (Status, error) {
	if m == nil {
		return Status{}, fmt.Errorf("embedded downloader not configured")
	}
	t, err := m.client.AddMagnet(magnet)
	if err != nil {
		return Status{}, fmt.Errorf("add magnet: %w", err)
	}
	if title != "" {
		t.SetDisplayName(title)
	}
	t.DisallowDataUpload()
	if id == "" {
		id = t.InfoHash().HexString()
	}
	m.mu.Lock()
	m.jobs[id] = t
	delete(m.failed, id)
	m.mu.Unlock()
	go func() {
		select {
		case <-t.GotInfo():
			t.DisallowDataUpload()
			t.DownloadAll()
		case <-time.After(metadataTimeout):
			// No peers/seeds ever produced metadata. Drop the torrent
			// (frees conns/piece state) and record a terminal failure so
			// Status returns "failed" and the reconciler stops polling and
			// fails the request out, instead of leaking forever.
			t.Drop()
			m.mu.Lock()
			delete(m.jobs, id)
			m.failed[id] = "metadata timeout: no peers/seeds for magnet"
			m.mu.Unlock()
		case <-m.closed:
			return
		}
	}()
	return m.Status(ctx, id)
}

func (m *Manager) Status(_ context.Context, id string) (Status, error) {
	if m == nil {
		return Status{}, fmt.Errorf("embedded downloader not configured")
	}
	m.mu.Lock()
	if reason, bad := m.failed[id]; bad {
		m.mu.Unlock()
		return Status{ID: id, Status: "failed", Title: reason}, nil
	}
	t := m.jobs[id]
	m.mu.Unlock()
	if t == nil {
		return Status{ID: id, Status: "queued"}, nil
	}
	status := "queued"
	progress := 0
	length := t.Length()
	if length > 0 {
		completed := t.BytesCompleted()
		progress = int(completed * 100 / length)
		status = "downloading"
		if completed >= length {
			progress = 100
			status = "imported"
			t.DisallowDataUpload()
			t.Drop()
			m.mu.Lock()
			delete(m.jobs, id)
			m.mu.Unlock()
		}
	}
	return Status{ID: id, Title: t.Name(), Status: status, Progress: progress}, nil
}
