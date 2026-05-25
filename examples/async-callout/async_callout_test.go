package async_callout

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up"
)

func TestCheckAuth_success(t *testing.T) {
	code, body := CheckAuth(up.HTTPCalloutSuccess, "ok")
	require.Equal(t, 0, code)
	require.Nil(t, body)
}

func TestCheckAuth_denied(t *testing.T) {
	code, body := CheckAuth(up.HTTPCalloutSuccess, "denied")
	require.Equal(t, 403, code)
	require.Equal(t, `{"error":"denied"}`, string(body))
}

func TestCheckAuth_emptyBody(t *testing.T) {
	code, body := CheckAuth(up.HTTPCalloutSuccess, "")
	require.Equal(t, 403, code)
	require.Equal(t, `{"error":"denied"}`, string(body))
}

func TestCheckAuth_calloutFailed(t *testing.T) {
	code, body := CheckAuth(up.HTTPCalloutReset, "")
	require.Equal(t, 503, code)
	require.Equal(t, `{"error":"auth unavailable"}`, string(body))
}
