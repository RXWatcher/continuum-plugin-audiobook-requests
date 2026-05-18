package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	goruntime "runtime"
	"sync/atomic"

	"github.com/hashicorp/go-hclog"
	"github.com/jackc/pgx/v5/pgxpool"

	pluginv1 "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginproto/continuum/plugin/v1"
	publicmanifest "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginsdk/runtime"

	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/audiobookbay"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/consumer"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/embedded"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/event"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/httproutes"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/migrate"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/reconciler"
	pluginrt "github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/runtime"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/scheduler"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/server"
	"github.com/ContinuumApp/continuum-plugin-audiobookbay-requests/internal/store"
)

//go:embed manifest.json
var manifestRaw []byte

func main() {
	logger := hclog.New(&hclog.LoggerOptions{Name: "continuum-plugin-audiobookbay-requests"})

	manifest, err := loadManifest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load manifest: %v\n", err)
		os.Exit(1)
	}

	httpSrv := httproutes.NewServer()
	httpSrv.SetHandler(server.New(server.Deps{}).Handler())

	var (
		poolPtr       atomic.Pointer[pgxpool.Pool]
		embeddedPtr   atomic.Pointer[embedded.Manager]
		consumerDepsP atomic.Pointer[consumer.Deps]
		reconcilerPtr atomic.Pointer[reconciler.Reconciler]
	)

	consumerHandler := consumer.New(func() *consumer.Deps { return consumerDepsP.Load() }, logger.Named("consumer"))
	schedulerSrv := scheduler.New(func() *reconciler.Reconciler { return reconcilerPtr.Load() })

	rt := pluginrt.New(manifest, func(cfg pluginrt.Config) error {
		ctx := context.Background()
		// Explicit MaxConns cap. The pgx default scales with GOMAXPROCS and
		// can be as low as 4; the search API + reconciler mix can starve
		// under that. 16 is generous without saturating a shared Postgres.
		// Operators override via DSN (?pool_max_conns=N).
		pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("parse db: %w", err)
		}
		if pcfg.MaxConns < 16 {
			pcfg.MaxConns = 16
		}
		p, err := pgxpool.NewWithConfig(ctx, pcfg)
		if err != nil {
			return fmt.Errorf("pgxpool: %w", err)
		}
		if err := migrate.Run(ctx, cfg.DatabaseURL); err != nil {
			p.Close()
			return fmt.Errorf("migrate: %w", err)
		}
		st := store.New(p)
		var embeddedManager *embedded.Manager
		if cfg.DownloadMode == "embedded" {
			embeddedManager, err = embedded.New(embedded.Config{
				DownloadDir: cfg.EmbeddedDownloadDir,
				ListenPort:  cfg.EmbeddedListenPort,
			})
			if err != nil {
				p.Close()
				return err
			}
		}
		abbClient := audiobookbay.NewClient(audiobookbay.Config{
			BaseURL:             cfg.BaseURL,
			DownloadMode:        cfg.DownloadMode,
			QBitURL:             cfg.QBitURL,
			QBitUsername:        cfg.QBitUsername,
			QBitPassword:        cfg.QBitPassword,
			Category:            cfg.QBitCategory,
			SavePath:            cfg.QBitSavePath,
			EmbeddedDownloadDir: cfg.EmbeddedDownloadDir,
			EmbeddedListenPort:  cfg.EmbeddedListenPort,
			Trackers:            cfg.Trackers,
			SearchLimit:         cfg.SearchLimit,
		}, embeddedManager)
		if embeddedManager != nil {
			if rows, err := st.ListNonTerminal(ctx, 200); err == nil {
				for _, row := range rows {
					if row.ExternalID != "" && row.MagnetURI != "" {
						if err := abbClient.RestoreDownload(ctx, row.ExternalID, row.MagnetURI, row.SelectedTitle); err != nil {
							logger.Warn("restore embedded download", "request_id", row.RequestID, "err", err)
						}
					}
				}
			}
		}

		srv := server.New(server.Deps{
			AudiobookBayClient: abbClient,
			Store:              st,
			Config:             cfg,
		})
		httpSrv.SetHandler(srv.Handler())

		ev := event.New(sdkruntime.Host(), logger.Named("event"))
		consumerDepsP.Store(&consumer.Deps{
			Store: st, Pub: ev, ABB: abbClient,
			PluginID: "continuum.audiobookbay-requests",
		})
		reconcilerPtr.Store(reconciler.New(reconciler.Deps{
			Store: st, Pub: ev, ABB: abbClient,
			PluginID: "continuum.audiobookbay-requests",
		}))

		if old := embeddedPtr.Swap(embeddedManager); old != nil {
			old.Close()
		}
		if old := poolPtr.Swap(p); old != nil {
			old.Close()
		}
		logger.Info("configured", "base_url", cfg.BaseURL)
		return nil
	})

	sdkruntime.Serve(sdkruntime.ServeConfig{
		Logger: logger,
		Servers: sdkruntime.CapabilityServers{
			Runtime:       rt,
			HttpRoutes:    httpSrv,
			EventConsumer: consumerHandler,
			ScheduledTask: schedulerSrv,
		},
	})
}

func loadManifest() (*pluginv1.PluginManifest, error) {
	manifest, err := publicmanifest.Load(manifestRaw)
	if err != nil {
		return nil, fmt.Errorf("load embedded manifest: %w", err)
	}
	executablePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	binaryData, err := os.ReadFile(executablePath)
	if err != nil {
		return nil, fmt.Errorf("read executable %q: %w", executablePath, err)
	}
	checksum := sha256.Sum256(binaryData)
	manifest.Checksum = hex.EncodeToString(checksum[:])
	if len(manifest.GetSupportedPlatforms()) == 0 {
		manifest.SupportedPlatforms = []*pluginv1.SupportedPlatform{
			{Os: goruntime.GOOS, Arch: goruntime.GOARCH},
		}
	}
	return manifest, nil
}
