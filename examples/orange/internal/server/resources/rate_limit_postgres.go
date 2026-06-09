package resources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminv1 "github.com/dio/transit/examples/orange/api/orange/config/admin/v1"
	"github.com/dio/transit/examples/orange/internal/server/apikeys"
	"github.com/dio/transit/examples/orange/internal/server/scopes"
)

const rateLimitDDL = `
CREATE TABLE IF NOT EXISTS rate_limit_tiers (
    workspace_id             TEXT          NOT NULL,
    name                     TEXT          NOT NULL,
    usd_per_minute           NUMERIC(18,8) NOT NULL DEFAULT 0,
    usd_per_hour             NUMERIC(18,8) NOT NULL DEFAULT 0,
    usd_per_day              NUMERIC(18,8) NOT NULL DEFAULT 0,
    rpm                      INT           NOT NULL DEFAULT 0,
    rph                      INT           NOT NULL DEFAULT 0,
    rpd                      INT           NOT NULL DEFAULT 0,
    input_tokens_per_minute  INT           NOT NULL DEFAULT 0,
    input_tokens_per_hour    INT           NOT NULL DEFAULT 0,
    input_tokens_per_day     INT           NOT NULL DEFAULT 0,
    output_tokens_per_minute INT           NOT NULL DEFAULT 0,
    output_tokens_per_hour   INT           NOT NULL DEFAULT 0,
    output_tokens_per_day    INT           NOT NULL DEFAULT 0,
    cache_read_tokens_per_hour  INT        NOT NULL DEFAULT 0,
    cache_read_tokens_per_day   INT        NOT NULL DEFAULT 0,
    cache_write_tokens_per_hour INT        NOT NULL DEFAULT 0,
    cache_write_tokens_per_day  INT        NOT NULL DEFAULT 0,
    on_exceed    TEXT NOT NULL DEFAULT 'reject'
        CHECK (on_exceed IN ('reject','throttle','log_only')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at   TIMESTAMPTZ,
    PRIMARY KEY (workspace_id, name),
    CHECK (name <> '')
);
CREATE INDEX IF NOT EXISTS rate_limit_tiers_ws_idx
    ON rate_limit_tiers (workspace_id) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS rate_limit_policies (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id TEXT     NOT NULL,
    scope        TEXT     NOT NULL,
    models       TEXT[]   NOT NULL,
    tier_name    TEXT,
    sort_order   INT      NOT NULL DEFAULT 0,
    usd_per_minute           NUMERIC(18,8) NOT NULL DEFAULT 0,
    usd_per_hour             NUMERIC(18,8) NOT NULL DEFAULT 0,
    usd_per_day              NUMERIC(18,8) NOT NULL DEFAULT 0,
    rpm                      INT NOT NULL DEFAULT 0,
    rph                      INT NOT NULL DEFAULT 0,
    rpd                      INT NOT NULL DEFAULT 0,
    input_tokens_per_minute  INT NOT NULL DEFAULT 0,
    input_tokens_per_hour    INT NOT NULL DEFAULT 0,
    input_tokens_per_day     INT NOT NULL DEFAULT 0,
    output_tokens_per_minute INT NOT NULL DEFAULT 0,
    output_tokens_per_hour   INT NOT NULL DEFAULT 0,
    output_tokens_per_day    INT NOT NULL DEFAULT 0,
    cache_read_tokens_per_hour  INT NOT NULL DEFAULT 0,
    cache_read_tokens_per_day   INT NOT NULL DEFAULT 0,
    cache_write_tokens_per_hour INT NOT NULL DEFAULT 0,
    cache_write_tokens_per_day  INT NOT NULL DEFAULT 0,
    on_exceed TEXT CHECK (on_exceed IN ('reject','throttle','log_only'))
);
CREATE INDEX IF NOT EXISTS rate_limit_policies_scope_idx
    ON rate_limit_policies (workspace_id, scope, sort_order);
`

// rateLimitDB holds the pool and logger for rate-limit tier and scope
// operations. Methods on this type are called by ConfigService once
// InitRateLimit has been called.
type rateLimitDB struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

func newRateLimitDB(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) (*rateLimitDB, error) {
	if _, err := pool.Exec(ctx, rateLimitDDL); err != nil {
		return nil, err
	}
	return &rateLimitDB{pool: pool, logger: logger.With("component", "rate_limit")}, nil
}

