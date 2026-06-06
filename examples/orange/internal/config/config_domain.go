package config

import (
	"strings"

	"github.com/shopspring/decimal"
)

// ── Provider ──────────────────────────────────────────────────────────────────

// ProviderKind is the wire protocol used to reach an upstream LLM provider.
type ProviderKind string

const (
	ProviderKindAnthropic ProviderKind = "anthropic"
	ProviderKindOpenAI    ProviderKind = "openai"
	ProviderKindBedrock   ProviderKind = "bedrock"
)

// BackendSchema overrides the translator selected for a provider. When absent
// the provider's Kind is used as the schema name.
type BackendSchema string

const (
	BackendSchemaGCPVertex    BackendSchema = "gcpvertexai"
	BackendSchemaGCPAnthropic BackendSchema = "gcpanthropic"
	BackendSchemaAWSBedrock   BackendSchema = "awsbedrock"
)

// AuthConfig carries an auth type and an opaque secret reference.
// SecretRef uses env://, file://, literal://, or any scheme supported by the
// configured SecretResolver. The reference is never resolved inside the snapshot;
// callers ask a SecretResolver at request time so secrets can rotate without a
// config reload.
type AuthConfig struct {
	Type      string
	SecretRef string
}

// ProviderRecord is the compiled, immutable form of one upstream LLM provider.
// SecretRef strings inside Auth and Extra are kept as-is from the config;
// see AuthConfig for the resolution contract.
type ProviderRecord struct {
	Kind          ProviderKind
	BackendSchema BackendSchema
	Endpoint      string
	Auth          AuthConfig
	Extra         map[string]string
}

// ── Model ─────────────────────────────────────────────────────────────────────

// ModelPricing holds per-model token prices in USD per million tokens.
// Nil means no pricing is configured; a USD-based rate limit rule targeting
// this model will be rejected at compile time.
// decimal.Decimal prevents the floating-point rounding that accumulates when
// multiplying fractional USD/MTok prices by large token counts.
type ModelPricing struct {
	InputMTok      decimal.Decimal
	OutputMTok     decimal.Decimal
	CacheReadMTok  decimal.Decimal
	CacheWriteMTok decimal.Decimal
}

// Cost returns the total USD cost for one response given its token counts.
// Returns decimal.Zero when p is nil so callers never need a nil check.
func (p *ModelPricing) Cost(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int) decimal.Decimal {
	if p == nil {
		return decimal.Zero
	}
	M := decimal.NewFromInt(1_000_000)
	return decimal.NewFromInt(int64(inputTokens)).Mul(p.InputMTok).Div(M).
		Add(decimal.NewFromInt(int64(outputTokens)).Mul(p.OutputMTok).Div(M)).
		Add(decimal.NewFromInt(int64(cacheReadTokens)).Mul(p.CacheReadMTok).Div(M)).
		Add(decimal.NewFromInt(int64(cacheWriteTokens)).Mul(p.CacheWriteMTok).Div(M))
}

// ModelMetadata carries informational attributes surfaced in GET /v1/models.
type ModelMetadata struct {
	Description   string
	ContextLength int
	MaxTokens     int
	Tags          []string
}

// ModelRecord is the compiled, immutable form of one catalog entry.
// Provider is a resolved pointer; EndpointOverrides maps operation names
// (e.g. "chat_completions") to alternate providers for that operation only.
type ModelRecord struct {
	Provider          *ProviderRecord
	APIName           string // backend model name; defaults to the catalog key
	EndpointOverrides map[string]*ProviderRecord
	Pricing           *ModelPricing  // nil when not configured
	Metadata          *ModelMetadata // nil when not configured
}

// ── MCP Server ────────────────────────────────────────────────────────────────

// ServerRecord is the compiled, immutable form of one MCP server definition.
type ServerRecord struct {
	Endpoint     string
	Namespace    string
	Auth         *AuthConfig // nil when no auth is configured
	ToolsInclude []string    // server-level allowlist; profiles must be a subset
}

// ── Rate limits ───────────────────────────────────────────────────────────────

// RateLimitRule is the compiled form of RawRateLimitRule.
// Zero values are unconstrained for that dimension. USD limits use
// decimal.Decimal; token counts use int (exact integers, no fractional values).
type RateLimitRule struct {
	Models []string

	USDPerMinute decimal.Decimal
	USDPerHour   decimal.Decimal
	USDPerDay    decimal.Decimal

	RPM int
	RPH int
	RPD int

	InputTokensPerMinute int
	InputTokensPerHour   int
	InputTokensPerDay    int

	OutputTokensPerMinute int
	OutputTokensPerHour   int
	OutputTokensPerDay    int

	CacheReadTokensPerHour int
	CacheReadTokensPerDay  int

	CacheWriteTokensPerHour int
	CacheWriteTokensPerDay  int

	// OnExceed is "reject" (default), "throttle", or "log_only".
	OnExceed string
}

