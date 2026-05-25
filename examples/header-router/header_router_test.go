package headerrouter_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	headerrouter "github.com/dio/transit/examples/header-router"
)

func TestResolveHost_a(t *testing.T) {
	addr, ok := headerrouter.ResolveHost("a", "host-a:8080", "host-b:8081")
	require.True(t, ok)
	require.Equal(t, "host-a:8080", addr)
}

func TestResolveHost_b(t *testing.T) {
	addr, ok := headerrouter.ResolveHost("b", "host-a:8080", "host-b:8081")
	require.True(t, ok)
	require.Equal(t, "host-b:8081", addr)
}

func TestResolveHost_missing(t *testing.T) {
	addr, ok := headerrouter.ResolveHost("", "host-a:8080", "host-b:8081")
	require.False(t, ok)
	require.Equal(t, "", addr)
}

func TestResolveHost_unknown(t *testing.T) {
	addr, ok := headerrouter.ResolveHost("c", "host-a:8080", "host-b:8081")
	require.False(t, ok)
	require.Equal(t, "", addr)
}
