package server

// egress_serve_local_mcp.go — "mcp" top-level REPL command.
//
// Mirrors demos/mcp in-process. All requests go through the local Envoy proxy.
//
// Commands (dispatched by cmdMCP):
//
//	mcp profile <name>         set active profile + initialize
//	mcp server  <name>         set active server  + initialize
//	mcp initialize             (re)initialize current target, print session ID
//	mcp list                   tools/list (requires session)
//	mcp call <tool> [<json>]   tools/call (requires session)
//	mcp stream                 GET SSE stream (requires session)
//	mcp delete                 DELETE session
//	mcp set base <url>         change MCP base URL (default: http://localhost:8080/mcp)
//	mcp status                 show settings + active session ID
//	mcp help / ?               this help

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// cmdMCP routes "mcp ..." subcommands.
func (s *serveLocalState) cmdMCP(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return s.mcpStatus()
	}
	switch args[0] {
	case "profile":
		if len(args) < 2 || args[1] == "list" {
			return s.mcpListProfiles()
		}
		s.mcpProfile = args[1]
		s.mcpServer = ""
		s.mcpSessionID = ""
		return s.mcpInitialize(ctx)

	case "server":
		if len(args) < 2 || args[1] == "list" {
			return s.mcpListServers()
		}
		s.mcpServer = args[1]
		s.mcpProfile = ""
		s.mcpSessionID = ""
		return s.mcpInitialize(ctx)

	case "initialize", "init":
		s.mcpSessionID = ""
		return s.mcpInitialize(ctx)

	case "list":
		return s.mcpList(ctx)

	case "call":
		if len(args) < 2 {
			return fmt.Errorf("usage: mcp call <tool> [<json-args>]")
		}
		jsonArgs := "{}"
		if len(args) > 2 {
			jsonArgs = strings.Join(args[2:], " ")
		}
		return s.mcpCall(ctx, args[1], jsonArgs)

	case "stream":
		return s.mcpStream(ctx)

	case "delete":
		return s.mcpDelete(ctx)

	case "set":
		return s.mcpSet(args[1:])

	case "status":
		return s.mcpStatus()

	case "help", "?":
		printMCPHelp()
		return nil

	default:
		return fmt.Errorf("unknown mcp subcommand %q — type 'mcp help'", args[0])
	}
}

// ── URL helpers ───────────────────────────────────────────────────────────────

func (s *serveLocalState) mcpURL() string {
	base := strings.TrimRight(s.mcpBaseURL, "/")
	if s.mcpServer != "" {
		return base + "/s/" + s.mcpServer
	}
	if s.mcpProfile != "" {
		return base + "/" + s.mcpProfilePath()
	}
	return base
}

// mcpProfilePath resolves the active profile's opaque URL path from the
// config snapshot. Falls back to the raw profile key if not found.
func (s *serveLocalState) mcpProfilePath() string {
	if raw, err := s.snapshotFn(); err == nil && raw != nil {
		if p, ok := raw.Profiles[s.mcpProfile]; ok && p.Path != "" {
			return p.Path
		}
	}
	return s.mcpProfile
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func (s *serveLocalState) mcpPost(ctx context.Context, payload any) (*http.Response, time.Duration, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.mcpURL(), bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.mcpSessionID != "" {
		req.Header.Set("Mcp-Session-Id", s.mcpSessionID)
	}
	start := time.Now()
	resp, err := s.llmClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("POST %s: %w", s.mcpURL(), err)
	}
	return resp, time.Since(start), nil
}

func (s *serveLocalState) mcpDo(ctx context.Context, method string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.mcpURL(), nil)
	if err != nil {
		return nil, err
	}
	if s.mcpSessionID != "" {
		req.Header.Set("Mcp-Session-Id", s.mcpSessionID)
	}
	resp, err := s.llmClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, s.mcpURL(), err)
	}
	return resp, nil
}

// extractSession pulls the mcp-session-id header (case-insensitive).
func extractSession(resp *http.Response) string {
	return resp.Header.Get("Mcp-Session-Id")
}

