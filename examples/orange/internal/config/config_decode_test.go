package config

import (
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	configv1 "github.com/dio/transit/examples/orange/api/orange/config/v1"
)

// ── verifyChecksum ────────────────────────────────────────────────────────────

func TestVerifyChecksum_Valid(t *testing.T) {
	payload := []byte("hello config")
	sum := sha256.Sum256(payload)
	require.NoError(t, verifyChecksum(payload, sum[:]))
}

func TestVerifyChecksum_Mismatch(t *testing.T) {
	payload := []byte("hello config")
	wrong := make([]byte, 32) // all zeros — guaranteed mismatch for any non-empty payload
	err := verifyChecksum(payload, wrong)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mismatch")
}

func TestVerifyChecksum_Absent_Skips(t *testing.T) {
	// An empty checksum means "not provided" — verification is skipped entirely.
	require.NoError(t, verifyChecksum([]byte("any payload"), nil))
	require.NoError(t, verifyChecksum([]byte("any payload"), []byte{}))
}

func TestVerifyChecksum_WrongLength_IsError(t *testing.T) {
	err := verifyChecksum([]byte("payload"), make([]byte, 16))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "32 bytes")
}

func TestVerifyChecksum_EmptyPayload(t *testing.T) {
	sum := sha256.Sum256(nil)
	require.NoError(t, verifyChecksum(nil, sum[:]))
}

// ── decompress ────────────────────────────────────────────────────────────────

