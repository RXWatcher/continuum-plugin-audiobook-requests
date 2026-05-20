package embeddednzbget

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestEmbeddedBundle_IntegrityAndContent confirms the vendored
// tarball is reachable, matches its recorded SHA, and unpacks the
// four files the supervisor expects. Catches accidental dist/
// corruption + version drift between embed.go's recorded SHA and
// the actual bytes go:embed pulled in.
func TestEmbeddedBundle_IntegrityAndContent(t *testing.T) {
	if len(nzbgetBundle) < 1<<20 {
		t.Fatalf("embedded bundle suspiciously small: %d bytes", len(nzbgetBundle))
	}
	sum := sha256.Sum256(nzbgetBundle)
	if got := hex.EncodeToString(sum[:]); got != nzbgetBundleSHA256 {
		t.Fatalf("bundle SHA = %s, want %s (regenerate embed.go's SHA or restore the dist/ file)", got, nzbgetBundleSHA256)
	}

	dir := t.TempDir()
	mgr, err := New(Config{
		DownloadDir: dir,
		Provider: NewsProvider{
			Host: "news.example.invalid", Port: 119,
			Username: "u", Password: "p",
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := mgr.ensureExtracted(); err != nil {
		t.Fatalf("ensureExtracted: %v", err)
	}
	for _, name := range []string{"nzbget-x86_64", "unrar-x86_64", "7za-x86_64", "cacert.pem"} {
		p := filepath.Join(mgr.layout.BinDir, name)
		fi, err := os.Stat(p)
		if err != nil {
			t.Errorf("missing extracted file %s: %v", name, err)
			continue
		}
		if fi.Mode()&0o100 == 0 && name != "cacert.pem" {
			t.Errorf("%s is not executable (mode %o)", name, fi.Mode())
		}
	}

	// Re-running should be a no-op (idempotent) because the version
	// tag matches.
	beforeStat, _ := os.Stat(filepath.Join(mgr.layout.BinDir, "nzbget-x86_64"))
	if err := mgr.ensureExtracted(); err != nil {
		t.Fatalf("second ensureExtracted: %v", err)
	}
	afterStat, _ := os.Stat(filepath.Join(mgr.layout.BinDir, "nzbget-x86_64"))
	if beforeStat.ModTime() != afterStat.ModTime() {
		t.Errorf("second ensureExtracted rewrote the binary; expected no-op")
	}
}

// TestGenerateConf_ContainsCriticalSettings pins the supervisor-
// generated nzbget.conf against the settings that, if missing or
// wrong, would either expose the RPC port externally or break the
// download pipeline.
func TestGenerateConf_ContainsCriticalSettings(t *testing.T) {
	layout := newPathLayout(t.TempDir())
	creds := rpcCredentials{Username: "u1", Password: "p1"}
	conf := generateConf(layout, NewsProvider{
		Host:        "news.example.com",
		Port:        563,
		SSL:         true,
		Username:    "operator",
		Password:    "hunter2",
		Connections: 12,
	}, creds, 6789)
	for _, want := range []string{
		"ControlIP=127.0.0.1",  // never bind public
		"AuthorizedIP=",        // no allowlist override
		"ControlUsername=u1",   // auth required
		"ControlPassword=p1",
		"ControlPort=6789",
		"Server1.Host=news.example.com",
		"Server1.Port=563",
		"Server1.Username=operator",
		"Server1.Password=hunter2",
		"Server1.Encryption=yes",
		"Server1.Connections=12",
		"Unpack=yes",
		"ParCheck=auto",
	} {
		if !contains(conf, want) {
			t.Errorf("generated conf missing %q", want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > len(sub) && indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
