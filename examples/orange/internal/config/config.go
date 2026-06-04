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
	Providers map[string]Provider   `yaml:"providers"`
	Models    map[string]ModelEntry `yaml:"models"`

	resolvedSecrets map[string]string
}

// Provider describes a single upstream LLM provider.
type Provider struct {
	Kind          string            `yaml:"kind"`
	BackendSchema string            `yaml:"backend_schema,omitempty"`
	Endpoint      string            `yaml:"endpoint"`
	PathPrefix    *string           `yaml:"path_prefix,omitempty"`
	Extra         map[string]string `yaml:"extra,omitempty"`
	Auth          Auth              `yaml:"auth"`
}

// Auth describes how the gateway authenticates to a provider.
type Auth struct {
	Type      string `yaml:"type"`
	SecretRef string `yaml:"secret_ref,omitempty"`
}

// ModelEntry maps a client-facing model ID to a provider and optional backend name.
type ModelEntry struct {
	Provider string         `yaml:"provider"`
	Name     string         `yaml:"name,omitempty"`
	Metadata map[string]any `yaml:"metadata,omitempty"`
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

// LookupModel returns the provider name and effective backend model name for
// the given client model ID. Both return values are empty when there is no
// match. When ModelEntry.Name is unset the map key is used as the backend name.
func (c *Config) LookupModel(model string) (provider, backendModel string) {
	e, ok := c.Models[model]
	if !ok {
		return "", ""
	}
	if e.Name != "" {
		return e.Provider, e.Name
	}
	return e.Provider, model
}

// LookupModelProvider returns the upstream name and Provider for the given
// client model ID. ok is false when the model is not configured.
func (c *Config) LookupModelProvider(model string) (upstream string, provider Provider, ok bool) {
	upstream, _ = c.LookupModel(model)
	if upstream == "" {
		return "", Provider{}, false
	}
	return upstream, c.Providers[upstream], true
}

// ProviderSecret returns the resolved credential for the named provider.
// Empty if the provider is unknown or carries no auth secret.
func (c *Config) ProviderSecret(name string) string {
	return c.resolvedSecrets[name]
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

	// Semantic: every models[].provider must exist in providers.
	for id, entry := range cfg.Models {
		if _, ok := cfg.Providers[entry.Provider]; !ok {
			return nil, fmt.Errorf("orange/config: models[%q].provider %q: not in providers", id, entry.Provider)
		}
	}

	if err := resolveSecrets(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
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
	return http.DefaultTransport.(*http.Transport).Clone()
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
		defer resp.Body.Close()
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
func resolveSecrets(cfg *Config) error {
	cfg.resolvedSecrets = make(map[string]string, len(cfg.Providers))
	for name, p := range cfg.Providers {
		if p.Auth.SecretRef == "" {
			continue
		}
		v, err := resolveSecretRef(p.Auth.SecretRef)
		if err != nil {
			return fmt.Errorf("orange/config: provider %q: %w", name, err)
		}
		cfg.resolvedSecrets[name] = v
	}
	return nil
}

// resolveSecretRef resolves a secret_ref URI. Only env://VAR_NAME is supported.
func resolveSecretRef(ref string) (string, error) {
	const prefix = "env://"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("unsupported secret_ref scheme %q (only env:// is supported)", ref)
	}
	name := ref[len(prefix):]
	if name == "" {
		return "", fmt.Errorf("env:// secret_ref is missing the variable name")
	}
	v, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("env var %s referenced by secret_ref is not set", name)
	}
	return v, nil
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
	p.RefreshOnce(context.Background()) //nolint:errcheck — nil snapshot causes Get() to panic
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
