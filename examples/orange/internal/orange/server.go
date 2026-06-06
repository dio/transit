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
		localMode     bool
		bootstrapOrg  string
		bootstrapEmail string
		port          string
	)

	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the orange management plane server",
		Long: `Starts the orange management plane HTTP/2 server.

Local development (embedded Postgres, auto-generated KEK):

  orange server --local --bootstrap=acme

The first admin API key is printed once to stderr on bootstrap.
Use it with: export ORANGE_API_KEY=sk-org-...`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			return runServer(cmd.Context(), serverCfg{
				local:          localMode,
				bootstrapOrg:   bootstrapOrg,
				bootstrapEmail: bootstrapEmail,
				port:           port,
				logger:         logger,
			})
		},
	}

	cmd.Flags().BoolVar(&localMode, "local", false, "use embedded Postgres and auto-generate KEK in ~/.orange/")
	cmd.Flags().StringVar(&bootstrapOrg, "bootstrap", envOr("ORANGE_BOOTSTRAP_ORG", ""), "create first org on empty database (org name)")
	cmd.Flags().StringVar(&bootstrapEmail, "bootstrap-email", envOr("ORANGE_BOOTSTRAP_EMAIL", ""), "admin email for bootstrap (default: admin@<org>)")
	cmd.Flags().StringVar(&port, "port", envOr("PORT", "8080"), "listen port")

	return cmd
}

type serverCfg struct {
	local          bool
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

	storeDSN, cleanup, err := resolveStoreDSN(ctx, cfg.local, cfg.logger)
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

	// ── API key store + bootstrap ─────────────────────────────────────────────

	keyStore, err := apikeys.NewStore(ctx, pool)
	if err != nil {
		return fmt.Errorf("init api key store: %w", err)
	}
	if err := serverBootstrap(ctx, pool, keyStore, cfg); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
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
// database is empty and --bootstrap (or ORANGE_BOOTSTRAP_ORG) is set.
func serverBootstrap(ctx context.Context, pool *pgxpool.Pool, store *apikeys.Store, cfg serverCfg) error {
	if cfg.bootstrapOrg == "" {
		return nil
	}
	has, err := apikeys.HasOrgs(ctx, pool)
	if err != nil {
		return err
	}
	if has {
		cfg.logger.Info("bootstrap: orgs already exist, skipping")
		return nil
	}

	email := cfg.bootstrapEmail
	if email == "" {
		email = "admin@" + cfg.bootstrapOrg
	}
	cfg.logger.Info("bootstrap: creating first org and admin user", "org", cfg.bootstrapOrg, "email", email)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	now := time.Now().UTC()
	orgID := uuid.Must(uuid.NewV7()).String()
	userID := uuid.Must(uuid.NewV7()).String()

	if _, err = tx.Exec(ctx,
		`INSERT INTO orgs (org_id, name, created_at, updated_at) VALUES ($1, $2, $3, $3)`,
		orgID, cfg.bootstrapOrg, now,
	); err != nil {
		return fmt.Errorf("create bootstrap org: %w", err)
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO users (user_id, org_id, email, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)`,
		userID, orgID, email, now,
	); err != nil {
		return fmt.Errorf("create bootstrap user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	plaintext, rec, err := store.Issue(ctx, orgID, userID, "", []string{apikeys.ScopeAdmin}, "bootstrap admin key")
	if err != nil {
		return fmt.Errorf("issue bootstrap key: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\n"+
		"╔══════════════════════════════════════════════════════════╗\n"+
		"║              ORANGE BOOTSTRAP COMPLETE                  ║\n"+
		"╠══════════════════════════════════════════════════════════╣\n"+
		"║  org:     %-46s ║\n"+
		"║  org_id:  %-46s ║\n"+
		"║  email:   %-46s ║\n"+
		"║  user_id: %-46s ║\n"+
		"║  key_id:  %-46s ║\n"+
		"║                                                          ║\n"+
		"║  ADMIN API KEY (shown only once — save it now):         ║\n"+
		"║  %-56s ║\n"+
		"╚══════════════════════════════════════════════════════════╝\n\n"+
		"  export ORANGE_API_KEY=%s\n"+
		"  orange auth login --org %s --user %s\n\n",
		cfg.bootstrapOrg, orgID, email, userID, rec.KeyID, plaintext,
		plaintext, cfg.bootstrapOrg, email,
	)
	cfg.logger.Info("bootstrap complete", "org_id", orgID, "user_id", userID, "key_id", rec.KeyID)
	return nil
}

// resolveLocalKEK returns an env:// URI backed by a KEK persisted in ~/.orange/kek.
// The file contains a single base64url-encoded 32-byte key (no padding).
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

// resolveStoreDSN returns the Postgres DSN.
// In local mode it starts embedded Postgres with data in ~/.orange/data/.
// Otherwise it reads STORE_DSN from the environment.
func resolveStoreDSN(ctx context.Context, local bool, logger *slog.Logger) (dsn string, cleanup func(), err error) {
	if !local {
		if dsn = os.Getenv("STORE_DSN"); dsn != "" {
			return dsn, func() {}, nil
		}
	}

	var dataDir string
	if local {
		dir, err := OrangeDir()
		if err != nil {
			return "", nil, err
		}
		dataDir = filepath.Join(dir, "data")
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return "", nil, err
		}
	}

	logger.Info("starting embedded postgres", "data_dir", dataDir)
	inst, err := embeddedpg.Start(embeddedpg.Config{})
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
