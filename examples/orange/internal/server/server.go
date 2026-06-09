package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c" //nolint:staticcheck

	apikeyconnect "github.com/dio/transit/examples/orange/api/orange/apikey/admin/v1/adminv1connect"
	configadminconnect "github.com/dio/transit/examples/orange/api/orange/config/admin/v1/adminv1connect"
	configv1connect "github.com/dio/transit/examples/orange/api/orange/config/v1/configv1connect"
	egressconnect "github.com/dio/transit/examples/orange/api/orange/egress/admin/v1/adminv1connect"
	egressv1connect "github.com/dio/transit/examples/orange/api/orange/egress/v1/egressv1connect"
	keyconnect "github.com/dio/transit/examples/orange/api/orange/keyentry/admin/v1/adminv1connect"
	orgconnect "github.com/dio/transit/examples/orange/api/orange/org/admin/v1/adminv1connect"
	policyconnect "github.com/dio/transit/examples/orange/api/orange/policy/admin/v1/adminv1connect"
	profileconnect "github.com/dio/transit/examples/orange/api/orange/profile/admin/v1/adminv1connect"
	projectconnect "github.com/dio/transit/examples/orange/api/orange/project/admin/v1/adminv1connect"
	secretconnect "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1/adminv1connect"
	secretv1connect "github.com/dio/transit/examples/orange/api/orange/secret/v1/secretv1connect"
	userconnect "github.com/dio/transit/examples/orange/api/orange/user/admin/v1/adminv1connect"
	workspaceconnect "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1/adminv1connect"
	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/embeddedpg"
	"github.com/dio/transit/examples/orange/internal/server/apikeys"
	"github.com/dio/transit/examples/orange/internal/server/egressauth"
	"github.com/dio/transit/examples/orange/internal/server/resources"
	"github.com/dio/transit/examples/orange/internal/server/secret"
	"github.com/dio/transit/examples/orange/internal/server/secret/crypto"
	"github.com/dio/transit/examples/orange/internal/server/secret/kms"
	secretstore "github.com/dio/transit/examples/orange/internal/server/secret/store"
	"github.com/dio/transit/examples/orange/internal/vtprotocodec"
)

func newServerCmd() *cobra.Command {
	var (
		localMode  bool
		purge      bool
		noSeed     bool
		configPath string
		org        string
		project    string
		user       string
		port       string
		publicURL  string
	)

	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the orange management plane server",
		Long: `Starts the orange management plane HTTP/2 server.

In --local mode the server bootstraps itself from orange.yaml on first start,
enters an admin REPL, and shuts down when the REPL exits:

  orange server --local            # start / re-attach to existing data
  orange server --local --purge    # wipe ~/.orange/ and re-bootstrap
  orange server --local --purge --no-seed  # wipe and start with empty DB

For production or manual bootstrap, use:

  orange bootstrap --local --org=acme --include=proj,ws
  orange server --local`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			if purge && !localMode {
				return fmt.Errorf("--purge is only valid with --local")
			}
			return runServer(cmd.Context(), serverCfg{
				local:      localMode,
				purge:      purge,
				noSeed:     noSeed,
				configPath: configPath,
				org:        org,
				project:    project,
				user:       user,
				port:       port,
				publicURL:  publicURL,
				logger:     logger,
			})
		},
	}

	cmd.Flags().BoolVar(&localMode, "local", false, "use embedded Postgres and auto-generate KEK in ~/.orange/")
	cmd.Flags().BoolVar(&purge, "purge", false, "purge ~/.orange/data and KEK before starting (requires --local)")
	cmd.Flags().BoolVar(&noSeed, "no-seed", false, "skip auto-bootstrap after --purge (start with empty DB)")
	cmd.Flags().StringVar(&configPath, "config", envOr("ORANGE_CONFIG", "orange.yaml"), "bootstrap config file read for workspace/key names (--local mode; env: ORANGE_CONFIG)")
	cmd.Flags().StringVar(&org, "org", "orange.io", "org name for local bootstrap")
	cmd.Flags().StringVar(&project, "project", "proj1", "project name for local bootstrap")
	cmd.Flags().StringVar(&user, "user", "dio", "initial user name for local bootstrap")
	cmd.Flags().StringVar(&port, "port", envOr("PORT", "3000"), "listen port")
	cmd.Flags().StringVar(&publicURL, "public-url", envOr("ORANGE_PUBLIC_URL", ""), "public URL written into egress bundles (env: ORANGE_PUBLIC_URL; default: http://localhost:<port>)")

	return cmd
}

