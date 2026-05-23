package mcpprofilegateway

import (
	"encoding/base64"
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
const profileSessionPrefix = "mcp-profile-gateway."

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

// GatewayDump is the redacted view returned by GET /dump. It never includes
// raw credential values, session IDs, or API keys.
type GatewayDump struct {
	CatalogServers map[string]CatalogDump  `json:"catalog_servers"`
	Profiles       map[string]ProfileDump  `json:"profiles,omitempty"`
}

type CatalogDump struct {
	Target      string `json:"target"`
	LastRequest string `json:"last_request,omitempty"`
}

type ProfileDump struct {
	Name           string                      `json:"name"`
	AuthConfigured bool                        `json:"auth_configured"`
	Servers        map[string]ProfileServerDump `json:"servers"`
}

type ProfileServerDump struct {
	Prefix                      string `json:"prefix"`
	EnabledToolsCount           *int   `json:"enabled_tools_count,omitempty"`
	CredentialRefConfigured     bool   `json:"credential_ref_configured,omitempty"`
	CredentialEnvelopeConfigured bool  `json:"credential_envelope_configured,omitempty"`
}

type profileSession struct {
	ProfileID string            `json:"profile_id"`
	Backends  map[string]string `json:"backends"`
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
			if server.Prefix != "" && strings.Contains(server.Prefix, ".") {
				return fmt.Errorf("profile %q server %q prefix %q must not contain .", profileID, serverID, server.Prefix)
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

func (g *Gateway) Dump() GatewayDump {
	g.mu.Lock()
	runtimeState := make(map[string]CatalogDump, len(g.servers))
	for k, v := range g.servers {
		runtimeState[k] = v
	}
	g.mu.Unlock()

	catalogServers := make(map[string]CatalogDump, len(g.config.CatalogServers))
	for serverID, server := range g.config.CatalogServers {
		d := CatalogDump{Target: server.URL}
		if state, ok := runtimeState[serverID]; ok {
			d.LastRequest = state.LastRequest
		}
		catalogServers[serverID] = d
	}

	var profiles map[string]ProfileDump
	if len(g.config.Profiles) > 0 {
		profiles = make(map[string]ProfileDump, len(g.config.Profiles))
		for profileID, profile := range g.config.Profiles {
			servers := make(map[string]ProfileServerDump, len(profile.Servers))
			for serverID, server := range profile.Servers {
				var enabledCount *int
				if server.EnabledTools != nil {
					n := 0
					for _, v := range server.EnabledTools {
						if v {
							n++
						}
					}
					enabledCount = &n
				}
				servers[serverID] = ProfileServerDump{
					Prefix:                      serverPrefix(serverID, server),
					EnabledToolsCount:            enabledCount,
					CredentialRefConfigured:      server.CredentialRef != "",
					CredentialEnvelopeConfigured: server.CredentialEnvelope != "",
				}
			}
			profiles[profileID] = ProfileDump{
				Name:           profile.Name,
				AuthConfigured: profile.APIKey != "",
				Servers:        servers,
			}
		}
	}
	return GatewayDump{
		CatalogServers: catalogServers,
		Profiles:       profiles,
	}
}

func (g *Gateway) record(id string, server CatalogServer, state string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.servers[id] = CatalogDump{
		Target:      server.URL,
		LastRequest: state,
	}
}

// encodeProfileSession encodes the L1 profile session as a prefixed base64url
// JSON value. This format is intentionally readable for the example. Production
// must replace it with an authenticated and encrypted envelope (e.g. AEAD or
// signed JWT) that binds profile ID, server IDs, audience, subject, and expiry,
// and never exposes backend session IDs as client-visible plaintext. A forged
// or replayed envelope allows impersonation of any backend session.
func encodeProfileSession(profileID string, backends map[string]string) (string, error) {
	raw, err := json.Marshal(profileSession{
		ProfileID: profileID,
		Backends:  copyStringMap(backends),
	})
	if err != nil {
		return "", err
	}
	return profileSessionPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeProfileSession(profileID, sessionID string) (profileSession, bool) {
	raw, ok := strings.CutPrefix(sessionID, profileSessionPrefix)
	if !ok || raw == "" {
		return profileSession{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return profileSession{}, false
	}
	var session profileSession
	if err := json.Unmarshal(decoded, &session); err != nil {
		return profileSession{}, false
	}
	if session.ProfileID != profileID || len(session.Backends) == 0 {
		return profileSession{}, false
	}
	session.Backends = copyStringMap(session.Backends)
	return session, true
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func SortedCatalogServerIDs(servers map[string]CatalogServer) []string {
	out := make([]string, 0, len(servers))
	for id := range servers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func sortedProfileServerIDs(servers map[string]ProfileServer) []string {
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
	if strings.Contains(value, ".") {
		return fmt.Errorf("%s %q must not contain .", kind, value)
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

func profileToolEnabled(server ProfileServer, name string) bool {
	if server.EnabledTools == nil {
		return true
	}
	return server.EnabledTools[name]
}

func profileAuthorized(apiKey string, header func(string) string) bool {
	if apiKey == "" {
		return true
	}
	if header("x-api-key") == apiKey {
		return true
	}
	return strings.TrimSpace(header("authorization")) == "Bearer "+apiKey
}

func toolsListRequestBody(id json.RawMessage) ([]byte, error) {
	return json.Marshal(mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      cloneRaw(id),
		Method:  mcpprofilerouter.MethodToolsList,
	})
}

func syntheticInitializeResult() mcpprofilerouter.InitializeResult {
	return mcpprofilerouter.InitializeResult{
		ProtocolVersion: mcpprofilerouter.ProtocolVersion,
		Capabilities: map[string]any{
			"tools": map[string]any{},
		},
		ServerInfo: mcpprofilerouter.Implementation{Name: "mcp-profile-gateway", Version: "dev"},
	}
}

func decodeInitializeResult(body []byte) error {
	var rpc mcpprofilerouter.JSONRPCResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		return err
	}
	if rpc.Error != nil {
		return errors.New(rpc.Error.Message)
	}
	raw, err := json.Marshal(rpc.Result)
	if err != nil {
		return err
	}
	var out mcpprofilerouter.InitializeResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	return nil
}

func mergeToolsResult(serverID string, server ProfileServer, body []byte) ([]mcpprofilerouter.Tool, error) {
	var rpc mcpprofilerouter.JSONRPCResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		return nil, err
	}
	if rpc.Error != nil {
		return nil, errors.New(rpc.Error.Message)
	}
	raw, err := json.Marshal(rpc.Result)
	if err != nil {
		return nil, err
	}
	var out mcpprofilerouter.ListToolsResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	tools := make([]mcpprofilerouter.Tool, 0, len(out.Tools))
	for _, tool := range out.Tools {
		if !profileToolEnabled(server, tool.Name) {
			continue
		}
		tool.Name = serverPrefix(serverID, server) + "." + tool.Name
		tools = append(tools, tool)
	}
	return tools, nil
}

func sortTools(tools []mcpprofilerouter.Tool) {
	sort.Slice(tools, func(i, j int) bool {
		return tools[i].Name < tools[j].Name
	})
}

func serverByPrefix(profile Profile, prefix string) (string, ProfileServer, bool) {
	for serverID, server := range profile.Servers {
		if serverPrefix(serverID, server) == prefix {
			return serverID, server, true
		}
	}
	return "", ProfileServer{}, false
}

func resolveProfileTool(paramsRaw json.RawMessage, profile Profile) (serverID, backendTool string, params mcpprofilerouter.CallToolParams, errCode int, errMsg string) {
	if err := json.Unmarshal(paramsRaw, &params); err != nil || params.Name == "" {
		return "", "", params, -32602, "invalid tools/call params"
	}
	prefix, tool, ok := strings.Cut(params.Name, ".")
	if !ok || tool == "" {
		return "", "", params, -32602, "tool name must be namespaced as prefix.tool"
	}
	sid, _, ok := serverByPrefix(profile, prefix)
	if !ok {
		return "", "", params, -32602, fmt.Sprintf("unknown tool: %s", params.Name)
	}
	server := profile.Servers[sid]
	if !profileToolEnabled(server, tool) {
		return "", "", params, -32602, fmt.Sprintf("disabled tool: %s", params.Name)
	}
	return sid, tool, params, 0, ""
}

func toolCallForwardBody(id json.RawMessage, backendTool string, params mcpprofilerouter.CallToolParams) ([]byte, error) {
	params.Name = backendTool
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return json.Marshal(mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      cloneRaw(id),
		Method:  mcpprofilerouter.MethodToolsCall,
		Params:  paramsRaw,
	})
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
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
