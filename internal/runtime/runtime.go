// Package runtime implements the plugin's Runtime gRPC server.
package runtime

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"

	pluginv1 "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginsdk/runtimedefault"
)

// Config is the parsed plugin global config (per spec Layer 9.3).
//
// DownloadMode is the legacy single global mode. It is preserved only so
// existing rows continue to work; new code reads AudiobookBayDownloadMode and
// AbookDownloadMode through the EffectiveAudiobookBayMode / EffectiveAbookMode
// helpers, which fall back to DownloadMode when the per-source field is empty.
type Config struct {
	DatabaseURL              string `json:"database_url,omitempty"`
	BaseURL                  string `json:"base_url"`
	DownloadMode             string `json:"download_mode,omitempty"`
	AudiobookBayDownloadMode string `json:"audiobookbay_download_mode"`
	AbookDownloadMode        string `json:"abook_download_mode"`
	QBitURL                  string `json:"qbittorrent_url"`
	QBitUsername        string   `json:"qbittorrent_username"`
	QBitPassword        string   `json:"qbittorrent_password,omitempty"`
	QBitCategory        string   `json:"qbittorrent_category"`
	QBitSavePath        string   `json:"qbittorrent_save_path"`
	EmbeddedDownloadDir   string `json:"embedded_download_dir"`
	EmbeddedListenPort    int    `json:"embedded_listen_port"`
	// EmbeddedMaxConcurrent caps in-process torrents at any one time. 0 or
	// negative defers to the embedded package's default. Bump if you've
	// sized fd limits + connection-tracking accordingly.
	EmbeddedMaxConcurrent int `json:"embedded_max_concurrent"`
	Trackers            []string `json:"trackers"`
	SearchLimit         int      `json:"search_limit"`

	// abook.link parallel source. When AbookEmail+AbookPassword are set,
	// the consumer also searches abook.link for each incoming request and
	// hands the best abook hit off to NZBGet. AbookCookie holds the most
	// recently minted SMF session so re-logins only happen on expiry.
	AbookBaseURL  string `json:"abook_base_url"`
	AbookEmail    string `json:"abook_email"`
	AbookPassword string `json:"abook_password,omitempty"`
	AbookCookie   string `json:"abook_cookie,omitempty"`

	// NZBGet handoff. Required when abook is configured; the abook source
	// doesn't produce magnets, so without NZBGet there's nothing to do
	// with a winning hit. In download_mode=embedded_nzbget these are
	// auto-filled from the supervised daemon and any operator-provided
	// values are ignored.
	NZBGetURL      string `json:"nzbget_url"`
	NZBGetUsername string `json:"nzbget_username"`
	NZBGetPassword string `json:"nzbget_password,omitempty"`
	NZBGetCategory string `json:"nzbget_category"`

	// Usenet provider credentials, used by the embedded NZBGet daemon
	// (download_mode=embedded_nzbget). Independent of the NZBGetUsername
	// /Password above — those gate NZBGet's RPC port; these are what
	// NZBGet uses to fetch articles from the operator's news provider.
	UsenetHost        string `json:"usenet_host"`
	UsenetPort        int    `json:"usenet_port"`
	UsenetSSL         bool   `json:"usenet_ssl"`
	UsenetUsername    string `json:"usenet_username"`
	UsenetPassword    string `json:"usenet_password,omitempty"`
	UsenetConnections int    `json:"usenet_connections"`
}

// EffectiveAudiobookBayMode returns the AudiobookBay-side download mode
// the runtime should act on. Falls back to the legacy DownloadMode field
// only when it represents a torrent path (scrape_only/qbittorrent/embedded);
// "embedded_nzbget" is an abook-side mode and would otherwise leak.
func (c Config) EffectiveAudiobookBayMode() string {
	if c.AudiobookBayDownloadMode != "" {
		return c.AudiobookBayDownloadMode
	}
	switch c.DownloadMode {
	case "scrape_only", "qbittorrent", "embedded":
		return c.DownloadMode
	}
	return "scrape_only"
}

