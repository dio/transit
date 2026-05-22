package demo

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"
)

const defaultVersion = "bootstrap"

type L1Config struct {
	Version      string                 `json:"version"`
	DefaultShard string                 `json:"default_shard"`
	Shards       map[string]ShardConfig `json:"shards,omitempty"`
}

type ShardConfig struct {
	Target   string   `json:"target"`
	Prefixes []string `json:"prefixes,omitempty"`
	Shard    string   `json:"shard,omitempty"`
	Status   string   `json:"status,omitempty"`
}

type L2Config struct {
	Version string                 `json:"version"`
	Models  map[string]ModelConfig `json:"models,omitempty"`
}

type ModelConfig struct {
	Target     string `json:"target"`
	Provider   string `json:"provider"`
	AuthHeader string `json:"auth_header,omitempty"`
	Profile    string `json:"profile,omitempty"`
	BYOKKeyID  string `json:"byok_key_id,omitempty"`
}

type InitialConfig struct {
	L1 L1Config            `json:"l1"`
	L2 map[string]L2Config `json:"l2"`
}

type ShardUpdate struct {
	Name     string   `json:"name"`
	Target   string   `json:"target"`
	Prefixes []string `json:"prefixes,omitempty"`
	Shard    string   `json:"shard,omitempty"`
	Status   string   `json:"status,omitempty"`
	Version  string   `json:"version,omitempty"`
}

type ModelUpdate struct {
	Shard      string `json:"shard"`
	Name       string `json:"name"`
	Target     string `json:"target"`
	Provider   string `json:"provider"`
	AuthHeader string `json:"auth_header,omitempty"`
	Profile    string `json:"profile,omitempty"`
	BYOKKeyID  string `json:"byok_key_id,omitempty"`
	Version    string `json:"version,omitempty"`
}

type DumpConfig struct {
	L1 L1Config                `json:"l1"`
	L2 map[string]DumpL2Config `json:"l2"`
}

type DumpL2Config struct {
	Version string               `json:"version"`
	Models  map[string]DumpModel `json:"models"`
}

type DumpModel struct {
	Target         string `json:"target"`
	Provider       string `json:"provider"`
	Profile        string `json:"profile,omitempty"`
	BYOKKeyID      string `json:"byok_key_id,omitempty"`
	AuthConfigured bool   `json:"auth_configured"`
}

type ConfigStore struct {
	mu sync.RWMutex
	l1 L1Config
	l2 map[string]L2Config
}

func NewConfigStore(initial InitialConfig) (*ConfigStore, error) {
	l1 := initial.L1
	if l1.Version == "" {
		l1.Version = defaultVersion
	}
	if l1.DefaultShard == "" {
		l1.DefaultShard = "a"
	}
	if l1.Shards == nil {
		l1.Shards = make(map[string]ShardConfig)
	}
	if err := validateL1(l1); err != nil {
		return nil, err
	}

	l2 := maps.Clone(initial.L2)
	if l2 == nil {
		l2 = make(map[string]L2Config)
	}
	for shard, cfg := range l2 {
		if cfg.Version == "" {
			cfg.Version = defaultVersion
		}
		if cfg.Models == nil {
			cfg.Models = make(map[string]ModelConfig)
		}
		if err := validateL2(shard, cfg); err != nil {
			return nil, err
		}
		l2[shard] = cfg
	}

	return &ConfigStore{
		l1: cloneL1Config(l1),
		l2: cloneL2Configs(l2),
	}, nil
}

func NewConfigStoreJSON(raw []byte) (*ConfigStore, error) {
	if len(raw) == 0 {
		return NewConfigStore(DefaultConfig())
	}
	var cfg InitialConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse initial config: %w", err)
	}
	return NewConfigStore(cfg)
}

func DefaultConfig() InitialConfig {
	return InitialConfig{
		L1: L1Config{
			Version:      defaultVersion,
			DefaultShard: "a",
			Shards: map[string]ShardConfig{
				"a": {
					Target:   "l2-a.transit-dataplane.svc.cluster.local:80",
					Prefixes: []string{"a"},
					Shard:    "a",
					Status:   "active",
				},
				"b": {
					Target:   "l2-b.transit-dataplane.svc.cluster.local:80",
					Prefixes: []string{"b"},
					Shard:    "b",
					Status:   "active",
				},
			},
		},
		L2: map[string]L2Config{
			"a": {
				Version: defaultVersion,
				Models: map[string]ModelConfig{
					"gpt-fast": {
						Target:     "upstream-a.transit-dataplane.svc.cluster.local:8080",
						Provider:   "openai",
						AuthHeader: "Bearer shard-a-openai-token",
						Profile:    "profile-a",
						BYOKKeyID:  "key-a-001",
					},
					"claude-safe": {
						Target:     "upstream-b.transit-dataplane.svc.cluster.local:8080",
						Provider:   "anthropic",
						AuthHeader: "Bearer shard-a-anthropic-token",
						Profile:    "profile-a",
						BYOKKeyID:  "key-a-002",
					},
				},
			},
			"b": {
				Version: defaultVersion,
				Models: map[string]ModelConfig{
					"gpt-fast": {
						Target:     "upstream-c.transit-dataplane.svc.cluster.local:8080",
						Provider:   "openai",
						AuthHeader: "Bearer shard-b-openai-token",
						Profile:    "profile-b",
						BYOKKeyID:  "key-b-001",
					},
					"kimi-fast": {
						Target:     "upstream-d.transit-dataplane.svc.cluster.local:8080",
						Provider:   "moonshot",
						AuthHeader: "Bearer shard-b-moonshot-token",
						Profile:    "profile-b",
						BYOKKeyID:  "key-b-002",
					},
				},
			},
		},
	}
}

