package orange

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
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
	keyconnect "github.com/dio/transit/examples/orange/api/orange/key/admin/v1/adminv1connect"
	orgconnect "github.com/dio/transit/examples/orange/api/orange/org/admin/v1/adminv1connect"
	policyconnect "github.com/dio/transit/examples/orange/api/orange/policy/admin/v1/adminv1connect"
	profileconnect "github.com/dio/transit/examples/orange/api/orange/profile/admin/v1/adminv1connect"
	projectconnect "github.com/dio/transit/examples/orange/api/orange/project/admin/v1/adminv1connect"
	secretconnect "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1/adminv1connect"
	userconnect "github.com/dio/transit/examples/orange/api/orange/user/admin/v1/adminv1connect"
	workspaceconnect "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1/adminv1connect"
	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/embeddedpg"
	"github.com/dio/transit/examples/orange/internal/orange/apikeys"
	"github.com/dio/transit/examples/orange/internal/orange/egressauth"
	"github.com/dio/transit/examples/orange/internal/resources"
	"github.com/dio/transit/examples/orange/internal/secret"
	"github.com/dio/transit/examples/orange/internal/secret/crypto"
	"github.com/dio/transit/examples/orange/internal/secret/kms"
	secretstore "github.com/dio/transit/examples/orange/internal/secret/store"
	"github.com/dio/transit/examples/orange/internal/vtprotocodec"
)

func newServerCmd() *cobra.Command {
	var (
		localMode bool
		port      string
		publicURL string
	)

	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the orange management plane server",
		Long: `Starts the orange management plane HTTP/2 server.

Bootstrap a fresh database first:

  orange bootstrap --local --org=acme
  orange bootstrap --local --org=acme --include=proj,ws

Then start the server:

  orange server --local`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			return runServer(cmd.Context(), serverCfg{
				local:     localMode,
				port:      port,
				publicURL: publicURL,
				logger:    logger,
			})
		},
	}

	cmd.Flags().BoolVar(&localMode, "local", false, "use embedded Postgres and auto-generate KEK in ~/.orange/")
	cmd.Flags().StringVar(&port, "port", envOr("PORT", "8080"), "listen port")
	cmd.Flags().StringVar(&publicURL, "public-url", envOr("ORANGE_PUBLIC_URL", ""), "public URL written into egress bundles (env: ORANGE_PUBLIC_URL; default: http://localhost:<port>)")

	return cmd
}

type serverCfg struct {
	local     bool
	port      string
	publicURL string
	logger    *slog.Logger
}

func runServer(parent context.Context, cfg serverCfg) error {
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	listenAddr := cfg.port
	if listenAddr[0] != ':' {
		listenAddr = ":" + listenAddr
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
	userSvc := resources.NewUserService(pool, cfg.logger.With("component", "user"))
	if err := userSvc.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("init user schema: %w", err)
	}
	keySvc, err := resources.NewKeyService(pool, cfg.logger.With("component", "key"))
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

	// ── API key store ─────────────────────────────────────────────────────────

	keyStore, err := apikeys.NewStore(ctx, pool)
	if err != nil {
		return fmt.Errorf("init api key store: %w", err)
	}

	// ── Config snapshot store + service ───────────────────────────────────────

	snapshotStore, err := config.NewPgSnapshotStore(ctx, pool)
	if err != nil {
		return fmt.Errorf("init snapshot store: %w", err)
	}
	configSvc := resources.NewConfigService(snapshotStore, cfg.logger.With("component", "config"))

	// ── HTTP mux ──────────────────────────────────────────────────────────────

	codecOpt := connect.WithCodec(vtprotocodec.Codec{})
	authOpt := connect.WithInterceptors(apikeys.Interceptor(keyStore))
	egressAuthStore := egressauth.NewStore(pool)
	egressAuthOpt := connect.WithInterceptors(egressauth.Interceptor(egressAuthStore))
	opts := []connect.HandlerOption{codecOpt, authOpt}

	mux := http.NewServeMux()
	mux.Handle(secretconnect.NewSecretAdminServiceHandler(secretSvc, opts...))
	mux.Handle(orgconnect.NewOrgAdminServiceHandler(orgSvc, opts...))
	mux.Handle(projectconnect.NewProjectAdminServiceHandler(projectSvc, opts...))
	mux.Handle(workspaceconnect.NewWorkspaceAdminServiceHandler(workspaceSvc, opts...))
	mux.Handle(userconnect.NewUserAdminServiceHandler(userSvc, opts...))
	mux.Handle(keyconnect.NewKeyAdminServiceHandler(keySvc, opts...))
	mux.Handle(egressconnect.NewEgressAdminServiceHandler(egressSvc, opts...))
	mux.Handle(policyconnect.NewPolicyAdminServiceHandler(policySvc, opts...))
	mux.Handle(profileconnect.NewProfileAdminServiceHandler(profileSvc, opts...))
	mux.Handle(apikeyconnect.NewAPIKeyAdminServiceHandler(apikeys.NewService(keyStore), opts...))
	// Config admin: management-plane, requires API key auth.
	mux.Handle(configadminconnect.NewConfigAdminServiceHandler(configSvc, opts...))
	// Snapshot fetch: data-plane facing, requires egress assertion auth.
	mux.Handle(configv1connect.NewSnapshotServiceHandler(configSvc, codecOpt, egressAuthOpt))
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

	cfg.logger.Info("server starting", "addr", listenAddr)
	errCh := make(chan error, 1)
	go func() {
		ready.Store(true)
		errCh <- srv.ListenAndServe()
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