// EffectiveAbookMode returns the abook-side download mode the runtime should
// act on. Falls back to legacy DownloadMode only when it represents an NZB
// path (scrape_only/embedded_nzbget) or "external_nzbget"; torrent modes
// imply abook is opt-out (external_nzbget when nzbget_url is set, else scrape_only).
func (c Config) EffectiveAbookMode() string {
	if c.AbookDownloadMode != "" {
		return c.AbookDownloadMode
	}
	switch c.DownloadMode {
	case "scrape_only", "external_nzbget", "embedded_nzbget":
		return c.DownloadMode
	case "qbittorrent", "embedded":
		if c.NZBGetURL != "" {
			return "external_nzbget"
		}
		return "scrape_only"
	}
	return "scrape_only"
}

// AbookConfigured reports whether the abook pipeline has enough configuration
// to participate in searches. Returns false in scrape_only — abook is a
// closed account so we don't want to hit it without a downloader to consume
// the NZB hit.
func (c Config) AbookConfigured() bool {
	if c.AbookEmail == "" || c.AbookPassword == "" {
		return false
	}
	switch c.EffectiveAbookMode() {
	case "embedded_nzbget":
		return c.EmbeddedNZBGetConfigured()
	case "external_nzbget":
		return c.NZBGetURL != ""
	}
	return false
}

// EmbeddedNZBGetConfigured reports whether the operator has provided
// every field the embedded NZBGet daemon needs to fetch articles from
// their news provider.
func (c Config) EmbeddedNZBGetConfigured() bool {
	return c.UsenetHost != "" && c.UsenetPort > 0 &&
		c.UsenetUsername != "" && c.UsenetPassword != "" &&
		c.EmbeddedDownloadDir != ""
}

func (c Config) Configured() bool {
	return c.DatabaseURL != ""
}

func (c Config) ProviderConfigured() bool {
	return c.BaseURL != ""
}

type Server struct {
	runtimedefault.Server
	manifest *pluginv1.PluginManifest
	onCfg    func(Config) error

	mu  sync.RWMutex
	cfg Config
}

func New(manifest *pluginv1.PluginManifest, onConfig func(Config) error) *Server {
	return &Server{manifest: manifest, onCfg: onConfig}
}