func TestDecompress_None_Passthrough(t *testing.T) {
	data := []byte("raw payload bytes")
	got, err := decompress(CompressionNone, data)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestDecompress_None_Nil(t *testing.T) {
	got, err := decompress(CompressionNone, nil)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestDecompress_Zstd_Roundtrip(t *testing.T) {
	original := []byte("the quick brown fox jumps over the lazy dog — repeated to create compressible data. " +
		"the quick brown fox jumps over the lazy dog — repeated to create compressible data.")
	compressed := zstdCompress(t, original)
	got, err := decompress(CompressionZstd, compressed)
	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestDecompress_Zstd_InvalidBytes_IsError(t *testing.T) {
	_, err := decompress(CompressionZstd, []byte("not zstd"))
	require.Error(t, err)
}

func TestDecompress_Zstd_EmptyInput(t *testing.T) {
	// An empty zstd stream is valid (produces empty output).
	compressed := zstdCompress(t, []byte{})
	got, err := decompress(CompressionZstd, compressed)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestDecompress_UnknownKind_IsError(t *testing.T) {
	_, err := decompress(CompressionKind("brotli"), []byte("data"))
	require.Error(t, err)
}

// ── Enum converters ───────────────────────────────────────────────────────────

func TestProviderKindToString(t *testing.T) {
	cases := []struct {
		in   configv1.ProviderKind
		want string
	}{
		{configv1.ProviderKind_PROVIDER_KIND_ANTHROPIC, "anthropic"},
		{configv1.ProviderKind_PROVIDER_KIND_OPENAI, "openai"},
		{configv1.ProviderKind_PROVIDER_KIND_BEDROCK, "bedrock"},
		{configv1.ProviderKind_PROVIDER_KIND_UNSPECIFIED, ""},
		{configv1.ProviderKind(99), ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, providerKindToString(tc.in), "input: %v", tc.in)
	}
}

func TestAuthTypeToString(t *testing.T) {
	cases := []struct {
		in   configv1.AuthType
		want string
	}{
		{configv1.AuthType_AUTH_TYPE_BEARER, "bearer"},
		{configv1.AuthType_AUTH_TYPE_ANTHROPIC, "anthropic"},
		{configv1.AuthType_AUTH_TYPE_GCP, "gcp"},
		{configv1.AuthType_AUTH_TYPE_AWS, "aws"},
		{configv1.AuthType_AUTH_TYPE_GEMINI, "gemini"},
		{configv1.AuthType_AUTH_TYPE_UNSPECIFIED, ""},
		{configv1.AuthType(99), ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, authTypeToString(tc.in), "input: %v", tc.in)
	}
}

func TestOnExceedToString(t *testing.T) {
	cases := []struct {
		in   configv1.OnExceed
		want string
	}{
		{configv1.OnExceed_ON_EXCEED_REJECT, "reject"},
		{configv1.OnExceed_ON_EXCEED_THROTTLE, "throttle"},
		{configv1.OnExceed_ON_EXCEED_LOG_ONLY, "log_only"},
		{configv1.OnExceed_ON_EXCEED_UNSPECIFIED, ""},
		{configv1.OnExceed(99), ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, onExceedToString(tc.in), "input: %v", tc.in)
	}
}

// ── protoToRaw ────────────────────────────────────────────────────────────────

// makePayload builds a ConfigPayload with a pre-populated string table.
// Callers pass a flat list of unique strings; index 0 is reserved (""),
// and makePayload returns a lookup function that maps each string to its index.
func makePayload(strs ...string) (*configv1.ConfigPayload, func(string) uint32) {
	table := append([]string{""}, strs...) // index 0 reserved
	idx := func(s string) uint32 {
		for i, v := range table {
			if v == s {
				return uint32(i)
			}
		}
		panic("string not in table: " + s)
	}
	return &configv1.ConfigPayload{
		Strings: &configv1.StringTable{Strings: table},
	}, idx
}

func TestProtoToRaw_Nil_IsError(t *testing.T) {
	_, err := protoToRaw(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

func TestProtoToRaw_Provider_ZeroNameIdx_IsError(t *testing.T) {
	p, _ := makePayload("anthropic")
	p.Providers = []*configv1.Provider{{NameIdx: 0}} // zero idx = unset
	_, err := protoToRaw(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zero name_idx")
}

func TestProtoToRaw_Server_ZeroNameIdx_IsError(t *testing.T) {
	p, _ := makePayload("endpoint")
	p.Servers = []*configv1.Server{{NameIdx: 0, EndpointIdx: 1}}
	_, err := protoToRaw(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zero name_idx")
}

func TestProtoToRaw_Profile_ZeroIdIdx_IsError(t *testing.T) {
	p, _ := makePayload("demo/alice")
	p.Profiles = []*configv1.Profile{{IdIdx: 0}}
	_, err := protoToRaw(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zero id_idx")
}

func TestProtoToRaw_Key_ZeroIdIdx_IsError(t *testing.T) {
	p, _ := makePayload("demo/alice/sk-001")
	p.Keys = []*configv1.Key{{IdIdx: 0}}
	_, err := protoToRaw(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zero id_idx")
}

func TestProtoToRaw_Providers(t *testing.T) {
	p, idx := makePayload(
		"anthropic", "https://api.anthropic.com",
		"env://ANTHROPIC_API_KEY",
		"anthropic_version", "2023-06-01",
	)
	p.Providers = []*configv1.Provider{{
		NameIdx:     idx("anthropic"),
		Kind:        configv1.ProviderKind_PROVIDER_KIND_ANTHROPIC,
		EndpointIdx: idx("https://api.anthropic.com"),
		Auth: &configv1.Auth{
			Type:      configv1.AuthType_AUTH_TYPE_ANTHROPIC,
			SecretIdx: idx("env://ANTHROPIC_API_KEY"),
		},
		Extra: []*configv1.KV{{
			KeyIdx:   idx("anthropic_version"),
			ValueIdx: idx("2023-06-01"),
		}},
	}}

	raw, err := protoToRaw(p)
	require.NoError(t, err)

	prov, ok := raw.LLM.Providers["anthropic"]
	require.True(t, ok)
	assert.Equal(t, "anthropic", prov.Kind)
	assert.Equal(t, "https://api.anthropic.com", prov.Endpoint)
	assert.Equal(t, "anthropic", prov.Auth.Type)
	assert.Equal(t, "env://ANTHROPIC_API_KEY", prov.Auth.SecretRef)
	assert.Equal(t, "2023-06-01", prov.Extra["anthropic_version"])
}

func TestProtoToRaw_AllProviderKinds(t *testing.T) {
	cases := []struct {
		proto configv1.ProviderKind
		want  string
	}{
		{configv1.ProviderKind_PROVIDER_KIND_ANTHROPIC, "anthropic"},
		{configv1.ProviderKind_PROVIDER_KIND_OPENAI, "openai"},
		{configv1.ProviderKind_PROVIDER_KIND_BEDROCK, "bedrock"},
	}
	for _, tc := range cases {
		p, idx := makePayload("prov", "ep", "ref")
		p.Providers = []*configv1.Provider{{
			NameIdx:     idx("prov"),
			Kind:        tc.proto,
			EndpointIdx: idx("ep"),
			Auth:        &configv1.Auth{Type: configv1.AuthType_AUTH_TYPE_BEARER, SecretIdx: idx("ref")},
		}}
		raw, err := protoToRaw(p)
		require.NoError(t, err, "kind %v", tc.proto)
		assert.Equal(t, tc.want, raw.LLM.Providers["prov"].Kind)
	}
}

func TestProtoToRaw_Model_WithPricingAndMetadata(t *testing.T) {
	p, idx := makePayload(
		"anthropic", "claude-haiku-4-5", "claude-haiku-4-5-20251001",
		"chat_completions", "vertex", "fast", "experimental",
	)
	p.Providers = []*configv1.Provider{{
		NameIdx: idx("anthropic"), Kind: configv1.ProviderKind_PROVIDER_KIND_ANTHROPIC,
	}}
	p.Models = []*configv1.Model{{
		NameIdx:     idx("claude-haiku-4-5"),
		ProviderIdx: idx("anthropic"),
		ApiNameIdx:  idx("claude-haiku-4-5-20251001"),
		EndpointOverrides: []*configv1.EndpointOverride{{
			OperationIdx: idx("chat_completions"),
			ProviderIdx:  idx("vertex"),
		}},
		Pricing: &configv1.ModelPricing{
			InputMtok:      0.80,
			OutputMtok:     4.00,
			CacheReadMtok:  0.08,
			CacheWriteMtok: 1.00,
		},
		Metadata: &configv1.ModelMetadata{
			ContextLength: 200000,
			MaxTokens:     8192,
			TagIdxs:       []uint32{idx("fast"), idx("experimental")},
		},
	}}

	raw, err := protoToRaw(p)
	require.NoError(t, err)

	m, ok := raw.LLM.Models["claude-haiku-4-5"]
	require.True(t, ok)
	assert.Equal(t, "anthropic", m.Provider)
	assert.Equal(t, "claude-haiku-4-5-20251001", m.Name)
	assert.Equal(t, "vertex", m.EndpointOverrides["chat_completions"])

	require.NotNil(t, m.Pricing)
	assert.True(t, m.Pricing.InputMTok.Equal(decimal.NewFromFloat(0.80)))
	assert.True(t, m.Pricing.OutputMTok.Equal(decimal.NewFromFloat(4.00)))
	assert.True(t, m.Pricing.CacheReadMTok.Equal(decimal.NewFromFloat(0.08)))
	assert.True(t, m.Pricing.CacheWriteMTok.Equal(decimal.NewFromFloat(1.00)))

	require.NotNil(t, m.Metadata)
	assert.Equal(t, 200000, m.Metadata.ContextLength)
	assert.Equal(t, 8192, m.Metadata.MaxTokens)
	assert.Equal(t, []string{"fast", "experimental"}, m.Metadata.Tags)
}

func TestProtoToRaw_Model_ZeroPricing_IsNil(t *testing.T) {
	// A model with both input and output at zero should produce nil pricing.
	p, idx := makePayload("anthropic", "my-model")
	p.Providers = []*configv1.Provider{{NameIdx: idx("anthropic"), Kind: configv1.ProviderKind_PROVIDER_KIND_ANTHROPIC}}
	p.Models = []*configv1.Model{{
		NameIdx:     idx("my-model"),
		ProviderIdx: idx("anthropic"),
		Pricing:     &configv1.ModelPricing{InputMtok: 0, OutputMtok: 0},
	}}
	raw, err := protoToRaw(p)
	require.NoError(t, err)
	assert.Nil(t, raw.LLM.Models["my-model"].Pricing)
}

func TestProtoToRaw_Model_ZeroMetadata_IsNil(t *testing.T) {
	p, idx := makePayload("anthropic", "my-model")
	p.Providers = []*configv1.Provider{{NameIdx: idx("anthropic"), Kind: configv1.ProviderKind_PROVIDER_KIND_ANTHROPIC}}
	p.Models = []*configv1.Model{{
		NameIdx:     idx("my-model"),
		ProviderIdx: idx("anthropic"),
		Metadata:    &configv1.ModelMetadata{}, // all zero
	}}
	raw, err := protoToRaw(p)
	require.NoError(t, err)
	assert.Nil(t, raw.LLM.Models["my-model"].Metadata)
}

func TestProtoToRaw_Server_WithAuth(t *testing.T) {
	p, idx := makePayload("github", "https://github.com/mcp", "default", "env://GITHUB_TOKEN")
	p.Servers = []*configv1.Server{{
		NameIdx:      idx("github"),
		EndpointIdx:  idx("https://github.com/mcp"),
		NamespaceIdx: idx("default"),
		Auth: &configv1.Auth{
			Type:      configv1.AuthType_AUTH_TYPE_BEARER,
			SecretIdx: idx("env://GITHUB_TOKEN"),
		},
		ToolsIncludeIdxs: []uint32{},
	}}

	raw, err := protoToRaw(p)
	require.NoError(t, err)

	srv, ok := raw.MCP.Servers["github"]
	require.True(t, ok)
	assert.Equal(t, "https://github.com/mcp", srv.Endpoint)
	assert.Equal(t, "default", srv.Namespace)
	require.NotNil(t, srv.Auth)
	assert.Equal(t, "bearer", srv.Auth.Type)
	assert.Equal(t, "env://GITHUB_TOKEN", srv.Auth.SecretRef)
}

func TestProtoToRaw_Server_UnspecifiedAuth_IsNil(t *testing.T) {
	// A server with AUTH_TYPE_UNSPECIFIED must produce nil Auth in RawServer.
	p, idx := makePayload("srv", "https://example.com", "ns")
	p.Servers = []*configv1.Server{{
		NameIdx:      idx("srv"),
		EndpointIdx:  idx("https://example.com"),
		NamespaceIdx: idx("ns"),
		Auth:         &configv1.Auth{Type: configv1.AuthType_AUTH_TYPE_UNSPECIFIED},
	}}
	raw, err := protoToRaw(p)
	require.NoError(t, err)
	assert.Nil(t, raw.MCP.Servers["srv"].Auth)
}

func TestProtoToRaw_Profile(t *testing.T) {
	p, idx := makePayload(
		"demo/alice", "github", "list_repos", "search_repos",
		"env://ALICE_GITHUB_TOKEN",
	)
	p.Profiles = []*configv1.Profile{{
		IdIdx: idx("demo/alice"),
		Tools: []*configv1.ToolFilter{{
			ServerIdx:   idx("github"),
			IncludeIdxs: []uint32{idx("list_repos"), idx("search_repos")},
			Optional:    true,
		}},
		AuthOverrides: []*configv1.AuthOverride{{
			ServerIdx: idx("github"),
			Auth: &configv1.Auth{
				Type:      configv1.AuthType_AUTH_TYPE_BEARER,
				SecretIdx: idx("env://ALICE_GITHUB_TOKEN"),
			},
		}},
	}}

	raw, err := protoToRaw(p)
	require.NoError(t, err)

	prof, ok := raw.Profiles["demo/alice"]
	require.True(t, ok)
	tf, ok := prof.Tools["github"]
	require.True(t, ok)
	assert.Equal(t, []string{"list_repos", "search_repos"}, tf.Include)
	assert.True(t, tf.Optional)
	ao, ok := prof.Auth["github"]
	require.True(t, ok)
	assert.Equal(t, "bearer", ao.Type)
	assert.Equal(t, "env://ALICE_GITHUB_TOKEN", ao.SecretRef)
}

func TestProtoToRaw_Key_WithRoutingTarget(t *testing.T) {
	p, idx := makePayload("demo/alice/sk-001", "gpt-4o-mini", "openai", "gpt-4o-mini-backend")
	p.Keys = []*configv1.Key{{
		IdIdx: idx("demo/alice/sk-001"),
		RoutingOverrides: []*configv1.RoutingOverride{{
			ModelIdx: idx("gpt-4o-mini"),
			Node: &configv1.RoutingNode{
				Kind: &configv1.RoutingNode_Target{
					Target: &configv1.RoutingTarget{
						ProviderIdx: idx("openai"),
						NameIdx:     idx("gpt-4o-mini-backend"),
					},
				},
			},
		}},
	}}

	raw, err := protoToRaw(p)
	require.NoError(t, err)

	key, ok := raw.Keys["demo/alice/sk-001"]
	require.True(t, ok)
	node, ok := key.RoutingOverrides["gpt-4o-mini"]
	require.True(t, ok)
	require.NotNil(t, node.Target)
	assert.Equal(t, "openai", node.Target.Provider)
	assert.Equal(t, "gpt-4o-mini-backend", node.Target.Name)
}

func TestProtoToRaw_Key_WithChainRouting(t *testing.T) {
	p, idx := makePayload(
		"demo/alice/sk-chain", "claude-haiku-4-5",
		"primary", "fallback",
		"connect-failure,5xx",
	)
	p.Keys = []*configv1.Key{{
		IdIdx: idx("demo/alice/sk-chain"),
		RoutingOverrides: []*configv1.RoutingOverride{{
			ModelIdx: idx("claude-haiku-4-5"),
			Node: &configv1.RoutingNode{
				Kind: &configv1.RoutingNode_Chain{
					Chain: &configv1.ChainConfig{
						Retry: &configv1.RetryPolicy{
							RetryOnIdx:      idx("connect-failure,5xx"),
							PerTryTimeoutMs: 5000,
						},
						Children: []*configv1.RoutingNode{
							{Kind: &configv1.RoutingNode_Target{Target: &configv1.RoutingTarget{ProviderIdx: idx("primary")}}},
							{Kind: &configv1.RoutingNode_Target{Target: &configv1.RoutingTarget{ProviderIdx: idx("fallback")}}},
						},
					},
				},
			},
		}},
	}}

	raw, err := protoToRaw(p)
	require.NoError(t, err)

	node := raw.Keys["demo/alice/sk-chain"].RoutingOverrides["claude-haiku-4-5"]
	require.NotNil(t, node.Chain)
	require.NotNil(t, node.Chain.Retry)
	assert.Equal(t, "connect-failure,5xx", node.Chain.Retry.RetryOn)
	assert.Equal(t, 5000, node.Chain.Retry.PerTryTimeoutMs)
	assert.Len(t, node.Chain.Children, 2)
	assert.Equal(t, "primary", node.Chain.Children[0].Target.Provider)
	assert.Equal(t, "fallback", node.Chain.Children[1].Target.Provider)
}

func TestProtoToRaw_Key_WithSplitRouting(t *testing.T) {
	p, idx := makePayload("demo/alice/sk-split", "gpt-4o", "arm1", "arm2")
	p.Keys = []*configv1.Key{{
		IdIdx: idx("demo/alice/sk-split"),
		RoutingOverrides: []*configv1.RoutingOverride{{
			ModelIdx: idx("gpt-4o"),
			Node: &configv1.RoutingNode{
				Kind: &configv1.RoutingNode_Split{
					Split: &configv1.SplitConfig{
						Children: []*configv1.SplitChild{
							{Weight: 70, Node: &configv1.RoutingNode{Kind: &configv1.RoutingNode_Target{Target: &configv1.RoutingTarget{ProviderIdx: idx("arm1")}}}},
							{Weight: 30, Node: &configv1.RoutingNode{Kind: &configv1.RoutingNode_Target{Target: &configv1.RoutingTarget{ProviderIdx: idx("arm2")}}}},
						},
					},
				},
			},
		}},
	}}

	raw, err := protoToRaw(p)
	require.NoError(t, err)

	node := raw.Keys["demo/alice/sk-split"].RoutingOverrides["gpt-4o"]
	require.NotNil(t, node.Split)
	require.Len(t, node.Split.Children, 2)
	assert.Equal(t, 70, node.Split.Children[0].Weight)
	assert.Equal(t, "arm1", node.Split.Children[0].Target.Provider)
	assert.Equal(t, 30, node.Split.Children[1].Weight)
}

func TestProtoToRaw_RoutingNode_Nil_IsError(t *testing.T) {
	p, idx := makePayload("demo/alice/sk-001", "gpt-4o")
	p.Keys = []*configv1.Key{{
		IdIdx: idx("demo/alice/sk-001"),
		RoutingOverrides: []*configv1.RoutingOverride{{
			ModelIdx: idx("gpt-4o"),
			Node:     nil,
		}},
	}}
	_, err := protoToRaw(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil routing node")
}

func TestProtoToRaw_RoutingNode_EmptyOneof_IsError(t *testing.T) {
	p, idx := makePayload("demo/alice/sk-001", "gpt-4o")
	p.Keys = []*configv1.Key{{
		IdIdx: idx("demo/alice/sk-001"),
		RoutingOverrides: []*configv1.RoutingOverride{{
			ModelIdx: idx("gpt-4o"),
			Node:     &configv1.RoutingNode{}, // kind not set
		}},
	}}
	_, err := protoToRaw(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no kind set")
}

func TestProtoToRaw_RateLimits_AdminScope(t *testing.T) {
	p, idx := makePayload("demo", "*", "claude-haiku-4-5")
	p.RateLimits = []*configv1.RateLimitScope{{
		ScopeIdx: idx("demo"),
		Rules: []*configv1.RateLimitRule{{
			ModelIdxs: []uint32{idx("*")},
			UsdPerDay: 50.0,
			Rpm:       100,
			OnExceed:  configv1.OnExceed_ON_EXCEED_THROTTLE,
		}, {
			ModelIdxs:             []uint32{idx("claude-haiku-4-5")},
			InputTokensPerMinute:  500000,
			OutputTokensPerMinute: 100000,
		}},
	}}

	raw, err := protoToRaw(p)
	require.NoError(t, err)

	rules, ok := raw.RateLimit.Policies["demo"]
	require.True(t, ok)
	require.Len(t, rules, 2)

	r0 := rules[0]
	assert.Equal(t, []string{"*"}, r0.Models)
	assert.True(t, r0.USDPerDay.Equal(decimal.NewFromFloat(50.0)))
	assert.Equal(t, 100, r0.RPM)
	assert.Equal(t, "throttle", r0.OnExceed)

	r1 := rules[1]
	assert.Equal(t, []string{"claude-haiku-4-5"}, r1.Models)
	assert.Equal(t, 500000, r1.InputTokensPerMinute)
	assert.Equal(t, "", r1.OnExceed) // UNSPECIFIED → "" → compile defaults to "reject"
}

func TestProtoToRaw_RateLimits_KeyScope_ViaKeyRecord(t *testing.T) {
	// Key-scope rate limit rules come from Key.rate_limit_rules and are stored
	// in raw.RateLimit.Policies under the key ID (3-segment scope).
	p, idx := makePayload("demo/alice/sk-001", "*")
	p.Keys = []*configv1.Key{{
		IdIdx: idx("demo/alice/sk-001"),
		RateLimitRules: []*configv1.RateLimitRule{{
			ModelIdxs: []uint32{idx("*")},
			Rpd:       1000,
		}},
	}}

	raw, err := protoToRaw(p)
	require.NoError(t, err)

	rules, ok := raw.RateLimit.Policies["demo/alice/sk-001"]
	require.True(t, ok, "key-scope rate limits must appear in raw.RateLimit.Policies")
	require.Len(t, rules, 1)
	assert.Equal(t, 1000, rules[0].RPD)
}

func TestProtoToRaw_OnExceedUnspecified_ProducesEmpty(t *testing.T) {
	// ON_EXCEED_UNSPECIFIED → "" in RawRateLimitRule.OnExceed.
	// compile() then defaults "" to "reject".
	p, idx := makePayload("demo", "*")
	p.RateLimits = []*configv1.RateLimitScope{{
		ScopeIdx: idx("demo"),
		Rules: []*configv1.RateLimitRule{{
			ModelIdxs: []uint32{idx("*")},
			OnExceed:  configv1.OnExceed_ON_EXCEED_UNSPECIFIED,
		}},
	}}
	raw, err := protoToRaw(p)
	require.NoError(t, err)
	assert.Equal(t, "", raw.RateLimit.Policies["demo"][0].OnExceed)
}

// ── decodeRawConfig ───────────────────────────────────────────────────────────

// decodeMinimalYAML is the smallest valid config that satisfies compile().
const decodeMinimalYAML = `
llm:
  providers:
    openai:
      kind: openai
      endpoint: https://api.openai.com
      auth:
        type: bearer
        secret_ref: env://OPENAI_API_KEY
  models:
    gpt-4o-mini:
      provider: openai
mcp:
  servers: {}
`

func TestDecodeRawConfig_YAML(t *testing.T) {
	payload := []byte(decodeMinimalYAML)
	env := SnapshotEnvelope{
		Format:      SnapshotFormatYAML,
		Compression: CompressionNone,
		Payload:     payload,
	}
	raw, err := decodeRawConfig(env)
	require.NoError(t, err)
	assert.Contains(t, raw.LLM.Providers, "openai")
	assert.Contains(t, raw.LLM.Models, "gpt-4o-mini")
}

func TestDecodeRawConfig_YAML_WithChecksum(t *testing.T) {
	payload := []byte(decodeMinimalYAML)
	sum := sha256.Sum256(payload)
	env := SnapshotEnvelope{
		Format:      SnapshotFormatYAML,
		Compression: CompressionNone,
		Payload:     payload,
		Checksum:    sum[:],
	}
	_, err := decodeRawConfig(env)
	require.NoError(t, err)
}

func TestDecodeRawConfig_YAML_WrongChecksum_IsError(t *testing.T) {
	payload := []byte(decodeMinimalYAML)
	env := SnapshotEnvelope{
		Format:      SnapshotFormatYAML,
		Compression: CompressionNone,
		Payload:     payload,
		Checksum:    make([]byte, 32), // all zeros
	}
	_, err := decodeRawConfig(env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksum")
}

func TestDecodeRawConfig_JSON(t *testing.T) {
	raw := RawConfig{
		LLM: RawLLM{
			Providers: map[string]RawProvider{
				"openai": {
					Kind:     "openai",
					Endpoint: "https://api.openai.com",
					Auth:     RawAuth{Type: "bearer", SecretRef: "env://OPENAI_API_KEY"},
				},
			},
			Models: map[string]RawModel{
				"gpt-4o-mini": {Provider: "openai"},
			},
		},
		MCP: RawMCP{Servers: map[string]RawServer{}},
	}
	payload, err := json.Marshal(raw)
	require.NoError(t, err)

	env := SnapshotEnvelope{
		Format:      SnapshotFormatJSON,
		Compression: CompressionNone,
		Payload:     payload,
	}
	got, err := decodeRawConfig(env)
	require.NoError(t, err)
	assert.Contains(t, got.LLM.Providers, "openai")
}

func TestDecodeRawConfig_Proto(t *testing.T) {
	// Build a ConfigPayload, marshal it, and decode via decodeRawConfig.
	p, idx := makePayload(
		"openai", "https://api.openai.com",
		"bearer", "env://OPENAI_API_KEY",
		"gpt-4o-mini",
	)
	p.Providers = []*configv1.Provider{{
		NameIdx:     idx("openai"),
		Kind:        configv1.ProviderKind_PROVIDER_KIND_OPENAI,
		EndpointIdx: idx("https://api.openai.com"),
		Auth: &configv1.Auth{
			Type:      configv1.AuthType_AUTH_TYPE_BEARER,
			SecretIdx: idx("env://OPENAI_API_KEY"),
		},
	}}
	p.Models = []*configv1.Model{{
		NameIdx:     idx("gpt-4o-mini"),
		ProviderIdx: idx("openai"),
	}}

	payload, err := proto.Marshal(p)
	require.NoError(t, err)

	env := SnapshotEnvelope{
		Format:      SnapshotFormatProto,
		Compression: CompressionNone,
		Payload:     payload,
	}
	raw, err := decodeRawConfig(env)
	require.NoError(t, err)
	assert.Contains(t, raw.LLM.Providers, "openai")
	assert.Equal(t, "openai", raw.LLM.Providers["openai"].Kind)
	assert.Contains(t, raw.LLM.Models, "gpt-4o-mini")
}

func TestDecodeRawConfig_Proto_Zstd(t *testing.T) {
	// Full proto + zstd roundtrip.
	p, idx := makePayload("openai", "https://api.openai.com", "bearer", "env://OPENAI_API_KEY", "gpt-4o-mini")
	p.Providers = []*configv1.Provider{{
		NameIdx: idx("openai"), Kind: configv1.ProviderKind_PROVIDER_KIND_OPENAI,
		EndpointIdx: idx("https://api.openai.com"),
		Auth:        &configv1.Auth{Type: configv1.AuthType_AUTH_TYPE_BEARER, SecretIdx: idx("env://OPENAI_API_KEY")},
	}}
	p.Models = []*configv1.Model{{NameIdx: idx("gpt-4o-mini"), ProviderIdx: idx("openai")}}

	protoBytes, err := proto.Marshal(p)
	require.NoError(t, err)

	compressed := zstdCompress(t, protoBytes)
	sum := sha256.Sum256(protoBytes) // checksum of the decompressed payload

	env := SnapshotEnvelope{
		Format:      SnapshotFormatProto,
		Compression: CompressionZstd,
		Payload:     compressed,
		Checksum:    sum[:],
	}
	raw, err := decodeRawConfig(env)
	require.NoError(t, err)
	assert.Contains(t, raw.LLM.Providers, "openai")
}

func TestDecodeRawConfig_UnsupportedFormat_IsError(t *testing.T) {
	env := SnapshotEnvelope{
		Format:      SnapshotFormatMsgpack,
		Compression: CompressionNone,
		Payload:     []byte("msgpack-bytes"),
	}
	_, err := decodeRawConfig(env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported format")
}

func TestDecodeRawConfig_InvalidYAML_IsError(t *testing.T) {
	env := SnapshotEnvelope{
		Format:      SnapshotFormatYAML,
		Compression: CompressionNone,
		Payload:     []byte("{not: valid: yaml:"),
	}
	_, err := decodeRawConfig(env)
	require.Error(t, err)
}

func TestDecodeRawConfig_InvalidJSON_IsError(t *testing.T) {
	env := SnapshotEnvelope{
		Format:      SnapshotFormatJSON,
		Compression: CompressionNone,
		Payload:     []byte("{not valid json"),
	}
	_, err := decodeRawConfig(env)
	require.Error(t, err)
}

func TestDecodeRawConfig_InvalidProto_IsError(t *testing.T) {
	env := SnapshotEnvelope{
		Format:      SnapshotFormatProto,
		Compression: CompressionNone,
		Payload:     []byte("not valid protobuf bytes \xff\xfe"),
	}
	_, err := decodeRawConfig(env)
	require.Error(t, err)
}

func TestDecodeRawConfig_BadZstd_IsError(t *testing.T) {
	env := SnapshotEnvelope{
		Format:      SnapshotFormatYAML,
		Compression: CompressionZstd,
		Payload:     []byte("not zstd"),
	}
	_, err := decodeRawConfig(env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decompress")
}

// ── Integration: proto → protoToRaw → compile ─────────────────────────────────

func TestDecodeRawConfig_ProtoRoundtripToCompile(t *testing.T) {
	// Build a ConfigPayload, decode it to RawConfig, then compile() it to
	// confirm the full pipeline (proto → raw → domain) produces a valid snapshot.
	p, idx := makePayload(
		"openai", "https://api.openai.com", "bearer", "env://OPENAI_API_KEY",
		"gpt-4o-mini",
	)
	p.Providers = []*configv1.Provider{{
		NameIdx:     idx("openai"),
		Kind:        configv1.ProviderKind_PROVIDER_KIND_OPENAI,
		EndpointIdx: idx("https://api.openai.com"),
		Auth:        &configv1.Auth{Type: configv1.AuthType_AUTH_TYPE_BEARER, SecretIdx: idx("env://OPENAI_API_KEY")},
	}}
	p.Models = []*configv1.Model{{
		NameIdx:     idx("gpt-4o-mini"),
		ProviderIdx: idx("openai"),
	}}

	protoBytes, err := proto.Marshal(p)
	require.NoError(t, err)

	env := SnapshotEnvelope{
		Format:      SnapshotFormatProto,
		Compression: CompressionNone,
		Payload:     protoBytes,
	}
	raw, err := decodeRawConfig(env)
	require.NoError(t, err)

	interns := NewInternPool()
	snap, err := compile(raw, interns, 1)
	require.NoError(t, err)
	assert.Contains(t, snap.Global.Providers, "openai")
	assert.Contains(t, snap.Global.Models, "gpt-4o-mini")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// zstdCompress compresses data using a one-shot zstd encoder. Test-only.
func zstdCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	return enc.EncodeAll(data, nil)
}
