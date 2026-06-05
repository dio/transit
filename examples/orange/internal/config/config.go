// Package config loads and validates the orange.yaml configuration file.
//
// The file shape is governed by the embedded JSON Schema (config.schema.json),
// which is applied at load time before the typed unmarshal. Structural errors
// (missing required fields, unknown auth type, wrong field types) are caught by
// the schema. Semantic errors (model → provider cross-references, secret
// resolution) are checked immediately after.
//
// Entry points:
//
//   - [Load] decodes config from raw bytes.
//   - [Decoder] returns an [up.ConfigDecoder] that wraps [Load].
//   - [NewSource] builds a [up.ConfigSource] from a DSN (file path or HTTP URL).
//   - [Get] returns the current snapshot for normal server operation.
//   - [Start] begins background config polling; wire into cluster lifetime.
package config

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"gopkg.in/yaml.v3"

	"github.com/dio/transit/examples/orange/internal/observability"
	"github.com/dio/transit/up"
)

// EnvVar names the env var that points at the orange config file.
const EnvVar = "ORANGE_CONFIG"

//go:embed config.schema.json
var rawSchema []byte

// compiledSchema is compiled once at package init from the embedded JSON Schema.
// A panic here means the embedded schema JSON is malformed — a build-time defect.
var compiledSchema = func() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	if err := c.AddResource("config.schema.json", bytes.NewReader(rawSchema)); err != nil {
		panic(fmt.Sprintf("BUG: compile embedded schema: %v", err))
	}
	sch, err := c.Compile("config.schema.json")
	if err != nil {
		panic(fmt.Sprintf("BUG: compile embedded schema: %v", err))
	}
	return sch
}()

// Config is the fully validated, secret-resolved configuration.
type Config struct {
	LLM  LLMConfig           `yaml:"llm"`
	MCP  *MCPConfig          `yaml:"mcp,omitempty"`
	Keys map[string]*KeyBlob `yaml:"keys,omitempty"`

	Providers map[string]Provider   `yaml:"-"`
	Models    map[string]ModelEntry `yaml:"-"`

	resolvedSecrets        map[string]string
	resolvedMCPCredentials map[string]string
}

// LLMConfig describes provider and model routing for LLM APIs.
type LLMConfig struct {
	Providers map[string]Provider   `yaml:"providers"`
	Models    map[string]ModelEntry `yaml:"models"`
}

// Provider describes a single upstream LLM provider.
type Provider struct {
	Kind          string            `yaml:"kind"`
	BackendSchema string            `yaml:"backend_schema,omitempty"`
	Endpoint      string            `yaml:"endpoint,omitempty"` // implicit "default" binding (back-compat)
	PathPrefix    *string           `yaml:"path_prefix,omitempty"`
	Extra         map[string]string `yaml:"extra,omitempty"`
	Auth          Auth              `yaml:"auth"`
	Bindings      []Binding         `yaml:"bindings,omitempty"` // named endpoint variants within this provider
}

// Binding describes a named endpoint variant within a Provider.
// Bindings share the parent's auth configuration but address different
// upstream endpoints (e.g. regional replicas of the same vendor's API).
type Binding struct {
	Name     string `yaml:"name"`     // unique within the provider
	Endpoint string `yaml:"endpoint"` // overrides Provider.Endpoint for this binding
}

// Auth describes how the gateway authenticates to a provider.
type Auth struct {
	Type      string `yaml:"type"`
	SecretRef string `yaml:"secret_ref,omitempty"`
}

// ModelEntry maps a client-facing model ID to a provider and optional backend name.
type ModelEntry struct {
	Provider  string            `yaml:"provider,omitempty"`
	Binding   string            `yaml:"binding,omitempty"` // optional named binding within the provider
	Name      string            `yaml:"name,omitempty"`
	Metadata  map[string]any    `yaml:"metadata,omitempty"`
	Endpoints map[string]string `yaml:"endpoints,omitempty"`
	Routing   *RoutingNode      `yaml:"routing,omitempty"`
}

// RoutingNode is a single node in a routing tree.
// Exactly one of Chain, Target, or Split must be set.
type RoutingNode struct {
	Chain  *ChainNode  `yaml:"chain,omitempty"`
	Target *TargetLeaf `yaml:"target,omitempty"`
	Split  *SplitNode  `yaml:"split,omitempty"`
}

// SplitNode is a routing node that samples one child by weight on
// every request. Weights must be positive integers summing to 100.
// Decision.Model is always the Models map key (the client-facing alias;
// it can be any string and need not match a real provider model ID).
type SplitNode struct {
	Children []SplitChild `yaml:"children"`
}

