package config

import "github.com/shopspring/decimal"

// RawConfig is the top-level serde struct for the orange config format.
// It mirrors the five logical sections exactly and is produced by all
// decode paths (YAML, JSON, proto expansion) before compile() is called.
// No validation or cross-reference resolution happens at this layer.
type RawConfig struct {
	LLM        RawLLM                        `yaml:"llm"                  json:"llm"`
	MCP        RawMCP                        `yaml:"mcp"                  json:"mcp"`
	Profiles   map[string]RawProfile         `yaml:"profiles,omitempty"   json:"profiles,omitempty"`
	Keys       map[string]RawKey             `yaml:"keys,omitempty"       json:"keys,omitempty"`
	RateLimits map[string][]RawRateLimitRule `yaml:"rate_limits,omitempty" json:"rate_limits,omitempty"`
}

// RawLLM holds the admin-owned LLM subsystem: providers and the model catalog.
type RawLLM struct {
	Providers map[string]RawProvider `yaml:"providers" json:"providers"`
	Models    map[string]RawModel    `yaml:"models"    json:"models"`
}

// RawMCP holds the admin-owned MCP subsystem: server definitions.
type RawMCP struct {
	Servers map[string]RawServer `yaml:"servers" json:"servers"`
}

// RawProvider is the serde form of one upstream LLM provider.
type RawProvider struct {
	Kind          string            `yaml:"kind"                    json:"kind"`
	BackendSchema string            `yaml:"backend_schema,omitempty" json:"backend_schema,omitempty"`
	Endpoint      string            `yaml:"endpoint"                json:"endpoint"`
	Auth          RawAuth           `yaml:"auth"                    json:"auth"`
	Extra         map[string]string `yaml:"extra,omitempty"         json:"extra,omitempty"`
}

// RawModel is the serde form of one client-facing model catalog entry.
// Provider is required; Name defaults to the map key when absent.
type RawModel struct {
	Provider          string            `yaml:"provider"                    json:"provider"`
	Name              string            `yaml:"name,omitempty"              json:"name,omitempty"`
	EndpointOverrides map[string]string `yaml:"endpoint_overrides,omitempty" json:"endpoint_overrides,omitempty"`
	Pricing           *RawModelPricing  `yaml:"pricing,omitempty"           json:"pricing,omitempty"`
	Metadata          *RawMetadata      `yaml:"metadata,omitempty"          json:"metadata,omitempty"`
}

// RawModelPricing holds per-model token prices in USD per million tokens.
// decimal.Decimal is used for exact monetary arithmetic; YAML underscore
// separators (e.g. 1_000.00) are accepted by the yaml.v3 decoder.
type RawModelPricing struct {
	InputMTok      decimal.Decimal `yaml:"input_mtok"               json:"input_mtok"`
	OutputMTok     decimal.Decimal `yaml:"output_mtok"              json:"output_mtok"`
	CacheReadMTok  decimal.Decimal `yaml:"cache_read_mtok,omitempty"  json:"cache_read_mtok,omitempty"`
	CacheWriteMTok decimal.Decimal `yaml:"cache_write_mtok,omitempty" json:"cache_write_mtok,omitempty"`
}

// RawMetadata carries informational model attributes surfaced in GET /v1/models.
type RawMetadata struct {
	Description   string   `yaml:"description,omitempty"    json:"description,omitempty"`
	ContextLength int      `yaml:"context_length,omitempty" json:"context_length,omitempty"`
	MaxTokens     int      `yaml:"max_tokens,omitempty"     json:"max_tokens,omitempty"`
	Tags          []string `yaml:"tags,omitempty"           json:"tags,omitempty"`
}

// RawServer is the serde form of one MCP server definition.
// ToolsInclude is the server-level allowlist; profile includes must be a subset.
type RawServer struct {
	Endpoint     string   `yaml:"endpoint"               json:"endpoint"`
	Namespace    string   `yaml:"namespace"              json:"namespace"`
	Auth         *RawAuth `yaml:"auth,omitempty"         json:"auth,omitempty"`
	ToolsInclude []string `yaml:"tools_include,omitempty" json:"tools_include,omitempty"`
}

// RawAuth is the serde form of an auth configuration block.
// SecretRef uses env://, file://, or literal:// schemes.
type RawAuth struct {
	Type      string `yaml:"type"       json:"type"`
	SecretRef string `yaml:"secret_ref" json:"secret_ref"`
}

// RawProfile is the serde form of a user-owned MCP profile.
// The map key in Profiles is the workspace/user/name identity.
type RawProfile struct {
	Tools map[string]RawToolFilter `yaml:"tools"           json:"tools"`
	Auth  map[string]RawAuth       `yaml:"auth,omitempty"  json:"auth,omitempty"`
}

// RawToolFilter is the per-server capability slice within a profile.
// An empty Include means: expose all tools the server allows.
type RawToolFilter struct {
	Include  []string `yaml:"include,omitempty"  json:"include,omitempty"`
	Optional bool     `yaml:"optional,omitempty" json:"optional,omitempty"`
}

