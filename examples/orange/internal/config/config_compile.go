package config

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// compile transforms a decoded RawConfig into an immutable ConfigSnapshot.
// It runs four sequential phases so that each phase can rely on the output of
// the previous one:
//
//  1. Leaf nodes: providers and servers — no cross-references.
//  2. Dependent nodes: models — resolve provider pointers, compile pricing.
//  3. Rate limits — validate USD dependencies against compiled models.
//  4. User records — compile keys and profiles, intern routing shapes and tool
//     filter sets into Pools.
//
// compile is called from ApplySnapshotEnvelope on every incoming payload.
// If any step returns an error the entire snapshot is discarded; the previous
// generation remains in service.
func compile(raw *RawConfig, interns *InternPool, generation uint64) (*ConfigSnapshot, error) {
	// ── Phase 1: leaf nodes ───────────────────────────────────────────────────

	providers := make(map[string]*ProviderRecord, len(raw.LLM.Providers))
	for id, r := range raw.LLM.Providers {
		kind := ProviderKind(r.Kind)
		if kind != ProviderKindAnthropic &&
			kind != ProviderKindOpenAI &&
			kind != ProviderKindBedrock {
			return nil, fmt.Errorf("provider %q: unknown kind %q", id, r.Kind)
		}
		bindings := make([]Binding, len(r.Bindings))
		for i, b := range r.Bindings {
			bindings[i] = Binding{Name: b.Name, Endpoint: b.Endpoint}
		}
		providers[id] = &ProviderRecord{
			Kind:          kind,
			BackendSchema: BackendSchema(r.BackendSchema),
			Endpoint:      r.Endpoint,
			PathPrefix:    r.PathPrefix,
			Auth:          AuthConfig(r.Auth),
			Extra:         cloneStringMap(r.Extra),
			Bindings:      bindings,
		}
	}

	servers := make(map[string]*ServerRecord, len(raw.MCP.Servers))
	for id, r := range raw.MCP.Servers {
		var auth *AuthConfig
		if r.Auth != nil {
			v := AuthConfig(*r.Auth)
			auth = &v
		}
		servers[id] = &ServerRecord{
			Endpoint:     r.Endpoint,
			Namespace:    r.Namespace,
			Auth:         auth,
			ToolsInclude: slices.Clone(r.ToolsInclude),
		}
	}

	// ── Phase 2: models ───────────────────────────────────────────────────────

	models := make(map[string]*ModelRecord, len(raw.LLM.Models))
	for id, r := range raw.LLM.Models {
		provider, ok := providers[r.Provider]
		if !ok {
			return nil, fmt.Errorf("model %q: unknown provider %q", id, r.Provider)
		}
		overrides := make(map[string]*ProviderRecord, len(r.EndpointOverrides))
		for op, provID := range r.EndpointOverrides {
			ep, ok := providers[provID]
			if !ok {
				return nil, fmt.Errorf(
					"model %q endpoint_override %q: unknown provider %q", id, op, provID,
				)
			}
			overrides[op] = ep
		}
		// APIName defaults to the catalog key when the raw record omits Name.
		apiName := r.Name
		if apiName == "" {
			apiName = id
		}
		models[id] = &ModelRecord{
			Provider:          provider,
			APIName:           apiName,
			Binding:           r.Binding,
			EndpointOverrides: overrides,
			RequestMutations:  compileRequestMutations(r.RequestMutations),
			Pricing:           compilePricing(r.Pricing),
			Metadata:          compileMetadata(r.Metadata),
		}
	}

	// ── Phase 3: rate limits (workspace and user scopes only) ────────────────
	// Key-scope rules (3-segment keys) are user-managed and compiled into
	// KeyRecord.RateLimitRules in Phase 4, not into GlobalConfig.RateLimits.

	rateLimits := make(map[string][]RateLimitRule, len(raw.RateLimit.Policies))
	for scope, rawRules := range raw.RateLimit.Policies {
		if err := validateScopeKey(scope); err != nil {
			return nil, fmt.Errorf("rate_limit.policies[%q]: %w", scope, err)
		}
		// Skip 3-segment (key-scope) entries; they are handled in Phase 4.
		if strings.Count(scope, "/") == 2 {
			continue
		}
		expanded, err := expandPolicyEntries(rawRules, raw.RateLimit.Tiers, scope)
		if err != nil {
			return nil, err
		}
		rules, err := compileRateLimitRules(expanded, models, scope)
		if err != nil {
			return nil, err
		}
		rateLimits[scope] = rules
	}

	global := &GlobalConfig{
		Providers:  providers,
		Models:     models,
		Servers:    servers,
		RateLimits: rateLimits,
		Interns:    interns,
	}
	pools := &Pools{
		Routing:     &RoutingPool{index: make(map[string]uint32)},
		ToolFilters: &ToolFilterPool{index: make(map[string]uint32)},
		Auth:        &AuthPool{index: make(map[string]uint32)},
	}

	// ── Phase 4: user records ─────────────────────────────────────────────────
	// Keys and Profiles are nil when the snapshot payload did not include user
	// records (production delivers them separately via streaming updates).

	var compiledKeys map[string]*KeyRecord
	if raw.Keys != nil {
		compiledKeys = make(map[string]*KeyRecord, len(raw.Keys))
		for id, rawKey := range raw.Keys {
			pid, err := parseID(id, interns)
			if err != nil {
				return nil, fmt.Errorf("keys[%q]: %w", id, err)
			}
			shapeKeys := make(map[string]string, len(rawKey.RoutingOverrides))
			for modelID, rawNode := range rawKey.RoutingOverrides {
				routing, shapeKey, err := compileRoutingNode(rawNode, providers)
				if err != nil {
					return nil, fmt.Errorf("keys[%q].routing_overrides[%q]: %w", id, modelID, err)
				}
				pools.Routing.Intern(shapeKey, routing)
				shapeKeys[modelID] = shapeKey
			}
			// Key-scope rate limits are looked up from the shared rate_limits map by
			// the key's full ID. They are user-managed and not present in GlobalConfig.
			var keyRules []RateLimitRule
			if rawKeyRules, ok := raw.RateLimit.Policies[id]; ok {
				keyRules, err = compileRateLimitRules(rawKeyRules, models, id)
				if err != nil {
					return nil, fmt.Errorf("keys[%q].rate_limits: %w", id, err)
				}
			}
			compiledKeys[id] = &KeyRecord{
				Workspace:        pid.Workspace,
				User:             pid.User,
				Name:             pid.Name,
				RoutingShapeKeys: shapeKeys,
				RateLimitRules:   keyRules,
			}
		}
	}

	var compiledProfiles map[string]*ProfileRecord
	var profilesByPath map[string]*ProfileRecord
	if raw.Profiles != nil {
		compiledProfiles = make(map[string]*ProfileRecord, len(raw.Profiles))
		profilesByPath = make(map[string]*ProfileRecord, len(raw.Profiles))
		for id, rawProfile := range raw.Profiles {
			pid, err := parseID(id, interns)
			if err != nil {
				return nil, fmt.Errorf("profiles[%q]: %w", id, err)
			}
			if rawProfile.Path != "" && strings.Contains(rawProfile.Path, "/") {
				return nil, fmt.Errorf("profiles[%q]: path %q must not contain slashes", id, rawProfile.Path)
			}
			toolShapeKey, authShapeKey, err := compileProfileShapes(rawProfile, servers, pools)
			if err != nil {
				return nil, fmt.Errorf("profiles[%q]: %w", id, err)
			}
			rec := &ProfileRecord{
				Workspace:          pid.Workspace,
				User:               pid.User,
				Name:               pid.Name,
				Path:               rawProfile.Path,
				ToolFilterShapeKey: toolShapeKey,
				AuthShapeKey:       authShapeKey,
			}
			compiledProfiles[id] = rec
			if rawProfile.Path != "" {
				if existing, dup := profilesByPath[rawProfile.Path]; dup {
					return nil, fmt.Errorf("profiles[%q]: path %q already used by another profile", id, existing.Path)
				}
				profilesByPath[rawProfile.Path] = rec
			}
		}
	}

	return &ConfigSnapshot{
		Generation:     generation,
		Global:         global,
		Pools:          pools,
		Keys:           compiledKeys,
		Profiles:       compiledProfiles,
		ProfilesByPath: profilesByPath,
	}, nil
}