// MatchesModel reports whether this rule applies to modelID.
// The wildcard "*" matches any model.
func (r RateLimitRule) MatchesModel(modelID string) bool {
	for _, m := range r.Models {
		if m == "*" || m == modelID {
			return true
		}
	}
	return false
}

// ── GlobalConfig ──────────────────────────────────────────────────────────────

// GlobalConfig is the admin-owned configuration tree for one snapshot
// generation. It is immutable after publication; changing any admin data
// requires compiling a new ConfigSnapshot and atomically swapping the pointer.
//
// RateLimits holds only workspace-scope (1-segment) and user-scope (2-segment)
// rules. Key-scope rules are user-managed and live in KeyRecord.RateLimitRules
// so that key owners can set limits on their own keys without admin involvement.
type GlobalConfig struct {
	Providers  map[string]*ProviderRecord
	Models     map[string]*ModelRecord
	Servers    map[string]*ServerRecord
	RateLimits map[string][]RateLimitRule // workspace and user scopes only
	Interns    *InternPool
}

// ResolveRateLimitRules returns the admin-managed rules that apply to the given
// keyID and modelID at the workspace and user scopes, in that order. Both scopes
// always accumulate; neither short-circuits the other.
//
// Key-scope rules are not included here — callers that hold a *KeyRecord should
// append filterRulesByModel(key.RateLimitRules, modelID) themselves.
//
// Returns nil for malformed keyIDs (fewer than three slash-separated segments).
func (g *GlobalConfig) ResolveRateLimitRules(keyID, modelID string) []RateLimitRule {
	// SplitN with limit 3 produces at most 3 parts; we only need parts[0] and
	// parts[1] to form the workspace and workspace/user scope keys.
	parts := strings.SplitN(keyID, "/", 3)
	if len(parts) != 3 {
		return nil
	}
	var result []RateLimitRule
	result = append(result, filterRulesByModel(g.RateLimits[parts[0]], modelID)...)
	result = append(result, filterRulesByModel(g.RateLimits[parts[0]+"/"+parts[1]], modelID)...)
	return result
}

func filterRulesByModel(rules []RateLimitRule, modelID string) []RateLimitRule {
	var result []RateLimitRule
	for _, r := range rules {
		if r.MatchesModel(modelID) {
			result = append(result, r)
		}
	}
	return result
}

// ── Routing ───────────────────────────────────────────────────────────────────

// RoutingKind identifies which field of a RoutingConfig is populated.
type RoutingKind string

const (
	RoutingKindTarget RoutingKind = "target"
	RoutingKindChain  RoutingKind = "chain"
	RoutingKindSplit  RoutingKind = "split"
)

// RoutingTarget is a leaf routing node: one provider and an optional backend
// model name override. ModelName empty means use the catalog's APIName.
type RoutingTarget struct {
	Provider  *ProviderRecord
	ModelName string
}

// RetryConfig carries Envoy-compatible retry settings for a chain node.
type RetryConfig struct {
	RetryOn         string
	PerTryTimeoutMs int
}

// RoutingConfig is one node in a compiled routing tree.
// Exactly one of Target, Chain, or Split is non-nil; Kind names which one.
type RoutingConfig struct {
	Kind   RoutingKind
	Target *RoutingTarget
	Chain  *ChainConfig
	Split  *SplitConfig
}

// ChainConfig tries children in order, stopping at the first success.
type ChainConfig struct {
	Retry    *RetryConfig // nil when no retry policy is configured
	Children []RoutingConfig
}

// SplitConfig samples one child per request by weight. Weights must sum to 100.
type SplitConfig struct {
	Children []WeightedRoutingConfig
}

// WeightedRoutingConfig is one arm of a SplitConfig.
type WeightedRoutingConfig struct {
	Weight int
	Child  RoutingConfig
}

// ── Tool and auth overrides ───────────────────────────────────────────────────

// ToolFilter is the compiled per-server capability slice within a profile.
// Empty Include means: expose all tools the server allows (open boundary).
type ToolFilter struct {
	ServerID string
	Include  []string
	Optional bool
}

// AuthOverride replaces the server-level auth for sessions using a profile.
type AuthOverride struct {
	ServerID string
	Auth     AuthConfig
}

// ── Pools ─────────────────────────────────────────────────────────────────────

// RoutingPool deduplicates compiled RoutingConfig values across all user keys.
// Entries are identified by a stable string shape key derived from the raw
// routing node; the uint32 index is pool-local and must not be stored durably.
type RoutingPool struct {
	entries []RoutingConfig
	index   map[string]uint32
}

// Intern adds routing under shapeKey if not already present and returns its
// index. Calling Intern with the same shapeKey twice is idempotent.
func (p *RoutingPool) Intern(shapeKey string, routing RoutingConfig) uint32 {
	if id, ok := p.index[shapeKey]; ok {
		return id
	}
	id := uint32(len(p.entries))
	p.entries = append(p.entries, routing)
	p.index[shapeKey] = id
	return id
}

