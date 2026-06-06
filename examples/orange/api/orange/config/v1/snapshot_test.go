package configv1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// roundTrip marshals m to bytes and unmarshals into a new message of the same
// type, returning the round-tripped copy.
func roundTrip[T proto.Message](t *testing.T, m T) T {
	t.Helper()
	b, err := proto.Marshal(m)
	require.NoError(t, err)
	out := m.ProtoReflect().New().Interface().(T)
	require.NoError(t, proto.Unmarshal(b, out))
	return out
}

// ── SnapshotEnvelope ──────────────────────────────────────────────────────────

func TestSnapshotEnvelope_RoundTrip(t *testing.T) {
	msg := &SnapshotEnvelope{
		Version:     42,
		Format:      PayloadFormat_PAYLOAD_FORMAT_PROTO,
		Compression: Compression_COMPRESSION_ZSTD,
		Payload:     []byte("compressed-payload"),
		Checksum:    []byte("sha256checksum"),
	}
	got := roundTrip(t, msg)
	assert.Equal(t, uint64(42), got.Version)
	assert.Equal(t, PayloadFormat_PAYLOAD_FORMAT_PROTO, got.Format)
	assert.Equal(t, Compression_COMPRESSION_ZSTD, got.Compression)
	assert.Equal(t, []byte("compressed-payload"), got.Payload)
	assert.Equal(t, []byte("sha256checksum"), got.Checksum)
}

func TestSnapshotEnvelope_Defaults(t *testing.T) {
	// Zero-value fields must reflect UNSPECIFIED enums so protoToRaw() can
	// apply sensible defaults (NONE for compression, PROTO for format).
	msg := &SnapshotEnvelope{}
	assert.Equal(t, PayloadFormat_PAYLOAD_FORMAT_UNSPECIFIED, msg.Format)
	assert.Equal(t, Compression_COMPRESSION_UNSPECIFIED, msg.Compression)
}

// ── ConfigPayload / StringTable ───────────────────────────────────────────────

func TestConfigPayload_RoundTrip(t *testing.T) {
	msg := &ConfigPayload{
		Strings: &StringTable{
			// index 0 is reserved (empty), real strings start at 1
			Strings: []string{"", "anthropic", "my-model", "env://MY_KEY"},
		},
		Providers: []*Provider{
			{
				NameIdx:     1, // "anthropic"
				Kind:        ProviderKind_PROVIDER_KIND_ANTHROPIC,
				EndpointIdx: 3,
				Auth: &Auth{
					Type:      AuthType_AUTH_TYPE_ANTHROPIC,
					SecretIdx: 3, // "env://MY_KEY"
				},
			},
		},
		Models: []*Model{
			{
				NameIdx:     2, // "my-model"
				ProviderIdx: 1, // "anthropic"
				Pricing: &ModelPricing{
					InputMtok:  3.0,
					OutputMtok: 15.0,
				},
			},
		},
	}
	got := roundTrip(t, msg)

	require.Len(t, got.Strings.Strings, 4)
	assert.Equal(t, "anthropic", got.Strings.Strings[1])
	assert.Equal(t, "my-model", got.Strings.Strings[2])

	require.Len(t, got.Providers, 1)
	assert.Equal(t, uint32(1), got.Providers[0].NameIdx)
	assert.Equal(t, ProviderKind_PROVIDER_KIND_ANTHROPIC, got.Providers[0].Kind)
	assert.Equal(t, AuthType_AUTH_TYPE_ANTHROPIC, got.Providers[0].Auth.Type)

	require.Len(t, got.Models, 1)
	assert.InDelta(t, 3.0, got.Models[0].Pricing.InputMtok, 1e-9)
	assert.InDelta(t, 15.0, got.Models[0].Pricing.OutputMtok, 1e-9)
}

// ── RoutingNode (oneof) ───────────────────────────────────────────────────────

func TestRoutingNode_Target_RoundTrip(t *testing.T) {
	msg := &RoutingNode{
		Kind: &RoutingNode_Target{
			Target: &RoutingTarget{ProviderIdx: 7, NameIdx: 3},
		},
	}
	got := roundTrip(t, msg)
	tgt := got.GetTarget()
	require.NotNil(t, tgt)
	assert.Equal(t, uint32(7), tgt.ProviderIdx)
	assert.Equal(t, uint32(3), tgt.NameIdx)
}

func TestRoutingNode_Chain_RoundTrip(t *testing.T) {
	msg := &RoutingNode{
		Kind: &RoutingNode_Chain{
			Chain: &ChainConfig{
				Retry: &RetryPolicy{
					RetryOnIdx:      5,
					PerTryTimeoutMs: 3000,
				},
				Children: []*RoutingNode{
					{Kind: &RoutingNode_Target{Target: &RoutingTarget{ProviderIdx: 1}}},
					{Kind: &RoutingNode_Target{Target: &RoutingTarget{ProviderIdx: 2}}},
				},
			},
		},
	}
	got := roundTrip(t, msg)
	chain := got.GetChain()
	require.NotNil(t, chain)
	assert.Equal(t, uint32(5), chain.Retry.RetryOnIdx)
	assert.Equal(t, int32(3000), chain.Retry.PerTryTimeoutMs)
	assert.Len(t, chain.Children, 2)
}