// ── Routing compilation ───────────────────────────────────────────────────────

// compileRoutingNode compiles a raw routing tree node into a RoutingConfig and
// returns a stable shape key that identifies the node's structure for pool
// deduplication. The shape key is the JSON encoding of the raw node; it is
// consistent within a process lifetime and does not need to survive restarts.
func compileRoutingNode(node RawRoutingNode, providers map[string]*ProviderRecord) (RoutingConfig, string, error) {
	cfg, err := compileRoutingRecursive(node, providers, "")
	if err != nil {
		return RoutingConfig{}, "", err
	}
	keyBytes, err := json.Marshal(node)
	if err != nil {
		// RawRoutingNode contains only string and int fields; Marshal cannot fail here.
		return RoutingConfig{}, "", fmt.Errorf("internal: routing shape key: %w", err)
	}
	return cfg, string(keyBytes), nil
}

// compileRoutingRecursive validates and compiles one routing node.
// parentKind carries the parent's kind string ("chain", "split", or "") to
// enforce nesting rules: chain-of-chain and split-of-split are rejected.
func compileRoutingRecursive(node RawRoutingNode, providers map[string]*ProviderRecord, parentKind string) (RoutingConfig, error) {
	setCount := 0
	if node.Target != nil {
		setCount++
	}
	if node.Chain != nil {
		setCount++
	}
	if node.Split != nil {
		setCount++
	}
	if setCount != 1 {
		return RoutingConfig{}, fmt.Errorf("exactly one of target/chain/split must be set, got %d", setCount)
	}

	switch {
	case node.Target != nil:
		return compileTargetNode(node.Target, providers)

	case node.Chain != nil:
		if parentKind == "chain" {
			return RoutingConfig{}, fmt.Errorf("chain-of-chain is not supported")
		}
		return compileChainNode(node.Chain, providers)

	default: // node.Split != nil
		if parentKind == "split" {
			return RoutingConfig{}, fmt.Errorf("split-of-split is not supported")
		}
		return compileSplitNode(node.Split, providers)
	}
}

