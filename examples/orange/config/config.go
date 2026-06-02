// Package config loads and exposes the single orange.yaml file.
//
// Each module (classify, translate, hostpick) calls Get() lazily and reads
// the section it cares about. The first Get() resolves the path from the
// ORANGE_CONFIG env var; missing/unreadable/invalid → panic. Loud failures
// at module init are the demo-friendly default.
package config

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/dio/transit/up"
	"gopkg.in/yaml.v3"
)

// EnvVar names the env var that points at orange.yaml.
const EnvVar = "ORANGE_CONFIG"

// Config is the full parsed shape of orange.yaml. Modules read the subset
// they need; unknown fields in YAML are ignored (yaml.v3 default).
type Config struct {
	Providers map[string]Provider `yaml:"providers"`
	Models    []ModelMatch        `yaml:"models"`
	Classify  ClassifyCfg         `yaml:"classify"`
	Translate TranslateCfg        `yaml:"translate"`
	Hostpick  HostpickCfg         `yaml:"hostpick"`

	// resolvedSecrets maps provider name → resolved auth secret. Filled at
	// Load time so missing env vars / unreadable files fail Envoy boot.
	resolvedSecrets map[string]string
}

type Provider struct {
	Kind             string  `yaml:"kind"`
	Endpoint         string  `yaml:"endpoint"`
	AnthropicVersion string  `yaml:"anthropic_version,omitempty"`
	PathPrefix       *string `yaml:"path_prefix,omitempty"`
	Auth             Auth    `yaml:"auth"`
}

// ResolvedPathPrefix returns the configured path prefix, defaulting to "/v1".
func (p Provider) ResolvedPathPrefix() string {
	if p.PathPrefix == nil {
		return "/v1"
	}
	return *p.PathPrefix
}

type Auth struct {
	Type   string `yaml:"type"`
	Secret string `yaml:"secret"` // e.g. env://OPENAI_API_KEY
}

type ModelMatch struct {
	Match    string `yaml:"match"`
	Provider string `yaml:"provider"`
}

type ClassifyCfg struct {
	ModelField string `yaml:"model_field"`
	OnMiss     OnMiss `yaml:"on_miss"`
}

type OnMiss struct {
	Status int    `yaml:"status"`
	Code   string `yaml:"code"`
}

type TranslateCfg struct {
	StripRequestHeaders []string `yaml:"strip_request_headers"`
}

type HostpickCfg struct {
	UpstreamKey string `yaml:"upstream_key"`
}

// decodeConfig is the ConfigDecoder for *Config. It applies defaults and resolves secrets.
func decodeConfig(data []byte) (*Config, error) {
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("orange/config: parse: %w", err)
	}
	applyDefaults(cfg)
	if err := resolveSecrets(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// pc is the package-level PipelineConfig.
// It is initialised (lazily) with the path from ORANGE_CONFIG.
var pc *up.PipelineConfig[*Config]

// init sets up the package-level PipelineConfig using ORANGE_CONFIG.
// The snapshot is not loaded until the first Get() call.
func init() {
	path := resolvedPath()
	if path != "" {
		pc = up.NewFileConfig[*Config](path, decodeConfig, up.PollOptions{})
	}
}

// resolvedPath returns the absolute path from ORANGE_CONFIG, or "".
func resolvedPath() string {
	path := os.Getenv(EnvVar)
	if strings.TrimSpace(path) == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	return path
}

// Get returns the parsed config, loading it once on the first call.
// Subsequent calls return the cached last-good snapshot.
// Panics if ORANGE_CONFIG is unset, unreadable, or invalid.
func Get() *Config {
	if pc == nil {
		panic(fmt.Sprintf("orange/config: %s is required", EnvVar))
	}
	// Load on first call if no snapshot yet.
	if cfg := pc.Snapshot(); cfg != nil {
		return cfg
	}
	if err := pc.RefreshOnce(context.Background()); err != nil {
		panic(err)
	}
	return pc.Snapshot()
}

// MustReload clears the cached snapshot and re-initialises the PipelineConfig.
// Intended for tests only.
func MustReload() {
	path := resolvedPath()
	if path != "" {
		pc = up.NewFileConfig[*Config](path, decodeConfig, up.PollOptions{})
	} else {
		pc = nil
	}
}

// LoadFile parses orange.yaml at the given path. Exposed for tests.
func LoadFile(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("orange/config: read %s: %w", path, err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("orange/config: parse %s: %w", path, err)
	}
	applyDefaults(cfg)
	if err := resolveSecrets(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyDefaults fills in zero-value fields with their defaults.
func applyDefaults(cfg *Config) {
	if cfg.Classify.ModelField == "" {
		cfg.Classify.ModelField = "model"
	}
	if cfg.Classify.OnMiss.Status == 0 {
		cfg.Classify.OnMiss.Status = 404
	}
	if cfg.Classify.OnMiss.Code == "" {
		cfg.Classify.OnMiss.Code = "orange.model_not_found"
	}
	if cfg.Hostpick.UpstreamKey == "" {
		cfg.Hostpick.UpstreamKey = "orange.upstream"
	}
}

// resolveSecrets populates cfg.resolvedSecrets from provider Auth.Secret refs.
func resolveSecrets(cfg *Config) error {
	cfg.resolvedSecrets = make(map[string]string, len(cfg.Providers))
	for name, p := range cfg.Providers {
		if p.Auth.Secret == "" {
			continue
		}
		v, err := resolveSecret(p.Auth.Secret)
		if err != nil {
			return fmt.Errorf("orange/config: provider %q: %w", name, err)
		}
		cfg.resolvedSecrets[name] = v
	}
	return nil
}

// resolveSecret expands secret references. Currently only `env://NAME` is
// supported; missing env vars are an error so Envoy fails to boot loudly.
func resolveSecret(ref string) (string, error) {
	const envPrefix = "env://"
	if !strings.HasPrefix(ref, envPrefix) {
		return "", fmt.Errorf("unsupported secret_ref scheme %q (only env:// is supported)", ref)
	}
	name := ref[len(envPrefix):]
	if name == "" {
		return "", fmt.Errorf("env:// reference is missing var name")
	}
	v, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("env var %s is not set", name)
	}
	return v, nil
}

// ProviderSecret returns the resolved auth secret for the named provider.
// Empty if the provider is unknown or has no auth.
func (c *Config) ProviderSecret(name string) string {
	return c.resolvedSecrets[name]
}

// LookupModel returns the first provider whose match glob matches model.
// Empty string means no match.
func (c *Config) LookupModel(model string) string {
	for _, m := range c.Models {
		if ok, _ := filepath.Match(m.Match, model); ok {
			return m.Provider
		}
	}
	return ""
}

// Host returns the hostname portion of a provider endpoint URL, e.g.
// "api.openai.com" for "https://api.openai.com". Empty if unparseable.
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
