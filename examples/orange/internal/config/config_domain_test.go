package config

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── ModelPricing ──────────────────────────────────────────────────────────────

func TestModelPricing_Cost_Nil(t *testing.T) {
	var p *ModelPricing
	got := p.Cost(1000, 500, 200, 100)
	assert.True(t, got.IsZero(), "nil receiver must return zero")
}

func TestModelPricing_Cost_Basic(t *testing.T) {
	p := &ModelPricing{
		InputMTok:      decimal.RequireFromString("3.00"),
		OutputMTok:     decimal.RequireFromString("15.00"),
		CacheReadMTok:  decimal.RequireFromString("0.30"),
		CacheWriteMTok: decimal.RequireFromString("3.75"),
	}
	// 1M input → $3.00, 1M output → $15.00, 1M cache_read → $0.30, 1M cache_write → $3.75
	got := p.Cost(1_000_000, 1_000_000, 1_000_000, 1_000_000)
	want := decimal.RequireFromString("22.05")
	assert.True(t, want.Equal(got), "want %s got %s", want, got)
}

func TestModelPricing_Cost_SmallCounts(t *testing.T) {
	p := &ModelPricing{
		InputMTok:  decimal.RequireFromString("3.00"),
		OutputMTok: decimal.RequireFromString("15.00"),
	}
	// 100 input tokens at $3/MTok = $0.0003
	// 50 output tokens at $15/MTok = $0.00075
	got := p.Cost(100, 50, 0, 0)
	want := decimal.RequireFromString("0.00105")
	assert.True(t, want.Equal(got), "want %s got %s", want, got)
}

func TestModelPricing_Cost_ZeroCounts(t *testing.T) {
	p := &ModelPricing{
		InputMTok:  decimal.RequireFromString("3.00"),
		OutputMTok: decimal.RequireFromString("15.00"),
	}
	got := p.Cost(0, 0, 0, 0)
	assert.True(t, got.IsZero())
}

func TestModelPricing_Cost_DecimalPrecision(t *testing.T) {
	// Verify decimal arithmetic avoids floating-point drift.
	p := &ModelPricing{
		InputMTok:  decimal.RequireFromString("0.0000001"),
		OutputMTok: decimal.RequireFromString("123456789.123456789"),
	}
	got := p.Cost(1, 1, 0, 0)
	// $0.0000001 / 1M = 0.0000000000001
	// $123456789.123456789 / 1M = 0.123456789123456789
	want := decimal.RequireFromString("0.0000001").
		Div(decimal.NewFromInt(1_000_000)).
		Add(decimal.RequireFromString("123456789.123456789").Div(decimal.NewFromInt(1_000_000)))
	assert.True(t, want.Equal(got), "want %s got %s", want, got)
}

// ── RateLimitRule ─────────────────────────────────────────────────────────────

func TestRateLimitRule_MatchesModel(t *testing.T) {
	tests := []struct {
		name    string
		models  []string
		modelID string
		want    bool
	}{
		{
			name:    "wildcard matches any",
			models:  []string{"*"},
			modelID: "claude-3-5-sonnet",
			want:    true,
		},
		{
			name:    "exact match",
			models:  []string{"gpt-4o", "claude-3-5-sonnet"},
			modelID: "gpt-4o",
			want:    true,
		},
		{
			name:    "no match",
			models:  []string{"gpt-4o", "claude-3-5-sonnet"},
			modelID: "gemini-pro",
			want:    false,
		},
		{
			name:    "empty model list",
			models:  []string{},
			modelID: "anything",
			want:    false,
		},
		{
			name:    "wildcard in list with others",
			models:  []string{"gpt-4o", "*"},
			modelID: "gemini-pro",
			want:    true,
		},
		{
			name:    "partial prefix does not match",
			models:  []string{"claude-3"},
			modelID: "claude-3-5-sonnet",
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := RateLimitRule{Models: tc.models}
			assert.Equal(t, tc.want, r.MatchesModel(tc.modelID))
		})
	}
}

// ── GlobalConfig.ResolveRateLimitRules ────────────────────────────────────────