func TestRoutingNode_Split_RoundTrip(t *testing.T) {
	msg := &RoutingNode{
		Kind: &RoutingNode_Split{
			Split: &SplitConfig{
				Children: []*SplitChild{
					{Weight: 70, Node: &RoutingNode{Kind: &RoutingNode_Target{Target: &RoutingTarget{ProviderIdx: 1}}}},
					{Weight: 30, Node: &RoutingNode{Kind: &RoutingNode_Target{Target: &RoutingTarget{ProviderIdx: 2}}}},
				},
			},
		},
	}
	got := roundTrip(t, msg)
	split := got.GetSplit()
	require.NotNil(t, split)
	require.Len(t, split.Children, 2)
	assert.Equal(t, int32(70), split.Children[0].Weight)
	assert.Equal(t, int32(30), split.Children[1].Weight)
}

// ── Key with rate_limit_rules ─────────────────────────────────────────────────

func TestKey_WithRateLimitRules_RoundTrip(t *testing.T) {
	msg := &Key{
		IdIdx: 9,
		RateLimitRules: []*RateLimitRule{
			{
				ModelIdxs:    []uint32{1, 2},
				UsdPerDay:    10.0,
				Rpm:          60,
				OnExceed:     OnExceed_ON_EXCEED_THROTTLE,
			},
		},
	}
	got := roundTrip(t, msg)
	assert.Equal(t, uint32(9), got.IdIdx)
	require.Len(t, got.RateLimitRules, 1)
	r := got.RateLimitRules[0]
	assert.Equal(t, []uint32{1, 2}, r.ModelIdxs)
	assert.InDelta(t, 10.0, r.UsdPerDay, 1e-9)
	assert.Equal(t, int32(60), r.Rpm)
	assert.Equal(t, OnExceed_ON_EXCEED_THROTTLE, r.OnExceed)
}

// ── OnExceed zero value is UNSPECIFIED (treated as REJECT by protoToRaw) ──────

func TestOnExceed_ZeroIsUnspecified(t *testing.T) {
	r := &RateLimitRule{}
	assert.Equal(t, OnExceed_ON_EXCEED_UNSPECIFIED, r.OnExceed,
		"zero RateLimitRule must have UNSPECIFIED on_exceed so protoToRaw() can default to reject")
}

// ── Compression zero value is UNSPECIFIED (treated as NONE by protoToRaw) ─────

func TestCompression_ZeroIsUnspecified(t *testing.T) {
	env := &SnapshotEnvelope{}
	assert.Equal(t, Compression_COMPRESSION_UNSPECIFIED, env.Compression,
		"zero SnapshotEnvelope must have UNSPECIFIED compression so protoToRaw() can default to none")
}

// ── Server / Profile / Auth ───────────────────────────────────────────────────

func TestServer_RoundTrip(t *testing.T) {
	msg := &Server{
		NameIdx:          3,
		EndpointIdx:      4,
		NamespaceIdx:     5,
		ToolsIncludeIdxs: []uint32{10, 11, 12},
		Auth: &Auth{
			Type:      AuthType_AUTH_TYPE_BEARER,
			SecretIdx: 6,
		},
	}
	got := roundTrip(t, msg)
	assert.Equal(t, uint32(3), got.NameIdx)
	assert.Equal(t, []uint32{10, 11, 12}, got.ToolsIncludeIdxs)
	assert.Equal(t, AuthType_AUTH_TYPE_BEARER, got.Auth.Type)
}

func TestProfile_RoundTrip(t *testing.T) {
	msg := &Profile{
		IdIdx: 7,
		Tools: []*ToolFilter{
			{ServerIdx: 1, IncludeIdxs: []uint32{5, 6}, Optional: true},
		},
		AuthOverrides: []*AuthOverride{
			{ServerIdx: 1, Auth: &Auth{Type: AuthType_AUTH_TYPE_BEARER, SecretIdx: 9}},
		},
	}
	got := roundTrip(t, msg)
	assert.Equal(t, uint32(7), got.IdIdx)
	require.Len(t, got.Tools, 1)
	assert.True(t, got.Tools[0].Optional)
	require.Len(t, got.AuthOverrides, 1)
	assert.Equal(t, uint32(9), got.AuthOverrides[0].Auth.SecretIdx)
}

// ── RateLimitScope ────────────────────────────────────────────────────────────

func TestRateLimitScope_RoundTrip(t *testing.T) {
	msg := &RateLimitScope{
		ScopeIdx: 2,
		Rules: []*RateLimitRule{
			{
				ModelIdxs:             []uint32{0}, // "*" wildcard index
				UsdPerMinute:          1.5,
				InputTokensPerMinute:  100000,
				OutputTokensPerMinute: 50000,
				OnExceed:              OnExceed_ON_EXCEED_REJECT,
			},
		},
	}
	got := roundTrip(t, msg)
	assert.Equal(t, uint32(2), got.ScopeIdx)
	require.Len(t, got.Rules, 1)
	assert.InDelta(t, 1.5, got.Rules[0].UsdPerMinute, 1e-9)
	assert.Equal(t, int32(100000), got.Rules[0].InputTokensPerMinute)
	assert.Equal(t, OnExceed_ON_EXCEED_REJECT, got.Rules[0].OnExceed)
}