type serverCfg struct {
	local  bool
	purge  bool
	noSeed bool
	configPath string
	org        string
	project    string
	user       string
	port       string
	publicURL  string
	logger     *slog.Logger
}

func runServer(parent context.Context, cfg serverCfg) error {
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	listenAddr := cfg.port
	if listenAddr[0] != ':' {
		listenAddr = ":" + listenAddr
	}

	// Bind the listen socket before any setup work. This gives an immediate
	// "address already in use" error when a stale server still holds the port,
	// preventing silent fall-through to waitForServer on the stale process.
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}

	// ── Local mode: purge before starting embedded PG ─────────────────────────
	if cfg.local && cfg.purge {
		if err := purgeLocalData(cfg.logger); err != nil {
			return fmt.Errorf("purge: %w", err)
		}
	}

	// ── KEK resolution ────────────────────────────────────────────────────────

	masterKEKURI := os.Getenv("MASTER_KEK_URI")
	if cfg.local {
		uri, err := resolveLocalKEK(cfg.logger)
		if err != nil {
			return fmt.Errorf("local KEK: %w", err)
		}
		masterKEKURI = uri
	}
	if masterKEKURI == "" {
		return fmt.Errorf("MASTER_KEK_URI is required (or use --local for development)")
	}

	// ── Store DSN ─────────────────────────────────────────────────────────────

	storeDSN, cleanup, err := startEmbeddedOrExternal(ctx, cfg.local, cfg.logger)
	if err != nil {
		return fmt.Errorf("store backend: %w", err)
	}
	defer cleanup()

	pool, err := pgxpool.New(ctx, storeDSN)
	if err != nil {
		return fmt.Errorf("pgx pool: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pgx ping: %w", err)
	}

	// ── Secret service ────────────────────────────────────────────────────────

	st, err := secretstore.NewPGSecretStore(ctx, pool)
	if err != nil {
		return fmt.Errorf("init secret store: %w", err)
	}
	cfg.logger.Info("loading master kek", "uri_scheme", kms.Scheme(masterKEKURI))
	provider, err := kms.Load(ctx, masterKEKURI)
	if err != nil {
		return fmt.Errorf("init KMS: %w", err)
	}
	secretSvc, err := secret.New(ctx, secret.Config{
		Provider:  provider,
		Encryptor: crypto.New(),
		Store:     st,
		Logger:    cfg.logger.With("component", "secret"),
	})
	if err != nil {
		return fmt.Errorf("init secret service: %w", err)
	}

	// ── Resource services ─────────────────────────────────────────────────────

	orgSvc, err := resources.NewOrgService(pool, cfg.logger.With("component", "org"))
	if err != nil {
		return fmt.Errorf("init org service: %w", err)
	}
	projectSvc, err := resources.NewProjectService(pool, cfg.logger.With("component", "project"))
	if err != nil {
		return fmt.Errorf("init project service: %w", err)
	}
	workspaceSvc := resources.NewWorkspaceService(pool, cfg.logger.With("component", "workspace"))
	if err := workspaceSvc.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("init workspace schema: %w", err)
	}
	// ── API key store (initialized early; userSvc depends on it for scope binding)

	keyStore, err := apikeys.NewStore(ctx, pool)
	if err != nil {
		return fmt.Errorf("init api key store: %w", err)
	}

	userSvc := resources.NewUserService(pool, cfg.logger.With("component", "user"), keyStore)
	if err := userSvc.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("init user schema: %w", err)
	}
	keySvc, err := resources.NewKeyEntryService(pool, cfg.logger.With("component", "key"), secretSvc)
	if err != nil {
		return fmt.Errorf("init key service: %w", err)
	}
	serverURL := cfg.publicURL
	if serverURL == "" {
		serverURL = "http://localhost:" + cfg.port
	}
	egressSvc, err := resources.NewEgressService(pool, cfg.logger.With("component", "egress"), serverURL, secretSvc)
	if err != nil {
		return fmt.Errorf("init egress service: %w", err)
	}
	workspaceSvc.SetEgressService(egressSvc)
	heartbeatRegistry := resources.NewHeartbeatRegistry(pool, cfg.logger.With("component", "heartbeat"))
	policySvc, err := resources.NewPolicyService(pool, cfg.logger.With("component", "policy"))
	if err != nil {
		return fmt.Errorf("init policy service: %w", err)
	}
	profileSvc, err := resources.NewProfileService(pool, cfg.logger.With("component", "profile"))
	if err != nil {
		return fmt.Errorf("init profile service: %w", err)
	}

	// ── Config snapshot store + service ───────────────────────────────────────

	snapshotStore, err := config.NewPgSnapshotStore(ctx, pool)
	if err != nil {
		return fmt.Errorf("init snapshot store: %w", err)
	}
	configSvc := resources.NewConfigService(snapshotStore, cfg.logger.With("component", "config"))
	configSvc.SetHierarchyResolver(workspaceSvc)

	// ── HTTP mux ──────────────────────────────────────────────────────────────

	codecOpt := connect.WithCodec(vtprotocodec.Codec{})
	authOpt := connect.WithInterceptors(apikeys.Interceptor(keyStore))
	egressAuthStore := egressauth.NewStore(pool)
	configSvc.SetWorkspaceNameLookup(egressAuthStore.WorkspaceName)
	egressAuthOpt := connect.WithInterceptors(egressauth.Interceptor(egressAuthStore, egressAuthStore))
	opts := []connect.HandlerOption{codecOpt, authOpt}

	mux := http.NewServeMux()
	mux.Handle(secretconnect.NewSecretAdminServiceHandler(secretSvc, opts...))
	mux.Handle(orgconnect.NewOrgAdminServiceHandler(orgSvc, opts...))
	mux.Handle(projectconnect.NewProjectAdminServiceHandler(projectSvc, opts...))
	mux.Handle(workspaceconnect.NewWorkspaceAdminServiceHandler(workspaceSvc, opts...))
	mux.Handle(userconnect.NewUserAdminServiceHandler(userSvc, opts...))
	mux.Handle(keyconnect.NewKeyEntryAdminServiceHandler(keySvc, opts...))
	mux.Handle(egressconnect.NewEgressAdminServiceHandler(egressSvc, opts...))
	mux.Handle(policyconnect.NewPolicyAdminServiceHandler(policySvc, opts...))
	mux.Handle(profileconnect.NewProfileAdminServiceHandler(profileSvc, opts...))
	mux.Handle(apikeyconnect.NewAPIKeyAdminServiceHandler(apikeys.NewService(keyStore), opts...))
	// Config admin: management-plane, requires API key auth.
	mux.Handle(configadminconnect.NewConfigAdminServiceHandler(configSvc, opts...))
	// Snapshot fetch: data-plane facing, requires egress assertion auth.
	mux.Handle(configv1connect.NewSnapshotServiceHandler(configSvc, codecOpt, egressAuthOpt))
	// Secret resolver: data-plane facing, resolves orange:// secret refs at runtime.
	mux.Handle(secretv1connect.NewSecretResolverServiceHandler(secretSvc, codecOpt, egressAuthOpt))
	// Heartbeat is egress-facing: requires egress assertion auth.
	mux.Handle(egressv1connect.NewEgressServiceHandler(
		resources.NewHeartbeatService(heartbeatRegistry),
		codecOpt, egressAuthOpt,
	))

	var ready atomic.Bool
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           h2c.NewHandler(mux, &http2.Server{}), //nolint:staticcheck
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	heartbeatRegistry.Start()

	// ── Local mode: bootstrap from yaml, start server goroutine, enter REPL ───
	if cfg.local {
		cfg.logger.Info("server starting (local+REPL mode)", "addr", listenAddr)
		serverErrCh := make(chan error, 1)
		go func() {
			ready.Store(true)
			serverErrCh <- srv.Serve(ln)
		}()

		if err := waitForServer(ctx, serverURL); err != nil {
			return fmt.Errorf("server not ready: %w", err)
		}

		// Bootstrap if no orgs exist yet (first run or after --purge),
		// unless --no-seed was requested.
		has, err := apikeys.HasOrgs(ctx, pool)
		if err != nil {
			return err
		}
		if !has && !cfg.noSeed {
			svc := localBootstrapSvc{
				pool:         pool,
				keyStore:     keyStore,
				projectSvc:   projectSvc,
				workspaceSvc: workspaceSvc,
				userSvc:      userSvc,
				secretSvc:    secretSvc,
				configSvc:    configSvc,
			}
			result, err := bootstrapFromYAML(ctx, cfg, svc)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "# local dev server ready\n")
			fmt.Fprintf(os.Stdout, "# admin: %s\n", result.adminEmail)
			fmt.Fprintf(os.Stdout, "export ORANGE_API_KEY=%s\n", result.adminKey)
			if result.userKey != "" {
				fmt.Fprintf(os.Stdout, "# user:  %s\n", result.userEmail)
				fmt.Fprintf(os.Stdout, "export ORANGE_USER_API_KEY=%s\n", result.userKey)
			}
			rc := makeAdminRunCtx(serverURL, result.adminKey)
			_ = runREPL(rc, []string{"org=" + result.orgID, "proj=" + result.projID})
		} else if has {
			rc, err := resolveRunCtx()
			if err != nil {
				return fmt.Errorf("org data exists but no credentials found (set ORANGE_API_KEY or run 'orange auth login'): %w", err)
			}
			_ = runREPL(rc, nil)
		} else {
			// --no-seed with empty DB: server runs until SIGINT/SIGTERM.
			cfg.logger.Info("seeding skipped (--no-seed); server running, use 'orange bootstrap' to initialise")
			select {
			case <-ctx.Done():
			case err := <-serverErrCh:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					return fmt.Errorf("http server: %w", err)
				}
				return nil
			}
		}

		// REPL exited: shut down server.
		stop()
		ready.Store(false)
		heartbeatRegistry.Stop()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			cfg.logger.Error("graceful shutdown failed", "err", err)
		}
		if err := <-serverErrCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	}

	// ── Normal mode: blocking until signal ────────────────────────────────────
	cfg.logger.Info("server starting", "addr", listenAddr)
	errCh := make(chan error, 1)
	go func() {
		ready.Store(true)
		errCh <- srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		cfg.logger.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	}

	ready.Store(false)
	heartbeatRegistry.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		cfg.logger.Error("graceful shutdown failed", "err", err)
		return err
	}
	cfg.logger.Info("server stopped")
	return nil
}