func TestGlobalConfig_ResolveRateLimitRules_Accumulates(t *testing.T) {
	// GlobalConfig.ResolveRateLimitRules returns workspace and user-scope rules
	// only. Key-scope rules live in KeyRecord.RateLimitRules and are appended by
	// the caller, so the full three-scope accumulation is the caller's job.
	wsRule := RateLimitRule{Models: []string{"*"}, RPM: 100, OnExceed: "reject"}
	userRule := RateLimitRule{Models: []string{"*"}, RPH: 500, OnExceed: "reject"}
	keyRule := RateLimitRule{Models: []string{"claude-3-5-sonnet"}, RPD: 1000, OnExceed: "reject"}

	g := &GlobalConfig{
		RateLimits: map[string][]RateLimitRule{
			"demo":       {wsRule},
			"demo/alice": {userRule},
			// No "demo/alice/k1" entry here — key-scope lives in KeyRecord.
		},
	}
	key := &KeyRecord{RateLimitRules: []RateLimitRule{keyRule}}

	// Admin-owned scopes: workspace + user.
	got := g.ResolveRateLimitRules("demo/alice/k1", "claude-3-5-sonnet")
	require.Len(t, got, 2)
	assert.Equal(t, wsRule, got[0])
	assert.Equal(t, userRule, got[1])

	// Full accumulation: caller appends key-scope rules.
	full := append(got, filterRulesByModel(key.RateLimitRules, "claude-3-5-sonnet")...)
	require.Len(t, full, 3)
	assert.Equal(t, keyRule, full[2])
}

func TestGlobalConfig_ResolveRateLimitRules_ModelFilter(t *testing.T) {
	// Rules at a user scope are filtered by model; non-matching rules are dropped.
	otherRule := RateLimitRule{Models: []string{"gpt-4o"}, RPM: 10}
	matchRule := RateLimitRule{Models: []string{"claude-3-5-sonnet"}, RPM: 50}

	g := &GlobalConfig{
		RateLimits: map[string][]RateLimitRule{
			"demo/alice": {otherRule, matchRule},
		},
	}

	got := g.ResolveRateLimitRules("demo/alice/k1", "claude-3-5-sonnet")
	require.Len(t, got, 1)
	assert.Equal(t, matchRule, got[0])
}

func TestGlobalConfig_ResolveRateLimitRules_MalformedKeyID(t *testing.T) {
	g := &GlobalConfig{RateLimits: map[string][]RateLimitRule{}}
	assert.Nil(t, g.ResolveRateLimitRules("bad", "any-model"))
	assert.Nil(t, g.ResolveRateLimitRules("a/b", "any-model"))
}

func TestGlobalConfig_ResolveRateLimitRules_NoRules(t *testing.T) {
	g := &GlobalConfig{RateLimits: map[string][]RateLimitRule{}}
	got := g.ResolveRateLimitRules("demo/alice/k1", "claude-3-5-sonnet")
	assert.Empty(t, got)
}

func TestGlobalConfig_ResolveRateLimitRules_WildcardModel(t *testing.T) {
	// A wildcard rule at workspace scope should propagate to every model.
	wsRule := RateLimitRule{Models: []string{"*"}, RPD: 10000}

	g := &GlobalConfig{
		RateLimits: map[string][]RateLimitRule{
			"demo": {wsRule},
		},
	}

	got := g.ResolveRateLimitRules("demo/alice/k1", "any-model")
	require.Len(t, got, 1)
	assert.Equal(t, wsRule, got[0])
}

// ── filterRulesByModel ────────────────────────────────────────────────────────

func TestFilterRulesByModel(t *testing.T) {
	rules := []RateLimitRule{
		{Models: []string{"*"}, RPM: 1},
		{Models: []string{"gpt-4o"}, RPM: 2},
		{Models: []string{"claude-3-5-sonnet"}, RPM: 3},
	}
	got := filterRulesByModel(rules, "gpt-4o")
	require.Len(t, got, 2)
	assert.Equal(t, 1, got[0].RPM)
	assert.Equal(t, 2, got[1].RPM)
}

func TestFilterRulesByModel_Empty(t *testing.T) {
	assert.Empty(t, filterRulesByModel(nil, "x"))
	assert.Empty(t, filterRulesByModel([]RateLimitRule{}, "x"))
}

// ── RoutingPool ───────────────────────────────────────────────────────────────

func newTestRoutingPool() *RoutingPool {
	return &RoutingPool{index: make(map[string]uint32)}
}

func makeTargetConfig(providerName string) RoutingConfig {
	return RoutingConfig{
		Kind: RoutingKindTarget,
		Target: &RoutingTarget{
			Provider:  &ProviderRecord{Kind: ProviderKind(providerName)},
			ModelName: providerName + "-model",
		},
	}
}

