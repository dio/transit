package config

import "strings"

// ProjectForWorkspace returns a shallow copy of raw containing only records
// that belong to workspaceID. The result is a self-contained, workspace-scoped
// snapshot: the egress can compile it without any cross-workspace data.
//
// Three things happen here:
//
//  1. User records (keys, profiles, rate-limit policies) are filtered to those
//     whose ID / scope starts with workspaceID.
//  2. The surviving records are walked to collect the provider and server names
//     they actually reference.
//  3. Providers, models, and servers are pruned to the referenced sets so the
//     egress does not receive credentials or endpoints it cannot use.
//
// Rate-limit tiers (named primitives) are kept verbatim because they are
// stateless templates; the policies that reference them are already filtered.
func ProjectForWorkspace(raw *RawConfig, workspaceID string) *RawConfig {
	prefix := workspaceID + "/"

	// ── Step 1: filter user records ─────────────────────────────────────────

	keys := filterMapByKey(raw.Keys, func(id string) bool {
		return strings.HasPrefix(id, prefix)
	})
	profiles := filterMapByKey(raw.Profiles, func(id string) bool {
		return strings.HasPrefix(id, prefix)
	})
	// Keep rate-limit policies whose first segment matches the workspace.
	// This covers both 1-segment (workspace) and 2-segment (workspace/user) scopes.
	// 3-segment (key) entries travel with their Key record and are already filtered
	// by the keys map above; they do not appear in raw.RateLimit.Policies.
	policies := filterMapByKey(raw.RateLimit.Policies, func(scope string) bool {
		seg0 := scope
		if i := strings.Index(scope, "/"); i >= 0 {
			seg0 = scope[:i]
		}
		return seg0 == workspaceID
	})

	// ── Step 2: collect referenced providers and servers ─────────────────────

	wantProviders := make(map[string]bool)
	wantServers := make(map[string]bool)

	// Every model in the catalog references a provider (and optional
	// endpoint_override providers). These are admin-defined and the workspace
	// egress needs the full model catalog to route arbitrary requests.
	for _, m := range raw.LLM.Models {
		wantProviders[m.Provider] = true
		for _, p := range m.EndpointOverrides {
			wantProviders[p] = true
		}
	}
	// Routing overrides in keys may reference providers that are not in the
	// global model catalog (e.g. fallback_p1 used only in a chain node).
	for _, k := range keys {
		for _, node := range k.RoutingOverrides {
			collectRoutingProviders(node, wantProviders)
		}
	}
	// Profiles reference servers by ID in their tools and auth maps.
	for _, p := range profiles {
		for serverID := range p.Tools {
			wantServers[serverID] = true
		}
		for serverID := range p.Auth {
			wantServers[serverID] = true
		}
	}

	// ── Step 3: prune to referenced sets ─────────────────────────────────────

	providers := filterMapByKey(raw.LLM.Providers, func(id string) bool {
		return wantProviders[id]
	})
	// Keep models whose primary provider survived — models that route to a
	// pruned provider would fail to compile anyway.
	models := filterMap(raw.LLM.Models, func(_ string, m RawModel) bool {
		return wantProviders[m.Provider]
	})
	servers := filterMapByKey(raw.MCP.Servers, func(id string) bool {
		return wantServers[id]
	})

	return &RawConfig{
		LLM:      RawLLM{Providers: providers, Models: models},
		MCP:      RawMCP{Servers: servers},
		Profiles: profiles,
		Keys:     keys,
		RateLimit: RawRateLimit{
			Tiers:    raw.RateLimit.Tiers,
			Policies: policies,
		},
	}
}

// collectRoutingProviders walks a RawRoutingNode tree depth-first and adds
// every provider name referenced by a leaf target node to set.
func collectRoutingProviders(node RawRoutingNode, set map[string]bool) {
	switch {
	case node.Target != nil:
		set[node.Target.Provider] = true
	case node.Chain != nil:
		for _, c := range node.Chain.Children {
			collectRoutingProviders(c, set)
		}
	case node.Split != nil:
		for _, c := range node.Split.Children {
			collectRoutingProviders(c.RawRoutingNode, set)
		}
	}
}

// filterMap returns a new map containing only entries for which keep returns
// true. Returns nil when the input map is empty or no entries pass the filter.
func filterMap[K comparable, V any](m map[K]V, keep func(K, V) bool) map[K]V {
	if len(m) == 0 {
		return nil
	}
	out := make(map[K]V, len(m))
	for k, v := range m {
		if keep(k, v) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// filterMapByKey is filterMap with a key-only predicate.
func filterMapByKey[K comparable, V any](m map[K]V, keep func(K) bool) map[K]V {
	return filterMap(m, func(k K, _ V) bool { return keep(k) })
}