// ── enum helpers ──────────────────────────────────────────────────────────────

func onExceedToString(oe adminv1.OnExceed) string {
	switch oe {
	case adminv1.OnExceed_ON_EXCEED_THROTTLE:
		return "throttle"
	case adminv1.OnExceed_ON_EXCEED_LOG_ONLY:
		return "log_only"
	default:
		return "reject"
	}
}

func onExceedFromString(s string) adminv1.OnExceed {
	switch s {
	case "throttle":
		return adminv1.OnExceed_ON_EXCEED_THROTTLE
	case "log_only":
		return adminv1.OnExceed_ON_EXCEED_LOG_ONLY
	default:
		return adminv1.OnExceed_ON_EXCEED_REJECT
	}
}

// nullableOnExceedFromString handles the nullable on_exceed column in
// rate_limit_policies (NULL means inherit from tier).
func nullableOnExceedFromString(s *string) adminv1.OnExceed {
	if s == nil {
		return adminv1.OnExceed_ON_EXCEED_UNSPECIFIED
	}
	return onExceedFromString(*s)
}

// ── tier row scanning ─────────────────────────────────────────────────────────

type tierRow struct {
	name                            string
	usdPerMinute                    float64
	usdPerHour                      float64
	usdPerDay                       float64
	rpm, rph, rpd                   int32
	inputTPM, inputTPH, inputTPD    int32
	outputTPM, outputTPH, outputTPD int32
	cacheReadTPH, cacheReadTPD      int32
	cacheWriteTPH, cacheWriteTPD    int32
	onExceed                        string
	createdAt                       time.Time
	updatedAt                       time.Time
}

func (r tierRow) toProto() *adminv1.RateLimitTier {
	return &adminv1.RateLimitTier{
		Name:                    r.name,
		UsdPerMinute:            r.usdPerMinute,
		UsdPerHour:              r.usdPerHour,
		UsdPerDay:               r.usdPerDay,
		Rpm:                     r.rpm,
		Rph:                     r.rph,
		Rpd:                     r.rpd,
		InputTokensPerMinute:    r.inputTPM,
		InputTokensPerHour:      r.inputTPH,
		InputTokensPerDay:       r.inputTPD,
		OutputTokensPerMinute:   r.outputTPM,
		OutputTokensPerHour:     r.outputTPH,
		OutputTokensPerDay:      r.outputTPD,
		CacheReadTokensPerHour:  r.cacheReadTPH,
		CacheReadTokensPerDay:   r.cacheReadTPD,
		CacheWriteTokensPerHour: r.cacheWriteTPH,
		CacheWriteTokensPerDay:  r.cacheWriteTPD,
		OnExceed:                onExceedFromString(r.onExceed),
		CreatedAt:               timestamppb.New(r.createdAt),
		UpdatedAt:               timestamppb.New(r.updatedAt),
	}
}

func (r *tierRow) scanArgs() []any {
	return []any{
		&r.name,
		&r.usdPerMinute, &r.usdPerHour, &r.usdPerDay,
		&r.rpm, &r.rph, &r.rpd,
		&r.inputTPM, &r.inputTPH, &r.inputTPD,
		&r.outputTPM, &r.outputTPH, &r.outputTPD,
		&r.cacheReadTPH, &r.cacheReadTPD,
		&r.cacheWriteTPH, &r.cacheWriteTPD,
		&r.onExceed,
		&r.createdAt, &r.updatedAt,
	}
}

const tierColumns = `name,
    usd_per_minute, usd_per_hour, usd_per_day,
    rpm, rph, rpd,
    input_tokens_per_minute, input_tokens_per_hour, input_tokens_per_day,
    output_tokens_per_minute, output_tokens_per_hour, output_tokens_per_day,
    cache_read_tokens_per_hour, cache_read_tokens_per_day,
    cache_write_tokens_per_hour, cache_write_tokens_per_day,
    on_exceed, created_at, updated_at`

// ── ConfigService: tier RPCs ──────────────────────────────────────────────────

func (s *ConfigService) CreateRateLimitTier(
	ctx context.Context,
	req *connect.Request[adminv1.CreateRateLimitTierRequest],
) (*connect.Response[adminv1.CreateRateLimitTierResponse], error) {
	if s.rl == nil {
		return nil, errRLNotConfigured
	}
	return s.rl.createTier(ctx, req.Msg)
}