// ── initialize ────────────────────────────────────────────────────────────────

func (s *serveLocalState) mcpInitialize(ctx context.Context) error {
	if s.mcpProfile == "" && s.mcpServer == "" {
		return fmt.Errorf("no target set — run: mcp profile <name>  or  mcp server <name>")
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      "orange-repl-init",
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "orange-repl", "version": "0.1.0"},
		},
	}
	fmt.Fprintf(os.Stderr, "→ POST %s  (initialize)\n", s.mcpURL())
	resp, elapsed, err := s.mcpPost(ctx, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "← HTTP %d  %.0fms\n", resp.StatusCode, float64(elapsed.Milliseconds()))
	if sid := extractSession(resp); sid != "" {
		s.mcpSessionID = sid
		fmt.Fprintf(os.Stderr, "  session: %s\n", sid)
	}
	return mcpPrintBody(resp)
}

// ── list ──────────────────────────────────────────────────────────────────────

func (s *serveLocalState) mcpList(ctx context.Context) error {
	if err := s.mcpRequireSession(); err != nil {
		return err
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      "orange-repl-list",
		"method":  "tools/list",
		"params":  map[string]any{},
	}
	fmt.Fprintf(os.Stderr, "→ POST %s  (tools/list)\n", s.mcpURL())
	resp, elapsed, err := s.mcpPost(ctx, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "← HTTP %d  %.0fms\n", resp.StatusCode, float64(elapsed.Milliseconds()))

	allowed := s.mcpAllowedTools()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if rpcResult, ok := result["result"].(map[string]any); ok {
		if tools, ok := rpcResult["tools"].([]any); ok {
			var shown int
			for _, t := range tools {
				m, ok := t.(map[string]any)
				if !ok {
					continue
				}
				name, _ := m["name"].(string)
				if allowed != nil && !allowed[name] {
					continue
				}
				desc, _ := m["description"].(string)
				if desc != "" {
					fmt.Printf("  %-40s  %s\n", name, desc)
				} else {
					fmt.Println(" ", name)
				}
				shown++
			}
			total := len(tools)
			if allowed != nil && shown < total {
				fmt.Fprintf(os.Stderr, "  %d tool(s)  (%d filtered by tools_include)\n", shown, total-shown)
			} else {
				fmt.Fprintf(os.Stderr, "  %d tool(s)\n", shown)
			}
			return nil
		}
	}
	llmPrintJSON(result)
	return nil
}

// mcpAllowedTools returns a set of allowed tool names derived from the
// orange config for the current target, or nil if no filtering applies.
// Tool names are namespaced: "<namespace>__<tool>".
func (s *serveLocalState) mcpAllowedTools() map[string]bool {
	raw, err := s.snapshotFn()
	if err != nil || raw == nil {
		return nil
	}

	allowed := map[string]bool{}

	if s.mcpServer != "" {
		srv, ok := raw.MCP.Servers[s.mcpServer]
		if !ok || len(srv.ToolsInclude) == 0 {
			return nil
		}
		ns := srv.Namespace
		if ns == "" {
			ns = s.mcpServer
		}
		for _, t := range srv.ToolsInclude {
			allowed[ns+"__"+t] = true
		}
		return allowed
	}

	if s.mcpProfile != "" {
		profile, ok := raw.Profiles[s.mcpProfile]
		if !ok {
			return nil
		}
		for srvName, filter := range profile.Tools {
			srv, ok := raw.MCP.Servers[srvName]
			if !ok {
				continue
			}
			ns := srv.Namespace
			if ns == "" {
				ns = srvName
			}
			include := filter.Include
			if len(include) == 0 {
				// No profile-level override — use server's tools_include.
				include = srv.ToolsInclude
			}
			for _, t := range include {
				allowed[ns+"__"+t] = true
			}
		}
		if len(allowed) == 0 {
			return nil
		}
		return allowed
	}

	return nil
}

// ── call ──────────────────────────────────────────────────────────────────────

