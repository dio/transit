package resources

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	policyv1 "github.com/dio/transit/examples/orange/api/orange/policy/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/policy/admin/v1/adminv1connect"
)

// PolicyService implements adminv1connect.PolicyAdminServiceHandler using a PostgreSQL pool.
type PolicyService struct {
	adminv1connect.UnimplementedPolicyAdminServiceHandler
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewPolicyService creates the policies and policy_rules tables if they do not exist
// and returns a new PolicyService.
func NewPolicyService(pool *pgxpool.Pool, logger *slog.Logger) (*PolicyService, error) {
	ddl := `
CREATE TABLE IF NOT EXISTS policies (
  policy_id   TEXT PRIMARY KEY,
  scope_type  TEXT NOT NULL CHECK (scope_type IN ('project','workspace','key')),
  scope_id    TEXT NOT NULL,
  policy_type TEXT NOT NULL CHECK (policy_type IN ('floor','flexible')),
  description TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS policy_rules (
  rule_id     TEXT PRIMARY KEY,
  policy_id   TEXT NOT NULL REFERENCES policies(policy_id) ON DELETE CASCADE,
  position    INT NOT NULL,
  rule_json   JSONB NOT NULL
)`
	if _, err := pool.Exec(context.Background(), ddl); err != nil {
		return nil, err
	}
	return &PolicyService{pool: pool, logger: logger}, nil
}

// ── enum conversions ─────────────────────────────────────────────────────────

func scopeTypeToString(st policyv1.PolicyScopeType) string {
	switch st {
	case policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_PROJECT:
		return "project"
	case policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_WORKSPACE:
		return "workspace"
	case policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_KEY:
		return "key"
	default:
		return "project"
	}
}

func scopeTypeFromString(s string) policyv1.PolicyScopeType {
	switch s {
	case "project":
		return policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_PROJECT
	case "workspace":
		return policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_WORKSPACE
	case "key":
		return policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_KEY
	default:
		return policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_UNSPECIFIED
	}
}

func policyTypeToString(pt policyv1.PolicyType) string {
	switch pt {
	case policyv1.PolicyType_POLICY_TYPE_FLOOR:
		return "floor"
	case policyv1.PolicyType_POLICY_TYPE_FLEXIBLE:
		return "flexible"
	default:
		return "floor"
	}
}

func policyTypeFromString(s string) policyv1.PolicyType {
	switch s {
	case "floor":
		return policyv1.PolicyType_POLICY_TYPE_FLOOR
	case "flexible":
		return policyv1.PolicyType_POLICY_TYPE_FLEXIBLE
	default:
		return policyv1.PolicyType_POLICY_TYPE_UNSPECIFIED
	}
}

// ── JSON rule serialisation ───────────────────────────────────────────────────

// policyRuleJSON is a JSON-serialisable mirror of policyv1.PolicyRule.
type policyRuleJSON struct {
	Models                  []string `json:"models,omitempty"`
	UsdPerMinute            float64  `json:"usd_per_minute,omitempty"`
	UsdPerHour              float64  `json:"usd_per_hour,omitempty"`
	UsdPerDay               float64  `json:"usd_per_day,omitempty"`
	Rpm                     int32    `json:"rpm,omitempty"`
	Rph                     int32    `json:"rph,omitempty"`
	Rpd                     int32    `json:"rpd,omitempty"`
	InputTokensPerMinute    int32    `json:"input_tokens_per_minute,omitempty"`
	InputTokensPerHour      int32    `json:"input_tokens_per_hour,omitempty"`
	InputTokensPerDay       int32    `json:"input_tokens_per_day,omitempty"`
	OutputTokensPerMinute   int32    `json:"output_tokens_per_minute,omitempty"`
	OutputTokensPerHour     int32    `json:"output_tokens_per_hour,omitempty"`
	OutputTokensPerDay      int32    `json:"output_tokens_per_day,omitempty"`
	CacheReadTokensPerHour  int32    `json:"cache_read_tokens_per_hour,omitempty"`
	CacheReadTokensPerDay   int32    `json:"cache_read_tokens_per_day,omitempty"`
	CacheWriteTokensPerHour int32    `json:"cache_write_tokens_per_hour,omitempty"`
	CacheWriteTokensPerDay  int32    `json:"cache_write_tokens_per_day,omitempty"`
	OnExceed                int32    `json:"on_exceed,omitempty"`
}

func protoRuleToJSON(r *policyv1.PolicyRule) ([]byte, error) {
	j := policyRuleJSON{
		Models:                  r.GetModels(),
		UsdPerMinute:            r.GetUsdPerMinute(),
		UsdPerHour:              r.GetUsdPerHour(),
		UsdPerDay:               r.GetUsdPerDay(),
		Rpm:                     r.GetRpm(),
		Rph:                     r.GetRph(),
		Rpd:                     r.GetRpd(),
		InputTokensPerMinute:    r.GetInputTokensPerMinute(),
		InputTokensPerHour:      r.GetInputTokensPerHour(),
		InputTokensPerDay:       r.GetInputTokensPerDay(),
		OutputTokensPerMinute:   r.GetOutputTokensPerMinute(),
		OutputTokensPerHour:     r.GetOutputTokensPerHour(),
		OutputTokensPerDay:      r.GetOutputTokensPerDay(),
		CacheReadTokensPerHour:  r.GetCacheReadTokensPerHour(),
		CacheReadTokensPerDay:   r.GetCacheReadTokensPerDay(),
		CacheWriteTokensPerHour: r.GetCacheWriteTokensPerHour(),
		CacheWriteTokensPerDay:  r.GetCacheWriteTokensPerDay(),
		OnExceed:                int32(r.GetOnExceed()),
	}
	return json.Marshal(j)
}

func jsonToProtoRule(data []byte) (*policyv1.PolicyRule, error) {
	var j policyRuleJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, err
	}
	return &policyv1.PolicyRule{
		Models:                  j.Models,
		UsdPerMinute:            j.UsdPerMinute,
		UsdPerHour:              j.UsdPerHour,
		UsdPerDay:               j.UsdPerDay,
		Rpm:                     j.Rpm,
		Rph:                     j.Rph,
		Rpd:                     j.Rpd,
		InputTokensPerMinute:    j.InputTokensPerMinute,
		InputTokensPerHour:      j.InputTokensPerHour,
		InputTokensPerDay:       j.InputTokensPerDay,
		OutputTokensPerMinute:   j.OutputTokensPerMinute,
		OutputTokensPerHour:     j.OutputTokensPerHour,
		OutputTokensPerDay:      j.OutputTokensPerDay,
		CacheReadTokensPerHour:  j.CacheReadTokensPerHour,
		CacheReadTokensPerDay:   j.CacheReadTokensPerDay,
		CacheWriteTokensPerHour: j.CacheWriteTokensPerHour,
		CacheWriteTokensPerDay:  j.CacheWriteTokensPerDay,
		OnExceed:                policyv1.OnExceed(j.OnExceed),
	}, nil
}

