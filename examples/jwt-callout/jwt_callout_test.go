package jwtcallout

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up"
)

func TestParseBearer_valid(t *testing.T) {
	token, ok := ParseBearer("Bearer tok123")
	require.True(t, ok)
	require.Equal(t, "tok123", token)
}

func TestParseBearer_missing(t *testing.T) {
	token, ok := ParseBearer("")
	require.False(t, ok)
	require.Equal(t, "", token)
}

func TestParseBearer_basic(t *testing.T) {
	token, ok := ParseBearer("Basic abc")
	require.False(t, ok)
	require.Equal(t, "", token)
}

func TestParseBearer_emptyToken(t *testing.T) {
	token, ok := ParseBearer("Bearer ")
	require.False(t, ok)
	require.Equal(t, "", token)
}

func TestCheckIntrospection_active(t *testing.T) {
	code, sub, errBody := CheckIntrospection(up.HTTPCalloutSuccess, `{"active":true,"sub":"alice"}`)
	require.Equal(t, 0, code)
	require.Equal(t, "alice", sub)
	require.Nil(t, errBody)
}

func TestCheckIntrospection_inactive(t *testing.T) {
	code, sub, errBody := CheckIntrospection(up.HTTPCalloutSuccess, `{"active":false}`)
	require.Equal(t, 401, code)
	require.Equal(t, "", sub)
	require.NotNil(t, errBody)
}

func TestCheckIntrospection_badJSON(t *testing.T) {
	code, sub, errBody := CheckIntrospection(up.HTTPCalloutSuccess, "garbage")
	require.Equal(t, 401, code)
	require.Equal(t, "", sub)
	require.NotNil(t, errBody)
}

func TestCheckIntrospection_calloutFailed(t *testing.T) {
	code, sub, errBody := CheckIntrospection(up.HTTPCalloutReset, "")
	require.Equal(t, 401, code)
	require.Equal(t, "", sub)
	require.NotNil(t, errBody)
}