// SplitChild is one weighted arm of a SplitNode.
type SplitChild struct {
	Weight      int `yaml:"weight"`
	RoutingNode `yaml:",inline"`
}

// ChainRetryPolicy configures how Envoy retries for a fallback chain.
// These values are injected as per-request x-envoy-* headers in match's
// headers phase so that Envoy's RetryStateImpl picks them up.
//
//   - RetryOn maps to x-envoy-retry-on (additive OR onto the route's retry_on).
//   - PerTryTimeoutMs maps to x-envoy-upstream-rq-per-try-timeout-ms.
//   - MaxRetries is auto-derived from len(children)-1; do not set manually.
type ChainRetryPolicy struct {
	RetryOn         string `yaml:"retry_on,omitempty"`
	PerTryTimeoutMs int    `yaml:"per_try_timeout_ms,omitempty"`
}

// ChainNode is a routing node that tries children in order,
// using retry-count to select the active child.
type ChainNode struct {
	Retry    *ChainRetryPolicy `yaml:"retry,omitempty"`
	Children []RoutingNode     `yaml:"children"`
}

// TargetLeaf is a leaf node that names a concrete provider and optional model.
type TargetLeaf struct {
	Provider string `yaml:"provider"`
	Name     string `yaml:"name,omitempty"`
}

// KeyBlob is the materialized per-key policy blob loaded from keys[].
// The map key in keys[] must start with workspace/user/.
type KeyBlob struct {
	Workspace string `yaml:"workspace"`
	User      string `yaml:"user"`
	LLM       KeyLLM `yaml:"llm"`
}

// KeyLLM holds the per-key LLM model routing table.
// Models has the same shape as the top-level llm.models.
type KeyLLM struct {
	Models map[string]ModelEntry `yaml:"models"`
}

// MCPConfig describes Orange-managed MCP server profiles.
type MCPConfig struct {
	Profiles map[string]MCPProfile `yaml:"profiles"`
	Servers  map[string]MCPServer  `yaml:"servers"`
}

// MCPProfile describes a named group of MCP servers.
type MCPProfile struct {
	Tools map[string]MCPProfileTools `yaml:"tools"`          // per-server tool scoping; keys define the server set
	Auth  map[string]Auth            `yaml:"auth,omitempty"` // per-server auth overrides; key is server name
}

