// Package runtime implements the plugin's Runtime gRPC server.
package runtime

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"

	pluginv1 "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginproto/continuum/plugin/v1"
	"github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginsdk/runtimedefault"
)

// Config is the parsed plugin global config (per spec Layer 9.3).
type Config struct {
	DatabaseURL         string   `json:"database_url,omitempty"`
	BaseURL             string   `json:"base_url"`
	DownloadMode        string   `json:"download_mode"`
	QBitURL             string   `json:"qbittorrent_url"`
	QBitUsername        string   `json:"qbittorrent_username"`
	QBitPassword        string   `json:"qbittorrent_password,omitempty"`
	QBitCategory        string   `json:"qbittorrent_category"`
	QBitSavePath        string   `json:"qbittorrent_save_path"`
	EmbeddedDownloadDir string   `json:"embedded_download_dir"`
	EmbeddedListenPort  int      `json:"embedded_listen_port"`
	Trackers            []string `json:"trackers"`
	SearchLimit         int      `json:"search_limit"`
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
		case "trackers":
			cfg.Trackers = stringSliceFromValue(m["value"])
		case "search_limit":
			cfg.SearchLimit = intFromValue(m["value"])
		}
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
	case "", "scrape_only", "qbittorrent", "embedded":
	default:
		return fmt.Errorf("download_mode must be scrape_only, qbittorrent, or embedded")
	}
	if cfg.DownloadMode == "qbittorrent" && cfg.QBitURL == "" {
		return fmt.Errorf("qbittorrent_url is required when download_mode is qbittorrent")
	}
	if cfg.QBitURL != "" {
		if err := validateOriginURL(cfg.QBitURL, true); err != nil {
			return fmt.Errorf("qbittorrent_url: %w", err)
		}
	}
	if cfg.DownloadMode == "embedded" && cfg.EmbeddedDownloadDir == "" {
		return fmt.Errorf("embedded_download_dir is required when download_mode is embedded")
	}
	if cfg.EmbeddedListenPort < 0 || cfg.EmbeddedListenPort > 65535 {
		return fmt.Errorf("embedded_listen_port must be between 0 and 65535")
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
