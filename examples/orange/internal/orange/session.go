package orange

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is persisted at ~/.orange/config.
type Config struct {
	ActiveOrg string               `yaml:"active_org,omitempty"`
	Orgs      map[string]*OrgEntry `yaml:"orgs,omitempty"`
}

// OrgEntry holds the connection details and active identity for one org.
type OrgEntry struct {
	Server     string `yaml:"server,omitempty"`
	APIKey     string `yaml:"api_key,omitempty"`
	ActiveUser string `yaml:"active_user,omitempty"`
}

// configPath returns ~/.orange/config.
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".orange", "config"), nil
}

// OrangeDir returns ~/.orange, creating it if needed.
func OrangeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".orange")
	return dir, os.MkdirAll(dir, 0o700)
}

// LoadConfig reads ~/.orange/config. Returns an empty Config if the file
// does not exist yet.
func LoadConfig() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Orgs: make(map[string]*OrgEntry)}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Orgs == nil {
		cfg.Orgs = make(map[string]*OrgEntry)
	}
	return &cfg, nil
}

// SaveConfig writes cfg to ~/.orange/config (mode 0600).
func SaveConfig(cfg *Config) error {
	dir, err := OrangeDir()
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config"), data, 0o600)
}

// ActiveEntry returns the OrgEntry for the currently active org.
func (c *Config) ActiveEntry() (*OrgEntry, error) {
	if c.ActiveOrg == "" {
		return nil, errors.New("not logged in — run: orange auth login --org <org>")
	}
	e, ok := c.Orgs[c.ActiveOrg]
	if !ok || e.APIKey == "" {
		return nil, fmt.Errorf("no credentials for org %q — run: orange auth login --org %s", c.ActiveOrg, c.ActiveOrg)
	}
	return e, nil
}

// SetOrg upserts an OrgEntry and sets it as active.
func (c *Config) SetOrg(org string, entry OrgEntry) {
	if c.Orgs == nil {
		c.Orgs = make(map[string]*OrgEntry)
	}
	c.Orgs[org] = &entry
	c.ActiveOrg = org
}