func (s *Server) GetManifest(_ context.Context, _ *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

func (s *Server) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	cfg := Config{}
	for _, e := range req.GetConfig() {
		v := e.GetValue()
		if v == nil {
			continue
		}
		m := v.AsMap()
		switch e.GetKey() {
		case "database_url":
			cfg.DatabaseURL = stringFromValue(m["value"])
		case "base_url":
			cfg.BaseURL = stringFromValue(m["value"])
		case "download_mode":
			cfg.DownloadMode = stringFromValue(m["value"])
		case "audiobookbay_download_mode":
			cfg.AudiobookBayDownloadMode = stringFromValue(m["value"])
		case "abook_download_mode":
			cfg.AbookDownloadMode = stringFromValue(m["value"])
		case "qbittorrent_url":
			cfg.QBitURL = stringFromValue(m["value"])
		case "qbittorrent_username":
			cfg.QBitUsername = stringFromValue(m["value"])
		case "qbittorrent_password":
			cfg.QBitPassword = stringFromValue(m["value"])
		case "qbittorrent_category":
			cfg.QBitCategory = stringFromValue(m["value"])
		case "qbittorrent_save_path":
			cfg.QBitSavePath = stringFromValue(m["value"])
		case "embedded_download_dir":
			cfg.EmbeddedDownloadDir = stringFromValue(m["value"])
		case "embedded_listen_port":
			cfg.EmbeddedListenPort = intFromValue(m["value"])
		case "embedded_max_concurrent":
			cfg.EmbeddedMaxConcurrent = intFromValue(m["value"])
		case "abook_base_url":
			cfg.AbookBaseURL = stringFromValue(m["value"])
		case "abook_email":
			cfg.AbookEmail = stringFromValue(m["value"])
		case "abook_password":
			cfg.AbookPassword = stringFromValue(m["value"])
		case "abook_cookie":
			cfg.AbookCookie = stringFromValue(m["value"])
		case "nzbget_url":
			cfg.NZBGetURL = stringFromValue(m["value"])
		case "nzbget_username":
			cfg.NZBGetUsername = stringFromValue(m["value"])
		case "nzbget_password":
			cfg.NZBGetPassword = stringFromValue(m["value"])
		case "nzbget_category":
			cfg.NZBGetCategory = stringFromValue(m["value"])
		case "usenet_host":
			cfg.UsenetHost = stringFromValue(m["value"])
		case "usenet_port":
			cfg.UsenetPort = intFromValue(m["value"])
		case "usenet_ssl":
			cfg.UsenetSSL = boolFromValue(m["value"])
		case "usenet_username":
			cfg.UsenetUsername = stringFromValue(m["value"])
		case "usenet_password":
			cfg.UsenetPassword = stringFromValue(m["value"])
		case "usenet_connections":
			cfg.UsenetConnections = intFromValue(m["value"])
		case "trackers":
			cfg.Trackers = stringSliceFromValue(m["value"])
		case "search_limit":
			cfg.SearchLimit = intFromValue(m["value"])
		}
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://audiobookbay.lu"
	}
	if cfg.AbookBaseURL == "" {
		cfg.AbookBaseURL = "https://abook.link/book"
	}
	if cfg.DatabaseURL == "" {
		s.mu.Lock()
		s.cfg = cfg
		s.mu.Unlock()
		return &pluginv1.ConfigureResponse{}, nil
	}
	if err := ValidateAppConfig(cfg); err != nil {
		return nil, err
	}
	if s.onCfg != nil {
		if err := s.onCfg(cfg); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	return &pluginv1.ConfigureResponse{}, nil
}

func ValidateAppConfig(cfg Config) error {
	if cfg.BaseURL != "" {
		if err := validateOriginURL(cfg.BaseURL, false); err != nil {
			return fmt.Errorf("base_url: %w", err)
		}
	}
	switch cfg.DownloadMode {
	case "", "scrape_only", "qbittorrent", "embedded", "embedded_nzbget", "external_nzbget":
	default:
		return fmt.Errorf("download_mode must be scrape_only, qbittorrent, embedded, embedded_nzbget, or external_nzbget")
	}
	abMode := cfg.EffectiveAudiobookBayMode()
	switch abMode {
	case "scrape_only", "qbittorrent", "embedded":
	default:
		return fmt.Errorf("audiobookbay_download_mode must be scrape_only, qbittorrent, or embedded")
	}
	abookMode := cfg.EffectiveAbookMode()
	switch abookMode {
	case "scrape_only", "external_nzbget", "embedded_nzbget":
	default:
		return fmt.Errorf("abook_download_mode must be scrape_only, external_nzbget, or embedded_nzbget")
	}
	if abookMode == "embedded_nzbget" {
		// All four parts of the usenet provider config are load-bearing —
		// the supervised daemon refuses to start without a working
		// Server1.* block. Validate up-front so the operator sees the
		// problem on Save, not after the daemon refuses to come up.
		if cfg.UsenetHost == "" || cfg.UsenetPort == 0 ||
			cfg.UsenetUsername == "" || cfg.UsenetPassword == "" {
			return fmt.Errorf("usenet_host, usenet_port, usenet_username, and usenet_password are all required for abook_download_mode=embedded_nzbget")
		}
		if cfg.EmbeddedDownloadDir == "" {
			return fmt.Errorf("embedded_download_dir is required for abook_download_mode=embedded_nzbget (used as the NZBGet working dir)")
		}
	}
	if cfg.UsenetPort < 0 || cfg.UsenetPort > 65535 {
		return fmt.Errorf("usenet_port must be between 0 and 65535")
	}
	if cfg.UsenetConnections < 0 || cfg.UsenetConnections > 64 {
		return fmt.Errorf("usenet_connections must be between 0 and 64 (0 = default 8)")
	}
	if abMode == "qbittorrent" && cfg.QBitURL == "" {
		return fmt.Errorf("qbittorrent_url is required for audiobookbay_download_mode=qbittorrent")
	}
	if cfg.QBitURL != "" {
		if err := validateOriginURL(cfg.QBitURL, true); err != nil {
			return fmt.Errorf("qbittorrent_url: %w", err)
		}
	}
	if abMode == "embedded" && cfg.EmbeddedDownloadDir == "" {
		return fmt.Errorf("embedded_download_dir is required for audiobookbay_download_mode=embedded")
	}
	if cfg.EmbeddedListenPort < 0 || cfg.EmbeddedListenPort > 65535 {
		return fmt.Errorf("embedded_listen_port must be between 0 and 65535")
	}
	if cfg.AbookBaseURL != "" {
		if err := validateOriginURL(cfg.AbookBaseURL, false); err != nil {
			return fmt.Errorf("abook_base_url: %w", err)
		}
	}
	abookCredsPartial := (cfg.AbookEmail != "") != (cfg.AbookPassword != "")
	if abookCredsPartial {
		return fmt.Errorf("abook_email and abook_password must both be set together")
	}
	if abookMode == "external_nzbget" && cfg.NZBGetURL == "" {
		return fmt.Errorf("nzbget_url is required for abook_download_mode=external_nzbget")
	}
	if cfg.NZBGetURL != "" {
		if err := validateOriginURL(cfg.NZBGetURL, true); err != nil {
			return fmt.Errorf("nzbget_url: %w", err)
		}
	}
	nzbgetCredsPartial := (cfg.NZBGetUsername != "") != (cfg.NZBGetPassword != "")
	if nzbgetCredsPartial {
		return fmt.Errorf("nzbget_username and nzbget_password must both be set together")
	}
	if cfg.EmbeddedMaxConcurrent < 0 || cfg.EmbeddedMaxConcurrent > 64 {
		return fmt.Errorf("embedded_max_concurrent must be between 0 and 64")
	}
	if cfg.SearchLimit < 0 || cfg.SearchLimit > 100 {
		return fmt.Errorf("search_limit must be between 0 and 100")
	}
	for _, tracker := range cfg.Trackers {
		if err := validateTrackerURL(tracker); err != nil {
			return fmt.Errorf("trackers: %w", err)
		}
	}
	return nil
}

func validateOriginURL(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be an origin URL without credentials, query, or fragment")
	}
	if u.Scheme == "http" && !allowHTTP && !isLocalhost(u.Hostname()) {
		return fmt.Errorf("must use https except for localhost")
	}
	return nil
}

func validateTrackerURL(raw string) error {
	tracker := strings.TrimSpace(raw)
	if tracker == "" {
		return nil
	}
	u, err := url.Parse(tracker)
	if err != nil {
		return fmt.Errorf("%q is invalid: %w", tracker, err)
	}
	switch u.Scheme {
	case "udp", "http", "https":
	default:
		return fmt.Errorf("%q has unsupported scheme", tracker)
	}
	if u.Host == "" || u.User != nil {
		return fmt.Errorf("%q must include host and no credentials", tracker)
	}
	return nil
}

func isLocalhost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) Snapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
	c.Trackers = append([]string(nil), s.cfg.Trackers...)
	return c
}

func stringFromValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func stringSliceFromValue(v any) []string {
	a, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(a))
	for _, e := range a {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func intFromValue(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func boolFromValue(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}