// ── RPC implementations ──────────────────────────────────────────────────────

// CreatePolicy inserts a policy and its rules in a transaction, then returns the full Policy.
func (s *PolicyService) CreatePolicy(ctx context.Context, req *connect.Request[policyv1.CreatePolicyRequest]) (*connect.Response[policyv1.CreatePolicyResponse], error) {
	policyID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	const insertPolicy = `
INSERT INTO policies (policy_id, scope_type, scope_id, policy_type, description, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $6)
RETURNING policy_id, scope_type, scope_id, policy_type, description, created_at, updated_at`

	var (
		id          string
		scopeType   string
		scopeID     string
		policyType  string
		description *string
		createdAt   time.Time
		updatedAt   time.Time
	)
	err = tx.QueryRow(ctx, insertPolicy,
		policyID,
		scopeTypeToString(req.Msg.GetScopeType()),
		req.Msg.GetScopeId(),
		policyTypeToString(req.Msg.GetType()),
		req.Msg.Description,
		now,
	).Scan(&id, &scopeType, &scopeID, &policyType, &description, &createdAt, &updatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	rules, err := insertPolicyRules(ctx, tx, id, req.Msg.GetRules())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&policyv1.CreatePolicyResponse{
		Policy: &policyv1.Policy{
			PolicyId:    id,
			ScopeType:   scopeTypeFromString(scopeType),
			ScopeId:     scopeID,
			Type:        policyTypeFromString(policyType),
			Description: description,
			Rules:       rules,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		},
	}), nil
}