// compileTargetNode compiles a leaf routing node that names one provider.
func compileTargetNode(r *RawRoutingTarget, providers map[string]*ProviderRecord) (RoutingConfig, error) {
	provider, ok := providers[r.Provider]
	if !ok {
		return RoutingConfig{}, fmt.Errorf("unknown provider %q", r.Provider)
	}
	return RoutingConfig{
		Kind: RoutingKindTarget,
		Target: &RoutingTarget{
			Provider:  provider,
			ModelName: r.Name, // empty means: use catalog's APIName
		},
	}, nil
}

// compileChainNode compiles an ordered-fallback routing node.
// Children may be target or split nodes; chain-of-chain is forbidden.
func compileChainNode(r *RawChain, providers map[string]*ProviderRecord) (RoutingConfig, error) {
	if len(r.Children) == 0 {
		return RoutingConfig{}, fmt.Errorf("chain must have at least one child")
	}
	children := make([]RoutingConfig, len(r.Children))
	for i, child := range r.Children {
		cfg, err := compileRoutingRecursive(child, providers, "chain")
		if err != nil {
			return RoutingConfig{}, fmt.Errorf("child[%d]: %w", i, err)
		}
		children[i] = cfg
	}
	var retry *RetryConfig
	if r.Retry != nil {
		retry = &RetryConfig{
			RetryOn:              r.Retry.RetryOn,
			RetryGrpcOn:          r.Retry.RetryGrpcOn,
			PerTryTimeoutMs:      r.Retry.PerTryTimeoutMs,
			RetriableStatusCodes: r.Retry.RetriableStatusCodes,
			RetriableHeaderNames: r.Retry.RetriableHeaderNames,
		}
	}
	return RoutingConfig{
		Kind: RoutingKindChain,
		Chain: &ChainConfig{
			Retry:    retry,
			Children: children,
		},
	}, nil
}

// compileSplitNode compiles a weighted traffic-distribution routing node.
// Weights must sum to 100. Children may be target or chain nodes; split-of-split
// is forbidden.
func compileSplitNode(r *RawSplit, providers map[string]*ProviderRecord) (RoutingConfig, error) {
	if len(r.Children) == 0 {
		return RoutingConfig{}, fmt.Errorf("split must have at least one child")
	}
	total := 0
	children := make([]WeightedRoutingConfig, len(r.Children))
	for i, child := range r.Children {
		if child.Weight <= 0 {
			return RoutingConfig{}, fmt.Errorf("child[%d]: weight must be positive, got %d", i, child.Weight)
		}
		total += child.Weight
		cfg, err := compileRoutingRecursive(child.RawRoutingNode, providers, "split")
		if err != nil {
			return RoutingConfig{}, fmt.Errorf("child[%d]: %w", i, err)
		}
		children[i] = WeightedRoutingConfig{Weight: child.Weight, Child: cfg}
	}
	if total != 100 {
		return RoutingConfig{}, fmt.Errorf("split weights must sum to 100, got %d", total)
	}
	return RoutingConfig{
		Kind:  RoutingKindSplit,
		Split: &SplitConfig{Children: children},
	}, nil
}

