package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawForProjectionTest builds a RawConfig spanning two workspaces to verify
// that ProjectForWorkspace confines the output to the requested workspace.
//
// "unused" is a provider with no model or key referencing it — it must be pruned.
// "orphan" is an MCP server not referenced by any profile — it must be pruned.
func rawForProjectionTest() *RawConfig {
	return &RawConfig{
		LLM: RawLLM{
			Providers: map[string]RawProvider{
				"anthropic":   {Kind: "anthropic", Endpoint: "https://api.anthropic.com"},
				"openai":      {Kind: "openai", Endpoint: "https://api.openai.com"},
				"bedrock":     {Kind: "openai", Endpoint: "https://bedrock-runtime.us-east-1.amazonaws.com"},
				"fallback_p1": {Kind: "anthropic", Endpoint: "https://192.0.2.1"},
				// Unreferenced by any model or key routing tree — must be pruned.
				"unused": {Kind: "openai", Endpoint: "https://nowhere.example.com"},
			},
			Models: map[string]RawModel{
				"gpt-4o":    {Provider: "openai"},
				"claude-3":  {Provider: "anthropic"},
				"nova-lite": {Provider: "bedrock"},
				// No model references "unused", so it stays genuinely orphaned.
			},
		},
		MCP: RawMCP{
			Servers: map[string]RawServer{
				"kiwi":   {Endpoint: "https://mcp.kiwi.com", Namespace: "kiwi"},
				"github": {Endpoint: "https://api.github.com/mcp/", Namespace: "github"},
				// Not referenced by any profile in either workspace.
				"orphan": {Endpoint: "https://orphan.example.com", Namespace: "orphan"},
			},
		},
		Profiles: map[string]RawProfile{
			"acme/alice/default": {
				Tools: map[string]RawToolFilter{
					"kiwi": {Include: []string{"search-flight"}},
				},
			},
			// other workspace — must not appear in acme projection
			"other/bob/default": {
				Tools: map[string]RawToolFilter{
					"github": {Include: []string{"search_repositories"}},
				},
			},
		},
		Keys: map[string]RawKey{
			"acme/alice/sk-main": {
				RoutingOverrides: map[string]RawRoutingNode{
					"claude-3": {
						Chain: &RawChain{
							Children: []RawRoutingNode{
								{Target: &RawRoutingTarget{Provider: "fallback_p1", Name: "claude-3"}},
								{Target: &RawRoutingTarget{Provider: "anthropic", Name: "claude-3"}},
							},
						},
					},
				},
			},
			// other workspace key — must not appear in acme projection
			"other/bob/sk-main": {},
		},
		RateLimit: RawRateLimit{
			Tiers: map[string]RawRateLimitTier{
				"standard": {RPM: 100},
			},
			Policies: map[string][]RawRateLimitPolicyEntry{
				"acme":       {{Rule: "standard", Models: []string{"*"}}},
				"acme/alice": {{Rule: "standard", Models: []string{"gpt-4o"}}},
				"other":      {{Rule: "standard", Models: []string{"*"}}},
			},
		},
	}
}

func TestProjectForWorkspace_keysAndProfilesScopedToWorkspace(t *testing.T) {
	got := ProjectForWorkspace(rawForProjectionTest(), "acme")

	require.Contains(t, got.Keys, "acme/alice/sk-main")
	assert.NotContains(t, got.Keys, "other/bob/sk-main")

	require.Contains(t, got.Profiles, "acme/alice/default")
	assert.NotContains(t, got.Profiles, "other/bob/default")
}

func TestProjectForWorkspace_rateLimitPoliciesScopedToWorkspace(t *testing.T) {
	got := ProjectForWorkspace(rawForProjectionTest(), "acme")

	assert.Contains(t, got.RateLimit.Policies, "acme")
	assert.Contains(t, got.RateLimit.Policies, "acme/alice")
	assert.NotContains(t, got.RateLimit.Policies, "other")
	// Tiers travel verbatim (they are referenced by policy entries).
	assert.Contains(t, got.RateLimit.Tiers, "standard")
}

