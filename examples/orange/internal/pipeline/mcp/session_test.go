package mcp

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecureSessionEnvelopeRoundTrip(t *testing.T) {
	c := newSessionCrypto("key")
	want := sessionEnvelope{
		Route:   "default",
		Subject: "user@example.com",
		Backends: []backendSession{
			{Backend: "kiwi", SessionID: "kiwi-session", Capabilities: capabilities{Tools: true, ToolsListChanged: true}},
			{Backend: "github", SessionID: "github-session", Capabilities: capabilities{Tools: true, Resources: true}},
		},
	}

	token, err := encodeSecureSessionID(c, want)
	require.NoError(t, err)
	assert.NotContains(t, token, "kiwi-session")
	assert.NotContains(t, token, "github-session")

	got, err := decodeSecureSessionID(c, token, "user@example.com")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestSecureSessionEnvelopeWrongSubject(t *testing.T) {
	c := newSessionCrypto("key")
	token, err := encodeSecureSessionID(c, sessionEnvelope{
		Route:    "default",
		Subject:  "alice",
		Backends: []backendSession{{Backend: "kiwi"}},
	})
	require.NoError(t, err)

	_, err = decodeSecureSessionID(c, token, "bob")
	require.ErrorContains(t, err, "subject mismatch")
}

func TestSecureSessionEnvelopeTamperRejected(t *testing.T) {
	c := newSessionCrypto("key")
	token, err := encodeSecureSessionID(c, sessionEnvelope{
		Route:    "default",
		Backends: []backendSession{{Backend: "kiwi"}},
	})
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(token)
	require.NoError(t, err)
	raw[len(raw)-1] ^= 0xff
	tampered := base64.RawURLEncoding.EncodeToString(raw)
	_, err = decodeSecureSessionID(c, tampered, "")
	require.Error(t, err)
}

func TestSecureSessionEnvelopeFallbackKey(t *testing.T) {
	old := newSessionCrypto("old")
	rotated := newSessionCrypto("new", "old")

	token, err := encodeSecureSessionID(old, sessionEnvelope{
		Route:    "default",
		Subject:  "alice",
		Backends: []backendSession{{Backend: "aws-knowledge"}},
	})
	require.NoError(t, err)

	got, err := decodeSecureSessionID(rotated, token, "alice")
	require.NoError(t, err)
	assert.Equal(t, "aws-knowledge", got.Backends[0].Backend)
}

func TestSecureLastEventIDRoundTrip(t *testing.T) {
	c := newSessionCrypto("key")
	token, err := encodeSecureLastEventID(c, eventEnvelope{Backend: "github", EventID: "evt-123"})
	require.NoError(t, err)
	assert.False(t, strings.Contains(token, "evt-123"))

	got, err := decodeSecureLastEventID(c, token)
	require.NoError(t, err)
	assert.Equal(t, eventEnvelope{Backend: "github", EventID: "evt-123"}, got)
}

func TestCapabilitiesRoundTrip(t *testing.T) {
	for _, tc := range []capabilities{
		{},
		{Tools: true},
		{Tools: true, ToolsListChanged: true},
		{Logging: true, Resources: true, ResourcesSubscribe: true},
		{Tools: true, Prompts: true, PromptsListChanged: true, Completions: true},
	} {
		assert.Equal(t, tc, decodeCapabilities(encodeCapabilities(tc)))
	}
}

func TestDecodeCapabilitiesInvalidDefaultsToAll(t *testing.T) {
	got := decodeCapabilities("not-hex")
	assert.True(t, got.Tools)
	assert.True(t, got.ToolsListChanged)
	assert.True(t, got.Prompts)
	assert.True(t, got.PromptsListChanged)
	assert.True(t, got.Logging)
	assert.True(t, got.Resources)
	assert.True(t, got.ResourcesListChanged)
	assert.True(t, got.ResourcesSubscribe)
	assert.True(t, got.Completions)
}
