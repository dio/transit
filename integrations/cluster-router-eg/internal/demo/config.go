package demo

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"
)

const defaultVersion = "bootstrap"

type RouteConfig struct {
	Version string                 `json:"version"`
	Models  map[string]ModelConfig `json:"models,omitempty"`
}

type ModelConfig struct {
	Target     string `json:"target"`
	Provider   string `json:"provider"`
	AuthHeader string `json:"auth_header,omitempty"`
}

type ModelUpdate struct {
	Name       string `json:"name"`
	Target     string `json:"target"`
	Provider   string `json:"provider"`
	AuthHeader string `json:"auth_header,omitempty"`
	Version    string `json:"version,omitempty"`
}

type DumpConfig struct {
	Version string               `json:"version"`
	Models  map[string]DumpModel `json:"models"`
}

type DumpModel struct {
	Target         string `json:"target"`
	Provider       string `json:"provider"`
	AuthConfigured bool   `json:"auth_configured"`
}

type ConfigStore struct {
	mu  sync.RWMutex
	cfg RouteConfig
}

func NewConfigStore(initial RouteConfig) (*ConfigStore, error) {
	if initial.Version == "" {
		initial.Version = defaultVersion
	}
	if initial.Models == nil {
		initial.Models = make(map[string]ModelConfig)
	}
	if err := validateConfig(initial); err != nil {
		return nil, err
	}
	return &ConfigStore{cfg: cloneRouteConfig(initial)}, nil
}

func NewConfigStoreJSON(raw []byte) (*ConfigStore, error) {
	if len(raw) == 0 {
		return NewConfigStore(DefaultRouteConfig())
	}
	var cfg RouteConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse initial config: %w", err)
	}
	return NewConfigStore(cfg)
}

func DefaultRouteConfig() RouteConfig {
	return RouteConfig{
		Version: defaultVersion,
		Models: map[string]ModelConfig{
			"gpt-fast": {
				Target:     "upstream-a.default.svc.cluster.local:8080",
				Provider:   "openai",
				AuthHeader: "Bearer openai-token",
			},
			"claude-safe": {
				Target:     "upstream-b.default.svc.cluster.local:8080",
				Provider:   "anthropic",
				AuthHeader: "Bearer anthropic-token",
			},
		},
	}
}

func (s *ConfigStore) Current() RouteConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneRouteConfig(s.cfg)
}

func (s *ConfigStore) Replace(next RouteConfig) error {
	if next.Version == "" {
		next.Version = "updated"
	}
	if next.Models == nil {
		next.Models = make(map[string]ModelConfig)
	}
	if err := validateConfig(next); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cloneRouteConfig(next)
	return nil
}

func (s *ConfigStore) UpsertModel(update ModelUpdate) error {
	if update.Name == "" {
		return errors.New("model name is required")
	}
	if update.Version == "" {
		update.Version = "updated"
	}
	model := ModelConfig{
		Target:     update.Target,
		Provider:   update.Provider,
		AuthHeader: update.AuthHeader,
	}
	if err := validateModel(update.Name, model); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneRouteConfig(s.cfg)
	next.Version = update.Version
	next.Models[update.Name] = model
	s.cfg = next
	return nil
}

func (s *ConfigStore) Dump() DumpConfig {
	current := s.Current()
	out := DumpConfig{
		Version: current.Version,
		Models:  make(map[string]DumpModel, len(current.Models)),
	}
	for name, model := range current.Models {
		out.Models[name] = DumpModel{
			Target:         model.Target,
			Provider:       model.Provider,
			AuthConfigured: model.AuthHeader != "",
		}
	}
	return out
}

func validateConfig(cfg RouteConfig) error {
	if cfg.Version == "" {
		return errors.New("version is required")
	}
	for name, model := range cfg.Models {
		if err := validateModel(name, model); err != nil {
			return err
		}
	}
	return nil
}

func validateModel(name string, model ModelConfig) error {
	if name == "" {
		return errors.New("model name is required")
	}
	if model.Target == "" {
		return fmt.Errorf("model %q target is required", name)
	}
	if model.Provider == "" {
		return fmt.Errorf("model %q provider is required", name)
	}
	return nil
}

func cloneRouteConfig(in RouteConfig) RouteConfig {
	return RouteConfig{
		Version: in.Version,
		Models:  maps.Clone(in.Models),
	}
}