func (s *serveLocalState) mcpCall(ctx context.Context, tool, jsonArgs string) error {
	if err := s.mcpRequireSession(); err != nil {
		return err
	}
	var arguments any
	if err := json.Unmarshal([]byte(jsonArgs), &arguments); err != nil {
		return fmt.Errorf("invalid JSON arguments: %w", err)
	}
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      "orange-repl-call",
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": arguments},
	}
	fmt.Fprintf(os.Stderr, "→ POST %s  (tools/call %s)\n", s.mcpURL(), tool)
	resp, elapsed, err := s.mcpPost(ctx, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "← HTTP %d  %.0fms\n", resp.StatusCode, float64(elapsed.Milliseconds()))
	return mcpPrintBody(resp)
}

// ── stream ────────────────────────────────────────────────────────────────────

func (s *serveLocalState) mcpStream(ctx context.Context) error {
	if err := s.mcpRequireSession(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "→ GET %s  (stream, Ctrl+C to stop)\n", s.mcpURL())
	resp, err := s.mcpDo(ctx, http.MethodGet)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "← HTTP %d\n", resp.StatusCode)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Println(line)
	}
	return scanner.Err()
}

// ── delete ────────────────────────────────────────────────────────────────────

func (s *serveLocalState) mcpDelete(ctx context.Context) error {
	if err := s.mcpRequireSession(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "→ DELETE %s\n", s.mcpURL())
	resp, err := s.mcpDo(ctx, http.MethodDelete)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "← HTTP %d\n", resp.StatusCode)
	s.mcpSessionID = ""
	fmt.Println("session deleted")
	return nil
}

// ── profile list / server list ────────────────────────────────────────────────

func (s *serveLocalState) mcpListProfiles() error {
	raw, err := s.snapshotFn()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	if len(raw.Profiles) == 0 {
		fmt.Fprintln(os.Stderr, "  no profiles configured")
		return nil
	}
	names := make([]string, 0, len(raw.Profiles))
	for k := range raw.Profiles {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		p := raw.Profiles[name]
		servers := make([]string, 0, len(p.Tools))
		for srv := range p.Tools {
			servers = append(servers, srv)
		}
		sort.Strings(servers)
		active := ""
		if name == s.mcpProfile {
			active = "  ◀ active"
		}
		path := p.Path
		if path == "" {
			path = "(no path — not routable)"
		}
		fmt.Printf("  %-30s  path=%-15s  servers: %s%s\n", name, path, strings.Join(servers, ", "), active)
	}
	fmt.Fprintf(os.Stderr, "  %d profile(s)  — run: mcp profile <name>\n", len(names))
	return nil
}

func (s *serveLocalState) mcpListServers() error {
	raw, err := s.snapshotFn()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	if len(raw.MCP.Servers) == 0 {
		fmt.Fprintln(os.Stderr, "  no servers configured")
		return nil
	}
	names := make([]string, 0, len(raw.MCP.Servers))
	for k := range raw.MCP.Servers {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		srv := raw.MCP.Servers[name]
		active := ""
		if name == s.mcpServer {
			active = "  ◀ active"
		}
		fmt.Printf("  %-20s  ns=%-15s  tools: %s%s\n",
			name, srv.Namespace, strings.Join(srv.ToolsInclude, ", "), active)
	}
	fmt.Fprintf(os.Stderr, "  %d server(s)  — run: mcp server <name>\n", len(names))
	return nil
}

// ── set / status ──────────────────────────────────────────────────────────────

func (s *serveLocalState) mcpSet(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: mcp set base|profile|server <value>")
	}
	switch args[0] {
	case "base":
		s.mcpBaseURL = strings.TrimRight(args[1], "/")
		fmt.Printf("base → %s\n", s.mcpBaseURL)
	case "profile":
		s.mcpProfile = args[1]
		s.mcpServer = ""
		s.mcpSessionID = ""
		fmt.Printf("profile → %s  (session cleared)\n", s.mcpProfile)
	case "server":
		s.mcpServer = args[1]
		s.mcpProfile = ""
		s.mcpSessionID = ""
		fmt.Printf("server → %s  (session cleared)\n", s.mcpServer)
	default:
		return fmt.Errorf("unknown setting %q — try: base, profile, server", args[0])
	}
	return nil
}

