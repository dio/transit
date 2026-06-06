package config

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// mustDecodeYAML parses YAML into a RawConfig; fatal on error.
func mustDecodeYAML(t *testing.T, src string) *RawConfig {
	t.Helper()
	var raw RawConfig
	require.NoError(t, yaml.Unmarshal([]byte(src), &raw))
	return &raw
}

// mustCompile runs compile with a fresh InternPool and fails the test on error.
func mustCompile(t *testing.T, src string) *ConfigSnapshot {
	t.Helper()
	raw := mustDecodeYAML(t, src)
	snap, err := compile(raw, NewInternPool(), 1)
	require.NoError(t, err)
	return snap
}

// compileReadyYAML extends fullExampleYAML with the four extra providers that
// the keys section references in routing overrides. fullExampleYAML is the
// canonical raw-struct test fixture and intentionally omits these providers to
// keep the raw-layer tests free of compile-layer concerns.
var compileReadyYAML = strings.Replace(
	fullExampleYAML,
	// Insert after the last existing provider's auth line.
	"        secret_ref: env://OPENAI_API_KEY\n",
	"        secret_ref: env://OPENAI_API_KEY\n"+
		"    fallback_p1:\n"+
		"      kind: anthropic\n"+
		"      endpoint: https://fallback.example.com\n"+
		"      auth:\n"+
		"        type: bearer\n"+
		"        secret_ref: env://FB_TOKEN\n"+
		"    split_p1:\n"+
		"      kind: anthropic\n"+
		"      endpoint: https://split1.example.com\n"+
		"      auth:\n"+
		"        type: bearer\n"+
		"        secret_ref: env://S1_TOKEN\n"+
		"    split_p2:\n"+
		"      kind: openai\n"+
		"      endpoint: https://split2.example.com\n"+
		"      auth:\n"+
		"        type: bearer\n"+
		"        secret_ref: env://S2_TOKEN\n"+
		"    split_p3:\n"+
		"      kind: bedrock\n"+
		"      endpoint: https://split3.example.com\n"+
		"      auth:\n"+
		"        type: bearer\n"+
		"        secret_ref: env://S3_TOKEN\n",
	1,
)

// minimalYAML is the smallest valid config: one provider and one model.
const minimalYAML = `
llm:
  providers:
    p1:
      kind: anthropic
      endpoint: https://api.example.com
      auth:
        type: bearer
        secret_ref: env://TOKEN
  models:
    m1:
      provider: p1
mcp:
  servers: {}
`

// ── compile — happy path ───────────────────────────────────────────────────────

func TestCompile_FullExample(t *testing.T) {
	// Compile the full kitchen-sink YAML (extended with all referenced providers)
	// to verify that every section round-trips through compile without error.
	raw := mustDecodeYAML(t, compileReadyYAML)
	snap, err := compile(raw, NewInternPool(), 42)
	require.NoError(t, err)

	assert.Equal(t, uint64(42), snap.Generation)
	require.NotNil(t, snap.Global)
	require.NotNil(t, snap.Pools)
}

func TestCompile_ProviderFields(t *testing.T) {
	snap := mustCompile(t, compileReadyYAML)

	anth, ok := snap.Global.Providers["anthropic"]
	require.True(t, ok)
	assert.Equal(t, ProviderKindAnthropic, anth.Kind)
	assert.Equal(t, "https://api.anthropic.com", anth.Endpoint)
	assert.Equal(t, "anthropic", anth.Auth.Type)
	assert.Equal(t, "env://ANTHROPIC_API_KEY", anth.Auth.SecretRef)
	assert.Equal(t, "2023-06-01", anth.Extra["anthropic_version"])

	vertex, ok := snap.Global.Providers["vertex_anthropic"]
	require.True(t, ok)
	assert.Equal(t, BackendSchemaGCPAnthropic, vertex.BackendSchema)
}

func TestCompile_ModelFields(t *testing.T) {
	snap := mustCompile(t, compileReadyYAML)

	haiku, ok := snap.Global.Models["claude-haiku-4-5"]
	require.True(t, ok)
	assert.Equal(t, "claude-haiku-4-5-20251001", haiku.APIName)
	assert.Equal(t, ProviderKindAnthropic, haiku.Provider.Kind)

	// Endpoint override must resolve to the vertex_anthropic provider pointer.
	ep, ok := haiku.EndpointOverrides["chat_completions"]
	require.True(t, ok)
	assert.Equal(t, BackendSchemaGCPAnthropic, ep.BackendSchema)

	// Pricing must be compiled from decimal strings.
	require.NotNil(t, haiku.Pricing)
	assert.True(t, decimal.RequireFromString("0.80").Equal(haiku.Pricing.InputMTok))
	assert.True(t, decimal.RequireFromString("4.00").Equal(haiku.Pricing.OutputMTok))
}

