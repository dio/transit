package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeRawConfigs_bothNil(t *testing.T) {
	got := MergeRawConfigs(nil, nil)
	require.NotNil(t, got)
	assert.Nil(t, got.LLM.Providers)
	assert.Nil(t, got.LLM.Models)
}

func TestMergeRawConfigs_baseNil(t *testing.T) {
	overlay := &RawConfig{
		LLM: RawLLM{
			Providers: map[string]RawProvider{"p1": {Kind: "openai"}},
		},
	}
	got := MergeRawConfigs(nil, overlay)
	assert.Same(t, overlay, got)
}

func TestMergeRawConfigs_overlayNil(t *testing.T) {
	base := &RawConfig{
		LLM: RawLLM{
			Models: map[string]RawModel{"m1": {Provider: "p1"}},
		},
	}
	got := MergeRawConfigs(base, nil)
	assert.Same(t, base, got)
}

func TestMergeRawConfigs_orgProjWs(t *testing.T) {
	org := &RawConfig{
		LLM: RawLLM{
			Providers: map[string]RawProvider{
				"anthropic": {Kind: "anthropic"},
				"openai":    {Kind: "openai"},
			},
			Models: map[string]RawModel{
				"claude-3": {Provider: "anthropic"},
				"gpt-4":    {Provider: "openai"},
			},
		},
		MCP: RawMCP{
			Servers: map[string]RawServer{
				"knowledge": {Endpoint: "https://knowledge.example.com"},
			},
		},
	}

	proj := &RawConfig{
		LLM: RawLLM{
			// Project adds a model, overrides nothing.
			Models: map[string]RawModel{
				"proj-model": {Provider: "anthropic"},
			},
		},
		Profiles: map[string]RawProfile{
			"ws1/alice/default": {Tools: map[string]RawToolFilter{"knowledge": {}}},
		},
	}

	ws := &RawConfig{
		Keys: map[string]RawKey{
			"ws1/alice/sk-1": {},
		},
		// Workspace overrides org openai provider endpoint.
		LLM: RawLLM{
			Providers: map[string]RawProvider{
				"openai": {Kind: "openai", Endpoint: "https://custom.openai.example.com"},
			},
		},
	}

	// Simulate: step1 = org+proj, step2 = step1+ws.
	step1 := MergeRawConfigs(org, proj)
	got := MergeRawConfigs(step1, ws)

	// Org providers present.
	assert.Contains(t, got.LLM.Providers, "anthropic")
	// Workspace wins on openai provider collision.
	assert.Equal(t, "https://custom.openai.example.com", got.LLM.Providers["openai"].Endpoint)

	// Org models present.
	assert.Contains(t, got.LLM.Models, "claude-3")
	assert.Contains(t, got.LLM.Models, "gpt-4")
	// Project-level model present.
	assert.Contains(t, got.LLM.Models, "proj-model")

	// Org MCP server present.
	assert.Contains(t, got.MCP.Servers, "knowledge")

	// Project profile present.
	assert.Contains(t, got.Profiles, "ws1/alice/default")

	// Workspace key present.
	assert.Contains(t, got.Keys, "ws1/alice/sk-1")
}

func TestMergeRawConfigs_rateLimit(t *testing.T) {
	org := &RawConfig{
		RateLimit: RawRateLimit{
			Tiers: map[string]RawRateLimitTier{
				"standard": {},
			},
			Policies: map[string][]RawRateLimitPolicyEntry{
				"ws1": {{Rule: "standard"}},
			},
		},
	}
	ws := &RawConfig{
		RateLimit: RawRateLimit{
			Tiers: map[string]RawRateLimitTier{
				"premium": {},
			},
			Policies: map[string][]RawRateLimitPolicyEntry{
				"ws2": {{Rule: "premium"}},
			},
		},
	}

	got := MergeRawConfigs(org, ws)

	// Both tiers present.
	assert.Contains(t, got.RateLimit.Tiers, "standard")
	assert.Contains(t, got.RateLimit.Tiers, "premium")

	// Both policies present (different workspaces, no collision).
	assert.Contains(t, got.RateLimit.Policies, "ws1")
	assert.Contains(t, got.RateLimit.Policies, "ws2")
}

func TestMergeRawConfigs_overlayWinsOnCollision(t *testing.T) {
	base := &RawConfig{
		LLM: RawLLM{
			Providers: map[string]RawProvider{
				"p": {Kind: "openai", Endpoint: "https://base.example.com"},
			},
		},
	}
	overlay := &RawConfig{
		LLM: RawLLM{
			Providers: map[string]RawProvider{
				"p": {Kind: "openai", Endpoint: "https://overlay.example.com"},
			},
		},
	}
	got := MergeRawConfigs(base, overlay)
	assert.Equal(t, "https://overlay.example.com", got.LLM.Providers["p"].Endpoint)
}

func TestMergeRawConfigs_noMutation(t *testing.T) {
	base := &RawConfig{
		LLM: RawLLM{
			Providers: map[string]RawProvider{"p1": {Kind: "openai"}},
		},
	}
	overlay := &RawConfig{
		LLM: RawLLM{
			Providers: map[string]RawProvider{"p2": {Kind: "anthropic"}},
		},
	}
	got := MergeRawConfigs(base, overlay)

	// Mutating the result must not affect inputs.
	got.LLM.Providers["new"] = RawProvider{Kind: "gemini"}
	assert.NotContains(t, base.LLM.Providers, "new")
	assert.NotContains(t, overlay.LLM.Providers, "new")
}

func TestScopeIDs(t *testing.T) {
	assert.Equal(t, "org:acme", OrgScopeID("acme"))
	assert.Equal(t, "proj:proj-123", ProjectScopeID("proj-123"))

	assert.True(t, IsScopeID("org:acme"))
	assert.True(t, IsScopeID("proj:proj-123"))
	assert.False(t, IsScopeID("ws-abc123"))
	assert.False(t, IsScopeID(""))
}