func (s *ConfigService) GetRateLimitTier(
	ctx context.Context,
	req *connect.Request[adminv1.GetRateLimitTierRequest],
) (*connect.Response[adminv1.GetRateLimitTierResponse], error) {
	if s.rl == nil {
		return nil, errRLNotConfigured
	}
	return s.rl.getTier(ctx, req.Msg)
}

func (s *ConfigService) UpdateRateLimitTier(
	ctx context.Context,
	req *connect.Request[adminv1.UpdateRateLimitTierRequest],
) (*connect.Response[adminv1.UpdateRateLimitTierResponse], error) {
	if s.rl == nil {
		return nil, errRLNotConfigured
	}
	return s.rl.updateTier(ctx, req.Msg)
}

func (s *ConfigService) DeleteRateLimitTier(
	ctx context.Context,
	req *connect.Request[adminv1.DeleteRateLimitTierRequest],
) (*connect.Response[adminv1.DeleteRateLimitTierResponse], error) {
	if s.rl == nil {
		return nil, errRLNotConfigured
	}
	return s.rl.deleteTier(ctx, req.Msg)
}

func (s *ConfigService) ListRateLimitTiers(
	ctx context.Context,
	req *connect.Request[adminv1.ListRateLimitTiersRequest],
) (*connect.Response[adminv1.ListRateLimitTiersResponse], error) {
	if s.rl == nil {
		return nil, errRLNotConfigured
	}
	return s.rl.listTiers(ctx, req.Msg)
}

// ── ConfigService: scope RPCs ─────────────────────────────────────────────────

func (s *ConfigService) SetRateLimitScope(
	ctx context.Context,
	req *connect.Request[adminv1.SetRateLimitScopeRequest],
) (*connect.Response[adminv1.SetRateLimitScopeResponse], error) {
	if s.rl == nil {
		return nil, errRLNotConfigured
	}
	return s.rl.setScope(ctx, req.Msg)
}

func (s *ConfigService) GetRateLimitScope(
	ctx context.Context,
	req *connect.Request[adminv1.GetRateLimitScopeRequest],
) (*connect.Response[adminv1.GetRateLimitScopeResponse], error) {
	if s.rl == nil {
		return nil, errRLNotConfigured
	}
	return s.rl.getScope(ctx, req.Msg)
}

func (s *ConfigService) ListRateLimitScopes(
	ctx context.Context,
	req *connect.Request[adminv1.ListRateLimitScopesRequest],
) (*connect.Response[adminv1.ListRateLimitScopesResponse], error) {
	if s.rl == nil {
		return nil, errRLNotConfigured
	}
	return s.rl.listScopes(ctx, req.Msg)
}

func (s *ConfigService) DeleteRateLimitScope(
	ctx context.Context,
	req *connect.Request[adminv1.DeleteRateLimitScopeRequest],
) (*connect.Response[adminv1.DeleteRateLimitScopeResponse], error) {
	if s.rl == nil {
		return nil, errRLNotConfigured
	}
	return s.rl.deleteScope(ctx, req.Msg)
}

var errRLNotConfigured = connect.NewError(connect.CodeUnimplemented,
	errors.New("rate limit service not configured; call InitRateLimit during server setup"))

// ── rateLimitDB: tier implementations ────────────────────────────────────────

func (db *rateLimitDB) createTier(ctx context.Context, msg *adminv1.CreateRateLimitTierRequest) (*connect.Response[adminv1.CreateRateLimitTierResponse], error) {
	const q = `
INSERT INTO rate_limit_tiers (workspace_id, name,
    usd_per_minute, usd_per_hour, usd_per_day,
    rpm, rph, rpd,
    input_tokens_per_minute, input_tokens_per_hour, input_tokens_per_day,
    output_tokens_per_minute, output_tokens_per_hour, output_tokens_per_day,
    cache_read_tokens_per_hour, cache_read_tokens_per_day,
    cache_write_tokens_per_hour, cache_write_tokens_per_day,
    on_exceed)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
RETURNING ` + tierColumns

	var r tierRow
	err := db.pool.QueryRow(ctx, q,
		msg.GetWorkspaceId(), msg.GetName(),
		msg.GetUsdPerMinute(), msg.GetUsdPerHour(), msg.GetUsdPerDay(),
		msg.GetRpm(), msg.GetRph(), msg.GetRpd(),
		msg.GetInputTokensPerMinute(), msg.GetInputTokensPerHour(), msg.GetInputTokensPerDay(),
		msg.GetOutputTokensPerMinute(), msg.GetOutputTokensPerHour(), msg.GetOutputTokensPerDay(),
		msg.GetCacheReadTokensPerHour(), msg.GetCacheReadTokensPerDay(),
		msg.GetCacheWriteTokensPerHour(), msg.GetCacheWriteTokensPerDay(),
		onExceedToString(msg.GetOnExceed()),
	).Scan(r.scanArgs()...)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists,
				fmt.Errorf("tier %q already exists in workspace %q", msg.GetName(), msg.GetWorkspaceId()))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&adminv1.CreateRateLimitTierResponse{
		Tier: r.toProto(),
	}), nil
}

