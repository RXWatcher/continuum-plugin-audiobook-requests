package embedded

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
)

// metadataTimeout bounds how long we wait for a magnet to obtain its info
// (peers/seeds). A dead/peerless magnet's GotInfo() never fires; without this
// the waiter goroutine and the *torrent.Torrent (conns, piece state) would
// leak forever and the request would poll "downloading" indefinitely.
const metadataTimeout = 3 * time.Minute

// defaultMaxConcurrent caps how many in-flight torrent jobs the embedded
// manager will hold simultaneously. Without this an operator who selects
// embedded mode and then receives N concurrent user requests can spawn N
// torrent connection sets, consume all available file descriptors, and
// thrash the connection tracking table. Tuned conservatively; bump via
// Config.MaxConcurrent when you know your machine can handle more.
const defaultMaxConcurrent = 8

// minFreeBytes guards against starting downloads onto an almost-full disk.
// AudiobookBay torrents are typically a few hundred MB; refusing Adds below
// 2 GiB free is a generous lower bound that prevents ENOSPC mid-download
// (which leaves a partial file the operator has to clean up by hand).
const minFreeBytes uint64 = 2 << 30

type Config struct {
	DownloadDir string
	ListenPort  int
	// MaxConcurrent caps live torrents (queued + downloading) the manager
	// will hold. 0 / negative uses defaultMaxConcurrent.
	MaxConcurrent int
}

// ErrAtCapacity is returned from Add when MaxConcurrent jobs are already
// in flight. The caller records this on the request so the operator sees
// "at capacity, retry later" rather than a silent drop.
var ErrAtCapacity = fmt.Errorf("embedded downloader at concurrency cap")

// ErrInsufficientDisk is returned from Add when the data dir's free space
// is below minFreeBytes. Refusing the Add is safer than letting the
// torrent client crawl toward ENOSPC and partial-file corruption.
var ErrInsufficientDisk = fmt.Errorf("embedded download dir is below minimum free space")

// CheckPathWritable validates a directory is reachable, writable, and has
// at least minFreeBytes of free space. Used by both the runtime config
// validator (pre-flight) and the admin /preflight handler.
func CheckPathWritable(dir string) error {
	if dir == "" {
		return fmt.Errorf("path is empty")
	}
	abs := filepath.Clean(dir)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", abs, err)
	}
	probe, err := os.CreateTemp(abs, ".write-probe-*")
	if err != nil {
		return fmt.Errorf("write probe in %s: %w", abs, err)
	}
	probeName := probe.Name()
	probe.Close()
	os.Remove(probeName)

	var st syscall.Statfs_t
	if err := syscall.Statfs(abs, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", abs, err)
	}
	free := uint64(st.Bavail) * uint64(st.Bsize)
	if free < minFreeBytes {
		return fmt.Errorf("only %d MiB free at %s; need at least %d MiB", free>>20, abs, minFreeBytes>>20)
	}
	return nil
}

// DiskFree returns (used, free, total) bytes for the given dir. Used by
// the admin preflight endpoint to render a disk-pressure tile. Returns
// zero values on error rather than failing the whole endpoint.
func DiskFree(dir string) (free, total uint64) {
	if dir == "" {
		return 0, 0
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0
	}
	free = uint64(st.Bavail) * uint64(st.Bsize)
	total = uint64(st.Blocks) * uint64(st.Bsize)
	return
}

type Status struct {
	ID       string
	Title    string
	Status   string
	Progress int
}

type Manager struct {
	client     *torrent.Client
	dataDir    string
	maxJobs    int

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
	if err := CheckPathWritable(cfg.DownloadDir); err != nil {
		return nil, fmt.Errorf("embedded_download_dir: %w", err)
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
	max := cfg.MaxConcurrent
	if max <= 0 {
		max = defaultMaxConcurrent
	}
	return &Manager{
		client:  client,
		dataDir: cfg.DownloadDir,
		maxJobs: max,
		jobs:    map[string]*torrent.Torrent{},
		failed:  map[string]string{},
		closed:  make(chan struct{}),
	}, nil
}

// DataDir returns the resolved data directory the manager is writing to.
// Exposed so the admin /preflight endpoint can report disk space against
// the actual path in use.
func (m *Manager) DataDir() string {
	if m == nil {
		return ""
	}
	return m.dataDir
}

// JobCount returns the current count of live torrents (queued + downloading)
// the manager is tracking. Used by the admin readiness view.
func (m *Manager) JobCount() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.jobs)
}

// MaxConcurrent returns the configured cap on live torrents.
func (m *Manager) MaxConcurrent() int {
	if m == nil {
		return 0
	}
	return m.maxJobs
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
	// Pre-flight: cap concurrent torrents so a burst of user requests can't
	// exhaust file descriptors or peer-tracker memory. Operator sees the
	// request fail explicitly instead of the plugin OOM'ing.
	m.mu.Lock()
	if len(m.jobs) >= m.maxJobs {
		m.mu.Unlock()
		return Status{}, fmt.Errorf("%w: %d/%d slots in use", ErrAtCapacity, len(m.jobs), m.maxJobs)
	}
	m.mu.Unlock()
	// Pre-flight: refuse if disk is below the minimum free threshold. The
	// torrent client would otherwise crawl until ENOSPC and leave partial
	// files; failing fast keeps the data dir clean for human triage.
	if free, _ := DiskFree(m.dataDir); free > 0 && free < minFreeBytes {
		return Status{}, fmt.Errorf("%w: %d MiB free at %s, need at least %d MiB", ErrInsufficientDisk, free>>20, m.dataDir, minFreeBytes>>20)
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
