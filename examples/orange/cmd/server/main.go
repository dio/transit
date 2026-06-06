package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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
	"github.com/dio/transit/examples/orange/internal/adminauth"
	"github.com/dio/transit/examples/orange/internal/embeddedpg"
	"github.com/dio/transit/examples/orange/internal/resources"
	"github.com/dio/transit/examples/orange/internal/secret"
	"github.com/dio/transit/examples/orange/internal/secret/crypto"
	"github.com/dio/transit/examples/orange/internal/secret/kms"
	secretstore "github.com/dio/transit/examples/orange/internal/secret/store"
	"github.com/dio/transit/examples/orange/internal/vtprotocodec"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("server fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	listenAddr := envOr("PORT", "8080")
	if listenAddr[0] != ':' {
		listenAddr = ":" + listenAddr
	}

	storeDSN, cleanup, err := resolveStoreDSN(ctx, logger)
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

	masterKEKURI := os.Getenv("MASTER_KEK_URI")
	if masterKEKURI == "" {
		return fmt.Errorf("MASTER_KEK_URI is required (e.g. env://MASTER_KEK_B64 or file:///path/to/key)")
	}
	logger.Info("loading master kek", "uri_scheme", kms.Scheme(masterKEKURI))
	provider, err := kms.Load(ctx, masterKEKURI)
	if err != nil {
		return fmt.Errorf("init KMS: %w", err)
	}

	secretSvc, err := secret.New(ctx, secret.Config{
		Provider:  provider,
		Encryptor: crypto.New(),
		Store:     st,
		Logger:    logger.With("component", "secret"),
	})
	if err != nil {
		return fmt.Errorf("init secret service: %w", err)
	}

	// ── Resource services ─────────────────────────────────────────────────────

	orgSvc, err := resources.NewOrgService(pool, logger.With("component", "org"))
	if err != nil {
		return fmt.Errorf("init org service: %w", err)
	}

	projectSvc, err := resources.NewProjectService(pool, logger.With("component", "project"))
	if err != nil {
		return fmt.Errorf("init project service: %w", err)
	}

	workspaceSvc := resources.NewWorkspaceService(pool, logger.With("component", "workspace"))
	if err := workspaceSvc.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("init workspace schema: %w", err)
	}

	userSvc := resources.NewUserService(pool, logger.With("component", "user"))
	if err := userSvc.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("init user schema: %w", err)
	}

	keySvc, err := resources.NewKeyService(pool, logger.With("component", "key"))
	if err != nil {
		return fmt.Errorf("init key service: %w", err)
	}

	egressSvc, err := resources.NewEgressService(pool, logger.With("component", "egress"))
	if err != nil {
		return fmt.Errorf("init egress service: %w", err)
	}

	policySvc, err := resources.NewPolicyService(pool, logger.With("component", "policy"))
	if err != nil {
		return fmt.Errorf("init policy service: %w", err)
	}

	profileSvc, err := resources.NewProfileService(pool, logger.With("component", "profile"))
	if err != nil {
		return fmt.Errorf("init profile service: %w", err)
	}

	// ── Admin auth ────────────────────────────────────────────────────────────

	authStore, err := adminauth.NewStore(ctx, pool)
	if err != nil {
		return fmt.Errorf("init admin auth store: %w", err)
	}

	if err := bootstrap(ctx, pool, authStore, logger); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	// ── HTTP mux ──────────────────────────────────────────────────────────────

	codecOpt := connect.WithCodec(vtprotocodec.Codec{})
	authInterceptor := connect.WithInterceptors(adminauth.Interceptor(authStore))
	opts := []connect.HandlerOption{codecOpt, authInterceptor}

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

	logger.Info("server starting", "addr", listenAddr)

	errCh := make(chan error, 1)
	go func() {
		ready.Store(true)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
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
		logger.Error("graceful shutdown failed", "err", err)
		return err
	}
	logger.Info("server stopped")
	return nil
}

// bootstrap creates the first org + user + admin API key when
// ORANGE_BOOTSTRAP_ORG and ORANGE_BOOTSTRAP_EMAIL are set and no orgs exist.
// The generated API key is written once to stderr; it is never stored in plaintext.
func bootstrap(ctx context.Context, pool *pgxpool.Pool, store *adminauth.Store, logger *slog.Logger) error {
	org := os.Getenv("ORANGE_BOOTSTRAP_ORG")
	email := os.Getenv("ORANGE_BOOTSTRAP_EMAIL")
	if org == "" || email == "" {
		return nil
	}

	has, err := adminauth.HasOrgs(ctx, pool)
	if err != nil {
		return err
	}
	if has {
		logger.Info("bootstrap: orgs already exist, skipping")
		return nil
	}

	logger.Info("bootstrap: creating first org and admin user", "org", org, "email", email)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	now := time.Now().UTC()
	orgID := uuid.Must(uuid.NewV7()).String()
	userID := uuid.Must(uuid.NewV7()).String()

	// Insert org.
	if _, err = tx.Exec(ctx,
		`INSERT INTO orgs (org_id, name, created_at, updated_at) VALUES ($1, $2, $3, $3)`,
		orgID, org, now,
	); err != nil {
		return fmt.Errorf("create bootstrap org: %w", err)
	}

	// Insert user.
	if _, err = tx.Exec(ctx,
		`INSERT INTO users (user_id, org_id, email, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)`,
		userID, orgID, email, now,
	); err != nil {
		return fmt.Errorf("create bootstrap user: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	// Issue admin API key outside the transaction (Store uses the pool).
	plaintext, rec, err := store.Issue(ctx, orgID, userID, []string{"admin"})
	if err != nil {
		return fmt.Errorf("issue bootstrap admin key: %w", err)
	}

	// Print the key once; operator must save it.
	fmt.Fprintf(os.Stderr, "\n"+
		"╔══════════════════════════════════════════════════════════╗\n"+
		"║              ORANGE BOOTSTRAP COMPLETE                  ║\n"+
		"╠══════════════════════════════════════════════════════════╣\n"+
		"║  org_id  : %-44s ║\n"+
		"║  user_id : %-44s ║\n"+
		"║  key_id  : %-44s ║\n"+
		"║                                                          ║\n"+
		"║  ADMIN API KEY (save this — shown only once):           ║\n"+
		"║  %-56s ║\n"+
		"╚══════════════════════════════════════════════════════════╝\n\n",
		orgID, userID, rec.KeyID, plaintext,
	)
	logger.Info("bootstrap complete", "org_id", orgID, "user_id", userID, "key_id", rec.KeyID)
	return nil
}

// resolveStoreDSN returns the PostgreSQL DSN, starting an embedded postgres
// when STORE_DSN is not set.
func resolveStoreDSN(ctx context.Context, logger *slog.Logger) (dsn string, cleanup func(), err error) {
	if dsn = os.Getenv("STORE_DSN"); dsn != "" {
		return dsn, func() {}, nil
	}
	logger.Info("STORE_DSN unset — starting embedded postgres")
	inst, err := embeddedpg.Start(embeddedpg.Config{})
	if err != nil {
		return "", nil, fmt.Errorf("embedded postgres: %w", err)
	}
	logger.Info("embedded postgres ready",
		"root", inst.Config().Root,
		"port", inst.Config().Port,
	)
	return inst.DSN(), func() {
		if err := inst.Stop(); err != nil {
			logger.Warn("embedded postgres stop failed", "err", err)
		}
	}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
