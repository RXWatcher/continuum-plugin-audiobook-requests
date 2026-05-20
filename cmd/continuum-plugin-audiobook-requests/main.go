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
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/jackc/pgx/v5/pgxpool"

	pluginv1 "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginproto/continuum/plugin/v1"
	publicmanifest "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginsdk/runtime"

	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/abook"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/audiobookbay"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/consumer"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/embedded"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/embeddednzbget"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/event"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/httproutes"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/migrate"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/nzbget"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/nzbking"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/reconciler"
	pluginrt "github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/runtime"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/scheduler"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/server"
	"github.com/ContinuumApp/continuum-plugin-audiobook-requests/internal/store"
)

// cookieStore bridges consumer.CookieStore to the plugin's app_config so
// a freshly minted abook session cookie survives restarts. The whole
// config blob is rewritten on every save; this helper does
// read-modify-write so it doesn't clobber unrelated fields.
type cookieStore struct{ st *store.Store }

func (c cookieStore) SaveAbookCookie(ctx context.Context, cookie string) error {
	cfg, err := c.st.GetAppConfig(ctx)
	if err != nil {
		return err
	}
	cfg.AbookCookie = cookie
	return c.st.UpdateAppConfig(ctx, cfg)
}

//go:embed manifest.json
var manifestRaw []byte