// Get returns a pointer into the pool's backing slice for the given index.
// Returns nil when id is out of range.
func (p *RoutingPool) Get(id uint32) *RoutingConfig {
	if int(id) >= len(p.entries) {
		return nil
	}
	return &p.entries[id]
}

// GetByKey looks up a routing config by its shape key. Returns nil when the
// key is not present. Used by the resolve path in UserTable.Get.
func (p *RoutingPool) GetByKey(shapeKey string) *RoutingConfig {
	if id, ok := p.index[shapeKey]; ok {
		return p.Get(id)
	}
	return nil
}

// ToolFilterPool deduplicates compiled tool filter sets across all profiles.
type ToolFilterPool struct {
	entries [][]ToolFilter
	index   map[string]uint32
}

// Intern adds filters under shapeKey if not already present.
func (p *ToolFilterPool) Intern(shapeKey string, filters []ToolFilter) uint32 {
	if id, ok := p.index[shapeKey]; ok {
		return id
	}
	id := uint32(len(p.entries))
	p.entries = append(p.entries, filters)
	p.index[shapeKey] = id
	return id
}

// Get returns the tool filter slice for the given index, or nil if out of range.
func (p *ToolFilterPool) Get(id uint32) []ToolFilter {
	if int(id) >= len(p.entries) {
		return nil
	}
	return p.entries[id]
}

// GetByKey looks up a tool filter slice by shape key. Returns nil when absent.
func (p *ToolFilterPool) GetByKey(shapeKey string) []ToolFilter {
	if id, ok := p.index[shapeKey]; ok {
		return p.Get(id)
	}
	return nil
}

// AuthPool deduplicates compiled auth override sets across all profiles.
type AuthPool struct {
	entries [][]AuthOverride
	index   map[string]uint32
}

// Intern adds overrides under shapeKey if not already present.
func (p *AuthPool) Intern(shapeKey string, overrides []AuthOverride) uint32 {
	if id, ok := p.index[shapeKey]; ok {
		return id
	}
	id := uint32(len(p.entries))
	p.entries = append(p.entries, overrides)
	p.index[shapeKey] = id
	return id
}

// Get returns the auth override slice for the given index, or nil if out of range.
func (p *AuthPool) Get(id uint32) []AuthOverride {
	if int(id) >= len(p.entries) {
		return nil
	}
	return p.entries[id]
}

// GetByKey looks up an auth override slice by shape key. Returns nil when absent.
func (p *AuthPool) GetByKey(shapeKey string) []AuthOverride {
	if id, ok := p.index[shapeKey]; ok {
		return p.Get(id)
	}
	return nil
}

// Pools bundles the three dedup pools that are compiled alongside GlobalConfig.
// Pool entries may hold pointers into GlobalConfig, so Pools and GlobalConfig
// are always published together inside the same ConfigSnapshot.
type Pools struct {
	Routing     *RoutingPool
	ToolFilters *ToolFilterPool
	Auth        *AuthPool
}

// ── Minimal user records ──────────────────────────────────────────────────────

// KeyRecord is the minimal L2 representation of a user-owned API key.
// Workspace, User, and Name are intern handles rather than raw strings.
// RoutingShapeKeys maps client-facing model IDs to RoutingPool shape keys;
// an absent model ID means: use default routing from GlobalConfig.Models.
//
// RateLimitRules holds the key-scope rate-limit rules set by the key owner.
// They are applied after the admin-managed workspace and user-scope rules
// returned by GlobalConfig.ResolveRateLimitRules; all scopes accumulate.
type KeyRecord struct {
	Workspace        uint32
	User             uint32
	Name             uint32
	RoutingShapeKeys map[string]string
	RateLimitRules   []RateLimitRule // user-managed; key scope only
}

// ProfileRecord is the minimal L2 representation of a user-owned MCP profile.
// Shape keys reference entries in the snapshot's ToolFilterPool and AuthPool.
type ProfileRecord struct {
	Workspace          uint32
	User               uint32
	Name               uint32
	ToolFilterShapeKey string
	AuthShapeKey       string
}

// ── Snapshot ──────────────────────────────────────────────────────────────────

// ConfigSnapshot is the atomically-published unit of config state. GlobalConfig
// and Pools are always consistent with each other because pool entries may hold
// pointers into GlobalConfig. Keys and Profiles are nil when the snapshot
// payload did not include user records.
type ConfigSnapshot struct {
	Generation uint64
	Global     *GlobalConfig
	Pools      *Pools
	Keys       map[string]*KeyRecord     // compiled from snapshot payload; nil if absent
	Profiles   map[string]*ProfileRecord // compiled from snapshot payload; nil if absent
}