func (db *rateLimitDB) getTier(ctx context.Context, msg *adminv1.GetRateLimitTierRequest) (*connect.Response[adminv1.GetRateLimitTierResponse], error) {
	if !scopes.WorkspaceAccessForRLPolicy(callerScopes(ctx), msg.GetWorkspaceId()) {
		return nil, rlScopePermErr(msg.GetWorkspaceId())
	}

	const q = `SELECT ` + tierColumns + `
FROM rate_limit_tiers
WHERE workspace_id = $1 AND name = $2 AND deleted_at IS NULL`

	var r tierRow
	if err := db.pool.QueryRow(ctx, q, msg.GetWorkspaceId(), msg.GetName()).Scan(r.scanArgs()...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound,
				fmt.Errorf("tier %q not found in workspace %q", msg.GetName(), msg.GetWorkspaceId()))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&adminv1.GetRateLimitTierResponse{
		Tier: r.toProto(),
	}), nil
}

func (db *rateLimitDB) updateTier(ctx context.Context, msg *adminv1.UpdateRateLimitTierRequest) (*connect.Response[adminv1.UpdateRateLimitTierResponse], error) {
	const q = `
UPDATE rate_limit_tiers SET
    usd_per_minute = $3, usd_per_hour = $4, usd_per_day = $5,
    rpm = $6, rph = $7, rpd = $8,
    input_tokens_per_minute = $9,  input_tokens_per_hour = $10,  input_tokens_per_day = $11,
    output_tokens_per_minute = $12, output_tokens_per_hour = $13, output_tokens_per_day = $14,
    cache_read_tokens_per_hour = $15,  cache_read_tokens_per_day = $16,
    cache_write_tokens_per_hour = $17, cache_write_tokens_per_day = $18,
    on_exceed = $19,
    updated_at = now()
WHERE workspace_id = $1 AND name = $2 AND deleted_at IS NULL
RETURNING ` + tierColumns

	var r tierRow
	err := db.pool.QueryRow(ctx, q,
		msg.GetWorkspaceId(), msg.GetName(),
		msg.GetUsdPerMinute(), msg.GetUsdPerHour(), msg.GetUsdPerDay(),
		msg.GetRpm(), msg.GetRph(), msg.GetRpd(),
		msg.GetInputTokensPerMinute(), msg.GetInputTokensPerHour(), msg.GetInputTokensPerDay(),
		msg.GetOutputTokensPerMinute(), msg.GetOutputTokensPerHour(), msg.GetOutputTokensPerDay(),
		msg.GetCacheReadTokensPerHour(), msg.GetCacheReadTokensPerDay(),
		msg.GetCacheWriteTokensPerHour(), msg.GetCacheWriteTokensPerDay(),
		onExceedToString(msg.GetOnExceed()),
	).Scan(r.scanArgs()...)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound,
				fmt.Errorf("tier %q not found in workspace %q", msg.GetName(), msg.GetWorkspaceId()))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&adminv1.UpdateRateLimitTierResponse{
		Tier: r.toProto(),
	}), nil
}

