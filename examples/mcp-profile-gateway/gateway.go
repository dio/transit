package mcpprofilegateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
)

const ConfigEnv = "MCP_PROFILE_GATEWAY_CONFIG"

const DefaultCatalogCalloutCluster = "mcp-profile-gateway-l2"

type Config struct {
	TimeoutMillis  int                      `json:"timeout_millis,omitempty"`
	Profiles       map[string]Profile       `json:"profiles,omitempty"`
	CatalogServers map[string]CatalogServer `json:"catalog_servers"`
}

type Profile struct {
	Name    string                   `json:"name"`
	APIKey  string                   `json:"api_key,omitempty"`
	Servers map[string]ProfileServer `json:"servers"`
}

type ProfileServer struct {
	URL                string          `json:"url"`
	Prefix             string          `json:"prefix,omitempty"`
	CredentialRef      string          `json:"credential_ref,omitempty"`
	CredentialEnvelope string          `json:"credential_envelope,omitempty"`
	EnabledTools       map[string]bool `json:"enabled_tools,omitempty"`
}

type CatalogServer struct {
	URL     string `json:"url"`
	Cluster string `json:"cluster,omitempty"`
}

type Gateway struct {
	config Config
	client *http.Client

	mu      sync.Mutex
	servers map[string]CatalogDump
}

type CatalogDump struct {
	Target      string `json:"target"`
	LastRequest string `json:"last_request,omitempty"`
}

func LoadConfigFromEnv() (Config, error) {
	raw := os.Getenv(ConfigEnv)
	if strings.TrimSpace(raw) == "" {
		return Config{}, fmt.Errorf("%s is required", ConfigEnv)
	}
	var config Config
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", ConfigEnv, err)
	}
	if err := ValidateConfig(config); err != nil {
		return Config{}, fmt.Errorf("%s: %w", ConfigEnv, err)
	}
	return config, nil
}

func ValidateConfig(config Config) error {
	if len(config.CatalogServers) == 0 {
		return errors.New("at least one catalog server is required")
	}
	for id, server := range config.CatalogServers {
		if err := validateSlug("catalog server", id); err != nil {
			return err
		}
		if err := validateURL("catalog server", id, server.URL); err != nil {
			return err
		}
		if strings.Contains(server.Cluster, "/") {
			return fmt.Errorf("catalog server %q cluster must not contain /", id)
		}
	}
	for profileID, profile := range config.Profiles {
		if strings.TrimSpace(profileID) == "" {
			return errors.New("profile id is required")
		}
		if profile.Name == "" {
			return fmt.Errorf("profile %q name is required", profileID)
		}
		if len(profile.Servers) == 0 {
			return fmt.Errorf("profile %q must have at least one server", profileID)
		}
		prefixes := map[string]string{}
		for serverID, server := range profile.Servers {
			if err := validateSlug("profile server", serverID); err != nil {
				return err
			}
			if err := validateURL("profile server", serverID, server.URL); err != nil {
				return err
			}
			if _, ok := config.CatalogServers[serverID]; !ok {
				return fmt.Errorf("profile %q server %q is not in catalog_servers", profileID, serverID)
			}
			prefix := serverPrefix(serverID, server)
			if owner, ok := prefixes[prefix]; ok {
				return fmt.Errorf("profile %q duplicate tool prefix %q for %q and %q", profileID, prefix, owner, serverID)
			}
			prefixes[prefix] = serverID
		}
	}
	return nil
}

func New(config Config) *Gateway {
	timeout := time.Duration(config.TimeoutMillis) * time.Millisecond
	if timeout <= 0 {
		timeout = 800 * time.Millisecond
	}
	return &Gateway{
		config:  config,
		client:  &http.Client{Timeout: timeout},
		servers: make(map[string]CatalogDump, len(config.CatalogServers)),
	}
}

func (g *Gateway) Dump() map[string]CatalogDump {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]CatalogDump, len(g.servers))
	for k, v := range g.servers {
		out[k] = v
	}
	return out
}

func (g *Gateway) record(id string, server CatalogServer, state string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.servers[id] = CatalogDump{
		Target:      server.URL,
		LastRequest: state,
	}
}

func SortedCatalogServerIDs(servers map[string]CatalogServer) []string {
	out := make([]string, 0, len(servers))
	for id := range servers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func catalogURL(base, serverID string) string {
	return strings.TrimRight(base, "/") + "/mcp/s/" + url.PathEscape(serverID)
}

func validateSlug(kind, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s id is required", kind)
	}
	if strings.Contains(value, "/") {
		return fmt.Errorf("%s %q must not contain /", kind, value)
	}
	return nil
}

func validateURL(kind, id, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%s %q url is required", kind, id)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s %q url must be absolute", kind, id)
	}
	return nil
}

func serverPrefix(serverID string, server ProfileServer) string {
	if server.Prefix != "" {
		return server.Prefix
	}
	return serverID
}

func stripQuery(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}

func errorResponse(id json.RawMessage, code int, format string, args ...any) mcpprofilerouter.JSONRPCResponse {
	return mcpprofilerouter.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &mcpprofilerouter.JSONRPCError{
			Code:    code,
			Message: fmt.Sprintf(format, args...),
		},
	}
}
