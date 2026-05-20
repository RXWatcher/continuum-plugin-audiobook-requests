package embeddednzbget

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"
)

// supervisorShutdownTimeout caps how long Stop() waits for the daemon
// to exit on SIGTERM before escalating to SIGKILL. NZBGet typically
// flushes its queue + closes connections in under a second; 10s is
// generous slack for a slow disk fsync on the queue dir.
const supervisorShutdownTimeout = 10 * time.Second

// portProbeDeadline caps how long Start() waits for the daemon to
// open its RPC port. Cold start on a slow filesystem (or first-time
// binary extraction on a sluggish disk) can take a few seconds; 30s
// guards against a wedged daemon that never serves.
const portProbeDeadline = 30 * time.Second

// Config configures the supervisor. DownloadDir is the operator's
// existing embedded download dir; the supervisor namespaces its files
// under DownloadDir/.nzbget/ so they don't collide with torrent
// downloads also rooted there.
type Config struct {
	DownloadDir string
	Provider    NewsProvider
	Logger      hclog.Logger
}

// Manager owns one supervised NZBGet child process. Safe to call
// Start/Stop concurrently; the only field a caller should read after
// Start is Endpoint().
type Manager struct {
	cfg    Config
	layout pathLayout

	mu       sync.Mutex
	cmd      *exec.Cmd
	port     int
	creds    rpcCredentials
	logDone  chan struct{} // closed when the log-pump goroutines exit
	exitErr  error         // last Wait() result, populated by reaper
	stopping bool
}

// pathLayout collects the on-disk paths the supervisor manages.
// Computed once from Config.DownloadDir; the supervisor uses these
// for extraction, conf generation, and NZBGet's working directories.
type pathLayout struct {
	Root      string // <DownloadDir>/.nzbget
	BinDir    string // <Root>/bin (extracted from the embedded tarball)
	MainDir   string // <Root>/queue — NZBGet's MainDir
	ConfPath  string // <Root>/nzbget.conf
	CertStore string // <BinDir>/cacert.pem
	VersionTag string // <BinDir>/.bundle-version — sentinel for re-extract checks
}

func newPathLayout(downloadDir string) pathLayout {
	root := filepath.Join(downloadDir, ".nzbget")
	bin := filepath.Join(root, "bin")
	return pathLayout{
		Root:       root,
		BinDir:     bin,
		MainDir:    filepath.Join(root, "queue"),
		ConfPath:   filepath.Join(root, "nzbget.conf"),
		CertStore:  filepath.Join(bin, "cacert.pem"),
		VersionTag: filepath.Join(bin, ".bundle-version"),
	}
}

// New constructs a Manager but does not start the daemon. Call Start
// to extract binaries (if needed) and spawn NZBGet.
func New(cfg Config) (*Manager, error) {
	if cfg.DownloadDir == "" {
		return nil, fmt.Errorf("embedded nzbget: download_dir is required")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil, fmt.Errorf("embedded nzbget: only linux/amd64 is supported (running on %s/%s)", runtime.GOOS, runtime.GOARCH)
	}
	if cfg.Provider.Host == "" || cfg.Provider.Port == 0 ||
		cfg.Provider.Username == "" || cfg.Provider.Password == "" {
		return nil, fmt.Errorf("embedded nzbget: usenet provider host/port/username/password are all required")
	}
	if cfg.Logger == nil {
		cfg.Logger = hclog.NewNullLogger()
	}
	return &Manager{cfg: cfg, layout: newPathLayout(cfg.DownloadDir)}, nil
}

// Start extracts the bundled binary if needed, writes a fresh conf,
// picks a free localhost port, spawns NZBGet, and blocks until the
// RPC port answers (or portProbeDeadline elapses). It is safe to
// call Start() multiple times; subsequent calls return immediately if
// the daemon is already healthy.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.cmd != nil && m.cmd.Process != nil && !m.stopping {
		port := m.port
		m.mu.Unlock()
		// Already running. Cheap probe to make sure it's still answering.
		if probeRPC(ctx, port) == nil {
			return nil
		}
		// Crashed/closed underneath us; fall through after stopping.
		_ = m.Stop()
		m.mu.Lock()
	}
	defer m.mu.Unlock()
	m.stopping = false

	if err := m.ensureExtracted(); err != nil {
		return fmt.Errorf("embedded nzbget: extract: %w", err)
	}
	port, err := pickFreePort()
	if err != nil {
		return fmt.Errorf("embedded nzbget: pick port: %w", err)
	}
	creds, err := newRPCCredentials()
	if err != nil {
		return fmt.Errorf("embedded nzbget: mint rpc creds: %w", err)
	}
	if err := os.MkdirAll(m.layout.MainDir, 0o755); err != nil {
		return fmt.Errorf("embedded nzbget: mkdir queue: %w", err)
	}
	conf := generateConf(m.layout, m.cfg.Provider, creds, port)
	if err := os.WriteFile(m.layout.ConfPath, []byte(conf), 0o600); err != nil {
		return fmt.Errorf("embedded nzbget: write conf: %w", err)
	}

	binPath := filepath.Join(m.layout.BinDir, "nzbget-x86_64")
	// -s = foreground server mode; -c <file> = use generated conf.
	// We intentionally don't use --daemon so the supervisor owns
	// process lifecycle directly via cmd.Wait().
	cmd := exec.Command(binPath, "-c", m.layout.ConfPath, "-s")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("embedded nzbget: spawn: %w", err)
	}
	m.cfg.Logger.Info("nzbget spawned", "pid", cmd.Process.Pid, "port", port)
	m.cmd = cmd
	m.port = port
	m.creds = creds
	m.logDone = make(chan struct{}, 2)
	go pumpLines("stdout", stdout, m.cfg.Logger, m.logDone)
	go pumpLines("stderr", stderr, m.cfg.Logger, m.logDone)

	// Wait for the RPC port to come up. The daemon writes its queue
	// state to disk before opening the port, so this is the
	// authoritative "ready" signal.
	probeCtx, cancel := context.WithTimeout(ctx, portProbeDeadline)
	defer cancel()
	if err := waitForPort(probeCtx, port); err != nil {
		// Don't leave a zombie; SIGTERM and harvest.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
		m.cmd = nil
		return fmt.Errorf("embedded nzbget: rpc port never came up: %w", err)
	}
	return nil
}