// ── Profile compilation ───────────────────────────────────────────────────────

// compileProfileShapes compiles a raw profile into sorted tool-filter and
// auth-override slices, interns both into the snapshot's pools, and returns
// their shape keys for storage in ProfileRecord.
func compileProfileShapes(rawProfile RawProfile, servers map[string]*ServerRecord, pools *Pools) (toolShapeKey, authShapeKey string, err error) {
	filters := make([]ToolFilter, 0, len(rawProfile.Tools))
	for serverID, rawFilter := range rawProfile.Tools {
		srv, ok := servers[serverID]
		if !ok {
			return "", "", fmt.Errorf("tools[%q]: unknown server", serverID)
		}
		// Profile include must be a strict subset of the server-level allowlist.
		// An empty server allowlist means all tools are permitted.
		if len(srv.ToolsInclude) > 0 && len(rawFilter.Include) > 0 {
			srvSet := make(map[string]struct{}, len(srv.ToolsInclude))
			for _, t := range srv.ToolsInclude {
				srvSet[t] = struct{}{}
			}
			for _, t := range rawFilter.Include {
				if _, ok := srvSet[t]; !ok {
					return "", "", fmt.Errorf(
						"tools[%q]: tool %q not in server allowlist", serverID, t,
					)
				}
			}
		}
		filters = append(filters, ToolFilter{
			ServerID: serverID,
			Include:  slices.Clone(rawFilter.Include),
			Optional: rawFilter.Optional,
		})
	}
	// Sort by ServerID so the shape key is independent of map iteration order.
	slices.SortFunc(filters, func(a, b ToolFilter) int {
		return strings.Compare(a.ServerID, b.ServerID)
	})
	toolKey := buildToolFilterShapeKey(filters)
	pools.ToolFilters.Intern(toolKey, filters)

	overrides := make([]AuthOverride, 0, len(rawProfile.Auth))
	for serverID, rawAuth := range rawProfile.Auth {
		if _, ok := servers[serverID]; !ok {
			return "", "", fmt.Errorf("auth[%q]: unknown server", serverID)
		}
		overrides = append(overrides, AuthOverride{
			ServerID: serverID,
			Auth:     AuthConfig(rawAuth),
		})
	}
	// Sort by ServerID for the same reason as tool filters above.
	slices.SortFunc(overrides, func(a, b AuthOverride) int {
		return strings.Compare(a.ServerID, b.ServerID)
	})
	authKey := buildAuthShapeKey(overrides)
	pools.Auth.Intern(authKey, overrides)

	return toolKey, authKey, nil
}

