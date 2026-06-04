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
