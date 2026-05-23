package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "backend":
		err = runBackend(ctx, os.Args[2:])
	case "aggregator":
		err = runAggregator(ctx, os.Args[2:])
	case "tools-list":
		err = runToolsList(ctx, os.Stdout, os.Args[2:])
	case "tools-call":
		err = runToolsCall(ctx, os.Stdout, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mcp-profile-router backend|aggregator|tools-list|tools-call [flags]")
}

func runBackend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backend", flag.ExitOnError)
	addr := fs.String("addr", ":8081", "listen address")
	id := fs.String("id", "github", "backend server id")
	tools := fs.String("tools", "search", "comma-separated tool names")
	expectedAuth := fs.String("expected-auth", "", "expected backend Authorization header")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return serve(ctx, *addr, mcpprofilerouter.NewBackend(*id, splitCSV(*tools), *expectedAuth).Handler())
}

func runAggregator(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("aggregator", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	profileID := fs.String("profile-id", "9b3f7d0a80c4aa6d-67261ca9ea3dadb2", "profile ID")
	profileName := fs.String("profile-name", "kiwi", "profile name")
	apiKey := fs.String("api-key", "profile-key", "profile API key")
	routeHeader := fs.String("route-header", "", "backend routing header for cluster-router egress; defaults to x-mcp-server")
	servers := fs.String("server", "", "server specs: slug=url=prefix=credential[;tools=tool_a|tool_b],slug=url=prefix=credential")
	if err := fs.Parse(args); err != nil {
		return err
	}
	parsed, err := parseServers(*servers)
	if err != nil {
		return err
	}
	profile := mcpprofilerouter.Profile{
		ID:            *profileID,
		Name:          *profileName,
		APIKey:        *apiKey,
		RouteHeader:   *routeHeader,
		TimeoutMillis: 800,
		Servers:       parsed,
	}
	if err := mcpprofilerouter.ValidateProfile(profile); err != nil {
		return err
	}
	return serve(ctx, *addr, mcpprofilerouter.NewAggregator(profile).Handler())
}

func runToolsList(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("tools-list", flag.ExitOnError)
	url := fs.String("url", "http://127.0.0.1:8080/mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2", "MCP profile or catalog URL")
	apiKey := fs.String("api-key", "profile-key", "profile API key; leave empty for catalog endpoints without auth")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sessionID, err := initialize(ctx, *url, *apiKey)
	if err != nil {
		return err
	}
	return callAndPrint(ctx, out, *url, *apiKey, sessionID, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  mcpprofilerouter.MethodToolsList,
	})
}

func runToolsCall(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("tools-call", flag.ExitOnError)
	url := fs.String("url", "http://127.0.0.1:8080/mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2", "MCP profile or catalog URL")
	apiKey := fs.String("api-key", "profile-key", "profile API key; leave empty for catalog endpoints without auth")
	arguments := fs.String("arguments", `{}`, "JSON object arguments")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("tools-call requires one namespaced tool name")
	}
	var argsMap map[string]any
	if err := json.Unmarshal([]byte(*arguments), &argsMap); err != nil {
		return fmt.Errorf("parse arguments: %w", err)
	}
	sessionID, err := initialize(ctx, *url, *apiKey)
	if err != nil {
		return err
	}
	return callAndPrint(ctx, out, *url, *apiKey, sessionID, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  mcpprofilerouter.MethodToolsCall,
		Params: mustRaw(mcpprofilerouter.CallToolParams{
			Name:      fs.Arg(0),
			Arguments: argsMap,
		}),
	})
}

func initialize(ctx context.Context, profileURL, apiKey string) (string, error) {
	resp, err := postRPC(ctx, profileURL, apiKey, "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
		Params: mustRaw(mcpprofilerouter.InitializeParams{
			ProtocolVersion: mcpprofilerouter.ProtocolVersion,
			ClientInfo:      mcpprofilerouter.Implementation{Name: "mcp-profile-router-cli", Version: "dev"},
		}),
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("initialize status %d", resp.StatusCode)
	}
	sessionID := resp.Header.Get(mcpprofilerouter.SessionIDHeader)
	if sessionID == "" {
		return "", fmt.Errorf("initialize response did not include %s", mcpprofilerouter.SessionIDHeader)
	}
	return sessionID, nil
}

func callAndPrint(ctx context.Context, out io.Writer, profileURL, apiKey, sessionID string, req mcpprofilerouter.JSONRPCRequest) error {
	resp, err := postRPC(ctx, profileURL, apiKey, sessionID, req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("request status %d", resp.StatusCode)
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

func postRPC(ctx context.Context, profileURL, apiKey, sessionID string, req mcpprofilerouter.JSONRPCRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, profileURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("authorization", "Bearer "+apiKey)
	}
	if sessionID != "" {
		httpReq.Header.Set(mcpprofilerouter.SessionIDHeader, sessionID)
	}
	return http.DefaultClient.Do(httpReq)
}

func serve(ctx context.Context, addr string, handler http.Handler) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", addr)
		errc <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	case err := <-errc:
		return err
	}
}

func parseServers(value string) (map[string]mcpprofilerouter.Server, error) {
	if value == "" {
		return nil, fmt.Errorf("at least one --server spec is required")
	}
	out := map[string]mcpprofilerouter.Server{}
	for _, spec := range splitCSV(value) {
		parts := strings.SplitN(spec, "=", 4)
		if len(parts) < 3 {
			return nil, fmt.Errorf("invalid server spec %q", spec)
		}
		server := mcpprofilerouter.Server{URL: parts[1], Prefix: parts[2]}
		if len(parts) == 4 {
			credential, enabledTools := parseCredentialOptions(parts[3])
			server.Credential = credential
			server.EnabledTools = enabledTools
		}
		out[parts[0]] = server
	}
	return out, nil
}

func parseCredentialOptions(value string) (string, map[string]bool) {
	credential, toolsRaw, ok := strings.Cut(value, ";tools=")
	if !ok {
		return value, nil
	}
	enabledTools := map[string]bool{}
	for _, tool := range splitList(toolsRaw, "|") {
		enabledTools[tool] = true
	}
	return credential, enabledTools
}

func splitCSV(value string) []string {
	return splitList(value, ",")
}

func splitList(value, sep string) []string {
	parts := strings.Split(value, ",")
	if sep != "," {
		parts = strings.Split(value, sep)
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func mustRaw(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