func (db *rateLimitDB) deleteTier(ctx context.Context, msg *adminv1.DeleteRateLimitTierRequest) (*connect.Response[adminv1.DeleteRateLimitTierResponse], error) {
	// Refuse if any active policy entry still references this tier.
	const checkRef = `
SELECT COUNT(*) FROM rate_limit_policies
WHERE workspace_id = $1 AND tier_name = $2`

	var refCount int64
	if err := db.pool.QueryRow(ctx, checkRef, msg.GetWorkspaceId(), msg.GetName()).Scan(&refCount); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if refCount > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("tier %q is still referenced by %d active policy entr(ies); remove or update them first",
				msg.GetName(), refCount))
	}

	const q = `
UPDATE rate_limit_tiers SET deleted_at = now()
WHERE workspace_id = $1 AND name = $2 AND deleted_at IS NULL`

	tag, err := db.pool.Exec(ctx, q, msg.GetWorkspaceId(), msg.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("tier %q not found in workspace %q", msg.GetName(), msg.GetWorkspaceId()))
	}

	return connect.NewResponse(&adminv1.DeleteRateLimitTierResponse{}), nil
}

func (db *rateLimitDB) listTiers(ctx context.Context, msg *adminv1.ListRateLimitTiersRequest) (*connect.Response[adminv1.ListRateLimitTiersResponse], error) {
	if !scopes.WorkspaceAccessForRLPolicy(callerScopes(ctx), msg.GetWorkspaceId()) {
		return nil, rlScopePermErr(msg.GetWorkspaceId())
	}

	limit := int(msg.GetLimit())
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	args := []any{msg.GetWorkspaceId()}
	where := "workspace_id = $1 AND deleted_at IS NULL"
	if pt := msg.GetPageToken(); pt != "" {
		where += " AND name > $2"
		args = append(args, pt)
	}

	q := `SELECT ` + tierColumns + ` FROM rate_limit_tiers WHERE ` + where +
		` ORDER BY name LIMIT ` + itoa(limit+1)

	rows, err := db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var tiers []*adminv1.RateLimitTier
	for rows.Next() {
		var r tierRow
		if err := rows.Scan(r.scanArgs()...); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		tiers = append(tiers, r.toProto())
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(tiers) > limit {
		tiers = tiers[:limit]
		nextPageToken = tiers[len(tiers)-1].GetName()
	}

	return connect.NewResponse(&adminv1.ListRateLimitTiersResponse{
		Tiers:         tiers,
		NextPageToken: nextPageToken,
	}), nil
}

// ── rateLimitDB: scope implementations ───────────────────────────────────────

// scopeKey derives the scope string stored in the DB from workspace_id + optional user.
func scopeKey(workspaceID string, user *string) string {
	if user != nil && *user != "" {
		return workspaceID + "/" + *user
	}
	return workspaceID
}

// policyEntryRow mirrors one rate_limit_policies row.
type policyEntryRow struct {
	models                          []string
	tierName                        *string
	usdPerMinute                    float64
	usdPerHour                      float64
	usdPerDay                       float64
	rpm, rph, rpd                   int32
	inputTPM, inputTPH, inputTPD    int32
	outputTPM, outputTPH, outputTPD int32
	cacheReadTPH, cacheReadTPD      int32
	cacheWriteTPH, cacheWriteTPD    int32
	onExceed                        *string
}

func (r policyEntryRow) toProto() *adminv1.RateLimitPolicyEntry {
	e := &adminv1.RateLimitPolicyEntry{
		Models:                  r.models,
		TierName:                r.tierName,
		UsdPerMinute:            r.usdPerMinute,
		UsdPerHour:              r.usdPerHour,
		UsdPerDay:               r.usdPerDay,
		Rpm:                     r.rpm,
		Rph:                     r.rph,
		Rpd:                     r.rpd,
		InputTokensPerMinute:    r.inputTPM,
		InputTokensPerHour:      r.inputTPH,
		InputTokensPerDay:       r.inputTPD,
		OutputTokensPerMinute:   r.outputTPM,
		OutputTokensPerHour:     r.outputTPH,
		OutputTokensPerDay:      r.outputTPD,
		CacheReadTokensPerHour:  r.cacheReadTPH,
		CacheReadTokensPerDay:   r.cacheReadTPD,
		CacheWriteTokensPerHour: r.cacheWriteTPH,
		CacheWriteTokensPerDay:  r.cacheWriteTPD,
		OnExceed:                nullableOnExceedFromString(r.onExceed),
	}
	return e
}

func (r *policyEntryRow) scanArgs() []any {
	return []any{
		&r.models, &r.tierName,
		&r.usdPerMinute, &r.usdPerHour, &r.usdPerDay,
		&r.rpm, &r.rph, &r.rpd,
		&r.inputTPM, &r.inputTPH, &r.inputTPD,
		&r.outputTPM, &r.outputTPH, &r.outputTPD,
		&r.cacheReadTPH, &r.cacheReadTPD,
		&r.cacheWriteTPH, &r.cacheWriteTPD,
		&r.onExceed,
	}
}

const policyEntryColumns = `models, tier_name,
    usd_per_minute, usd_per_hour, usd_per_day,
    rpm, rph, rpd,
    input_tokens_per_minute, input_tokens_per_hour, input_tokens_per_day,
    output_tokens_per_minute, output_tokens_per_hour, output_tokens_per_day,
    cache_read_tokens_per_hour, cache_read_tokens_per_day,
    cache_write_tokens_per_hour, cache_write_tokens_per_day,
    on_exceed`

func (db *rateLimitDB) setScope(ctx context.Context, msg *adminv1.SetRateLimitScopeRequest) (*connect.Response[adminv1.SetRateLimitScopeResponse], error) {
	wsID := msg.GetWorkspaceId()
	scope := scopeKey(wsID, msg.User)
	if !scopes.AuthorizedForRLScope(callerScopes(ctx), scope) {
		return nil, rlScopePermErr(scope)
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Full replace: delete existing entries for this scope, then insert new ones.
	if _, err := tx.Exec(ctx,
		`DELETE FROM rate_limit_policies WHERE workspace_id = $1 AND scope = $2`,
		wsID, scope,
	); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	entries, err := insertPolicyEntries(ctx, tx, wsID, scope, msg.GetEntries())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := &adminv1.SetRateLimitScopeResponse{
		Scope: &adminv1.RateLimitScope{
			WorkspaceId: wsID,
			User:        msg.User,
			Entries:     entries,
		},
	}
	return connect.NewResponse(resp), nil
}

func (db *rateLimitDB) getScope(ctx context.Context, msg *adminv1.GetRateLimitScopeRequest) (*connect.Response[adminv1.GetRateLimitScopeResponse], error) {
	wsID := msg.GetWorkspaceId()
	scope := scopeKey(wsID, msg.User)
	if !scopes.AuthorizedForRLScope(callerScopes(ctx), scope) {
		return nil, rlScopePermErr(scope)
	}

	entries, err := loadPolicyEntries(ctx, db.pool, wsID, scope)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if len(entries) == 0 {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("no rate-limit scope found for %q", scope))
	}

	return connect.NewResponse(&adminv1.GetRateLimitScopeResponse{
		Scope: &adminv1.RateLimitScope{
			WorkspaceId: wsID,
			User:        msg.User,
			Entries:     entries,
		},
	}), nil
}

func (db *rateLimitDB) listScopes(ctx context.Context, msg *adminv1.ListRateLimitScopesRequest) (*connect.Response[adminv1.ListRateLimitScopesResponse], error) {
	limit := int(msg.GetLimit())
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	// Determine which scopes this caller is allowed to see.
	ctxs, isAdmin := scopes.RLScopeContexts(callerScopes(ctx))
	canSee := func(scopeKey string) bool {
		if isAdmin {
			return true
		}
		for _, c := range ctxs {
			if scopeKey == c || strings.HasPrefix(scopeKey, c+"/") {
				return true
			}
		}
		return false
	}

	args := []any{msg.GetWorkspaceId()}
	where := "workspace_id = $1"
	if pt := msg.GetPageToken(); pt != "" {
		where += " AND scope > $2"
		args = append(args, pt)
	}

	// Fetch all distinct scopes; filter by caller authorization in Go.
	// Scope counts per workspace are small (O(tens)), so this is fine.
	q := `SELECT DISTINCT scope FROM rate_limit_policies WHERE ` + where +
		` ORDER BY scope LIMIT ` + itoa(limit+1)

	rows, err := db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	var scopeKeys []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if canSee(s) {
			scopeKeys = append(scopeKeys, s)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(scopeKeys) > limit {
		scopeKeys = scopeKeys[:limit]
		nextPageToken = scopeKeys[len(scopeKeys)-1]
	}

	wsID := msg.GetWorkspaceId()
	result := make([]*adminv1.RateLimitScope, 0, len(scopeKeys))
	for _, sk := range scopeKeys {
		entries, err := loadPolicyEntries(ctx, db.pool, wsID, sk)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		rs := &adminv1.RateLimitScope{
			WorkspaceId: wsID,
			Entries:     entries,
		}
		if sk != wsID {
			// Extract user segment: sk = wsID + "/" + user
			user := sk[len(wsID)+1:]
			rs.User = &user
		}
		result = append(result, rs)
	}

	return connect.NewResponse(&adminv1.ListRateLimitScopesResponse{
		Scopes:        result,
		NextPageToken: nextPageToken,
	}), nil
}

func (db *rateLimitDB) deleteScope(ctx context.Context, msg *adminv1.DeleteRateLimitScopeRequest) (*connect.Response[adminv1.DeleteRateLimitScopeResponse], error) {
	wsID := msg.GetWorkspaceId()
	scope := scopeKey(wsID, msg.User)
	if !scopes.AuthorizedForRLScope(callerScopes(ctx), scope) {
		return nil, rlScopePermErr(scope)
	}

	tag, err := db.pool.Exec(ctx,
		`DELETE FROM rate_limit_policies WHERE workspace_id = $1 AND scope = $2`,
		wsID, scope,
	)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("no rate-limit scope found for %q", scope))
	}

	return connect.NewResponse(&adminv1.DeleteRateLimitScopeResponse{}), nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// rlQuerier is satisfied by *pgxpool.Pool and pgx.Tx.
type rlQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func insertPolicyEntries(ctx context.Context, tx policyTxExecer, wsID, scope string, protoEntries []*adminv1.RateLimitPolicyEntry) ([]*adminv1.RateLimitPolicyEntry, error) {
	const q = `
INSERT INTO rate_limit_policies (
    workspace_id, scope, models, tier_name, sort_order,
    usd_per_minute, usd_per_hour, usd_per_day,
    rpm, rph, rpd,
    input_tokens_per_minute, input_tokens_per_hour, input_tokens_per_day,
    output_tokens_per_minute, output_tokens_per_hour, output_tokens_per_day,
    cache_read_tokens_per_hour, cache_read_tokens_per_day,
    cache_write_tokens_per_hour, cache_write_tokens_per_day,
    on_exceed)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`

	for i, e := range protoEntries {
		var onExceed *string
		if oe := e.GetOnExceed(); oe != adminv1.OnExceed_ON_EXCEED_UNSPECIFIED {
			s := onExceedToString(oe)
			onExceed = &s
		}
		if _, err := tx.Exec(ctx, q,
			wsID, scope, e.GetModels(), e.TierName, i,
			e.GetUsdPerMinute(), e.GetUsdPerHour(), e.GetUsdPerDay(),
			e.GetRpm(), e.GetRph(), e.GetRpd(),
			e.GetInputTokensPerMinute(), e.GetInputTokensPerHour(), e.GetInputTokensPerDay(),
			e.GetOutputTokensPerMinute(), e.GetOutputTokensPerHour(), e.GetOutputTokensPerDay(),
			e.GetCacheReadTokensPerHour(), e.GetCacheReadTokensPerDay(),
			e.GetCacheWriteTokensPerHour(), e.GetCacheWriteTokensPerDay(),
			onExceed,
		); err != nil {
			return nil, err
		}
	}
	return protoEntries, nil
}

func loadPolicyEntries(ctx context.Context, q rlQuerier, wsID, scope string) ([]*adminv1.RateLimitPolicyEntry, error) {
	const query = `SELECT ` + policyEntryColumns + `
FROM rate_limit_policies
WHERE workspace_id = $1 AND scope = $2
ORDER BY sort_order`

	rows, err := q.Query(ctx, query, wsID, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*adminv1.RateLimitPolicyEntry
	for rows.Next() {
		var r policyEntryRow
		if err := rows.Scan(r.scanArgs()...); err != nil {
			return nil, err
		}
		entries = append(entries, r.toProto())
	}
	return entries, rows.Err()
}

// ── auth helpers ──────────────────────────────────────────────────────────────

// callerScopes extracts the API key scope list from the request context.
// Returns nil when no record is present (should not happen on protected RPCs).
func callerScopes(ctx context.Context) []string {
	rec, ok := apikeys.RecordFromContext(ctx)
	if !ok {
		return nil
	}
	return rec.Scopes
}

func rlScopePermErr(scopeKey string) error {
	return connect.NewError(connect.CodePermissionDenied,
		fmt.Errorf("rl-policy:write[%s] or admin required", scopeKey))
}
