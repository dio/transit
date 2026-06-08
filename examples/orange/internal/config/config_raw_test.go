package config

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// fullExampleYAML mirrors docs/orange-config.yaml so raw-struct tag mappings
// can be verified against a realistic payload without depending on a file path.
const fullExampleYAML = `
llm:
  providers:
    anthropic:
      kind: anthropic
      endpoint: https://api.anthropic.com
      auth:
        type: anthropic
        secret_ref: env://ANTHROPIC_API_KEY
      extra:
        anthropic_version: "2023-06-01"
    vertex_anthropic:
      kind: anthropic
      backend_schema: gcpanthropic
      endpoint: https://us-east5-aiplatform.googleapis.com
      auth:
        type: gcp
        secret_ref: env://GCP_SERVICE_ACCOUNT_JSON
      extra:
        anthropic_version: "vertex-2023-10-16"
        gcp_project: "env://GCP_PROJECT"
    openai:
      kind: openai
      endpoint: https://api.openai.com
      auth:
        type: bearer
        secret_ref: env://OPENAI_API_KEY
  models:
    claude-haiku-4-5:
      provider: anthropic
      name: claude-haiku-4-5-20251001
      endpoint_overrides:
        chat_completions: vertex_anthropic
      pricing:
        input_mtok: 0.80
        output_mtok: 4.00
        cache_read_mtok: 0.08
        cache_write_mtok: 1.00
    gpt-4o-mini:
      provider: openai
      pricing:
        input_mtok: 0.15
        output_mtok: 0.60
      metadata:
        context_length: 128000
        tags:
          - chat
          - fast
          - vision

mcp:
  servers:
    kiwi:
      endpoint: https://mcp.kiwi.com
      namespace: kiwi
      tools_include:
        - search-flight
    github:
      endpoint: https://api.githubcopilot.com/mcp/
      namespace: github
      auth:
        type: bearer
        secret_ref: env://GITHUB_TOKEN
      tools_include:
        - search_repositories
        - get_file_contents

profiles:
  demo/adi/default:
    tools:
      kiwi:
        include:
          - search-flight
      github:
        include:
          - search_repositories
        optional: true
    auth:
      github:
        type: bearer
        secret_ref: env://GITHUB_TOKEN
  demo/adi/kiwi-only:
    tools:
      kiwi: {}

keys:
  demo/adi/sk-direct:
    routing_overrides:
      claude-haiku-4-5:
        target:
          provider: anthropic
          name: claude-haiku-4-5-20251001
  demo/adi/sk-fallback:
    routing_overrides:
      claude-haiku-4-5:
        chain:
          retry:
            retry_on: "connect-failure,5xx"
            per_try_timeout_ms: 10000
          children:
            - target:
                provider: fallback_p1
                name: claude-haiku-4-5
            - target:
                provider: vertex_anthropic
                name: "claude-opus-4@20250514"
  demo/adi/sk-split:
    routing_overrides:
      claude-haiku-4-5:
        split:
          children:
            - weight: 34
              target:
                provider: split_p1
                name: claude-haiku-4-5
            - weight: 33
              target:
                provider: split_p2
                name: claude-haiku-4-5
            - weight: 33
              target:
                provider: split_p3
                name: claude-haiku-4-5
  demo/adi/sk-chain-split:
    routing_overrides:
      claude-haiku-4-5:
        chain:
          retry:
            retry_on: "connect-failure,reset,5xx"
            per_try_timeout_ms: 5000
          children:
            - split:
                children:
                  - weight: 50
                    target:
                      provider: split_p1
                  - weight: 50
                    target:
                      provider: split_p2
            - target:
                provider: fallback_p1
  demo/adi/sk-split-chain:
    routing_overrides:
      claude-haiku-4-5:
        split:
          children:
            - weight: 60
              chain:
                retry:
                  retry_on: "connect-failure,reset,5xx"
                  per_try_timeout_ms: 10000
                children:
                  - target:
                      provider: split_p1
                  - target:
                      provider: split_p2
            - weight: 40
              chain:
                retry:
                  retry_on: "connect-failure,reset,5xx"
                  per_try_timeout_ms: 8000
                children:
                  - target:
                      provider: split_p2
                  - target:
                      provider: split_p3

rate_limit:
  policies:
    demo:
      - models: ["*"]
        usd_per_day: 1000.00
        rpm: 500
    demo/adi:
      - models: ["*"]
        usd_per_day: 200.00
        rpm: 100
    demo/adi/sk-direct:
      - models: [claude-haiku-4-5, gpt-4o-mini]
        usd_per_hour: 5.00
        usd_per_day: 50.00
        input_tokens_per_hour: 800000
        output_tokens_per_hour: 200000
        cache_read_tokens_per_hour: 1000000
        cache_write_tokens_per_hour: 100000
        on_exceed: reject
      - models: ["*"]
        rpm: 20
        on_exceed: reject
`

