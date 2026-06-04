package up

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRouterGETDispatchesExactMethodAndPath(t *testing.T) {
	var called bool
	var notFound bool
	router := NewRouter(func(*Writer, *Request) {
		notFound = true
	}).GET("/v1/models", func(*Writer, *Request) {
		called = true
	})

	router.Dispatch(nil, &Request{Method: http.MethodGet, Path: "/v1/models"})

	require.True(t, called)
	require.False(t, notFound)
}

func TestRouterPOSTDispatchesExactMethodAndPath(t *testing.T) {
	var called bool
	var notFound bool
	router := NewRouter(func(*Writer, *Request) {
		notFound = true
	}).POST("/v1/messages", func(*Writer, *Request) {
		called = true
	})

	router.Dispatch(nil, &Request{Method: http.MethodPost, Path: "/v1/messages"})

	require.True(t, called)
	require.False(t, notFound)
}

func TestRouterDELETEDispatchesExactMethodAndPath(t *testing.T) {
	var called bool
	var notFound bool
	router := NewRouter(func(*Writer, *Request) {
		notFound = true
	}).DELETE("/mcp", func(*Writer, *Request) {
		called = true
	})

	router.Dispatch(nil, &Request{Method: http.MethodDelete, Path: "/mcp"})

	require.True(t, called)
	require.False(t, notFound)
}

func TestRouterPOSTPrefixMatchesSubPaths(t *testing.T) {
	var called bool
	var notFound bool
	router := NewRouter(func(*Writer, *Request) {
		notFound = true
	}).POSTPrefix("/mcp", func(*Writer, *Request) {
		called = true
	})

	router.Dispatch(nil, &Request{Method: http.MethodPost, Path: "/mcp/s/kiwi"})

	require.True(t, called)
	require.False(t, notFound)
}

func TestRouterPrefixDoesNotMatchSiblingPaths(t *testing.T) {
	var called bool
	router := NewRouter(nil).
		POSTPrefix("/mcp", func(*Writer, *Request) { called = true })

	router.Dispatch(nil, &Request{Method: http.MethodPost, Path: "/mcp-other"})

	require.False(t, called)
}

func TestRouterExactTakesPriorityOverPrefix(t *testing.T) {
	var exactCalled bool
	var prefixCalled bool
	router := NewRouter(nil).
		POST("/mcp", func(*Writer, *Request) { exactCalled = true }).
		POSTPrefix("/mcp", func(*Writer, *Request) { prefixCalled = true })

	router.Dispatch(nil, &Request{Method: http.MethodPost, Path: "/mcp"})

	require.True(t, exactCalled)
	require.False(t, prefixCalled)
}

func TestRouterWrongMethodUsesNotFound(t *testing.T) {
	var called bool
	var notFound bool
	router := NewRouter(func(*Writer, *Request) {
		notFound = true
	}).GET("/v1/models", func(*Writer, *Request) {
		called = true
	})

	router.Dispatch(nil, &Request{Method: http.MethodPost, Path: "/v1/models"})

	require.False(t, called)
	require.True(t, notFound)
}