func TestRoutingPool_Intern_Dedup(t *testing.T) {
	p := newTestRoutingPool()
	cfg := makeTargetConfig("anthropic")

	id0 := p.Intern("key-a", cfg)
	id1 := p.Intern("key-a", cfg) // same key → same id
	id2 := p.Intern("key-b", makeTargetConfig("openai"))

	assert.Equal(t, id0, id1, "repeated Intern with same key must return same id")
	assert.NotEqual(t, id0, id2, "different keys must return different ids")
}

func TestRoutingPool_Get(t *testing.T) {
	p := newTestRoutingPool()
	cfg := makeTargetConfig("anthropic")
	id := p.Intern("key-a", cfg)

	got := p.Get(id)
	require.NotNil(t, got)
	assert.Equal(t, RoutingKindTarget, got.Kind)
}

func TestRoutingPool_Get_OutOfRange(t *testing.T) {
	p := newTestRoutingPool()
	assert.Nil(t, p.Get(999))
}

func TestRoutingPool_GetByKey(t *testing.T) {
	p := newTestRoutingPool()
	cfg := makeTargetConfig("anthropic")
	p.Intern("my-key", cfg)

	got := p.GetByKey("my-key")
	require.NotNil(t, got)
	assert.Equal(t, RoutingKindTarget, got.Kind)

	assert.Nil(t, p.GetByKey("missing"))
}

// ── ToolFilterPool ────────────────────────────────────────────────────────────

func newTestToolFilterPool() *ToolFilterPool {
	return &ToolFilterPool{index: make(map[string]uint32)}
}

func TestToolFilterPool_Intern_Dedup(t *testing.T) {
	p := newTestToolFilterPool()
	filters := []ToolFilter{{ServerID: "srv-a", Include: []string{"tool1"}}}

	id0 := p.Intern("k", filters)
	id1 := p.Intern("k", filters)
	assert.Equal(t, id0, id1)
}

func TestToolFilterPool_Get(t *testing.T) {
	p := newTestToolFilterPool()
	filters := []ToolFilter{{ServerID: "srv-a", Include: []string{"tool1", "tool2"}, Optional: true}}
	id := p.Intern("k", filters)

	got := p.Get(id)
	require.Len(t, got, 1)
	assert.Equal(t, "srv-a", got[0].ServerID)
	assert.True(t, got[0].Optional)
}

func TestToolFilterPool_Get_OutOfRange(t *testing.T) {
	p := newTestToolFilterPool()
	assert.Nil(t, p.Get(0))
}

func TestToolFilterPool_GetByKey(t *testing.T) {
	p := newTestToolFilterPool()
	filters := []ToolFilter{{ServerID: "srv-a"}}
	p.Intern("key", filters)

	assert.NotNil(t, p.GetByKey("key"))
	assert.Nil(t, p.GetByKey("nope"))
}

// ── AuthPool ──────────────────────────────────────────────────────────────────

func newTestAuthPool() *AuthPool {
	return &AuthPool{index: make(map[string]uint32)}
}

func TestAuthPool_Intern_Dedup(t *testing.T) {
	p := newTestAuthPool()
	overrides := []AuthOverride{{ServerID: "srv-a", Auth: AuthConfig{Type: "bearer", SecretRef: "env://TOKEN"}}}

	id0 := p.Intern("k", overrides)
	id1 := p.Intern("k", overrides)
	assert.Equal(t, id0, id1)
}

func TestAuthPool_Get(t *testing.T) {
	p := newTestAuthPool()
	overrides := []AuthOverride{{ServerID: "srv-a", Auth: AuthConfig{Type: "bearer", SecretRef: "env://TOKEN"}}}
	id := p.Intern("k", overrides)

	got := p.Get(id)
	require.Len(t, got, 1)
	assert.Equal(t, "bearer", got[0].Auth.Type)
	assert.Equal(t, "env://TOKEN", got[0].Auth.SecretRef)
}

func TestAuthPool_Get_OutOfRange(t *testing.T) {
	p := newTestAuthPool()
	assert.Nil(t, p.Get(0))
}

func TestAuthPool_GetByKey(t *testing.T) {
	p := newTestAuthPool()
	overrides := []AuthOverride{{ServerID: "srv-b"}}
	p.Intern("key", overrides)

	assert.NotNil(t, p.GetByKey("key"))
	assert.Nil(t, p.GetByKey("absent"))
}