// GetPolicy fetches a policy and its rules by policy_id.
func (s *PolicyService) GetPolicy(ctx context.Context, req *connect.Request[policyv1.GetPolicyRequest]) (*connect.Response[policyv1.GetPolicyResponse], error) {
	const qPolicy = `SELECT policy_id, scope_type, scope_id, policy_type, description, created_at, updated_at FROM policies WHERE policy_id = $1`

	var (
		id          string
		scopeType   string
		scopeID     string
		policyType  string
		description *string
		createdAt   time.Time
		updatedAt   time.Time
	)
	err := s.pool.QueryRow(ctx, qPolicy, req.Msg.GetPolicyId()).
		Scan(&id, &scopeType, &scopeID, &policyType, &description, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	rules, err := loadPolicyRules(ctx, s.pool, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&policyv1.GetPolicyResponse{
		Policy: &policyv1.Policy{
			PolicyId:    id,
			ScopeType:   scopeTypeFromString(scopeType),
			ScopeId:     scopeID,
			Type:        policyTypeFromString(policyType),
			Description: description,
			Rules:       rules,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		},
	}), nil
}

// ListPolicies returns a page of policies with optional scope/type filters, loading rules for each.
func (s *PolicyService) ListPolicies(ctx context.Context, req *connect.Request[policyv1.ListPoliciesRequest]) (*connect.Response[policyv1.ListPoliciesResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	type policyRow struct {
		id          string
		scopeType   string
		scopeID     string
		policyType  string
		description *string
		createdAt   time.Time
		updatedAt   time.Time
	}

	args := []any{}
	where := ""
	argIdx := 1

	addFilter := func(clause string, val any) {
		if where == "" {
			where = " WHERE "
		} else {
			where += " AND "
		}
		where += clause
		args = append(args, val)
		argIdx++
	}

	pageToken := req.Msg.GetPageToken()
	if pageToken != "" {
		addFilter("policy_id > $"+itoa(argIdx), pageToken)
	}
	if req.Msg.ScopeType != nil && *req.Msg.ScopeType != policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_UNSPECIFIED {
		addFilter("scope_type = $"+itoa(argIdx), scopeTypeToString(*req.Msg.ScopeType))
	}
	if req.Msg.ScopeId != nil {
		addFilter("scope_id = $"+itoa(argIdx), *req.Msg.ScopeId)
	}
	if req.Msg.Type != nil && *req.Msg.Type != policyv1.PolicyType_POLICY_TYPE_UNSPECIFIED {
		addFilter("policy_type = $"+itoa(argIdx), policyTypeToString(*req.Msg.Type))
	}

	limitArg := "$" + itoa(argIdx)
	args = append(args, limit)

	q := "SELECT policy_id, scope_type, scope_id, policy_type, description, created_at, updated_at FROM policies" +
		where + " ORDER BY policy_id LIMIT " + limitArg

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var policyRows []policyRow
	for rows.Next() {
		var r policyRow
		if err := rows.Scan(&r.id, &r.scopeType, &r.scopeID, &r.policyType, &r.description, &r.createdAt, &r.updatedAt); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		policyRows = append(policyRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	policies := make([]*policyv1.Policy, 0, len(policyRows))
	for _, r := range policyRows {
		ruleList, err := loadPolicyRules(ctx, s.pool, r.id)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		policies = append(policies, &policyv1.Policy{
			PolicyId:    r.id,
			ScopeType:   scopeTypeFromString(r.scopeType),
			ScopeId:     r.scopeID,
			Type:        policyTypeFromString(r.policyType),
			Description: r.description,
			Rules:       ruleList,
			CreatedAt:   timestamppb.New(r.createdAt),
			UpdatedAt:   timestamppb.New(r.updatedAt),
		})
	}

	var nextPageToken string
	if len(policies) == limit {
		nextPageToken = policies[len(policies)-1].PolicyId
	}

	return connect.NewResponse(&policyv1.ListPoliciesResponse{
		Policies:      policies,
		NextPageToken: nextPageToken,
	}), nil
}

// UpdatePolicy updates the description and replaces the rule set in a transaction.
func (s *PolicyService) UpdatePolicy(ctx context.Context, req *connect.Request[policyv1.UpdatePolicyRequest]) (*connect.Response[policyv1.UpdatePolicyResponse], error) {
	now := time.Now().UTC()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	const updatePolicy = `
UPDATE policies SET description = $2, updated_at = $3
WHERE policy_id = $1
RETURNING policy_id, scope_type, scope_id, policy_type, description, created_at, updated_at`

	var (
		id          string
		scopeType   string
		scopeID     string
		policyType  string
		description *string
		createdAt   time.Time
		updatedAt   time.Time
	)
	err = tx.QueryRow(ctx, updatePolicy, req.Msg.GetPolicyId(), req.Msg.Description, now).
		Scan(&id, &scopeType, &scopeID, &policyType, &description, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Delete existing rules and re-insert.
	if _, err := tx.Exec(ctx, `DELETE FROM policy_rules WHERE policy_id = $1`, id); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	rules, err := insertPolicyRules(ctx, tx, id, req.Msg.GetRules())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&policyv1.UpdatePolicyResponse{
		Policy: &policyv1.Policy{
			PolicyId:    id,
			ScopeType:   scopeTypeFromString(scopeType),
			ScopeId:     scopeID,
			Type:        policyTypeFromString(policyType),
			Description: description,
			Rules:       rules,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		},
	}), nil
}

// DeletePolicy removes a policy by policy_id (rules cascade).
func (s *PolicyService) DeletePolicy(ctx context.Context, req *connect.Request[policyv1.DeletePolicyRequest]) (*connect.Response[policyv1.DeletePolicyResponse], error) {
	const q = `DELETE FROM policies WHERE policy_id = $1`

	tag, err := s.pool.Exec(ctx, q, req.Msg.GetPolicyId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("policy not found"))
	}

	return connect.NewResponse(&policyv1.DeletePolicyResponse{}), nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

type policyTxExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func insertPolicyRules(ctx context.Context, tx policyTxExecer, policyID string, protoRules []*policyv1.PolicyRule) ([]*policyv1.PolicyRule, error) {
	const insertRule = `INSERT INTO policy_rules (rule_id, policy_id, position, rule_json) VALUES ($1, $2, $3, $4)`

	result := make([]*policyv1.PolicyRule, 0, len(protoRules))
	for i, r := range protoRules {
		ruleID := uuid.Must(uuid.NewV7()).String()
		data, err := protoRuleToJSON(r)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, insertRule, ruleID, policyID, i, data); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, nil
}

func loadPolicyRules(ctx context.Context, pool *pgxpool.Pool, policyID string) ([]*policyv1.PolicyRule, error) {
	const q = `SELECT rule_json FROM policy_rules WHERE policy_id = $1 ORDER BY position`

	rows, err := pool.Query(ctx, q, policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []*policyv1.PolicyRule
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		r, err := jsonToProtoRule(data)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// itoa converts an int to its decimal string representation.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
