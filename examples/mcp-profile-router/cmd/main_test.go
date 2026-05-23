package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseServersEnabledTools(t *testing.T) {
	servers, err := parseServers("aws=http://127.0.0.1:8081=aws=Bearer aws-token;tools=read|search,kiwi=http://127.0.0.1:8082=kiwi=Bearer kiwi-token;tools=flight")
	require.NoError(t, err)

	require.Equal(t, "http://127.0.0.1:8081", servers["aws"].URL)
	require.Equal(t, "Bearer aws-token", servers["aws"].Credential)
	require.Equal(t, map[string]bool{
		"read":   true,
		"search": true,
	}, servers["aws"].EnabledTools)
	require.Equal(t, map[string]bool{
		"flight": true,
	}, servers["kiwi"].EnabledTools)
}

func TestParseServersPreservesEqualsInCredential(t *testing.T) {
	servers, err := parseServers("aws=http://127.0.0.1:8081=aws=Bearer abc==")
	require.NoError(t, err)

	require.Equal(t, "Bearer abc==", servers["aws"].Credential)
	require.Nil(t, servers["aws"].EnabledTools)
}
