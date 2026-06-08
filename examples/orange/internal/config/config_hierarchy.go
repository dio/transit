package config

import (
	"context"
	"strings"
)

// ── Scope ID helpers ─────────────────────────────────────────────────────────
//
// The SnapshotStore is keyed by an opaque string (originally "workspaceID").
// Org- and project-level configs are stored in the same store using a typed
// prefix so a single store implementation serves all three scopes without
// schema changes.
//
// Scope layout:
//
//	"org:{orgID}"   → admin-owned LLM providers, models, MCP servers, profiles
//	"proj:{projID}" → project overrides / additions of the same record types
//	"{workspaceID}" → workspace keys, rate-limit policies, plus any workspace-
//	                  specific provider/model/server/profile overrides
//
// The egress always identifies by workspace; the CP looks up the project and
// org to load the full three-level hierarchy before serving a snapshot.

// OrgScopeID returns the store key for an org-level config entry.
func OrgScopeID(orgID string) string { return "org:" + orgID }

// ProjectScopeID returns the store key for a project-level config entry.
func ProjectScopeID(projectID string) string { return "proj:" + projectID }

// IsScopeID reports whether id was produced by OrgScopeID or ProjectScopeID
// (as opposed to a plain workspace ID).
func IsScopeID(id string) bool {
	return strings.HasPrefix(id, "org:") || strings.HasPrefix(id, "proj:")
}

// ── Merge ────────────────────────────────────────────────────────────────────
//
// MergeRawConfigs merges overlay on top of base. The merge is additive for
// every map field: overlay entries win on key conflict. Neither argument is
// mutated; the returned *RawConfig owns its own map headers (values are shared
// where unchanged).
//
// Intended call sequence at Fetch time:
//
//	step1 := MergeRawConfigs(empty, orgRaw)
//	step2 := MergeRawConfigs(step1, projectRaw)
//	step3 := MergeRawConfigs(step2, workspaceRaw)
//	materialized := ProjectForWorkspace(step3, workspaceID)
//
// Rate-limit tiers and policies are merged independently: a project or
// workspace can add tiers without clobbering org-level ones, and policies at
// narrower scopes extend (not replace) broader-scope policies.
func MergeRawConfigs(base, overlay *RawConfig) *RawConfig {
	if base == nil && overlay == nil {
		return &RawConfig{}
	}
	if base == nil {
		return overlay
	}
	if overlay == nil {
		return base
	}
	return &RawConfig{
		LLM: RawLLM{
			Providers: mergeMapsCopy(base.LLM.Providers, overlay.LLM.Providers),
			Models:    mergeMapsCopy(base.LLM.Models, overlay.LLM.Models),
		},
		MCP: RawMCP{
			Servers: mergeMapsCopy(base.MCP.Servers, overlay.MCP.Servers),
		},
		Profiles:  mergeMapsCopy(base.Profiles, overlay.Profiles),
		Keys:      mergeMapsCopy(base.Keys, overlay.Keys),
		RateLimit: mergeRateLimits(base.RateLimit, overlay.RateLimit),
	}
}

// mergeMapsCopy returns a new map with all entries from base followed by all
// entries from overlay (overlay wins on collision).
func mergeMapsCopy[K comparable, V any](base, overlay map[K]V) map[K]V {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	out := make(map[K]V, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

// mergeRateLimits merges two RawRateLimit values. Tiers and policies are each
// merged independently so narrower scopes extend rather than replace broader ones.
func mergeRateLimits(base, overlay RawRateLimit) RawRateLimit {
	return RawRateLimit{
		Tiers:    mergeMapsCopy(base.Tiers, overlay.Tiers),
		Policies: mergeMapsCopy(base.Policies, overlay.Policies),
	}
}

// ── Hierarchy resolution interface ───────────────────────────────────────────

// WorkspaceHierarchy carries the org and project IDs for a workspace.
type WorkspaceHierarchy struct {
	OrgID     string
	ProjectID string
}

// HierarchyResolver resolves a workspace ID to its org and project IDs.
// ConfigService uses this to load the full three-level config hierarchy.
// Implementations must be safe for concurrent use.
type HierarchyResolver interface {
	ResolveWorkspaceHierarchy(ctx context.Context, workspaceID string) (WorkspaceHierarchy, error)
}