func mustUnmarshalRaw(t *testing.T, src string) *RawConfig {
	t.Helper()
	var rc RawConfig
	require.NoError(t, yaml.Unmarshal([]byte(src), &rc))
	return &rc
}

// ── Providers ─────────────────────────────────────────────────────────────────

func TestRawConfig_Providers(t *testing.T) {
	rc := mustUnmarshalRaw(t, fullExampleYAML)

	tests := []struct {
		name          string
		kind          string
		backendSchema string
		endpoint      string
		authType      string
		authSecret    string
		extraKey      string
		extraVal      string
	}{
		{
			name: "anthropic", kind: "anthropic", endpoint: "https://api.anthropic.com",
			authType: "anthropic", authSecret: "env://ANTHROPIC_API_KEY",
			extraKey: "anthropic_version", extraVal: "2023-06-01",
		},
		{
			name: "vertex_anthropic", kind: "anthropic", backendSchema: "gcpanthropic",
			endpoint: "https://us-east5-aiplatform.googleapis.com",
			authType: "gcp", authSecret: "env://GCP_SERVICE_ACCOUNT_JSON",
			extraKey: "anthropic_version", extraVal: "vertex-2023-10-16",
		},
		{
			name: "openai", kind: "openai", endpoint: "https://api.openai.com",
			authType: "bearer", authSecret: "env://OPENAI_API_KEY",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := rc.LLM.Providers[tc.name]
			require.True(t, ok, "provider %q not found", tc.name)
			assert.Equal(t, tc.kind, p.Kind)
			assert.Equal(t, tc.backendSchema, p.BackendSchema)
			assert.Equal(t, tc.endpoint, p.Endpoint)
			assert.Equal(t, tc.authType, p.Auth.Type)
			assert.Equal(t, tc.authSecret, p.Auth.SecretRef)
			if tc.extraKey != "" {
				assert.Equal(t, tc.extraVal, p.Extra[tc.extraKey])
			}
		})
	}
}

// ── Models ────────────────────────────────────────────────────────────────────

func TestRawConfig_Model_Pricing(t *testing.T) {
	rc := mustUnmarshalRaw(t, fullExampleYAML)

	m, ok := rc.LLM.Models["claude-haiku-4-5"]
	require.True(t, ok)
	assert.Equal(t, "anthropic", m.Provider)
	assert.Equal(t, "claude-haiku-4-5-20251001", m.Name)
	assert.Equal(t, "vertex_anthropic", m.EndpointOverrides["chat_completions"])

	require.NotNil(t, m.Pricing)
	assert.Equal(t, decimal.RequireFromString("0.80"), m.Pricing.InputMTok)
	assert.Equal(t, decimal.RequireFromString("4.00"), m.Pricing.OutputMTok)
	assert.Equal(t, decimal.RequireFromString("0.08"), m.Pricing.CacheReadMTok)
	assert.Equal(t, decimal.RequireFromString("1.00"), m.Pricing.CacheWriteMTok)
}

func TestRawConfig_Model_Metadata(t *testing.T) {
	rc := mustUnmarshalRaw(t, fullExampleYAML)

	m, ok := rc.LLM.Models["gpt-4o-mini"]
	require.True(t, ok)
	require.NotNil(t, m.Metadata)
	assert.Equal(t, 128000, m.Metadata.ContextLength)
	assert.Equal(t, []string{"chat", "fast", "vision"}, m.Metadata.Tags)
	require.NotNil(t, m.Pricing)
	assert.Equal(t, decimal.RequireFromString("0.15"), m.Pricing.InputMTok)
	assert.Equal(t, decimal.RequireFromString("0.60"), m.Pricing.OutputMTok)
}

func TestRawConfig_Model_NilPricing(t *testing.T) {
	src := `
llm:
  providers:
    p: {kind: openai, endpoint: https://example.com, auth: {type: bearer, secret_ref: env://X}}
  models:
    my-model:
      provider: p
`
	rc := mustUnmarshalRaw(t, src)
	m := rc.LLM.Models["my-model"]
	assert.Nil(t, m.Pricing, "omitted pricing must be nil")
	assert.Nil(t, m.Metadata)
}

// ── MCP servers ───────────────────────────────────────────────────────────────