// ServerNames returns the sorted list of server names for this profile,
// derived from the Tools map keys.
func (p MCPProfile) ServerNames() []string {
	names := make([]string, 0, len(p.Tools))
	for name := range p.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// MCPProfileTools is the profile-level capability slice for one server.
// Include and Exclude are mutually exclusive: use one or the other, not both.
// Exclude wins when both are set (consistent with MCPToolSelector semantics).
// Optional marks the backend as best-effort: initialize succeeds even if this
// backend fails, and the session simply won't include its tools.
type MCPProfileTools struct {
	Include  []string `yaml:"include,omitempty"`
	Exclude  []string `yaml:"exclude,omitempty"`
	Optional bool     `yaml:"optional,omitempty"`
}

// MCPServer describes one MCP backend reached via its endpoint.
type MCPServer struct {
	Endpoint    string          `yaml:"endpoint"`
	Namespace   string          `yaml:"namespace,omitempty"`
	Transport   string          `yaml:"transport,omitempty"`
	Timeout     string          `yaml:"timeout,omitempty"`
	HealthCheck *MCPHealthCheck `yaml:"health_check,omitempty"`
	Retry       *MCPRetry       `yaml:"retry,omitempty"`
	Auth        *Auth           `yaml:"auth,omitempty"`
	Tools       MCPToolSelector `yaml:"tools,omitempty"`
}

// MCPHealthCheck configures periodic health probing for an MCP server.
type MCPHealthCheck struct {
	Interval string `yaml:"interval,omitempty"`
	Path     string `yaml:"path,omitempty"`
}

// MCPRetry configures retry behaviour for failed upstream requests.
type MCPRetry struct {
	MaxAttempts int    `yaml:"max_attempts,omitempty"`
	Backoff     string `yaml:"backoff,omitempty"`
	BaseDelay   string `yaml:"base_delay,omitempty"`
}

// Host returns the hostname of the MCP server endpoint (e.g. "mcp.kiwi.com").
func (s MCPServer) Host() string {
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
func (s MCPServer) Path() string {
	if s.Endpoint == "" {
		return "/"
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Path == "" || u.Path == "/" {
		return "/"
	}
	return u.Path
}

// MCPToolSelector filters backend-visible tools. Later protocol code compiles
// these into deny-wins selectors.
type MCPToolSelector struct {
	Include      []string `yaml:"include,omitempty"`
	IncludeRegex []string `yaml:"include_regex,omitempty"`
	Exclude      []string `yaml:"exclude,omitempty"`
	ExcludeRegex []string `yaml:"exclude_regex,omitempty"`
}

// EffectiveBackendSchema returns BackendSchema if set, otherwise Kind.
func (p Provider) EffectiveBackendSchema() string {
	if p.BackendSchema != "" {
		return p.BackendSchema
	}
	return p.Kind
}

// ResolvedPathPrefix returns the path prefix, defaulting to "/v1".
func (p Provider) ResolvedPathPrefix() string {
	if p.PathPrefix == nil {
		return "/v1"
	}
	return *p.PathPrefix
}

// Host returns the hostname of the provider endpoint (e.g. "api.openai.com").
func (p Provider) Host() string {
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
// "default" binding from Provider.Endpoint to maintain back-compat.
func (p Provider) AllBindings() []Binding {
	if len(p.Bindings) > 0 {
		return p.Bindings
	}
	return []Binding{{Name: "default", Endpoint: p.Endpoint}}
}

// BindingEndpoint returns the endpoint URL for the named binding.
// Falls back to Provider.Endpoint when binding is empty, "default", or not found.
func (p Provider) BindingEndpoint(binding string) string {
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
// Falls back to the provider's top-level Host() when binding is empty,
// "default", or not found in Bindings.
func (p Provider) BindingHost(binding string) string {
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

// LookupModel returns the provider name, effective backend model name, and
// binding for the given client model ID and endpoint discriminator. All return
// values are empty when there is no match. When ModelEntry.Name is unset the
// map key is used as the backend name. If endpoint is non-empty and the model
// entry has a matching key in Endpoints, that provider overrides the default
// provider (binding stays on the original entry).
func (c *Config) LookupModel(model, endpoint string) (provider, backendModel, binding string) {
	e, ok := c.Models[model]
	if !ok {
		return "", "", ""
	}
	prov := e.Provider
	if endpoint != "" {
		if override, has := e.Endpoints[endpoint]; has {
			prov = override
		}
	}
	bm := model
	if e.Name != "" {
		bm = e.Name
	}
	return prov, bm, e.Binding
}

// LookupModelProvider returns the upstream name, Provider, and binding for the
// given client model ID and endpoint discriminator. ok is false when the model
// is not configured.
func (c *Config) LookupModelProvider(model, endpoint string) (upstream string, provider Provider, binding string, ok bool) {
	upstream, _, binding = c.LookupModel(model, endpoint)
	if upstream == "" {
		return "", Provider{}, "", false
	}
	return upstream, c.Providers[upstream], binding, true
}

// HasKeys reports whether any per-key blobs are configured.
// When false, the gateway uses the legacy global model routing path.
func (c *Config) HasKeys() bool {
	return len(c.Keys) > 0
}

// LookupKey returns the KeyBlob for keyID. One map read, no cascade.
func (c *Config) LookupKey(keyID string) (*KeyBlob, bool) {
	kb, ok := c.Keys[keyID]
	return kb, ok
}

// LookupModelForKey resolves model routing from a per-key blob.
// All strings are empty and ok is false when the model has no entry in the blob.
func (c *Config) LookupModelForKey(keyBlob *KeyBlob, model, endpoint string) (provider, backendModel, binding string, ok bool) {
	e, found := keyBlob.LLM.Models[model]
	if !found {
		return "", "", "", false
	}
	prov := e.Provider
	if endpoint != "" {
		if override, has := e.Endpoints[endpoint]; has {
			prov = override
		}
	}
	bm := model
	if e.Name != "" {
		bm = e.Name
	}
	return prov, bm, e.Binding, true
}

// LookupModelProviderForKey returns the upstream name, Provider, and binding
// for a per-key model lookup. ok is false when the model is absent from the blob.
func (c *Config) LookupModelProviderForKey(keyBlob *KeyBlob, model, endpoint string) (upstream string, provider Provider, binding string, ok bool) {
	upstream, _, binding, found := c.LookupModelForKey(keyBlob, model, endpoint)
	if !found || upstream == "" {
		return "", Provider{}, "", false
	}
	return upstream, c.Providers[upstream], binding, true
}

// MaxChainRetries returns the maximum number of retries needed to exhaust
// the deepest fallback chain configured across all models and key blobs.
// It equals max(len(chain.children)-1) over all chains; 0 means no chains.
// Used by match's headers phase to inject x-envoy-max-retries so that
// Envoy's RetryStateImpl allows enough attempts before chain traversal stalls.
func (c *Config) MaxChainRetries() int {
	max := 0
	walkModelChains(c.Models, func(cn *ChainNode) {
		if n := len(cn.Children) - 1; n > max {
			max = n
		}
	})
	for _, kb := range c.Keys {
		walkModelChains(kb.LLM.Models, func(cn *ChainNode) {
			if n := len(cn.Children) - 1; n > max {
				max = n
			}
		})
	}
	return max
}

// ChainRetryPolicy returns the union of retry_on conditions and the maximum
// per_try_timeout_ms across all configured chains. The result is used by
// match's headers phase to inject x-envoy-retry-on and
// x-envoy-upstream-rq-per-try-timeout-ms before RetryStateImpl is created.
// Returns ("", 0) when no chain carries a retry config.
func (c *Config) ChainRetryPolicy() (retryOn string, perTryTimeoutMs int) {
	seen := map[string]bool{}
	var conditions []string
	walk := func(cn *ChainNode) {
		if cn.Retry == nil {
			return
		}
		if cn.Retry.PerTryTimeoutMs > perTryTimeoutMs {
			perTryTimeoutMs = cn.Retry.PerTryTimeoutMs
		}
		for _, cond := range strings.Split(cn.Retry.RetryOn, ",") {
			cond = strings.TrimSpace(cond)
			if cond != "" && !seen[cond] {
				seen[cond] = true
				conditions = append(conditions, cond)
			}
		}
	}
	walkModelChains(c.Models, walk)
	for _, kb := range c.Keys {
		walkModelChains(kb.LLM.Models, walk)
	}
	sort.Strings(conditions)
	retryOn = strings.Join(conditions, ",")
	return
}

// walkModelChains calls fn for every ChainNode reachable from models.
func walkModelChains(models map[string]ModelEntry, fn func(*ChainNode)) {
	for _, e := range models {
		if e.Routing != nil {
			walkRoutingNode(*e.Routing, fn)
		}
	}
}

// walkRoutingNode recursively visits all ChainNodes in a routing tree.
func walkRoutingNode(node RoutingNode, fn func(*ChainNode)) {
	if node.Chain != nil {
		fn(node.Chain)
		for _, child := range node.Chain.Children {
			walkRoutingNode(child, fn)
		}
	}
	if node.Split != nil {
		for _, child := range node.Split.Children {
			walkRoutingNode(child.RoutingNode, fn)
		}
	}
}

// ProviderSecret returns the resolved credential for the named provider.
// Empty if the provider is unknown or carries no auth secret.
func (c *Config) ProviderSecret(name string) string {
	return c.resolvedSecrets[name]
}

// MCPCredential returns the resolved credential for an MCP profile/server pair.
// Profile-level auth overrides take precedence over server-level defaults.
// Empty means neither level has a configured credential.
func (c *Config) MCPCredential(profile, server string) string {
	if profile != "" {
		if v, ok := c.resolvedMCPCredentials[profile+":"+server]; ok {
			return v
		}
	}
	return c.resolvedMCPCredentials[server]
}

// V1Model is a single entry in the OpenAI-compatible GET /v1/models response.
type V1Model struct {
	ID       string         `json:"id"`
	Object   string         `json:"object"`
	OwnedBy  string         `json:"owned_by"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// V1ModelList is the OpenAI-compatible GET /v1/models response body.
type V1ModelList struct {
	Object string    `json:"object"`
	Data   []V1Model `json:"data"`
}

// OpenAIV1Models returns the model catalogue as an OpenAI-compatible list,
// sorted alphabetically by model ID. The list is reconstructed on every call.
func (c *Config) OpenAIV1Models() V1ModelList {
	ids := make([]string, 0, len(c.Models))
	for id := range c.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	data := make([]V1Model, 0, len(ids))
	for _, id := range ids {
		e := c.Models[id]
		data = append(data, V1Model{
			ID:       id,
			Object:   "model",
			OwnedBy:  e.Provider,
			Metadata: e.Metadata,
		})
	}
	return V1ModelList{Object: "list", Data: data}
}

// Load decodes and validates config from raw YAML bytes.
// This is the primitive; use a [ConfigProvider] for file or remote sources.
func Load(data []byte) (*Config, error) {
	if err := schemaValidate(data); err != nil {
		return nil, err
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("orange/config: unmarshal: %w", err)
	}
	cfg.Providers = cfg.LLM.Providers
	cfg.Models = cfg.LLM.Models

	// Semantic: each provider must have an endpoint or at least one binding;
	// binding names must be unique within a provider.
	for name, p := range cfg.Providers {
		if len(p.Bindings) == 0 && p.Endpoint == "" {
			return nil, fmt.Errorf("orange/config: provider %q: must have endpoint or bindings", name)
		}
		seen := make(map[string]struct{}, len(p.Bindings))
		for _, b := range p.Bindings {
			if b.Name == "" {
				return nil, fmt.Errorf("orange/config: provider %q: binding must have a name", name)
			}
			if _, dup := seen[b.Name]; dup {
				return nil, fmt.Errorf("orange/config: provider %q: duplicate binding name %q", name, b.Name)
			}
			seen[b.Name] = struct{}{}
		}
	}

	// Semantic: every models[].provider must exist in providers; binding (when
	// set) must name a binding of that provider.
	for id, entry := range cfg.Models {
		if err := validateModelEntry(cfg.Providers, "models["+id+"]", entry); err != nil {
			return nil, err
		}
	}
	if cfg.MCP != nil {
		// Namespace uniqueness across all servers.
		seenNS := make(map[string]string) // namespace → first server name
		for serverName, server := range cfg.MCP.Servers {
			if server.Namespace == "" {
				continue
			}
			if prior, dup := seenNS[server.Namespace]; dup {
				return nil, fmt.Errorf("orange/config: mcp.servers: duplicate namespace %q for servers %q and %q", server.Namespace, prior, serverName)
			}
			seenNS[server.Namespace] = serverName
		}
		for profileName, profile := range cfg.MCP.Profiles {
			if len(profile.Tools) == 0 {
				return nil, fmt.Errorf("orange/config: mcp.profiles[%q].tools: must not be empty", profileName)
			}
			for serverName, pt := range profile.Tools {
				server, ok := cfg.MCP.Servers[serverName]
				if !ok {
					return nil, fmt.Errorf("orange/config: mcp.profiles[%q].tools: server %q not found in mcp.servers", profileName, serverName)
				}
				expose := server.Tools.Include
				if len(expose) == 0 {
					// Open boundary — all tools available; no subset check needed.
					continue
				}
				exposeSet := make(map[string]struct{}, len(expose))
				for _, t := range expose {
					exposeSet[t] = struct{}{}
				}
				for _, tool := range pt.Include {
					if _, allowed := exposeSet[tool]; !allowed {
						return nil, fmt.Errorf("orange/config: mcp.profiles[%q].tools[%q]: tool %q not in server expose list; available: %v", profileName, serverName, tool, expose)
					}
				}
			}
		}
	}

	// Semantic: validate keys[].
	for keyID, kb := range cfg.Keys {
		expectedPrefix := kb.Workspace + "/" + kb.User + "/"
		if !strings.HasPrefix(keyID, expectedPrefix) {
			return nil, fmt.Errorf("orange/config: keys[%q]: id must start with %q (workspace=%q user=%q)", keyID, expectedPrefix, kb.Workspace, kb.User)
		}
		for modelID, entry := range kb.LLM.Models {
			if err := validateModelEntry(cfg.Providers, "keys["+keyID+"].llm.models["+modelID+"]", entry); err != nil {
				return nil, err
			}
		}
	}

	if err := resolveSecrets(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validateModelEntry validates a single ModelEntry for semantic correctness.
// path is used in error messages (e.g. "models[\"claude-3\"]" or "keys[...].llm.models[...]").
func validateModelEntry(providers map[string]Provider, path string, entry ModelEntry) error {
	hasProvider := entry.Provider != ""
	hasRouting := entry.Routing != nil
	if hasProvider && hasRouting {
		return fmt.Errorf("orange/config: %s: cannot set both provider and routing", path)
	}
	if !hasProvider && !hasRouting {
		return fmt.Errorf("orange/config: %s: must set either provider or routing", path)
	}
	if hasRouting {
		return validateRoutingNode(providers, path+".routing", *entry.Routing)
	}
	// Sugar path: validate provider + binding.
	p, ok := providers[entry.Provider]
	if !ok {
		return fmt.Errorf("orange/config: %s.provider %q: not in providers", path, entry.Provider)
	}
	if entry.Binding != "" {
		validBinding := false
		for _, b := range p.AllBindings() {
			if b.Name == entry.Binding {
				validBinding = true
				break
			}
		}
		if !validBinding {
			return fmt.Errorf("orange/config: %s.binding %q: not a binding of provider %q", path, entry.Binding, entry.Provider)
		}
	}
	for epKey, epProvider := range entry.Endpoints {
		if _, ok := providers[epProvider]; !ok {
			return fmt.Errorf("orange/config: %s.endpoints[%q] provider %q: not in providers", path, epKey, epProvider)
		}
	}
	return nil
}

// validateRoutingNode recursively validates a routing tree node.
func validateRoutingNode(providers map[string]Provider, path string, node RoutingNode) error {
	return validateRoutingNodeInner(providers, path, node, false)
}

func validateRoutingNodeInner(providers map[string]Provider, path string, node RoutingNode, insideSplit bool) error {
	hasChain := node.Chain != nil
	hasTarget := node.Target != nil
	hasSplit := node.Split != nil

	count := 0
	if hasChain {
		count++
	}
	if hasTarget {
		count++
	}
	if hasSplit {
		count++
	}
	if count > 1 {
		return fmt.Errorf("orange/config: %s: must set exactly one of chain, target, or split", path)
	}
	if count == 0 {
		return fmt.Errorf("orange/config: %s: must set exactly one of chain, target, or split", path)
	}

	if hasChain {
		if len(node.Chain.Children) < 1 {
			return fmt.Errorf("orange/config: %s.chain: must have at least 1 child", path)
		}
		if len(node.Chain.Children) > 8 {
			return fmt.Errorf("orange/config: %s.chain: must have at most 8 children", path)
		}
		for i, child := range node.Chain.Children {
			if err := validateRoutingNodeInner(providers, fmt.Sprintf("%s.chain.children[%d]", path, i), child, insideSplit); err != nil {
				return err
			}
		}
		return nil
	}

	if hasSplit {
		if insideSplit {
			return fmt.Errorf("orange/config: %s: nested split (split inside split) is not supported", path)
		}
		if len(node.Split.Children) < 2 {
			return fmt.Errorf("orange/config: %s.split: must have at least 2 children", path)
		}
		if len(node.Split.Children) > 8 {
			return fmt.Errorf("orange/config: %s.split: must have at most 8 children", path)
		}
		total := 0
		for _, c := range node.Split.Children {
			total += c.Weight
		}
		if total != 100 {
			return fmt.Errorf("orange/config: %s.split: weights must sum to 100 (got %d)", path, total)
		}
		for i, child := range node.Split.Children {
			if err := validateRoutingNodeInner(providers, fmt.Sprintf("%s.split.children[%d]", path, i), child.RoutingNode, true); err != nil {
				return err
			}
		}
		return nil
	}

	// Target leaf.
	if node.Target.Provider == "" {
		return fmt.Errorf("orange/config: %s.target.provider: must not be empty", path)
	}
	if _, ok := providers[node.Target.Provider]; !ok {
		return fmt.Errorf("orange/config: %s.target.provider %q: not in providers", path, node.Target.Provider)
	}
	return nil
}

// Decoder returns a [up.ConfigDecoder] that validates and decodes orange config YAML.
// It wraps [Load] and is passed to [up.NewPollingConfig] or [up.NewFileConfig].
func Decoder() up.ConfigDecoder[*Config] {
	return func(data []byte) (*Config, error) {
		return Load(data)
	}
}

// NewSource parses dsn into a [up.ConfigSource] and [up.PollOptions].
// Supported DSN forms:
//
//	/path/to/file.yaml                                  file path (absolute or relative)
//	file:///path/to/file.yaml                           explicit file scheme
//	http://host/config.yaml                             HTTP fetch
//	https://host/config.yaml                            HTTPS fetch
//
// Query parameters configure polling and are stripped from HTTP URLs before
// they are used as request targets:
//
//	poll_interval=30s    up.PollOptions.Interval (default: up.DefaultPollInterval)
//	poll_timeout=5s      up.PollOptions.Timeout  (default: up.DefaultPollTimeout)
//	poll_jitter=2s       up.PollOptions.Jitter
func NewSource(dsn string) (up.ConfigSource, up.PollOptions, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, up.PollOptions{}, fmt.Errorf("orange/config: DSN is empty; set %s to a file path or HTTP URL", EnvVar)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, up.PollOptions{}, fmt.Errorf("orange/config: parse DSN %q: %w", dsn, err)
	}
	opts, err := parsePollOptions(u.Query())
	if err != nil {
		return nil, up.PollOptions{}, err
	}
	switch u.Scheme {
	case "https", "http":
		return httpConfigSource(cleanURL(u), opts.Timeout), opts, nil
	default:
		// file:// or bare path; u.Path has the query already stripped.
		path := u.Path
		if !filepath.IsAbs(path) {
			if abs, err := filepath.Abs(path); err == nil {
				path = abs
			}
		}
		return fileConfigSource(path), opts, nil
	}
}

// parsePollOptions extracts poll_interval / poll_timeout / poll_jitter from q.
func parsePollOptions(q url.Values) (up.PollOptions, error) {
	var opts up.PollOptions
	for _, spec := range []struct {
		key  string
		dest *time.Duration
	}{
		{"poll_interval", &opts.Interval},
		{"poll_timeout", &opts.Timeout},
		{"poll_jitter", &opts.Jitter},
	} {
		if v := q.Get(spec.key); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return up.PollOptions{}, fmt.Errorf("orange/config: %s=%q: %w", spec.key, v, err)
			}
			*spec.dest = d
		}
	}
	return opts, nil
}

// orangeDSNParams is the set of query params consumed by NewSource and stripped
// from HTTP URLs before they are used as actual request targets.
var orangeDSNParams = map[string]bool{
	"poll_interval": true,
	"poll_timeout":  true,
	"poll_jitter":   true,
}

// cleanURL returns u as a string with all orangeDSNParams removed from the query.
func cleanURL(u *url.URL) string {
	q := u.Query()
	for k := range orangeDSNParams {
		q.Del(k)
	}
	c := *u
	if len(q) == 0 {
		c.RawQuery = ""
	} else {
		c.RawQuery = q.Encode()
	}
	return c.String()
}

func fileConfigSource(path string) up.ConfigSource {
	return func(_ context.Context) ([]byte, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("orange/config: read %s: %w", path, err)
		}
		return data, nil
	}
}

// configTransport is the shared HTTP transport for all config sources.
// Initialized once; provides connection pooling across refreshes and reloads.
var configTransport = sync.OnceValue(func() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return http.DefaultTransport
})

func httpConfigSource(rawURL string, timeout time.Duration) up.ConfigSource {
	if timeout == 0 {
		timeout = up.DefaultPollTimeout
	}
	// One client per source, sharing the singleton transport.
	// Timeout is the hard ceiling for calls without a context deadline
	// (e.g. the initial RefreshOnce); pollFetch also applies PollOptions.Timeout
	// via context, so the shorter of the two always wins.
	client := &http.Client{
		Timeout:   timeout,
		Transport: configTransport(),
	}
	return func(ctx context.Context) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("orange/config: build request %s: %w", rawURL, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("orange/config: fetch %s: %w", rawURL, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("orange/config: fetch %s: HTTP %d", rawURL, resp.StatusCode)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("orange/config: read body %s: %w", rawURL, err)
		}
		return data, nil
	}
}

// --- internal decode helpers --------------------------------------------------

// schemaValidate validates raw YAML bytes against the embedded JSON Schema.
// It converts YAML → generic value → JSON → validates to ensure JSON-compatible
// types throughout (yaml.v3 integers are Go int; json.Unmarshal gives float64).
func schemaValidate(data []byte) error {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("orange/config: parse: %w", err)
	}
	// Roundtrip through JSON to normalise types for the schema validator.
	jsonBytes, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("orange/config: yaml→json: %w", err)
	}
	var doc any
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		return fmt.Errorf("orange/config: json roundtrip: %w", err)
	}
	if err := compiledSchema.Validate(doc); err != nil {
		return fmt.Errorf("orange/config: schema: %w", err)
	}
	return nil
}

// resolveSecrets resolves all auth secret_ref values and caches them.
// It also resolves any extra map values that use a secret_ref scheme
// (env://, file://, literal://) in-place, so translators and auth handlers
// can read plain strings from Provider.Extra without knowing about indirection.
func resolveSecrets(cfg *Config) error {
	cfg.resolvedSecrets = make(map[string]string, len(cfg.Providers))
	for name, p := range cfg.Providers {
		// Resolve extra values that use secret_ref schemes.
		for k, val := range p.Extra {
			if !strings.Contains(val, "://") {
				continue
			}
			resolved, err := resolveSecretRef(val)
			if err != nil {
				return fmt.Errorf("orange/config: provider %q extra %q: %w", name, k, err)
			}
			p.Extra[k] = resolved
		}

		if p.Auth.SecretRef == "" {
			continue
		}
		var v string
		var err error
		if p.Auth.Type == "gcp" && strings.HasPrefix(p.Auth.SecretRef, "file://") {
			// For GCP, pass the file path directly so NewGCPAuth can use
			// CredentialsFile without reading the file here.
			v = strings.TrimPrefix(p.Auth.SecretRef, "file://")
		} else {
			v, err = resolveSecretRef(p.Auth.SecretRef)
			if err != nil {
				return fmt.Errorf("orange/config: provider %q: %w", name, err)
			}
		}
		cfg.resolvedSecrets[name] = v
	}
	cfg.resolvedMCPCredentials = make(map[string]string)
	if cfg.MCP != nil {
		for serverName, server := range cfg.MCP.Servers {
			if server.Auth == nil || server.Auth.SecretRef == "" {
				continue
			}
			v, err := resolveSecretRef(server.Auth.SecretRef)
			if err != nil {
				return fmt.Errorf("orange/config: mcp server %q: %w", serverName, err)
			}
			cfg.resolvedMCPCredentials[serverName] = v
		}
		for profileName, profile := range cfg.MCP.Profiles {
			for serverName, auth := range profile.Auth {
				if auth.SecretRef == "" {
					continue
				}
				v, err := resolveSecretRef(auth.SecretRef)
				if err != nil {
					return fmt.Errorf("orange/config: mcp profile %q server %q: %w", profileName, serverName, err)
				}
				cfg.resolvedMCPCredentials[profileName+":"+serverName] = v
			}
		}
	}
	return nil
}

// resolveSecretRef resolves a secret_ref URI.
// Supported schemes: env://VAR_NAME, file:///absolute/path, literal://value.
func resolveSecretRef(ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "env://"):
		name := ref[len("env://"):]
		if name == "" {
			return "", fmt.Errorf("env:// secret_ref is missing the variable name")
		}
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("env var %s referenced by secret_ref is not set", name)
		}
		return v, nil
	case strings.HasPrefix(ref, "file://"):
		path := ref[len("file://"):]
		if path == "" {
			return "", fmt.Errorf("file:// secret_ref is missing the file path")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("file:// secret_ref %q: %w", path, err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	case strings.HasPrefix(ref, "literal://"):
		return ref[len("literal://"):], nil
	default:
		return "", fmt.Errorf("unsupported secret_ref scheme %q (supported: env://, file://, literal://)", ref)
	}
}

// --- runtime singleton --------------------------------------------------------

var (
	globalMu       sync.Mutex
	globalPipeline *up.PipelineConfig[*Config]
	globalLogger   *slog.Logger
)

// SetLogger sets the logger used by the config package for refresh diagnostics.
// Must be called before the first [Get] or [Start] call.
func SetLogger(l *slog.Logger) {
	globalMu.Lock()
	globalLogger = l
	globalMu.Unlock()
}

// InitLogger initializes the global logger with Envoy-style formatting.
// Call this once at startup; it configures both the config and general loggers.
func InitLogger() {
	globalMu.Lock()
	globalLogger = observability.Logger("orange/config")
	globalMu.Unlock()
}

// EnsureLogger initializes the config refresh logger if it has not already been
// set. Components that only need to guarantee config diagnostics should call
// this instead of overwriting a test- or application-supplied logger.
func EnsureLogger() {
	globalMu.Lock()
	if globalLogger == nil {
		globalLogger = observability.Logger("orange/config")
	}
	globalMu.Unlock()
}

// pipeline returns the singleton PipelineConfig, initialising it on first call.
// If the DSN is empty or invalid the pipeline is still created but its snapshot
// stays nil; Get() will then panic with a descriptive message.
func pipeline() *up.PipelineConfig[*Config] {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalLogger == nil {
		panic("BUG: orange/config: logger not set; call SetLogger before Get or Start")
	}
	if globalPipeline != nil {
		return globalPipeline
	}
	dsn := os.Getenv(EnvVar)
	src, opts, err := NewSource(dsn)
	if err != nil {
		// Broken source: Snapshot stays nil so Get() can report the problem.
		globalPipeline = up.NewPollingConfig(
			func(_ context.Context) ([]byte, error) { return nil, err },
			Decoder(),
			up.PollOptions{},
		)
		return globalPipeline
	}
	// Log refresh cycles: only log when version changes (non-empty version).
	// Empty version means checksum unchanged (file not modified); skip log noise.
	// Errors are always logged.
	opts.Observe = func(ev up.ConfigEvent) {
		if ev.Err != nil {
			globalLogger.Error("config refresh failed, keeping last-good snapshot", "err", ev.Err)
		} else if ev.Version != "" {
			globalLogger.Info("config refreshed", "version", ev.Version, "duration", ev.Duration)
		}
	}
	p := up.NewPollingConfig(src, Decoder(), opts)
	_ = p.RefreshOnce(context.Background()) // nil snapshot causes Get() to panic with a descriptive message
	globalPipeline = p
	return p
}

// Get returns the current config snapshot.
// Panics if ORANGE_CONFIG is unset or no valid config has been loaded yet.
func Get() *Config {
	v := pipeline().Snapshot()
	if v == nil {
		panic(fmt.Sprintf("orange/config: no valid config loaded; set %s to a file path or HTTP URL", EnvVar))
	}
	return v
}

// Start begins background config polling tied to ctx. The first fetch has
// already happened inside pipeline(); Start drives subsequent refreshes.
// Returns a stop func that cancels polling and waits for any in-flight fetch.
// Wire into up.Group or Cluster.ServerInitialized / OnDestroy.
func Start(ctx context.Context) func() {
	return pipeline().Start(ctx)
}

// EnableFileWatch enables file system watching for the config file to trigger
// immediate refreshes when the file is modified. The path is typically from
// the ORANGE_CONFIG environment variable.
// Returns a stop func that cancels watching and waits for the watch goroutine.
// No-op if path is empty or watching fails to set up.
func EnableFileWatch(path string) func() {
	if path == "" {
		return func() {}
	}
	return up.StartFileWatch(pipeline(), path)
}
