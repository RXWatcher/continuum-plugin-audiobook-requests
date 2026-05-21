// Package store wraps pgx for the audiobookbay plugin.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	pluginrt "github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/runtime"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func DefaultAppConfig() pluginrt.Config {
	return pluginrt.Config{
		AudiobookBayDownloadMode: "scrape_only",
		AbookDownloadMode:        "scrape_only",
		SearchLimit:              10,
		Trackers:                 []string{},
	}
}

func (s *Store) GetAppConfig(ctx context.Context) (pluginrt.Config, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT data FROM app_config WHERE id = 1`).Scan(&raw)
	if err == pgx.ErrNoRows {
		if _, err := s.pool.Exec(ctx, `INSERT INTO app_config (id, data) VALUES (1, '{}'::jsonb) ON CONFLICT (id) DO NOTHING`); err != nil {
			return pluginrt.Config{}, fmt.Errorf("ensure app_config: %w", err)
		}
		return s.GetAppConfig(ctx)
	}
	if err != nil {
		return pluginrt.Config{}, fmt.Errorf("get app_config: %w", err)
	}
	cfg := DefaultAppConfig()
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return pluginrt.Config{}, fmt.Errorf("decode app_config: %w", err)
		}
	}
	normalize(&cfg)
	return cfg, nil
}

func (s *Store) UpdateAppConfig(ctx context.Context, cfg pluginrt.Config) error {
	cfg.DatabaseURL = ""
	normalize(&cfg)
	if err := pluginrt.ValidateAppConfig(cfg); err != nil {
		return err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode app_config: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO app_config (id, data, updated_at) VALUES (1, $1, NOW())
		ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data, updated_at = NOW()
	`, raw)
	if err != nil {
		return fmt.Errorf("update app_config: %w", err)
	}
	return nil
}

func (s *Store) ImportLegacyAppConfig(ctx context.Context, legacy pluginrt.Config) (pluginrt.Config, error) {
	current, err := s.GetAppConfig(ctx)
	if err != nil {
		return pluginrt.Config{}, err
	}
	if !reflect.DeepEqual(current, DefaultAppConfig()) {
		return current, nil
	}
	legacy.DatabaseURL = ""
	normalize(&legacy)
	if reflect.DeepEqual(legacy, current) {
		return current, nil
	}
	if err := s.UpdateAppConfig(ctx, legacy); err != nil {
		return pluginrt.Config{}, err
	}
	return s.GetAppConfig(ctx)
}

func normalize(cfg *pluginrt.Config) {
	if cfg.AudiobookBayDownloadMode == "" {
		// Carry legacy DownloadMode forward when it represents a torrent
		// path; otherwise default to scrape_only so the source stays
		// passive until the operator opts in.
		switch cfg.DownloadMode {
		case "scrape_only", "qbittorrent", "embedded":
			cfg.AudiobookBayDownloadMode = cfg.DownloadMode
		default:
			if cfg.QBitURL != "" {
				cfg.AudiobookBayDownloadMode = "qbittorrent"
			} else {
				cfg.AudiobookBayDownloadMode = "scrape_only"
			}
		}
	}
	if cfg.AbookDownloadMode == "" {
		switch cfg.DownloadMode {
		case "embedded_nzbget", "external_nzbget":
			cfg.AbookDownloadMode = cfg.DownloadMode
		case "qbittorrent", "embedded":
			if cfg.NZBGetURL != "" {
				cfg.AbookDownloadMode = "external_nzbget"
			} else {
				cfg.AbookDownloadMode = "scrape_only"
			}
		default:
			cfg.AbookDownloadMode = "scrape_only"
		}
	}
	// DownloadMode is legacy; clear it so the DB only reflects the new
	// per-source fields going forward.
	cfg.DownloadMode = ""
	if cfg.SearchLimit == 0 {
		cfg.SearchLimit = 10
	}
	if cfg.Trackers == nil {
		cfg.Trackers = []string{}
	}
}