func TestRawConfig_MCPServers(t *testing.T) {
	rc := mustUnmarshalRaw(t, fullExampleYAML)

	kiwi, ok := rc.MCP.Servers["kiwi"]
	require.True(t, ok)
	assert.Equal(t, "https://mcp.kiwi.com", kiwi.Endpoint)
	assert.Equal(t, "kiwi", kiwi.Namespace)
	assert.Nil(t, kiwi.Auth)
	assert.Equal(t, []string{"search-flight"}, kiwi.ToolsInclude)

	gh, ok := rc.MCP.Servers["github"]
	require.True(t, ok)
	require.NotNil(t, gh.Auth)
	assert.Equal(t, "bearer", gh.Auth.Type)
	assert.Equal(t, []string{"search_repositories", "get_file_contents"}, gh.ToolsInclude)
}

// ── Profiles ─────────────────────────────────────────────────────────────────

func TestRawConfig_Profiles(t *testing.T) {
	rc := mustUnmarshalRaw(t, fullExampleYAML)

	def, ok := rc.Profiles["demo/adi/default"]
	require.True(t, ok)

	kiwi := def.Tools["kiwi"]
	assert.Equal(t, []string{"search-flight"}, kiwi.Include)
	assert.False(t, kiwi.Optional)

	gh := def.Tools["github"]
	assert.Equal(t, []string{"search_repositories"}, gh.Include)
	assert.True(t, gh.Optional)

	authOverride, ok := def.Auth["github"]
	require.True(t, ok)
	assert.Equal(t, "bearer", authOverride.Type)

	kiwiOnly, ok := rc.Profiles["demo/adi/kiwi-only"]
	require.True(t, ok)
	emptyFilter := kiwiOnly.Tools["kiwi"]
	assert.Empty(t, emptyFilter.Include, "empty map value must decode to zero RawToolFilter")
}

// ── Routing nodes ─────────────────────────────────────────────────────────────

func TestRawRoutingNode_Target(t *testing.T) {
	rc := mustUnmarshalRaw(t, fullExampleYAML)

	node := rc.Keys["demo/adi/sk-direct"].RoutingOverrides["claude-haiku-4-5"]
	require.NotNil(t, node.Target)
	assert.Nil(t, node.Chain)
	assert.Nil(t, node.Split)
	assert.Equal(t, "anthropic", node.Target.Provider)
	assert.Equal(t, "claude-haiku-4-5-20251001", node.Target.Name)
}

func TestRawRoutingNode_Chain(t *testing.T) {
	rc := mustUnmarshalRaw(t, fullExampleYAML)

	node := rc.Keys["demo/adi/sk-fallback"].RoutingOverrides["claude-haiku-4-5"]
	require.NotNil(t, node.Chain)
	assert.Nil(t, node.Target)
	assert.Nil(t, node.Split)

	ch := node.Chain
	require.NotNil(t, ch.Retry)
	assert.Equal(t, "connect-failure,5xx", ch.Retry.RetryOn)
	assert.Equal(t, 10000, ch.Retry.PerTryTimeoutMs)

	require.Len(t, ch.Children, 2)
	assert.Equal(t, "fallback_p1", ch.Children[0].Target.Provider)
	assert.Equal(t, "vertex_anthropic", ch.Children[1].Target.Provider)
}

func TestRawRoutingNode_Split(t *testing.T) {
	rc := mustUnmarshalRaw(t, fullExampleYAML)

	node := rc.Keys["demo/adi/sk-split"].RoutingOverrides["claude-haiku-4-5"]
	require.NotNil(t, node.Split)
	assert.Nil(t, node.Target)
	assert.Nil(t, node.Chain)

	children := node.Split.Children
	require.Len(t, children, 3)

	// Weights and inline target extraction via RawSplitChild.
	assert.Equal(t, 34, children[0].Weight)
	require.NotNil(t, children[0].Target)
	assert.Equal(t, "split_p1", children[0].Target.Provider)

	assert.Equal(t, 33, children[1].Weight)
	assert.Equal(t, 33, children[2].Weight)
}

func TestRawRoutingNode_ChainOfSplit(t *testing.T) {
	rc := mustUnmarshalRaw(t, fullExampleYAML)

	node := rc.Keys["demo/adi/sk-chain-split"].RoutingOverrides["claude-haiku-4-5"]
	require.NotNil(t, node.Chain)

	children := node.Chain.Children
	require.Len(t, children, 2)

	// First child is a split.
	require.NotNil(t, children[0].Split)
	splitChildren := children[0].Split.Children
	require.Len(t, splitChildren, 2)
	assert.Equal(t, 50, splitChildren[0].Weight)
	assert.Equal(t, "split_p1", splitChildren[0].Target.Provider)

	// Second child is a target fallback.
	require.NotNil(t, children[1].Target)
	assert.Equal(t, "fallback_p1", children[1].Target.Provider)
}

