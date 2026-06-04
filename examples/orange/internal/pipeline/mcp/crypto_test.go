package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionCryptoRoundTrip(t *testing.T) {
	c := newSessionCrypto("primary")

	token, err := c.Encrypt([]byte("plaintext"))
	require.NoError(t, err)

	got, err := c.Decrypt(token)
	require.NoError(t, err)
	assert.Equal(t, "plaintext", string(got))
}

func TestSessionCryptoSalted(t *testing.T) {
	c := newSessionCrypto("primary")

	a, err := c.Encrypt([]byte("same"))
	require.NoError(t, err)
	b, err := c.Encrypt([]byte("same"))
	require.NoError(t, err)

	assert.NotEqual(t, a, b)
}

func TestSessionCryptoWrongKey(t *testing.T) {
	token, err := newSessionCrypto("old").Encrypt([]byte("secret"))
	require.NoError(t, err)

	_, err = newSessionCrypto("new").Decrypt(token)
	require.Error(t, err)
}

func TestSessionCryptoFallbackDecrypt(t *testing.T) {
	old := newSessionCrypto("old")
	rotated := newSessionCrypto("new", "old")

	token, err := old.Encrypt([]byte("secret"))
	require.NoError(t, err)

	got, err := rotated.Decrypt(token)
	require.NoError(t, err)
	assert.Equal(t, "secret", string(got))

	newToken, err := rotated.Encrypt([]byte("new-secret"))
	require.NoError(t, err)
	_, err = old.Decrypt(newToken)
	require.Error(t, err)
}