func TestProjectForWorkspace_providersReferencedByModelsAndKeys(t *testing.T) {
	got := ProjectForWorkspace(rawForProjectionTest(), "acme")

	// anthropic: in model catalog + in key routing tree → kept
	assert.Contains(t, got.LLM.Providers, "anthropic")
	// openai: in model catalog (gpt-4o) → kept
	assert.Contains(t, got.LLM.Providers, "openai")
	// bedrock: in model catalog (nova-lite) → kept
	assert.Contains(t, got.LLM.Providers, "bedrock")
	// fallback_p1: in acme key chain routing tree → kept
	assert.Contains(t, got.LLM.Providers, "fallback_p1")
	// unused: not in any model or key → pruned
	assert.NotContains(t, got.LLM.Providers, "unused")
}

func TestProjectForWorkspace_modelsWithSurvivingProviders(t *testing.T) {
	got := ProjectForWorkspace(rawForProjectionTest(), "acme")

	assert.Contains(t, got.LLM.Models, "gpt-4o")
	assert.Contains(t, got.LLM.Models, "claude-3")
	assert.Contains(t, got.LLM.Models, "nova-lite")
}

func TestProjectForWorkspace_serversReferencedByProfiles(t *testing.T) {
	got := ProjectForWorkspace(rawForProjectionTest(), "acme")

	// kiwi: referenced by acme/alice/default → kept
	assert.Contains(t, got.MCP.Servers, "kiwi")
	// github: only in other/bob/default (filtered out) → pruned
	assert.NotContains(t, got.MCP.Servers, "github")
	// orphan: no profile references it → pruned
	assert.NotContains(t, got.MCP.Servers, "orphan")
}

func TestProjectForWorkspace_splitRoutingCollectsAllChildProviders(t *testing.T) {
	raw := &RawConfig{
		LLM: RawLLM{
			Providers: map[string]RawProvider{
				"p1": {Kind: "anthropic", Endpoint: "https://p1.example.com"},
				"p2": {Kind: "anthropic", Endpoint: "https://p2.example.com"},
				"p3": {Kind: "anthropic", Endpoint: "https://p3.example.com"},
			},
			Models: map[string]RawModel{},
		},
		Keys: map[string]RawKey{
			"ws/u/sk": {
				RoutingOverrides: map[string]RawRoutingNode{
					"claude": {
						Split: &RawSplit{
							Children: []RawSplitChild{
								{Weight: 34, RawRoutingNode: RawRoutingNode{Target: &RawRoutingTarget{Provider: "p1"}}},
								{Weight: 33, RawRoutingNode: RawRoutingNode{Target: &RawRoutingTarget{Provider: "p2"}}},
								{Weight: 33, RawRoutingNode: RawRoutingNode{Target: &RawRoutingTarget{Provider: "p3"}}},
							},
						},
					},
				},
			},
		},
	}

	got := ProjectForWorkspace(raw, "ws")
	assert.Contains(t, got.LLM.Providers, "p1")
	assert.Contains(t, got.LLM.Providers, "p2")
	assert.Contains(t, got.LLM.Providers, "p3")
}

func TestProjectForWorkspace_unknownWorkspaceYieldsNoUserData(t *testing.T) {
	got := ProjectForWorkspace(rawForProjectionTest(), "nobody")

	assert.Nil(t, got.Keys)
	assert.Nil(t, got.Profiles)
	assert.Nil(t, got.RateLimit.Policies)
	// Model catalog survives (admin-managed, not workspace-scoped).
	assert.Contains(t, got.LLM.Providers, "anthropic")
	// Truly unreferenced provider is still pruned.
	assert.NotContains(t, got.LLM.Providers, "unused")
	// No profiles → no server references → all servers pruned.
	assert.Nil(t, got.MCP.Servers)
}

func TestProjectForWorkspace_compilesCleanly(t *testing.T) {
	projected := ProjectForWorkspace(rawForProjectionTest(), "acme")

	payload, err := MarshalRawYAML(projected)
	require.NoError(t, err)
	require.NoError(t, NewAppState().LoadConfig(payload),
		"projected config must compile without error")
}