func (s *serveLocalState) mcpStatus() error {
	target := "(none)"
	if s.mcpServer != "" {
		target = "server=" + s.mcpServer
	} else if s.mcpProfile != "" {
		target = "profile=" + s.mcpProfile
	}
	session := s.mcpSessionID
	if session == "" {
		session = "(none — run: mcp profile <name>)"
	}
	fmt.Printf("base:    %s\n", s.mcpBaseURL)
	fmt.Printf("target:  %s\n", target)
	fmt.Printf("url:     %s\n", s.mcpURL())
	fmt.Printf("session: %s\n", session)
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (s *serveLocalState) mcpRequireSession() error {
	if s.mcpSessionID == "" {
		target := "mcp profile <name>"
		if s.mcpServer != "" || s.mcpProfile != "" {
			target = "mcp initialize"
		}
		return fmt.Errorf("no active session — run: %s", target)
	}
	return nil
}

// mcpPrintBody prints the response body, handling both plain JSON and
// SSE (text/event-stream) responses that the MCP sidecar may return for POSTs.
func mcpPrintBody(resp *http.Response) error {
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") || strings.Contains(ct, "text/plain") {
		return mcpPrintSSE(resp)
	}
	// Try JSON first; if it fails the body probably is SSE without the header.
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(resp.Body); err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	var v any
	if err := json.Unmarshal(raw.Bytes(), &v); err == nil {
		llmPrintJSON(v)
		return nil
	}
	// Fall back to SSE parsing.
	scanner := bufio.NewScanner(&raw)
	return mcpScanSSE(scanner)
}

func mcpPrintSSE(resp *http.Response) error {
	return mcpScanSSE(bufio.NewScanner(resp.Body))
}

func mcpScanSSE(scanner *bufio.Scanner) error {
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var v any
		if err := json.Unmarshal([]byte(data), &v); err == nil {
			llmPrintJSON(v)
		} else {
			fmt.Println(data)
		}
	}
	return scanner.Err()
}

// ── help ──────────────────────────────────────────────────────────────────────

func printMCPHelp() {
	fmt.Print(`
mcp — MCP (Model Context Protocol) commands (routes through local Envoy on :8080/mcp)

Discover:
  mcp profile list           list profiles from orange.yaml
  mcp server  list           list servers  from orange.yaml

Session lifecycle:
  mcp profile <name>         set active profile + initialize (creates session)
  mcp server  <name>         set active server  + initialize (creates session)
  mcp initialize             (re)initialize current target, replace session
  mcp delete                 DELETE current session

Tools:
  mcp list                   tools/list  — show available tools
  mcp call <tool> [<json>]   tools/call  — call a tool with JSON arguments
  mcp stream                 GET SSE stream from current session

Config:
  mcp set base <url>         change MCP base URL (default: http://localhost:8080/mcp)
  mcp set profile <name>     switch profile without initializing
  mcp set server  <name>     switch server without initializing
  mcp status                 show base, active target, URL, session ID

Examples:
  mcp profile list
  mcp server  list
  mcp profile demo/dio/default
  mcp profile demo/dio/github
  mcp list
  mcp call kiwi__search-flight {"flyFrom":"SFO","flyTo":"JFK","departureDate":"15/07/2025"}
  mcp server github
  mcp list
  mcp call github__get_me {}
  mcp call github__search_repositories {"query":"envoy dynamic modules"}
  mcp call github__get_file_contents {"owner":"envoyproxy","repo":"envoy","path":"README.md"}
  mcp stream
  mcp delete

Tip: mcp profile / mcp server automatically initialize the session.
     Call mcp initialize explicitly to rotate to a fresh session.

`)
}