func TestCompile_ModelAPINameDefaults(t *testing.T) {
	// When raw model has no explicit name, APIName should equal the catalog key.
	snap := mustCompile(t, minimalYAML)
	m, ok := snap.Global.Models["m1"]
	require.True(t, ok)
	assert.Equal(t, "m1", m.APIName)
}

func TestCompile_ServerFields(t *testing.T) {
	snap := mustCompile(t, compileReadyYAML)

	gh, ok := snap.Global.Servers["github"]
	require.True(t, ok)
	assert.Equal(t, "https://api.githubcopilot.com/mcp/", gh.Endpoint)
	assert.Equal(t, "github", gh.Namespace)
	require.NotNil(t, gh.Auth)
	assert.Equal(t, "bearer", gh.Auth.Type)
}

func TestCompile_RateLimitFields(t *testing.T) {
	snap := mustCompile(t, compileReadyYAML)

	rules, ok := snap.Global.RateLimits["demo"]
	require.True(t, ok)
	require.NotEmpty(t, rules)
	// Default on_exceed must be normalised to "reject".
	assert.Equal(t, "reject", rules[0].OnExceed)
}

func TestCompile_KeyScopeRateLimitsInKeyRecord(t *testing.T) {
	// Key-scope rate-limit rules must be compiled into KeyRecord.RateLimitRules,
	// not into GlobalConfig.RateLimits, so that the key owner's rules are kept
	// separate from admin-managed workspace/user rules.
	const src = `
llm:
  providers:
    p1: {kind: anthropic, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    m1:
      provider: p1
      pricing: {input_mtok: "3.00", output_mtok: "15.00"}
mcp:
  servers: {}
keys:
  "ws/alice/k1": {}
rate_limits:
  ws:
    - models: ["*"]
      rpm: 500
  ws/alice:
    - models: ["*"]
      rpm: 100
  ws/alice/k1:
    - models: ["m1"]
      usd_per_day: "5.00"
      on_exceed: throttle
    - models: ["*"]
      rpm: 20
`
	snap := mustCompile(t, src)

	// Admin scopes must appear in GlobalConfig.RateLimits.
	assert.Contains(t, snap.Global.RateLimits, "ws")
	assert.Contains(t, snap.Global.RateLimits, "ws/alice")
	// Key scope must NOT appear in GlobalConfig.RateLimits.
	assert.NotContains(t, snap.Global.RateLimits, "ws/alice/k1",
		"key-scope rules must live in KeyRecord, not GlobalConfig")

	// Key-scope rules must be in the KeyRecord.
	key := snap.Keys["ws/alice/k1"]
	require.NotNil(t, key)
	require.Len(t, key.RateLimitRules, 2)
	assert.Equal(t, "throttle", key.RateLimitRules[0].OnExceed)
	assert.True(t, decimal.RequireFromString("5.00").Equal(key.RateLimitRules[0].USDPerDay))
	assert.Equal(t, 20, key.RateLimitRules[1].RPM)
	assert.Equal(t, "reject", key.RateLimitRules[1].OnExceed) // defaulted
}

func TestCompile_KeyWithNoRateLimits(t *testing.T) {
	// A key with no rate-limit entry must compile cleanly with nil RateLimitRules.
	snap := mustCompile(t, minimalYAML+"\nkeys:\n  \"ws/u/k1\": {}\n")
	key := snap.Keys["ws/u/k1"]
	require.NotNil(t, key)
	assert.Nil(t, key.RateLimitRules)
}