func (s *ConfigStore) L1() L1Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneL1Config(s.l1)
}

func (s *ConfigStore) L2(shard string) (L2Config, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg, ok := s.l2[shard]
	return cloneL2Config(cfg), ok
}

func (s *ConfigStore) UpsertShard(update ShardUpdate) error {
	if update.Name == "" {
		return errors.New("shard name is required")
	}
	if update.Version == "" {
		update.Version = "updated"
	}
	nextShard := ShardConfig{
		Target:   update.Target,
		Prefixes: append([]string(nil), update.Prefixes...),
		Shard:    update.Shard,
		Status:   update.Status,
	}
	if err := validateShard(update.Name, nextShard); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneL1Config(s.l1)
	next.Version = update.Version
	next.Shards[update.Name] = nextShard
	s.l1 = next
	return nil
}

func (s *ConfigStore) UpsertModel(update ModelUpdate) error {
	if update.Shard == "" {
		return errors.New("shard is required")
	}
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
		Profile:    update.Profile,
		BYOKKeyID:  update.BYOKKeyID,
	}
	if err := validateModel(update.Name, model); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneL2Configs(s.l2)
	cfg := next[update.Shard]
	cfg.Version = update.Version
	if cfg.Models == nil {
		cfg.Models = make(map[string]ModelConfig)
	}
	cfg.Models[update.Name] = model
	next[update.Shard] = cfg
	s.l2 = next
	return nil
}

func (s *ConfigStore) Dump() DumpConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := DumpConfig{
		L1: cloneL1Config(s.l1),
		L2: make(map[string]DumpL2Config, len(s.l2)),
	}
	for shard, cfg := range s.l2 {
		dump := DumpL2Config{
			Version: cfg.Version,
			Models:  make(map[string]DumpModel, len(cfg.Models)),
		}
		for name, model := range cfg.Models {
			dump.Models[name] = DumpModel{
				Target:         model.Target,
				Provider:       model.Provider,
				Profile:        model.Profile,
				BYOKKeyID:      model.BYOKKeyID,
				AuthConfigured: model.AuthHeader != "",
			}
		}
		out.L2[shard] = dump
	}
	return out
}

func validateL1(cfg L1Config) error {
	if cfg.Version == "" {
		return errors.New("l1 version is required")
	}
	if cfg.DefaultShard == "" {
		return errors.New("default shard is required")
	}
	if _, ok := cfg.Shards[cfg.DefaultShard]; !ok {
		return fmt.Errorf("default shard %q is not configured", cfg.DefaultShard)
	}
	for name, shard := range cfg.Shards {
		if err := validateShard(name, shard); err != nil {
			return err
		}
	}
	return nil
}

func validateShard(name string, shard ShardConfig) error {
	if name == "" {
		return errors.New("shard name is required")
	}
	if shard.Target == "" {
		return fmt.Errorf("shard %q target is required", name)
	}
	return nil
}

func validateL2(shard string, cfg L2Config) error {
	if shard == "" {
		return errors.New("l2 shard name is required")
	}
	if cfg.Version == "" {
		return fmt.Errorf("l2 shard %q version is required", shard)
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

func cloneL1Config(in L1Config) L1Config {
	out := L1Config{
		Version:      in.Version,
		DefaultShard: in.DefaultShard,
		Shards:       make(map[string]ShardConfig, len(in.Shards)),
	}
	for name, shard := range in.Shards {
		shard.Prefixes = append([]string(nil), shard.Prefixes...)
		out.Shards[name] = shard
	}
	return out
}

func cloneL2Configs(in map[string]L2Config) map[string]L2Config {
	out := make(map[string]L2Config, len(in))
	for shard, cfg := range in {
		out[shard] = cloneL2Config(cfg)
	}
	return out
}

func cloneL2Config(in L2Config) L2Config {
	return L2Config{
		Version: in.Version,
		Models:  maps.Clone(in.Models),
	}
}