func main() {
	logger := hclog.New(&hclog.LoggerOptions{Name: "continuum-plugin-audiobook-requests"})

	manifest, err := loadManifest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load manifest: %v\n", err)
		os.Exit(1)
	}

	httpSrv := httproutes.NewServer()
	httpSrv.SetHandler(server.New(server.Deps{}).Handler())

	var (
		poolPtr           atomic.Pointer[pgxpool.Pool]
		embeddedPtr       atomic.Pointer[embedded.Manager]
		embeddedNZBGetPtr atomic.Pointer[embeddednzbget.Manager]
		consumerDepsP     atomic.Pointer[consumer.Deps]
		reconcilerPtr     atomic.Pointer[reconciler.Reconciler]
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
		appCfg, err := st.ImportLegacyAppConfig(ctx, cfg)
		if err != nil {
			p.Close()
			return fmt.Errorf("import app config: %w", err)
		}
		appCfg.DatabaseURL = cfg.DatabaseURL
		cfg = appCfg

		var embeddedManager *embedded.Manager
		if cfg.ProviderConfigured() && cfg.DownloadMode == "embedded" {
			embeddedManager, err = embedded.New(embedded.Config{
				DownloadDir:   cfg.EmbeddedDownloadDir,
				ListenPort:    cfg.EmbeddedListenPort,
				MaxConcurrent: cfg.EmbeddedMaxConcurrent,
			})
			if err != nil {
				p.Close()
				return err
			}
		}
		var abbClient *audiobookbay.Client
		if cfg.ProviderConfigured() {
			abbClient = audiobookbay.NewClient(audiobookbay.Config{
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
		}
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

		ev := event.New(sdkruntime.Host(), logger.Named("event"))

		// Spin up the embedded NZBGet supervisor when the operator
		// asked for download_mode=embedded_nzbget. Started before the
		// nzbget RPC client below so we can overwrite the operator-
		// supplied creds with the supervised daemon's loopback URL +
		// generated credentials — the abook+nzbking path then talks
		// to our local daemon instead of an external one.
		var nzbgetSupervisor *embeddednzbget.Manager
		if cfg.DownloadMode == "embedded_nzbget" && cfg.EmbeddedNZBGetConfigured() {
			sup, sErr := embeddednzbget.New(embeddednzbget.Config{
				DownloadDir: cfg.EmbeddedDownloadDir,
				Provider: embeddednzbget.NewsProvider{
					Host:        cfg.UsenetHost,
					Port:        cfg.UsenetPort,
					SSL:         cfg.UsenetSSL,
					Username:    cfg.UsenetUsername,
					Password:    cfg.UsenetPassword,
					Connections: cfg.UsenetConnections,
				},
				Logger: logger.Named("nzbget"),
			})
			if sErr != nil {
				logger.Warn("embedded nzbget init failed; falling back to external nzbget config", "err", sErr)
			} else {
				startCtx, startCancel := context.WithTimeout(ctx, 45*time.Second)
				if sErr := sup.Start(startCtx); sErr != nil {
					startCancel()
					logger.Warn("embedded nzbget failed to start; falling back to external nzbget config", "err", sErr)
				} else {
					startCancel()
					nzbgetSupervisor = sup
					user, pass := sup.Credentials()
					cfg.NZBGetURL = fmt.Sprintf("http://127.0.0.1:%d", sup.Port())
					cfg.NZBGetUsername = user
					cfg.NZBGetPassword = pass
					if cfg.NZBGetCategory == "" {
						cfg.NZBGetCategory = "audiobooks"
					}
					logger.Info("embedded nzbget online", "port", sup.Port(), "bundle_version", embeddednzbget.Version())
				}
			}
		}

		// Build the abook + nzbking + nzbget trio when the operator has
		// configured both ends. Either end alone is useless — abook
		// produces NZB search codes that only nzbking can resolve; nzbking
		// gives us NZB URLs that only NZBGet can fetch.
		var (
			abookClient *abook.Client
			nzbkingCli  *nzbking.Client
			nzbgetCli   *nzbget.Client
		)
		if cfg.AbookConfigured() {
			abookBase := cfg.AbookBaseURL
			if abookBase == "" {
				abookBase = "https://abook.link/book"
			}
			abookClient, err = abook.New(abookBase)
			if err != nil {
				logger.Warn("abook client init", "err", err)
			} else if cfg.AbookCookie != "" {
				// Rehydrate from a previously saved session cookie so we
				// don't burn a fresh login on every restart.
				if err := abookClient.SetCookieHeader(cfg.AbookCookie); err != nil {
					logger.Warn("abook restore cookie", "err", err)
				}
			}
			nzbkingCli = nzbking.New()
			nzbgetCli, err = nzbget.New(cfg.NZBGetURL, cfg.NZBGetUsername, cfg.NZBGetPassword)
			if err != nil {
				logger.Warn("nzbget client init", "err", err)
				nzbgetCli = nil
				abookClient = nil
				nzbkingCli = nil
			}
		}

		var rc *reconciler.Reconciler
		if abbClient != nil {
			consumerDepsP.Store(&consumer.Deps{
				Store: st, Pub: ev, ABB: abbClient,
				PluginID:       "continuum.audiobook-requests",
				Abook:          abookClient,
				Nzbking:        nzbkingCli,
				Nzbget:         nzbgetCli,
				NZBGetCategory: cfg.NZBGetCategory,
				Cookies:        cookieStore{st: st},
				AbookEmail:     cfg.AbookEmail,
				AbookPassword:  cfg.AbookPassword,
			})
			rc = reconciler.New(reconciler.Deps{
				Store: st, Pub: ev, ABB: abbClient,
				Nzbget:   nzbgetCli,
				PluginID: "continuum.audiobook-requests",
			})
			reconcilerPtr.Store(rc)
		} else {
			consumerDepsP.Store(nil)
			reconcilerPtr.Store(nil)
		}

		srv := server.New(server.Deps{
			AudiobookBayClient: abbClient,
			Store:              st,
			Reconciler:         rc,
			Config:             cfg,
			Abook:              abookClient,
			Nzbget:             nzbgetCli,
		})
		httpSrv.SetHandler(srv.Handler())

		if old := embeddedPtr.Swap(embeddedManager); old != nil {
			old.Close()
		}
		if old := embeddedNZBGetPtr.Swap(nzbgetSupervisor); old != nil {
			// Stopping the old supervisor after Swap (rather than
			// before) means the new daemon is already serving when
			// the old one goes away — no window during which the
			// consumer would see "nzbget unavailable".
			_ = old.Stop()
		}
		if old := poolPtr.Swap(p); old != nil {
			old.Close()
		}
		logger.Info("configured", "base_url", cfg.BaseURL, "download_mode", cfg.DownloadMode)
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