// RawKey is the serde form of a user-owned API key.
// RoutingOverrides maps client-facing model IDs to routing tree roots.
type RawKey struct {
	RoutingOverrides map[string]RawRoutingNode `yaml:"routing_overrides,omitempty" json:"routing_overrides,omitempty"`
}

// RawRoutingNode is one node in a routing tree.
// Exactly one of Target, Chain, or Split must be set; compile() enforces this.
type RawRoutingNode struct {
	Target *RawRoutingTarget `yaml:"target,omitempty" json:"target,omitempty"`
	Chain  *RawChain         `yaml:"chain,omitempty"  json:"chain,omitempty"`
	Split  *RawSplit         `yaml:"split,omitempty"  json:"split,omitempty"`
}

// RawRoutingTarget is a leaf routing node naming one provider and optional model.
type RawRoutingTarget struct {
	Provider string `yaml:"provider"       json:"provider"`
	Name     string `yaml:"name,omitempty" json:"name,omitempty"`
}

// RawChain is an ordered fallback: children are tried in sequence, stopping at
// the first success. Retry carries Envoy retry semantics applied per attempt.
type RawChain struct {
	Retry    *RawRetry        `yaml:"retry,omitempty" json:"retry,omitempty"`
	Children []RawRoutingNode `yaml:"children"        json:"children"`
}

// RawRetry configures Envoy-compatible retry behaviour for a chain node.
type RawRetry struct {
	RetryOn         string `yaml:"retry_on,omitempty"          json:"retry_on,omitempty"`
	PerTryTimeoutMs int    `yaml:"per_try_timeout_ms,omitempty" json:"per_try_timeout_ms,omitempty"`
}

// RawSplit distributes traffic across children by weight; weights must sum to 100.
type RawSplit struct {
	Children []RawSplitChild `yaml:"children" json:"children"`
}

// RawSplitChild is one weighted arm of a split node. The routing node is
// inlined so YAML keys like target/chain appear alongside weight at the same
// level, matching the config file format.
type RawSplitChild struct {
	Weight         int `yaml:"weight" json:"weight"`
	RawRoutingNode `yaml:",inline" json:",inline"`
}

// RawRateLimitRule is one entry in a rate_limits scope list.
// Zero values are unconstrained for that dimension. Multiple limits on the
// same rule are independent — whichever is exhausted first triggers OnExceed.
// USD limits require a pricing block on every targeted model.
type RawRateLimitRule struct {
	Models []string `yaml:"models" json:"models"`

	// USD spend limits use decimal.Decimal for exact monetary arithmetic.
	USDPerMinute decimal.Decimal `yaml:"usd_per_minute,omitempty" json:"usd_per_minute,omitempty"`
	USDPerHour   decimal.Decimal `yaml:"usd_per_hour,omitempty"   json:"usd_per_hour,omitempty"`
	USDPerDay    decimal.Decimal `yaml:"usd_per_day,omitempty"    json:"usd_per_day,omitempty"`

	RPM int `yaml:"rpm,omitempty" json:"rpm,omitempty"`
	RPH int `yaml:"rph,omitempty" json:"rph,omitempty"`
	RPD int `yaml:"rpd,omitempty" json:"rpd,omitempty"`

	InputTokensPerMinute int `yaml:"input_tokens_per_minute,omitempty"  json:"input_tokens_per_minute,omitempty"`
	InputTokensPerHour   int `yaml:"input_tokens_per_hour,omitempty"    json:"input_tokens_per_hour,omitempty"`
	InputTokensPerDay    int `yaml:"input_tokens_per_day,omitempty"     json:"input_tokens_per_day,omitempty"`

	OutputTokensPerMinute int `yaml:"output_tokens_per_minute,omitempty" json:"output_tokens_per_minute,omitempty"`
	OutputTokensPerHour   int `yaml:"output_tokens_per_hour,omitempty"   json:"output_tokens_per_hour,omitempty"`
	OutputTokensPerDay    int `yaml:"output_tokens_per_day,omitempty"    json:"output_tokens_per_day,omitempty"`

	CacheReadTokensPerHour int `yaml:"cache_read_tokens_per_hour,omitempty"  json:"cache_read_tokens_per_hour,omitempty"`
	CacheReadTokensPerDay  int `yaml:"cache_read_tokens_per_day,omitempty"   json:"cache_read_tokens_per_day,omitempty"`

	CacheWriteTokensPerHour int `yaml:"cache_write_tokens_per_hour,omitempty" json:"cache_write_tokens_per_hour,omitempty"`
	CacheWriteTokensPerDay  int `yaml:"cache_write_tokens_per_day,omitempty"  json:"cache_write_tokens_per_day,omitempty"`

	// OnExceed controls the action when a limit is exhausted.
	// Valid values: "reject" (default), "throttle", "log_only".
	OnExceed string `yaml:"on_exceed,omitempty" json:"on_exceed,omitempty"`
}