func TestCompile_KeyScopeRateLimitUSDWithoutPricing(t *testing.T) {
	// Key-scope USD rules without a pricing block must be rejected at compile time.
	const src = `
llm:
  providers:
    p1: {kind: anthropic, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    m1: {provider: p1}
mcp:
  servers: {}
keys:
  "ws/alice/k1": {}
rate_limits:
  ws/alice/k1:
    - models: ["m1"]
      usd_per_day: "1.00"
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usd limit requires pricing block")
}

func TestCompile_KeysNilWhenAbsent(t *testing.T) {
	// A config with no keys: section should produce nil Keys on the snapshot.
	snap := mustCompile(t, minimalYAML)
	assert.Nil(t, snap.Keys)
	assert.Nil(t, snap.Profiles)
}

func TestCompile_KeysCompiled(t *testing.T) {
	snap := mustCompile(t, compileReadyYAML)
	require.NotNil(t, snap.Keys)
	_, ok := snap.Keys["demo/adi/sk-direct"]
	assert.True(t, ok, "expected demo/adi/sk-direct in compiled keys")
}

func TestCompile_ProfilesCompiled(t *testing.T) {
	snap := mustCompile(t, compileReadyYAML)
	require.NotNil(t, snap.Profiles)
	_, ok := snap.Profiles["demo/adi/default"]
	assert.True(t, ok, "expected demo/adi/default in compiled profiles")
}

func TestCompile_RoutingPoolDedup(t *testing.T) {
	// Two keys with identical routing nodes must share the same pool entry.
	const src = `
llm:
  providers:
    p1:
      kind: anthropic
      endpoint: https://api.example.com
      auth: {type: bearer, secret_ref: env://T}
  models:
    m1: {provider: p1}
mcp:
  servers: {}
keys:
  "ws/u/k1":
    routing_overrides:
      m1:
        target: {provider: p1}
  "ws/u/k2":
    routing_overrides:
      m1:
        target: {provider: p1}
`
	snap := mustCompile(t, src)
	k1 := snap.Keys["ws/u/k1"]
	k2 := snap.Keys["ws/u/k2"]
	require.NotNil(t, k1)
	require.NotNil(t, k2)
	// Same raw node → same shape key → same pool slot.
	assert.Equal(t, k1.RoutingShapeKeys["m1"], k2.RoutingShapeKeys["m1"])
}

// ── compile — provider errors ─────────────────────────────────────────────────

func TestCompile_UnknownProviderKind(t *testing.T) {
	const src = `
llm:
  providers:
    bad:
      kind: foobar
      endpoint: https://x
      auth: {type: bearer, secret_ref: env://T}
  models: {}
mcp:
  servers: {}
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown kind")
}

func TestCompile_ModelUnknownProvider(t *testing.T) {
	const src = `
llm:
  providers:
    p1:
      kind: anthropic
      endpoint: https://x
      auth: {type: bearer, secret_ref: env://T}
  models:
    m1:
      provider: missing_provider
mcp:
  servers: {}
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown provider")
}

func TestCompile_ModelUnknownEndpointOverrideProvider(t *testing.T) {
	const src = `
llm:
  providers:
    p1:
      kind: anthropic
      endpoint: https://x
      auth: {type: bearer, secret_ref: env://T}
  models:
    m1:
      provider: p1
      endpoint_overrides:
        chat_completions: nonexistent
mcp:
  servers: {}
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint_override")
	assert.Contains(t, err.Error(), "unknown provider")
}

// ── compile — rate limit errors ───────────────────────────────────────────────

func TestCompile_RateLimitEmptyModels(t *testing.T) {
	const src = `
llm:
  providers:
    p1: {kind: openai, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    m1: {provider: p1}
mcp:
  servers: {}
rate_limits:
  demo:
    - models: []
      rpm: 10
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "models must not be empty")
}

func TestCompile_RateLimitInvalidScopeKey(t *testing.T) {
	const src = `
llm:
  providers:
    p1: {kind: openai, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    m1: {provider: p1}
mcp:
  servers: {}
rate_limits:
  "a/b/c/d":
    - models: ["*"]
      rpm: 10
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too many segments")
}

func TestCompile_RateLimitUSDWithoutPricing(t *testing.T) {
	const src = `
llm:
  providers:
    p1: {kind: openai, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    m1: {provider: p1}
mcp:
  servers: {}
rate_limits:
  demo:
    - models: ["m1"]
      usd_per_day: "1.00"
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usd limit requires pricing block")
}

func TestCompile_RateLimitInvalidOnExceed(t *testing.T) {
	const src = `
llm:
  providers:
    p1: {kind: openai, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    m1: {provider: p1}
mcp:
  servers: {}
rate_limits:
  demo:
    - models: ["*"]
      rpm: 10
      on_exceed: blow_up
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid on_exceed")
}

// ── compile — key and profile errors ─────────────────────────────────────────

func TestCompile_KeyInvalidID(t *testing.T) {
	const src = `
llm:
  providers:
    p1: {kind: anthropic, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    m1: {provider: p1}
mcp:
  servers: {}
keys:
  "bad-id":
    routing_overrides: {}
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keys[")
}

func TestCompile_KeyUnknownProviderInRouting(t *testing.T) {
	const src = `
llm:
  providers:
    p1: {kind: anthropic, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    m1: {provider: p1}
mcp:
  servers: {}
keys:
  "ws/u/k1":
    routing_overrides:
      m1:
        target: {provider: ghost}
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown provider")
}

func TestCompile_ProfileInvalidID(t *testing.T) {
	const src = `
llm:
  providers:
    p1: {kind: anthropic, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    m1: {provider: p1}
mcp:
  servers:
    s1: {endpoint: https://s, namespace: ns}
profiles:
  "only-two/segments":
    tools:
      s1: {}
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "profiles[")
}

func TestCompile_ProfileUnknownServer(t *testing.T) {
	const src = `
llm:
  providers:
    p1: {kind: anthropic, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    m1: {provider: p1}
mcp:
  servers: {}
profiles:
  "ws/u/p1":
    tools:
      ghost_server: {}
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown server")
}

func TestCompile_ProfileToolNotInAllowlist(t *testing.T) {
	const src = `
llm:
  providers:
    p1: {kind: anthropic, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    m1: {provider: p1}
mcp:
  servers:
    s1:
      endpoint: https://s
      namespace: ns
      tools_include: [allowed_tool]
profiles:
  "ws/u/p1":
    tools:
      s1:
        include: [allowed_tool, forbidden_tool]
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in server allowlist")
}

// ── compileRoutingNode ────────────────────────────────────────────────────────

func makeProviders() map[string]*ProviderRecord {
	return map[string]*ProviderRecord{
		"p1": {Kind: ProviderKindAnthropic},
		"p2": {Kind: ProviderKindOpenAI},
		"p3": {Kind: ProviderKindBedrock},
	}
}

func TestCompileRoutingNode_Target(t *testing.T) {
	node := RawRoutingNode{Target: &RawRoutingTarget{Provider: "p1", Name: "claude-haiku"}}
	cfg, key, err := compileRoutingNode(node, makeProviders())
	require.NoError(t, err)
	assert.Equal(t, RoutingKindTarget, cfg.Kind)
	require.NotNil(t, cfg.Target)
	assert.Equal(t, ProviderKindAnthropic, cfg.Target.Provider.Kind)
	assert.Equal(t, "claude-haiku", cfg.Target.ModelName)
	assert.NotEmpty(t, key)
}

func TestCompileRoutingNode_Chain(t *testing.T) {
	node := RawRoutingNode{
		Chain: &RawChain{
			Retry: &RawRetry{RetryOn: "5xx", PerTryTimeoutMs: 5000},
			Children: []RawRoutingNode{
				{Target: &RawRoutingTarget{Provider: "p1"}},
				{Target: &RawRoutingTarget{Provider: "p2"}},
			},
		},
	}
	cfg, key, err := compileRoutingNode(node, makeProviders())
	require.NoError(t, err)
	assert.Equal(t, RoutingKindChain, cfg.Kind)
	require.NotNil(t, cfg.Chain)
	assert.Len(t, cfg.Chain.Children, 2)
	assert.Equal(t, "5xx", cfg.Chain.Retry.RetryOn)
	assert.Equal(t, 5000, cfg.Chain.Retry.PerTryTimeoutMs)
	assert.NotEmpty(t, key)
}

func TestCompileRoutingNode_Split(t *testing.T) {
	node := RawRoutingNode{
		Split: &RawSplit{
			Children: []RawSplitChild{
				{Weight: 60, RawRoutingNode: RawRoutingNode{Target: &RawRoutingTarget{Provider: "p1"}}},
				{Weight: 40, RawRoutingNode: RawRoutingNode{Target: &RawRoutingTarget{Provider: "p2"}}},
			},
		},
	}
	cfg, key, err := compileRoutingNode(node, makeProviders())
	require.NoError(t, err)
	assert.Equal(t, RoutingKindSplit, cfg.Kind)
	require.NotNil(t, cfg.Split)
	assert.Len(t, cfg.Split.Children, 2)
	assert.Equal(t, 60, cfg.Split.Children[0].Weight)
	assert.NotEmpty(t, key)
}

func TestCompileRoutingNode_ChainOfSplit(t *testing.T) {
	// chain → [split, target] is a valid composition.
	node := RawRoutingNode{
		Chain: &RawChain{
			Children: []RawRoutingNode{
				{Split: &RawSplit{Children: []RawSplitChild{
					{Weight: 50, RawRoutingNode: RawRoutingNode{Target: &RawRoutingTarget{Provider: "p1"}}},
					{Weight: 50, RawRoutingNode: RawRoutingNode{Target: &RawRoutingTarget{Provider: "p2"}}},
				}}},
				{Target: &RawRoutingTarget{Provider: "p3"}},
			},
		},
	}
	cfg, _, err := compileRoutingNode(node, makeProviders())
	require.NoError(t, err)
	assert.Equal(t, RoutingKindChain, cfg.Kind)
	assert.Equal(t, RoutingKindSplit, cfg.Chain.Children[0].Kind)
}

func TestCompileRoutingNode_SplitOfChains(t *testing.T) {
	// split → [chain, chain] is a valid composition.
	node := RawRoutingNode{
		Split: &RawSplit{
			Children: []RawSplitChild{
				{Weight: 60, RawRoutingNode: RawRoutingNode{Chain: &RawChain{
					Children: []RawRoutingNode{
						{Target: &RawRoutingTarget{Provider: "p1"}},
						{Target: &RawRoutingTarget{Provider: "p2"}},
					},
				}}},
				{Weight: 40, RawRoutingNode: RawRoutingNode{Chain: &RawChain{
					Children: []RawRoutingNode{
						{Target: &RawRoutingTarget{Provider: "p3"}},
					},
				}}},
			},
		},
	}
	cfg, _, err := compileRoutingNode(node, makeProviders())
	require.NoError(t, err)
	assert.Equal(t, RoutingKindSplit, cfg.Kind)
	assert.Equal(t, RoutingKindChain, cfg.Split.Children[0].Child.Kind)
}

func TestCompileRoutingNode_ShapeKeyDedup(t *testing.T) {
	// Two identical raw nodes must produce the same shape key.
	node := RawRoutingNode{Target: &RawRoutingTarget{Provider: "p1"}}
	_, key1, err := compileRoutingNode(node, makeProviders())
	require.NoError(t, err)
	_, key2, err := compileRoutingNode(node, makeProviders())
	require.NoError(t, err)
	assert.Equal(t, key1, key2)
}

func TestCompileRoutingNode_Error_NoneSet(t *testing.T) {
	_, _, err := compileRoutingNode(RawRoutingNode{}, makeProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one")
}

func TestCompileRoutingNode_Error_MultipleSet(t *testing.T) {
	node := RawRoutingNode{
		Target: &RawRoutingTarget{Provider: "p1"},
		Chain:  &RawChain{Children: []RawRoutingNode{{Target: &RawRoutingTarget{Provider: "p1"}}}},
	}
	_, _, err := compileRoutingNode(node, makeProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one")
}

func TestCompileRoutingNode_Error_ChainOfChain(t *testing.T) {
	inner := RawRoutingNode{
		Chain: &RawChain{Children: []RawRoutingNode{
			{Target: &RawRoutingTarget{Provider: "p1"}},
		}},
	}
	node := RawRoutingNode{
		Chain: &RawChain{Children: []RawRoutingNode{inner}},
	}
	_, _, err := compileRoutingNode(node, makeProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chain-of-chain")
}

func TestCompileRoutingNode_Error_SplitOfSplit(t *testing.T) {
	inner := RawRoutingNode{
		Split: &RawSplit{Children: []RawSplitChild{
			{Weight: 100, RawRoutingNode: RawRoutingNode{Target: &RawRoutingTarget{Provider: "p1"}}},
		}},
	}
	node := RawRoutingNode{
		Split: &RawSplit{Children: []RawSplitChild{
			{Weight: 100, RawRoutingNode: inner},
		}},
	}
	_, _, err := compileRoutingNode(node, makeProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "split-of-split")
}

func TestCompileRoutingNode_Error_SplitWeightNot100(t *testing.T) {
	node := RawRoutingNode{
		Split: &RawSplit{Children: []RawSplitChild{
			{Weight: 60, RawRoutingNode: RawRoutingNode{Target: &RawRoutingTarget{Provider: "p1"}}},
			{Weight: 30, RawRoutingNode: RawRoutingNode{Target: &RawRoutingTarget{Provider: "p2"}}},
		}},
	}
	_, _, err := compileRoutingNode(node, makeProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must sum to 100")
}

func TestCompileRoutingNode_Error_SplitNonPositiveWeight(t *testing.T) {
	node := RawRoutingNode{
		Split: &RawSplit{Children: []RawSplitChild{
			{Weight: 0, RawRoutingNode: RawRoutingNode{Target: &RawRoutingTarget{Provider: "p1"}}},
			{Weight: 100, RawRoutingNode: RawRoutingNode{Target: &RawRoutingTarget{Provider: "p2"}}},
		}},
	}
	_, _, err := compileRoutingNode(node, makeProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "weight must be positive")
}

func TestCompileRoutingNode_Error_ChainEmpty(t *testing.T) {
	node := RawRoutingNode{Chain: &RawChain{Children: nil}}
	_, _, err := compileRoutingNode(node, makeProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one child")
}

func TestCompileRoutingNode_Error_UnknownProvider(t *testing.T) {
	node := RawRoutingNode{Target: &RawRoutingTarget{Provider: "ghost"}}
	_, _, err := compileRoutingNode(node, makeProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown provider")
}

// ── validateScopeKey ──────────────────────────────────────────────────────────

func TestValidateScopeKey(t *testing.T) {
	tests := []struct {
		scope   string
		wantErr bool
	}{
		{"demo", false},
		{"demo/alice", false},
		{"demo/alice/sk-001", false},
		{"demo/alice/sk/extra", true}, // 4 segments
		{"", true},                    // empty
		{"/alice", true},              // empty first segment
		{"demo//sk", true},            // empty middle segment
	}
	for _, tc := range tests {
		t.Run(tc.scope, func(t *testing.T) {
			err := validateScopeKey(tc.scope)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ── validateUSDDependency ─────────────────────────────────────────────────────

func TestValidateUSDDependency_NoUSDLimits(t *testing.T) {
	r := RawRateLimitRule{Models: []string{"m1"}, RPM: 10}
	models := map[string]*ModelRecord{"m1": {}} // no pricing — must not matter
	assert.NoError(t, validateUSDDependency(r, models, "scope", 0))
}

func TestValidateUSDDependency_NoPricingBlock(t *testing.T) {
	r := RawRateLimitRule{
		Models:     []string{"m1"},
		USDPerDay:  decimal.RequireFromString("5.00"),
	}
	models := map[string]*ModelRecord{"m1": {Pricing: nil}}
	err := validateUSDDependency(r, models, "demo", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usd limit requires pricing block")
}

func TestValidateUSDDependency_WithPricing(t *testing.T) {
	r := RawRateLimitRule{
		Models:    []string{"m1"},
		USDPerDay: decimal.RequireFromString("5.00"),
	}
	models := map[string]*ModelRecord{
		"m1": {Pricing: &ModelPricing{
			InputMTok:  decimal.RequireFromString("3.00"),
			OutputMTok: decimal.RequireFromString("15.00"),
		}},
	}
	assert.NoError(t, validateUSDDependency(r, models, "demo", 0))
}

func TestValidateUSDDependency_WildcardAllPriced(t *testing.T) {
	r := RawRateLimitRule{
		Models:    []string{"*"},
		USDPerDay: decimal.RequireFromString("5.00"),
	}
	models := map[string]*ModelRecord{
		"m1": {Pricing: &ModelPricing{InputMTok: decimal.RequireFromString("1.00"), OutputMTok: decimal.RequireFromString("2.00")}},
	}
	assert.NoError(t, validateUSDDependency(r, models, "demo", 0))
}

func TestValidateUSDDependency_WildcardMissingPricing(t *testing.T) {
	r := RawRateLimitRule{
		Models:    []string{"*"},
		USDPerDay: decimal.RequireFromString("5.00"),
	}
	models := map[string]*ModelRecord{
		"m1": {Pricing: nil},
	}
	err := validateUSDDependency(r, models, "demo", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usd limit requires pricing block")
}

func TestValidateUSDDependency_ModelNotFound(t *testing.T) {
	r := RawRateLimitRule{
		Models:    []string{"ghost"},
		USDPerDay: decimal.RequireFromString("5.00"),
	}
	models := map[string]*ModelRecord{}
	err := validateUSDDependency(r, models, "demo", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model \"ghost\" not found")
}

// ── compileRateLimitRule ──────────────────────────────────────────────────────

func TestCompileRateLimitRule_DefaultsOnExceed(t *testing.T) {
	r := RawRateLimitRule{Models: []string{"*"}, RPM: 100}
	compiled, err := compileRateLimitRule(r)
	require.NoError(t, err)
	assert.Equal(t, "reject", compiled.OnExceed)
}

func TestCompileRateLimitRule_ValidOnExceedValues(t *testing.T) {
	for _, v := range []string{"reject", "throttle", "log_only"} {
		r := RawRateLimitRule{Models: []string{"*"}, OnExceed: v}
		compiled, err := compileRateLimitRule(r)
		require.NoError(t, err, "value %q should be valid", v)
		assert.Equal(t, v, compiled.OnExceed)
	}
}

func TestCompileRateLimitRule_InvalidOnExceed(t *testing.T) {
	r := RawRateLimitRule{Models: []string{"*"}, OnExceed: "explode"}
	_, err := compileRateLimitRule(r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid on_exceed")
}

func TestCompileRateLimitRule_ClonesModels(t *testing.T) {
	models := []string{"m1", "m2"}
	r := RawRateLimitRule{Models: models}
	compiled, err := compileRateLimitRule(r)
	require.NoError(t, err)
	// Mutating the original slice must not affect the compiled rule.
	models[0] = "mutated"
	assert.Equal(t, "m1", compiled.Models[0])
}

// ── compilePricing / compileMetadata ─────────────────────────────────────────

func TestCompilePricing_Nil(t *testing.T) {
	assert.Nil(t, compilePricing(nil))
}

func TestCompilePricing_NonNil(t *testing.T) {
	raw := &RawModelPricing{
		InputMTok:  decimal.RequireFromString("3.00"),
		OutputMTok: decimal.RequireFromString("15.00"),
	}
	p := compilePricing(raw)
	require.NotNil(t, p)
	assert.True(t, decimal.RequireFromString("3.00").Equal(p.InputMTok))
}

func TestCompileMetadata_Nil(t *testing.T) {
	assert.Nil(t, compileMetadata(nil))
}

func TestCompileMetadata_NonNil(t *testing.T) {
	raw := &RawMetadata{
		Description:   "a model",
		ContextLength: 128000,
		MaxTokens:     4096,
		Tags:          []string{"fast", "cheap"},
	}
	m := compileMetadata(raw)
	require.NotNil(t, m)
	assert.Equal(t, "a model", m.Description)
	assert.Equal(t, 128000, m.ContextLength)
	assert.Equal(t, []string{"fast", "cheap"}, m.Tags)
}

func TestCompileMetadata_ClonesTagSlice(t *testing.T) {
	tags := []string{"fast"}
	m := compileMetadata(&RawMetadata{Tags: tags})
	tags[0] = "mutated"
	assert.Equal(t, "fast", m.Tags[0])
}

// ── cloneStringMap ────────────────────────────────────────────────────────────

func TestCloneStringMap_Nil(t *testing.T) {
	assert.Nil(t, cloneStringMap(nil))
}

func TestCloneStringMap_NonNil(t *testing.T) {
	original := map[string]string{"k": "v"}
	clone := cloneStringMap(original)
	assert.Equal(t, original, clone)
	// Mutation of original must not affect clone.
	original["k"] = "mutated"
	assert.Equal(t, "v", clone["k"])
}

// ── compileProfileShapes / buildShapeKeys ─────────────────────────────────────

func makeServers() map[string]*ServerRecord {
	return map[string]*ServerRecord{
		"s1": {Endpoint: "https://s1", Namespace: "ns1", ToolsInclude: []string{"tool_a", "tool_b"}},
		"s2": {Endpoint: "https://s2", Namespace: "ns2"},
	}
}

func makePools() *Pools {
	return &Pools{
		ToolFilters: &ToolFilterPool{index: make(map[string]uint32)},
		Auth:        &AuthPool{index: make(map[string]uint32)},
		Routing:     &RoutingPool{index: make(map[string]uint32)},
	}
}

func TestCompileProfileShapes_Happy(t *testing.T) {
	profile := RawProfile{
		Tools: map[string]RawToolFilter{
			"s1": {Include: []string{"tool_a"}, Optional: false},
		},
		Auth: map[string]RawAuth{
			"s2": {Type: "bearer", SecretRef: "env://TOK"},
		},
	}
	pools := makePools()
	toolKey, authKey, err := compileProfileShapes(profile, makeServers(), pools)
	require.NoError(t, err)
	assert.NotEmpty(t, toolKey)
	assert.NotEmpty(t, authKey)

	// Keys must resolve to the interned filter slices.
	filters := pools.ToolFilters.GetByKey(toolKey)
	require.Len(t, filters, 1)
	assert.Equal(t, "s1", filters[0].ServerID)
	assert.Equal(t, []string{"tool_a"}, filters[0].Include)

	overrides := pools.Auth.GetByKey(authKey)
	require.Len(t, overrides, 1)
	assert.Equal(t, "s2", overrides[0].ServerID)
	assert.Equal(t, "bearer", overrides[0].Auth.Type)
}

func TestCompileProfileShapes_EmptyProfile(t *testing.T) {
	// A profile with no tools and no auth must produce empty shape keys (not panic).
	profile := RawProfile{}
	pools := makePools()
	toolKey, authKey, err := compileProfileShapes(profile, makeServers(), pools)
	require.NoError(t, err)
	assert.Empty(t, toolKey)
	assert.Empty(t, authKey)
}

func TestCompileProfileShapes_ToolSubsetValidation(t *testing.T) {
	profile := RawProfile{
		Tools: map[string]RawToolFilter{
			"s1": {Include: []string{"tool_a", "tool_c"}}, // tool_c not in s1 allowlist
		},
	}
	_, _, err := compileProfileShapes(profile, makeServers(), makePools())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in server allowlist")
}

func TestCompileProfileShapes_UnknownServer(t *testing.T) {
	profile := RawProfile{
		Tools: map[string]RawToolFilter{
			"ghost": {Include: []string{"tool"}},
		},
	}
	_, _, err := compileProfileShapes(profile, makeServers(), makePools())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown server")
}

func TestCompileProfileShapes_UnknownAuthServer(t *testing.T) {
	profile := RawProfile{
		Auth: map[string]RawAuth{
			"ghost": {Type: "bearer", SecretRef: "env://X"},
		},
	}
	_, _, err := compileProfileShapes(profile, makeServers(), makePools())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown server")
}

func TestCompileProfileShapes_Dedup(t *testing.T) {
	// Two profiles with identical tool filters must share the same pool entry.
	profile := RawProfile{
		Tools: map[string]RawToolFilter{
			"s1": {Include: []string{"tool_a"}},
		},
	}
	pools := makePools()
	key1, _, err := compileProfileShapes(profile, makeServers(), pools)
	require.NoError(t, err)
	key2, _, err := compileProfileShapes(profile, makeServers(), pools)
	require.NoError(t, err)
	assert.Equal(t, key1, key2)
	// The pool must contain exactly one entry despite two Intern calls.
	assert.Len(t, pools.ToolFilters.entries, 1)
}

func TestBuildToolFilterShapeKey_Deterministic(t *testing.T) {
	// Inserting filters in two different orders must produce the same key after sorting.
	f1 := []ToolFilter{{ServerID: "a", Include: []string{"x"}}, {ServerID: "b"}}
	f2 := []ToolFilter{{ServerID: "b"}, {ServerID: "a", Include: []string{"x"}}}
	// Sort both before calling (mirrors what compileProfileShapes does).
	sortFilters := func(fs []ToolFilter) {
		for i := 0; i < len(fs)-1; i++ {
			for j := i + 1; j < len(fs); j++ {
				if fs[j].ServerID < fs[i].ServerID {
					fs[i], fs[j] = fs[j], fs[i]
				}
			}
		}
	}
	sortFilters(f1)
	sortFilters(f2)
	assert.Equal(t, buildToolFilterShapeKey(f1), buildToolFilterShapeKey(f2))
}

func TestBuildToolFilterShapeKey_Empty(t *testing.T) {
	assert.Empty(t, buildToolFilterShapeKey(nil))
	assert.Empty(t, buildToolFilterShapeKey([]ToolFilter{}))
}

func TestBuildAuthShapeKey_Empty(t *testing.T) {
	assert.Empty(t, buildAuthShapeKey(nil))
	assert.Empty(t, buildAuthShapeKey([]AuthOverride{}))
}

// ── InternPool integration ────────────────────────────────────────────────────

func TestCompile_InternsWorkspaceAndUser(t *testing.T) {
	// After compiling two keys in the same workspace/user, intern IDs must be shared.
	const src = `
llm:
  providers:
    p1: {kind: anthropic, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    m1: {provider: p1}
mcp:
  servers: {}
keys:
  "ws/alice/k1": {}
  "ws/alice/k2": {}
`
	raw := mustDecodeYAML(t, src)
	interns := NewInternPool()
	snap, err := compile(raw, interns, 1)
	require.NoError(t, err)

	k1 := snap.Keys["ws/alice/k1"]
	k2 := snap.Keys["ws/alice/k2"]
	require.NotNil(t, k1)
	require.NotNil(t, k2)
	assert.Equal(t, k1.Workspace, k2.Workspace, "same workspace intern ID")
	assert.Equal(t, k1.User, k2.User, "same user intern ID")
	assert.NotEqual(t, k1.Name, k2.Name, "different name intern IDs")
	assert.Equal(t, "ws", interns.Lookup(k1.Workspace))
	assert.Equal(t, "alice", interns.Lookup(k1.User))
}

// ── server auth field ─────────────────────────────────────────────────────────

func TestCompile_ServerWithoutAuth(t *testing.T) {
	const src = `
llm:
  providers:
    p1: {kind: anthropic, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    m1: {provider: p1}
mcp:
  servers:
    s1:
      endpoint: https://s
      namespace: ns
`
	snap := mustCompile(t, src)
	s1, ok := snap.Global.Servers["s1"]
	require.True(t, ok)
	assert.Nil(t, s1.Auth)
}

func TestCompile_ProviderExtraIsCloned(t *testing.T) {
	snap := mustCompile(t, compileReadyYAML)
	anth := snap.Global.Providers["anthropic"]
	require.NotNil(t, anth.Extra)
	// The map on the snapshot must be a copy (no shared reference to raw).
	assert.Equal(t, "2023-06-01", anth.Extra["anthropic_version"])
}

// ── error message format ──────────────────────────────────────────────────────

func TestCompile_ErrorContainsContext(t *testing.T) {
	// Error messages must identify which section and key caused the failure.
	const src = `
llm:
  providers:
    p1: {kind: bedrock, endpoint: https://x, auth: {type: bearer, secret_ref: env://T}}
  models:
    my-model:
      provider: missing
mcp:
  servers: {}
`
	raw := mustDecodeYAML(t, src)
	_, err := compile(raw, NewInternPool(), 1)
	require.Error(t, err)
	// Error must name the offending model.
	assert.True(t,
		strings.Contains(err.Error(), "my-model"),
		"expected model name in error: %s", err,
	)
}