// Stop sends SIGTERM, waits up to supervisorShutdownTimeout, then
// escalates to SIGKILL. Safe to call multiple times; no-op if the
// daemon was never started.
func (m *Manager) Stop() error {
	m.mu.Lock()
	cmd := m.cmd
	logDone := m.logDone
	m.stopping = true
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	// SIGTERM the whole process group (Setpgid above) so any post-
	// processing child scripts get cleaned up too.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		m.cfg.Logger.Info("nzbget exited cleanly", "err", err)
	case <-time.After(supervisorShutdownTimeout):
		m.cfg.Logger.Warn("nzbget did not exit on SIGTERM; sending SIGKILL")
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}

	// Drain log goroutines so we don't return while they're still
	// writing to the logger (which may be closed by the caller).
	if logDone != nil {
		drainCount := 2
		for drainCount > 0 {
			select {
			case <-logDone:
				drainCount--
			case <-time.After(2 * time.Second):
				drainCount = 0
			}
		}
	}
	m.mu.Lock()
	m.cmd = nil
	m.mu.Unlock()
	return nil
}

// Endpoint returns the http://user:pass@127.0.0.1:port/ URL the
// nzbget package can use to talk to the supervised daemon. Returns
// the empty string before Start succeeds.
func (m *Manager) Endpoint() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil || m.port == 0 {
		return ""
	}
	u := &url.URL{
		Scheme: "http",
		User:   url.UserPassword(m.creds.Username, m.creds.Password),
		Host:   net.JoinHostPort("127.0.0.1", strconv.Itoa(m.port)),
	}
	return u.String()
}

// Credentials returns the generated RPC user/password the daemon is
// using. Useful when the caller wants to use the existing nzbget
// package (which takes user+pass separately from URL).
func (m *Manager) Credentials() (user, pass string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.creds.Username, m.creds.Password
}

// Port returns the bound localhost port, or 0 before Start succeeds.
func (m *Manager) Port() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.port
}

// ensureExtracted unpacks the embedded tarball into BinDir if the
// recorded bundle version doesn't match what we have. A version
// mismatch can happen on plugin upgrade — older binaries get
// overwritten with the new bundle.
func (m *Manager) ensureExtracted() error {
	tag, err := os.ReadFile(m.layout.VersionTag)
	if err == nil && string(tag) == nzbgetBundleVersion+":"+nzbgetBundleSHA256 {
		// Sanity-check the main binary still exists; if a janitor
		// nuked /var/lib we want a fresh extract, not a crash.
		if _, err := os.Stat(filepath.Join(m.layout.BinDir, "nzbget-x86_64")); err == nil {
			return nil
		}
	}
	if err := os.MkdirAll(m.layout.BinDir, 0o755); err != nil {
		return err
	}
	// Defence-in-depth: verify the embedded tarball still matches its
	// recorded SHA. Catches accidental ldflags injection / build-time
	// tampering that swaps the embedded bytes.
	sum := sha256.Sum256(nzbgetBundle)
	if hex.EncodeToString(sum[:]) != nzbgetBundleSHA256 {
		return fmt.Errorf("bundle SHA mismatch: embedded bundle has been tampered with")
	}
	gz, err := gzip.NewReader(bytes.NewReader(nzbgetBundle))
	if err != nil {
		return fmt.Errorf("gunzip bundle: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		out := filepath.Join(m.layout.BinDir, filepath.Base(hdr.Name))
		f, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("open %s: %w", out, err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return fmt.Errorf("write %s: %w", out, err)
		}
		f.Close()
	}
	if err := os.WriteFile(m.layout.VersionTag, []byte(nzbgetBundleVersion+":"+nzbgetBundleSHA256), 0o644); err != nil {
		return fmt.Errorf("write version tag: %w", err)
	}
	return nil
}

// pickFreePort asks the kernel for a free ephemeral port. There's a
// time-of-check / time-of-use gap before NZBGet binds it, but it's
// tiny enough in practice that I've never seen the kernel hand the
// same port to a second caller in that window.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForPort polls the port until something accepts a TCP connection
// (NZBGet's RPC listener) or ctx fires.
func waitForPort(ctx context.Context, port int) error {
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func probeRPC(ctx context.Context, port int) error {
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// pumpLines reads NZBGet's stdout/stderr line by line and re-emits
// each line via the plugin logger so operators can see NZBGet output
// in `docker logs`. Signals completion on `done` so Stop() can wait
// for the goroutines to finish before returning.
func pumpLines(stream string, r io.ReadCloser, logger hclog.Logger, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		logger.Info("nzbget["+stream+"]", "line", sc.Text())
	}
}
