package clusterdfp

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResolveTarget(t *testing.T) {
	addr, err := resolveTarget(context.Background(), net.JoinHostPort("localhost", "8080"), time.Second)
	require.NoError(t, err)

	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	require.Equal(t, "8080", port)
	require.NotEmpty(t, net.ParseIP(host))
}

func TestResolveTargetRequiresPort(t *testing.T) {
	_, err := resolveTarget(context.Background(), "localhost", time.Second)
	require.Error(t, err)
}

func TestParseConfigDefault(t *testing.T) {
	cfg, err := parseConfig(nil)
	require.NoError(t, err)
	require.Equal(t, 250*time.Millisecond, cfg.timeout())
}

func TestParseConfigTimeout(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"timeout_millis":50}`))
	require.NoError(t, err)
	require.Equal(t, 50*time.Millisecond, cfg.timeout())
}

func TestParseConfigModels(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"models":{"tiny":"localhost:8080","large":"localhost:9090"}}`))
	require.NoError(t, err)
	require.Equal(t, "localhost:8080", cfg.Models["tiny"])
	require.Equal(t, "localhost:9090", cfg.Models["large"])
}
