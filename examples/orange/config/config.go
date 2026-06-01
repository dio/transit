// Package config loads and exposes the single orange.yaml file.
//
// Each module (classify, credinject, hostpick) calls Get() lazily and reads
// the section it cares about. The first Get() resolves the path from the
// ORANGE_CONFIG env var; missing/unreadable/invalid → panic. Loud failures
// at module init are the demo-friendly default.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// EnvVar names the env var that points at orange.yaml.
const EnvVar = "ORANGE_CONFIG"

// Config is the full parsed shape of orange.yaml. Modules read the subset
// they need; unknown fields in YAML are ignored (yaml.v3 default).
type Config struct {
	Upstreams  map[string]Upstream `yaml:"upstreams"`
	Models     []ModelMatch        `yaml:"models"`
	Classify   ClassifyCfg         `yaml:"classify"`
	Credinject CredinjectCfg       `yaml:"credinject"`
	Hostpick   HostpickCfg         `yaml:"hostpick"`

	// resolvedSecrets maps upstream name → resolved auth secret. Filled at
	// Load time so missing env vars / unreadable files fail Envoy boot.
	resolvedSecrets map[string]string
}

type Upstream struct {
	Kind             string `yaml:"kind"`
	Endpoint         string `yaml:"endpoint"`
	AnthropicVersion string `yaml:"anthropic_version,omitempty"`
	Auth             Auth   `yaml:"auth"`
}

type Auth struct {
	Type   string `yaml:"type"`
	Secret string `yaml:"secret"` // e.g. env://OPENAI_API_KEY
}

type ModelMatch struct {
	Match    string `yaml:"match"`
	Upstream string `yaml:"upstream"`
}

type ClassifyCfg struct {
	ModelField string `yaml:"model_field"`
	OnMiss     OnMiss `yaml:"on_miss"`
}

type OnMiss struct {
	Status int    `yaml:"status"`
	Code   string `yaml:"code"`
}

type CredinjectCfg struct {
	StripRequestHeaders []string `yaml:"strip_request_headers"`
}

type HostpickCfg struct {
	UpstreamKey string `yaml:"upstream_key"`
}

var (
	once   sync.Once
	loaded *Config
	loaErr error
)

// Get returns the parsed config, loading it once from ORANGE_CONFIG.
// Subsequent calls return the cached value.
func Get() *Config {
	once.Do(func() { loaded, loaErr = load() })
	if loaErr != nil {
		panic(loaErr)
	}
	return loaded
}

// MustReload clears the cache. Intended for tests only.
func MustReload() {
	once = sync.Once{}
	loaded = nil
	loaErr = nil
}

func load() (*Config, error) {
	path := os.Getenv(EnvVar)
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("orange/config: %s is required", EnvVar)
	}
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	return LoadFile(path)
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
	cfg.resolvedSecrets = make(map[string]string, len(cfg.Upstreams))
	for name, ups := range cfg.Upstreams {
		if ups.Auth.Secret == "" {
			continue
		}
		v, err := resolveSecret(ups.Auth.Secret)
		if err != nil {
			return nil, fmt.Errorf("orange/config: upstream %q: %w", name, err)
		}
		cfg.resolvedSecrets[name] = v
	}
	return cfg, nil
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

// UpstreamSecret returns the resolved auth secret for the named upstream.
// Empty if the upstream is unknown or has no auth.
func (c *Config) UpstreamSecret(name string) string {
	return c.resolvedSecrets[name]
}

// LookupModel returns the first upstream whose match glob matches model.
// Empty string means no match.
func (c *Config) LookupModel(model string) string {
	for _, m := range c.Models {
		if ok, _ := filepath.Match(m.Match, model); ok {
			return m.Upstream
		}
	}
	return ""
}

// Host returns the hostname portion of an upstream endpoint URL, e.g.
// "api.openai.com" for "https://api.openai.com". Empty if unparseable.
func (u Upstream) Host() string {
	if u.Endpoint == "" {
		return ""
	}
	p, err := url.Parse(u.Endpoint)
	if err != nil {
		return ""
	}
	return p.Hostname()
}
