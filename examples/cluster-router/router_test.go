package clusterrouter

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseRouterConfigDefaults(t *testing.T) {
	cfg, err := parseRouterConfig(nil)
	require.NoError(t, err)
	require.Equal(t, defaultRefresh, cfg.refresh())
	require.Equal(t, defaultTimeout, cfg.timeout())
}

func TestParseRouterConfigValues(t *testing.T) {
	cfg, err := parseRouterConfig([]byte(`{"config_url":"http://127.0.0.1/routes.json","refresh_millis":25,"timeout_millis":50}`))
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1/routes.json", cfg.ConfigURL)
	require.Equal(t, 25*time.Millisecond, cfg.refresh())
	require.Equal(t, 50*time.Millisecond, cfg.timeout())
}

func TestParseRouterConfigInvalidJSON(t *testing.T) {
	_, err := parseRouterConfig([]byte(`{`))
	require.Error(t, err)
}

func TestResolveConfigSnapshot(t *testing.T) {
	target := net.JoinHostPort("localhost", "8080")
	snap, err := resolveConfigSnapshot(context.Background(), configSnapshot{
		Version: "v1",
		Models: map[string]modelConfig{
			"gpt-fast": {
				Target:   target,
				Provider: "openai",
				AuthRef:  "openai-default",
			},
		},
		Auth: map[string]authConfig{
			"openai-default": {Type: "static", Header: "Bearer platform-key"},
		},
	}, time.Second)
	require.NoError(t, err)

	route := snap.Models["gpt-fast"]
	require.Equal(t, target, route.Target)
	require.Equal(t, "openai", route.Provider)
	require.Equal(t, "openai-default", route.AuthRef)
	require.Contains(t, route.Address, ":8080")
	require.Equal(t, "static", snap.Auth["openai-default"].Type)
}

func TestResolveConfigSnapshotRejectsMissingTarget(t *testing.T) {
	_, err := resolveConfigSnapshot(context.Background(), configSnapshot{
		Models: map[string]modelConfig{"gpt-fast": {Provider: "openai"}},
	}, time.Second)
	require.Error(t, err)
}

func TestRouteStorePublishesSnapshotCopy(t *testing.T) {
	store := newRouteStore()
	snap := routeSnapshot{
		Version: "v1",
		Models: map[string]modelRoute{
			"gpt-fast": {Target: "localhost:8080", Address: "127.0.0.1:8080"},
		},
	}
	store.Publish(snap)
	snap.Models["gpt-fast"] = modelRoute{Target: "mutated", Address: "mutated"}

	route, ok := store.LookupModel("gpt-fast")
	require.True(t, ok)
	require.Equal(t, "localhost:8080", route.Target)
	require.Equal(t, "127.0.0.1:8080", route.Address)
}

func TestResolveAuthHeaderStatic(t *testing.T) {
	got := resolveAuthHeader(routeSnapshot{
		Auth: map[string]authPolicy{
			"openai-default": {Type: "static", Header: "Bearer platform-key"},
		},
	}, modelRoute{Provider: "openai", AuthRef: "openai-default"}, "")
	require.Equal(t, "Bearer platform-key", got)
}

func TestResolveAuthHeaderBYOK(t *testing.T) {
	got := resolveAuthHeader(routeSnapshot{
		Auth: map[string]authPolicy{
			"tenant-key": {Type: "byok", TenantHeader: tenantHeader},
		},
		BYOK: map[string]map[string]string{
			"tenant-a": {"openai": "Bearer tenant-key"},
		},
	}, modelRoute{Provider: "openai", AuthRef: "tenant-key"}, "tenant-a")
	require.Equal(t, "Bearer tenant-key", got)
}

func TestDebugSnapshotRedactsAuthHeaders(t *testing.T) {
	store := newRouteStore()
	store.Publish(routeSnapshot{
		Version: "v1",
		Models: map[string]modelRoute{
			"gpt-fast": {
				Target:   "localhost:8080",
				Address:  "127.0.0.1:8080",
				Provider: "openai",
				AuthRef:  "openai-default",
			},
		},
		Auth: map[string]authPolicy{
			"openai-default": {Type: "static", Header: "Bearer secret"},
		},
	})

	body, err := json.Marshal(store.DebugSnapshot())
	require.NoError(t, err)
	require.Contains(t, string(body), `"configured":true`)
	require.NotContains(t, string(body), "Bearer secret")
}
