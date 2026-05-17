// Package runtime implements the plugin's Runtime gRPC server.
package runtime

import (
	"context"
	"fmt"
	"sync"

	pluginv1 "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginproto/continuum/plugin/v1"
	"github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginsdk/runtimedefault"
)

// Config is the parsed plugin global config (per spec Layer 9.3).
type Config struct {
	DatabaseURL         string
	BaseURL             string
	DownloadMode        string
	QBitURL             string
	QBitUsername        string
	QBitPassword        string
	QBitCategory        string
	QBitSavePath        string
	EmbeddedDownloadDir string
	EmbeddedListenPort  int
	Trackers            []string
	SearchLimit         int
}

func (c Config) Configured() bool {
	return c.BaseURL != "" && c.DatabaseURL != ""
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
		return nil, fmt.Errorf("database_url is required")
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("base_url is required")
	}
	switch cfg.DownloadMode {
	case "", "scrape_only", "qbittorrent", "embedded":
	default:
		return nil, fmt.Errorf("download_mode must be scrape_only, qbittorrent, or embedded")
	}
	if cfg.DownloadMode == "qbittorrent" && cfg.QBitURL == "" {
		return nil, fmt.Errorf("qbittorrent_url is required when download_mode is qbittorrent")
	}
	if cfg.DownloadMode == "embedded" && cfg.EmbeddedDownloadDir == "" {
		return nil, fmt.Errorf("embedded_download_dir is required when download_mode is embedded")
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

func (s *Server) Snapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
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
