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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	egressconnect "github.com/dio/transit/examples/orange/api/orange/egress/admin/v1/adminv1connect"
	keyconnect "github.com/dio/transit/examples/orange/api/orange/key/admin/v1/adminv1connect"
	orgconnect "github.com/dio/transit/examples/orange/api/orange/org/admin/v1/adminv1connect"
	policyconnect "github.com/dio/transit/examples/orange/api/orange/policy/admin/v1/adminv1connect"
	profileconnect "github.com/dio/transit/examples/orange/api/orange/profile/admin/v1/adminv1connect"
	projectconnect "github.com/dio/transit/examples/orange/api/orange/project/admin/v1/adminv1connect"
	secretconnect "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1/adminv1connect"
	userconnect "github.com/dio/transit/examples/orange/api/orange/user/admin/v1/adminv1connect"
	workspaceconnect "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1/adminv1connect"
	"github.com/dio/transit/examples/orange/internal/embeddedpg"
	"github.com/dio/transit/examples/orange/internal/orange/apikeys"
	"github.com/dio/transit/examples/orange/internal/resources"
	"github.com/dio/transit/examples/orange/internal/secret"
	"github.com/dio/transit/examples/orange/internal/secret/crypto"
	"github.com/dio/transit/examples/orange/internal/secret/kms"
	secretstore "github.com/dio/transit/examples/orange/internal/secret/store"
	"github.com/dio/transit/examples/orange/internal/vtprotocodec"
)

func newServerCmd() *cobra.Command {
	var (
		localMode      bool
		purge          bool
		bootstrapOrg   string
		bootstrapEmail string
		port           string
	)

	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the orange management plane server",
		Long: `Starts the orange management plane HTTP/2 server.

Bootstrap workflow (run once on a fresh database):

  orange server --local --bootstrap=acme
  # prints: export ORANGE_API_KEY=sk-org-...
  # then exits

Start the server after bootstrap:

  orange server --local

Reset local data and re-bootstrap:

  orange server --local --purge --bootstrap=acme`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if purge && !localMode {
				return fmt.Errorf("--purge is only available with --local")
			}
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			return runServer(cmd.Context(), serverCfg{
				local:          localMode,
				purge:          purge,
				bootstrapOrg:   bootstrapOrg,
				bootstrapEmail: bootstrapEmail,
				port:           port,
				logger:         logger,
			})
		},
	}

	cmd.Flags().BoolVar(&localMode, "local", false, "use embedded Postgres and auto-generate KEK in ~/.orange/")
	cmd.Flags().BoolVar(&purge, "purge", false, "remove ~/.orange/data/ and ~/.orange/kek before starting (requires --local)")
	cmd.Flags().StringVar(&bootstrapOrg, "bootstrap", envOr("ORANGE_BOOTSTRAP_ORG", ""), "bootstrap first org then exit (org name)")
	cmd.Flags().StringVar(&bootstrapEmail, "bootstrap-email", envOr("ORANGE_BOOTSTRAP_EMAIL", ""), "admin email for bootstrap (default: admin@<org>)")
	cmd.Flags().StringVar(&port, "port", envOr("PORT", "8080"), "listen port")

	return cmd
}

type serverCfg struct {
	local          bool
	purge          bool
	bootstrapOrg   string
	bootstrapEmail string
	port           string
	logger         *slog.Logger
}

func runServer(parent context.Context, cfg serverCfg) error {
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	listenAddr := cfg.port
	if listenAddr[0] != ':' {
		listenAddr = ":" + listenAddr
	}

	// ── Optional purge ────────────────────────────────────────────────────────

	if cfg.purge {
		if err := purgeLocalData(cfg.logger); err != nil {
			return err
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
	userSvc := resources.NewUserService(pool, cfg.logger.With("component", "user"))
	if err := userSvc.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("init user schema: %w", err)
	}
	keySvc, err := resources.NewKeyService(pool, cfg.logger.With("component", "key"))
	if err != nil {
		return fmt.Errorf("init key service: %w", err)
	}
	egressSvc, err := resources.NewEgressService(pool, cfg.logger.With("component", "egress"))
	if err != nil {
		return fmt.Errorf("init egress service: %w", err)
	}
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

	// ── Bootstrap (exit immediately if it ran) ────────────────────────────────

	bootstrapped, err := serverBootstrap(ctx, pool, keyStore, cfg)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if bootstrapped {
		return nil // printed key; caller should re-run without --bootstrap
	}

	// ── HTTP mux ──────────────────────────────────────────────────────────────

	codecOpt := connect.WithCodec(vtprotocodec.Codec{})
	authOpt := connect.WithInterceptors(apikeys.Interceptor(keyStore))
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
		Handler:           h2c.NewHandler(mux, &http2.Server{}),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		cfg.logger.Error("graceful shutdown failed", "err", err)
		return err
	}
	cfg.logger.Info("server stopped")
	return nil
}

// serverBootstrap creates the first org + user + admin API key when the
// database is empty and --bootstrap is set.
// Returns true if bootstrap ran (caller should exit), false if skipped.
func serverBootstrap(ctx context.Context, pool *pgxpool.Pool, store *apikeys.Store, cfg serverCfg) (bool, error) {
	if cfg.bootstrapOrg == "" {
		return false, nil
	}
	has, err := apikeys.HasOrgs(ctx, pool)
	if err != nil {
		return false, err
	}
	if has {
		cfg.logger.Info("bootstrap: orgs already exist, skipping")
		return false, nil
	}

	email := cfg.bootstrapEmail
	if email == "" {
		email = "admin@" + cfg.bootstrapOrg
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	now := time.Now().UTC()
	orgID := uuid.Must(uuid.NewV7()).String()
	userID := uuid.Must(uuid.NewV7()).String()

	if _, err = tx.Exec(ctx,
		`INSERT INTO orgs (org_id, name, created_at, updated_at) VALUES ($1, $2, $3, $3)`,
		orgID, cfg.bootstrapOrg, now,
	); err != nil {
		return false, fmt.Errorf("create bootstrap org: %w", err)
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO users (user_id, org_id, email, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)`,
		userID, orgID, email, now,
	); err != nil {
		return false, fmt.Errorf("create bootstrap user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}

	plaintext, _, err := store.Issue(ctx, orgID, userID, "", []string{apikeys.ScopeAdmin}, "bootstrap admin key")
	if err != nil {
		return false, fmt.Errorf("issue bootstrap key: %w", err)
	}

	// Print to stdout so the export line can be eval'd by the shell.
	fmt.Fprintf(os.Stdout, "# org: %s  user: %s\nexport ORANGE_API_KEY=%s\n",
		cfg.bootstrapOrg, email, plaintext)

	cfg.logger.Info("bootstrap complete — server will exit; re-run without --bootstrap to start",
		"org_id", orgID, "user_id", userID)
	return true, nil
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