// purgeLocalData removes ~/.orange/data/ and ~/.orange/kek.
func purgeLocalData(logger *slog.Logger) error {
	dir, err := OrangeDir()
	if err != nil {
		return err
	}
	for _, target := range []string{filepath.Join(dir, "data"), filepath.Join(dir, "kek")} {
		if err := os.RemoveAll(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("purge %s: %w", target, err)
		}
		logger.Info("purged", "path", target)
	}
	return nil
}

// resolveLocalKEK returns an env:// URI backed by a KEK persisted in ~/.orange/kek.
// The file holds a single base64url-encoded (no padding) 32-byte key.
// It is created with mode 0600 on first call.
func resolveLocalKEK(logger *slog.Logger) (string, error) {
	dir, err := OrangeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "kek")

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		encoded := base64.RawURLEncoding.EncodeToString(raw)
		if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
			return "", fmt.Errorf("write local KEK: %w", err)
		}
		logger.Info("generated local KEK", "path", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read local KEK: %w", err)
	}
	encoded := strings.TrimSpace(string(data))
	if err := os.Setenv("ORANGE_LOCAL_KEK", encoded); err != nil {
		return "", err
	}
	return "env://ORANGE_LOCAL_KEK", nil
}

// startEmbeddedOrExternal returns a Postgres DSN.
// In local mode it starts embedded Postgres with Root = ~/.orange/.
// Otherwise it reads STORE_DSN from the environment, falling back to embedded.
func startEmbeddedOrExternal(ctx context.Context, local bool, logger *slog.Logger) (dsn string, cleanup func(), err error) {
	if !local {
		if dsn = os.Getenv("STORE_DSN"); dsn != "" {
			return dsn, func() {}, nil
		}
	}
	return startEmbeddedPG(ctx, local, logger)
}

// startEmbeddedPG starts embedded Postgres. When local is true the data is
// persisted under ~/.orange/; otherwise a temporary directory is used.
func startEmbeddedPG(ctx context.Context, persistent bool, logger *slog.Logger) (dsn string, cleanup func(), err error) {
	pgCfg := embeddedpg.Config{}
	if persistent {
		dir, err := OrangeDir()
		if err != nil {
			return "", nil, err
		}
		pgCfg.Root = dir
	}
	logger.Info("starting embedded postgres", "persistent", persistent)
	inst, err := embeddedpg.Start(pgCfg)
	if err != nil {
		return "", nil, fmt.Errorf("embedded postgres: %w", err)
	}
	logger.Info("embedded postgres ready", "port", inst.Config().Port)
	return inst.DSN(), func() {
		if err := inst.Stop(); err != nil {
			logger.Warn("embedded postgres stop failed", "err", err)
		}
	}, nil
}