func TestRawRoutingNode_SplitOfChains(t *testing.T) {
	rc := mustUnmarshalRaw(t, fullExampleYAML)

	node := rc.Keys["demo/adi/sk-split-chain"].RoutingOverrides["claude-haiku-4-5"]
	require.NotNil(t, node.Split)

	arms := node.Split.Children
	require.Len(t, arms, 2)

	// First arm: 60 % chain.
	assert.Equal(t, 60, arms[0].Weight)
	require.NotNil(t, arms[0].Chain)
	assert.Equal(t, 10000, arms[0].Chain.Retry.PerTryTimeoutMs)
	require.Len(t, arms[0].Chain.Children, 2)
	assert.Equal(t, "split_p1", arms[0].Chain.Children[0].Target.Provider)

	// Second arm: 40 % chain.
	assert.Equal(t, 40, arms[1].Weight)
	require.NotNil(t, arms[1].Chain)
	assert.Equal(t, 8000, arms[1].Chain.Retry.PerTryTimeoutMs)
}

// ── Rate limits ───────────────────────────────────────────────────────────────

func TestRawConfig_RateLimits(t *testing.T) {
	rc := mustUnmarshalRaw(t, fullExampleYAML)

	workspace := rc.RateLimit.Policies["demo"]
	require.Len(t, workspace, 1)
	assert.Equal(t, []string{"*"}, workspace[0].Models)
	assert.Equal(t, decimal.RequireFromString("1000.00"), workspace[0].USDPerDay)
	assert.Equal(t, 500, workspace[0].RPM)

	keyRules := rc.RateLimit.Policies["demo/adi/sk-direct"]
	require.Len(t, keyRules, 2)

	first := keyRules[0]
	assert.Equal(t, []string{"claude-haiku-4-5", "gpt-4o-mini"}, first.Models)
	assert.Equal(t, decimal.RequireFromString("5.00"), first.USDPerHour)
	assert.Equal(t, decimal.RequireFromString("50.00"), first.USDPerDay)
	assert.Equal(t, 800000, first.InputTokensPerHour)
	assert.Equal(t, 200000, first.OutputTokensPerHour)
	assert.Equal(t, 1000000, first.CacheReadTokensPerHour)
	assert.Equal(t, 100000, first.CacheWriteTokensPerHour)
	assert.Equal(t, "reject", first.OnExceed)

	second := keyRules[1]
	assert.Equal(t, []string{"*"}, second.Models)
	assert.Equal(t, 20, second.RPM)
	assert.True(t, second.USDPerDay.IsZero(), "absent usd_per_day must be zero")
}

// ── decimal.Decimal precision ─────────────────────────────────────────────────

func TestRawModelPricing_DecimalPrecision(t *testing.T) {
	src := `
llm:
  providers:
    p: {kind: openai, endpoint: https://example.com, auth: {type: bearer, secret_ref: env://X}}
  models:
    m:
      provider: p
      pricing:
        input_mtok: 0.0000001
        output_mtok: 123456789.123456789
`
	rc := mustUnmarshalRaw(t, src)
	p := rc.LLM.Models["m"].Pricing
	require.NotNil(t, p)

	// Verify no floating-point rounding at the raw struct level.
	assert.Equal(t, "0.0000001", p.InputMTok.String())
	assert.Equal(t, "123456789.123456789", p.OutputMTok.String())
}

// ── JSON tags ─────────────────────────────────────────────────────────────────

func TestRawConfig_JSONTags(t *testing.T) {
	// Ensure json tags are present and round-trip correctly alongside yaml.
	src := RawConfig{
		LLM: RawLLM{
			Providers: map[string]RawProvider{
				"p": {Kind: "openai", Endpoint: "https://example.com", Auth: RawAuth{Type: "bearer", SecretRef: "env://X"}},
			},
			Models: map[string]RawModel{
				"m": {Provider: "p", Pricing: &RawModelPricing{
					InputMTok:  decimal.RequireFromString("1.23"),
					OutputMTok: decimal.RequireFromString("4.56"),
				}},
			},
		},
	}

	data, err := json.Marshal(src)
	require.NoError(t, err)

	var got RawConfig
	require.NoError(t, json.Unmarshal(data, &got))

	assert.Equal(t, "openai", got.LLM.Providers["p"].Kind)
	assert.Equal(t, decimal.RequireFromString("1.23"), got.LLM.Models["m"].Pricing.InputMTok)
}