// buildToolFilterShapeKey serialises a sorted ToolFilter slice into a stable,
// human-readable string. The key only needs to be consistent within a single
// process lifetime — it is never stored durably.
func buildToolFilterShapeKey(filters []ToolFilter) string {
	if len(filters) == 0 {
		return ""
	}
	var b strings.Builder
	for _, f := range filters {
		b.WriteString(f.ServerID)
		b.WriteByte(':')
		b.WriteString(strings.Join(f.Include, ","))
		b.WriteByte(':')
		if f.Optional {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
		b.WriteByte(';')
	}
	return b.String()
}

// buildAuthShapeKey serialises a sorted AuthOverride slice into a stable,
// human-readable string.
func buildAuthShapeKey(overrides []AuthOverride) string {
	if len(overrides) == 0 {
		return ""
	}
	var b strings.Builder
	for _, o := range overrides {
		b.WriteString(o.ServerID)
		b.WriteByte(':')
		b.WriteString(o.Auth.Type)
		b.WriteByte(':')
		b.WriteString(o.Auth.SecretRef)
		b.WriteByte(';')
	}
	return b.String()
}

// ── Rate limit compilation ────────────────────────────────────────────────────

// compileRateLimitRules validates and compiles a slice of raw rate-limit rules
// for a given scope string. It is shared by Phase 3 (admin scopes) and Phase 4
// (key-scope rules from KeyRecord).
func compileRateLimitRules(rawRules []RawRateLimitPolicyEntry, models map[string]*ModelRecord, scope string) ([]RateLimitRule, error) {
	rules := make([]RateLimitRule, len(rawRules))
	for i, r := range rawRules {
		if len(r.Models) == 0 {
			return nil, fmt.Errorf("rate_limit.policies[%q][%d]: models must not be empty", scope, i)
		}
		if err := validateUSDDependency(r, models, scope, i); err != nil {
			return nil, err
		}
		compiled, err := compileRateLimitRule(r)
		if err != nil {
			return nil, fmt.Errorf("rate_limit.policies[%q][%d]: %w", scope, i, err)
		}
		rules[i] = compiled
	}
	return rules, nil
}

// validateUSDDependency rejects any USD spend limit that targets a model without
// a pricing block. Without pricing, the proxy cannot convert token counts to
// dollars and the limit would silently never trigger.
func validateUSDDependency(r RawRateLimitPolicyEntry, models map[string]*ModelRecord, scope string, i int) error {
	if r.USDPerMinute.IsZero() && r.USDPerHour.IsZero() && r.USDPerDay.IsZero() {
		return nil
	}
	check := func(modelID string) error {
		m, ok := models[modelID]
		if !ok {
			return fmt.Errorf("rate_limit.policies[%q][%d]: model %q not found", scope, i, modelID)
		}
		if m.Pricing == nil {
			return fmt.Errorf(
				"rate_limit.policies[%q][%d]: usd limit requires pricing block on model %q",
				scope, i, modelID,
			)
		}
		return nil
	}
	// A lone wildcard means every model in the catalog must carry pricing.
	if len(r.Models) == 1 && r.Models[0] == "*" {
		for modelID := range models {
			if err := check(modelID); err != nil {
				return err
			}
		}
		return nil
	}
	// Mixed lists: skip the bare wildcard element and check named models only.
	for _, modelID := range r.Models {
		if modelID == "*" {
			continue
		}
		if err := check(modelID); err != nil {
			return err
		}
	}
	return nil
}

// expandPolicyEntries resolves rule: references in each policy entry against
// the named tiers. Tier fields are used as the base; entry inline fields
// override them. Returns an error if an entry references an unknown tier.
func expandPolicyEntries(entries []RawRateLimitPolicyEntry, tiers map[string]RawRateLimitTier, scope string) ([]RawRateLimitPolicyEntry, error) {
	out := make([]RawRateLimitPolicyEntry, len(entries))
	for i, e := range entries {
		if e.Rule == "" {
			out[i] = e
			continue
		}
		tier, ok := tiers[e.Rule]
		if !ok {
			return nil, fmt.Errorf("rate_limit.policies[%q][%d]: unknown rule %q", scope, i, e.Rule)
		}
		out[i] = applyTier(e, tier)
	}
	return out, nil
}

// applyTier returns a copy of entry with any zero/empty field filled from tier.
// Non-zero entry fields take precedence (entry overrides tier).
func applyTier(entry RawRateLimitPolicyEntry, tier RawRateLimitTier) RawRateLimitPolicyEntry {
	if entry.USDPerMinute.IsZero() {
		entry.USDPerMinute = tier.USDPerMinute
	}
	if entry.USDPerHour.IsZero() {
		entry.USDPerHour = tier.USDPerHour
	}
	if entry.USDPerDay.IsZero() {
		entry.USDPerDay = tier.USDPerDay
	}
	if entry.RPM == 0 {
		entry.RPM = tier.RPM
	}
	if entry.RPH == 0 {
		entry.RPH = tier.RPH
	}
	if entry.RPD == 0 {
		entry.RPD = tier.RPD
	}
	if entry.InputTokensPerMinute == 0 {
		entry.InputTokensPerMinute = tier.InputTokensPerMinute
	}
	if entry.InputTokensPerHour == 0 {
		entry.InputTokensPerHour = tier.InputTokensPerHour
	}
	if entry.InputTokensPerDay == 0 {
		entry.InputTokensPerDay = tier.InputTokensPerDay
	}
	if entry.OutputTokensPerMinute == 0 {
		entry.OutputTokensPerMinute = tier.OutputTokensPerMinute
	}
	if entry.OutputTokensPerHour == 0 {
		entry.OutputTokensPerHour = tier.OutputTokensPerHour
	}
	if entry.OutputTokensPerDay == 0 {
		entry.OutputTokensPerDay = tier.OutputTokensPerDay
	}
	if entry.CacheReadTokensPerHour == 0 {
		entry.CacheReadTokensPerHour = tier.CacheReadTokensPerHour
	}
	if entry.CacheReadTokensPerDay == 0 {
		entry.CacheReadTokensPerDay = tier.CacheReadTokensPerDay
	}
	if entry.CacheWriteTokensPerHour == 0 {
		entry.CacheWriteTokensPerHour = tier.CacheWriteTokensPerHour
	}
	if entry.CacheWriteTokensPerDay == 0 {
		entry.CacheWriteTokensPerDay = tier.CacheWriteTokensPerDay
	}
	if entry.OnExceed == "" {
		entry.OnExceed = tier.OnExceed
	}
	return entry
}

// validateScopeKey ensures a rate-limit scope string is a valid 1-, 2-, or
// 3-segment slash-separated key (workspace, workspace/user, or workspace/user/name).
// Four or more segments are rejected to prevent accidental key-collisions.
func validateScopeKey(scope string) error {
	parts := strings.SplitN(scope, "/", 4)
	if len(parts) > 3 {
		return fmt.Errorf("too many segments (max 3: workspace/user/name)")
	}
	for i, p := range parts {
		if p == "" {
			return fmt.Errorf("segment %d is empty", i)
		}
	}
	return nil
}

// compileRateLimitRule converts a raw rate limit rule into its domain form.
// It defaults OnExceed to "reject" and validates that the supplied value is
// one of the three known actions.
func compileRateLimitRule(r RawRateLimitPolicyEntry) (RateLimitRule, error) {
	onExceed := r.OnExceed
	switch onExceed {
	case "", "reject":
		onExceed = "reject"
	case "throttle", "log_only":
		// valid, use as-is
	default:
		return RateLimitRule{}, fmt.Errorf("invalid on_exceed %q: want reject|throttle|log_only", onExceed)
	}
	return RateLimitRule{
		Models:                  slices.Clone(r.Models),
		USDPerMinute:            r.USDPerMinute,
		USDPerHour:              r.USDPerHour,
		USDPerDay:               r.USDPerDay,
		RPM:                     r.RPM,
		RPH:                     r.RPH,
		RPD:                     r.RPD,
		InputTokensPerMinute:    r.InputTokensPerMinute,
		InputTokensPerHour:      r.InputTokensPerHour,
		InputTokensPerDay:       r.InputTokensPerDay,
		OutputTokensPerMinute:   r.OutputTokensPerMinute,
		OutputTokensPerHour:     r.OutputTokensPerHour,
		OutputTokensPerDay:      r.OutputTokensPerDay,
		CacheReadTokensPerHour:  r.CacheReadTokensPerHour,
		CacheReadTokensPerDay:   r.CacheReadTokensPerDay,
		CacheWriteTokensPerHour: r.CacheWriteTokensPerHour,
		CacheWriteTokensPerDay:  r.CacheWriteTokensPerDay,
		OnExceed:                onExceed,
	}, nil
}

// ── Scalar helpers ────────────────────────────────────────────────────────────

// compileRequestMutations converts a raw request-mutations block to its domain form.
// Returns nil when r is nil so ModelRecord.RequestMutations is nil for unmodified models.
func compileRequestMutations(r *RawRequestMutations) *RequestMutations {
	if r == nil {
		return nil
	}
	return &RequestMutations{
		Headers: cloneStringMap(r.Headers),
		Body:    cloneStringMap(r.Body),
	}
}

// compilePricing converts a raw pricing block to its domain form.
// Returns nil when r is nil so ModelRecord.Pricing is nil for unpriced models.
func compilePricing(r *RawModelPricing) *ModelPricing {
	if r == nil {
		return nil
	}
	return &ModelPricing{
		InputMTok:      r.InputMTok,
		OutputMTok:     r.OutputMTok,
		CacheReadMTok:  r.CacheReadMTok,
		CacheWriteMTok: r.CacheWriteMTok,
	}
}

// compileMetadata converts a raw metadata block to its domain form.
// Returns nil when r is nil so ModelRecord.Metadata is nil for unlabeled models.
func compileMetadata(r *RawMetadata) *ModelMetadata {
	if r == nil {
		return nil
	}
	return &ModelMetadata{
		Description:   r.Description,
		ContextLength: r.ContextLength,
		MaxTokens:     r.MaxTokens,
		Tags:          slices.Clone(r.Tags),
	}
}

// cloneStringMap returns a shallow copy of m, or nil when m is nil.
func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
