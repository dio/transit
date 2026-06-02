package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadFile_minimal(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-anthropic")

	cfg, err := LoadFile("../orange.yaml")
	require.NoError(t, err, "LoadFile")
	require.Equal(t, "https://api.openai.com", cfg.Providers["openai_direct"].Endpoint)
	require.Equal(t, "2023-06-01", cfg.Providers["anthropic_direct"].AnthropicVersion)
	require.Equal(t, "sk-test-openai", cfg.ProviderSecret("openai_direct"))
	require.Equal(t, "sk-test-anthropic", cfg.ProviderSecret("anthropic_direct"))
	// Defaults applied.
	require.Equal(t, "model", cfg.Classify.ModelField)
	require.Equal(t, "orange.upstream", cfg.Hostpick.UpstreamKey)
}

func TestLoadFile_missingEnvVar(t *testing.T) {
	// OPENAI_API_KEY deliberately unset.
	t.Setenv("OPENAI_API_KEY", "")
	os.Unsetenv("OPENAI_API_KEY")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-anthropic")

	_, err := LoadFile("../orange.yaml")
	require.Error(t, err, "expected error when OPENAI_API_KEY is unset")
}

func TestLookupModel(t *testing.T) {
	cfg := &Config{Models: []ModelMatch{
		{Match: "gpt-4o*", Provider: "openai_direct"},
		{Match: "claude-*", Provider: "anthropic_direct"},
	}}
	cases := []struct {
		in, want string
	}{
		{"gpt-4o-mini", "openai_direct"},
		{"gpt-4o", "openai_direct"},
		{"claude-sonnet-4-5", "anthropic_direct"},
		{"gemini-1.5", ""},
		{"", ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, cfg.LookupModel(tc.in), "LookupModel(%q)", tc.in)
	}
}
