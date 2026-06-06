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

// OrgEntry holds connection details and a per-user credential map for one org.
type OrgEntry struct {
	Server     string                `yaml:"server,omitempty"`
	ActiveUser string                `yaml:"active_user,omitempty"`
	Users      map[string]*UserEntry `yaml:"users,omitempty"`
}

// UserEntry holds the API key for one user within an org.
type UserEntry struct {
	APIKey string `yaml:"api_key,omitempty"`
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

// ActiveEntry resolves the active org → active user → credentials.
// Returns the OrgEntry (for Server) and UserEntry (for APIKey).
func (c *Config) ActiveEntry() (*OrgEntry, *UserEntry, error) {
	if c.ActiveOrg == "" {
		return nil, nil, errors.New("not logged in — run: orange auth login --org <org> --user <user>")
	}
	org, ok := c.Orgs[c.ActiveOrg]
	if !ok {
		return nil, nil, fmt.Errorf("org %q not found — run: orange auth login --org %s --user <user>", c.ActiveOrg, c.ActiveOrg)
	}
	if org.ActiveUser == "" {
		return nil, nil, fmt.Errorf("no active user for org %q — run: orange auth login --org %s --user <user>", c.ActiveOrg, c.ActiveOrg)
	}
	u, ok := org.Users[org.ActiveUser]
	if !ok || u.APIKey == "" {
		return nil, nil, fmt.Errorf("no credentials for %s/%s — run: orange auth login --org %s --user %s",
			c.ActiveOrg, org.ActiveUser, c.ActiveOrg, org.ActiveUser)
	}
	return org, u, nil
}

// SetUser upserts a UserEntry under orgSlug/userID and makes it the active
// org+user. If server is empty the existing server for the org is kept.
func (c *Config) SetUser(orgSlug, userID, server, apiKey string) {
	if c.Orgs == nil {
		c.Orgs = make(map[string]*OrgEntry)
	}
	org, ok := c.Orgs[orgSlug]
	if !ok {
		org = &OrgEntry{}
		c.Orgs[orgSlug] = org
	}
	if server != "" {
		org.Server = server
	}
	if org.Users == nil {
		org.Users = make(map[string]*UserEntry)
	}
	org.Users[userID] = &UserEntry{APIKey: apiKey}
	org.ActiveUser = userID
	c.ActiveOrg = orgSlug
}
