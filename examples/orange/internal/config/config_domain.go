package config

import (
	"context"
	"net/url"
	"slices"
	"strings"

	"github.com/shopspring/decimal"
)

// ── Cross-cutting types ───────────────────────────────────────────────────────

// SecretResolver resolves opaque secret references (env://, file://, literal://, etc.)
// to their plaintext values. Implementations are allowed to cache resolved values;
// callers may call Invalidate to force a fresh lookup on the next Resolve call.
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
	Invalidate(ref string)
}

// Binding names an alternate endpoint for a provider. Providers with multiple
// bindings allow per-model selection of the same provider at different endpoints.
type Binding struct {
	Name     string
	Endpoint string
}

// V1Model is an OpenAI-compatible model entry for the GET /v1/models response.
type V1Model struct {
	ID       string         `json:"id"`
	Object   string         `json:"object"`
	OwnedBy  string         `json:"owned_by"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// V1ModelList is the OpenAI-compatible response body for GET /v1/models.
type V1ModelList struct {
	Object string    `json:"object"`
	Data   []V1Model `json:"data"`
}

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
	PathPrefix    *string // optional; nil means "/v1"
	Auth          AuthConfig
	Extra         map[string]string
	Bindings      []Binding
}

// EffectiveBackendSchema returns BackendSchema if set, otherwise Kind.
func (p *ProviderRecord) EffectiveBackendSchema() string {
	if p.BackendSchema != "" {
		return string(p.BackendSchema)
	}
	return string(p.Kind)
}

// ResolvedPathPrefix returns the path prefix, defaulting to "/v1".
func (p *ProviderRecord) ResolvedPathPrefix() string {
	if p.PathPrefix == nil {
		return "/v1"
	}
	return *p.PathPrefix
}

// Host returns the hostname of the provider endpoint.
func (p *ProviderRecord) Host() string {
	if p.Endpoint == "" {
		return ""
	}
	u, err := url.Parse(p.Endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// AllBindings returns the list of named bindings for this provider.
// When no explicit bindings are configured it synthesises a single
// "default" binding from Endpoint to maintain back-compat.
func (p *ProviderRecord) AllBindings() []Binding {
	if len(p.Bindings) > 0 {
		return p.Bindings
	}
	return []Binding{{Name: "default", Endpoint: p.Endpoint}}
}

// BindingEndpoint returns the endpoint URL for the named binding.
// Falls back to Endpoint when binding is empty, "default", or not found.
func (p *ProviderRecord) BindingEndpoint(binding string) string {
	if binding != "" && binding != "default" {
		for _, b := range p.Bindings {
			if b.Name == binding {
				return b.Endpoint
			}
		}
	}
	return p.Endpoint
}

// BindingHost returns the hostname for the named binding.
// Falls back to Host() when binding is empty, "default", or not found.
func (p *ProviderRecord) BindingHost(binding string) string {
	if binding != "" && binding != "default" {
		for _, b := range p.Bindings {
			if b.Name == binding {
				u, err := url.Parse(b.Endpoint)
				if err != nil {
					return ""
				}
				return u.Hostname()
			}
		}
	}
	return p.Host()
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

// RequestMutations carries header and body field injections applied to every
// upstream request for a model, after the translator has run.
// Values may use env://, file://, or literal:// references resolved at request time.
// Body keys support dot-path notation to address nested JSON fields.
type RequestMutations struct {
	Headers map[string]string // injected into upstream request headers
	Body    map[string]string // merged into upstream request body; dot-path keys create nested objects
}

// ModelRecord is the compiled, immutable form of one catalog entry.
// Provider is a resolved pointer; EndpointOverrides maps operation names
// (e.g. "chat_completions") to alternate providers for that operation only.
// Binding names a provider binding (alternate endpoint) to use for this model;
// empty means use the provider's default endpoint.
type ModelRecord struct {
	Provider          *ProviderRecord
	APIName           string // backend model name; defaults to the catalog key
	Binding           string
	EndpointOverrides map[string]*ProviderRecord
	RequestMutations  *RequestMutations // nil when not configured
	Pricing           *ModelPricing     // nil when not configured
	Metadata          *ModelMetadata    // nil when not configured
}

// ── MCP Server ────────────────────────────────────────────────────────────────

// ServerRecord is the compiled, immutable form of one MCP server definition.
type ServerRecord struct {
	Endpoint     string
	Namespace    string
	Auth         *AuthConfig // nil when no auth is configured
	ToolsInclude []string    // server-level allowlist; profiles must be a subset
}

// Host returns the hostname of the MCP server endpoint.
func (s *ServerRecord) Host() string {
	if s.Endpoint == "" {
		return ""
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// Path returns the path component of the MCP server endpoint, defaulting to "/".
func (s *ServerRecord) Path() string {
	if s.Endpoint == "" {
		return "/"
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Path == "" || u.Path == "/" {
		return "/"
	}
	return u.Path
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

// LookupModel returns the compiled model record for modelID. Returns false when
// modelID is not present in the catalog.
func (g *GlobalConfig) LookupModel(modelID string) (*ModelRecord, bool) {
	m, ok := g.Models[modelID]
	return m, ok
}

// V1Models returns a stable, sorted slice of V1Model entries for every model
// in the catalog. OwnedBy is set to the provider's Kind string.
func (g *GlobalConfig) V1Models() []V1Model {
	out := make([]V1Model, 0, len(g.Models))
	for id, m := range g.Models {
		ownedBy := ""
		if m.Provider != nil {
			ownedBy = string(m.Provider.Kind)
		}
		out = append(out, V1Model{ID: id, Object: "model", OwnedBy: ownedBy})
	}
	slices.SortFunc(out, func(a, b V1Model) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out
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
	RetryOn              string
	RetryGrpcOn          string
	PerTryTimeoutMs      int
	RetriableStatusCodes []int
	RetriableHeaderNames []string
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
// Path is the opaque URL token from the config (e.g. "github") used to route
// /mcp/<path> requests; empty when no path is configured.
type ProfileRecord struct {
	Workspace          uint32
	User               uint32
	Name               uint32
	Path               string
	ToolFilterShapeKey string
	AuthShapeKey       string
}

// ── Snapshot ──────────────────────────────────────────────────────────────────

// ConfigSnapshot is the atomically-published unit of config state. GlobalConfig
// and Pools are always consistent with each other because pool entries may hold
// pointers into GlobalConfig. Keys and Profiles are nil when the snapshot
// payload did not include user records.
type ConfigSnapshot struct {
	Generation     uint64
	Global         *GlobalConfig
	Pools          *Pools
	Keys           map[string]*KeyRecord     // compiled from snapshot payload; nil if absent
	Profiles       map[string]*ProfileRecord // keyed by workspace/user/name; nil if absent
	ProfilesByPath map[string]*ProfileRecord // keyed by profile.path opaque token; nil if absent
}
