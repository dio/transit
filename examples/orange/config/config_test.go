package config

import (
	"os"
	"testing"
)

func TestLoadFile_minimal(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-anthropic")

	cfg, err := LoadFile("../orange.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := cfg.Upstreams["openai_direct"].Endpoint; got != "https://api.openai.com" {
		t.Fatalf("openai endpoint: %q", got)
	}
	if got := cfg.Upstreams["anthropic_direct"].AnthropicVersion; got != "2023-06-01" {
		t.Fatalf("anthropic version: %q", got)
	}
	if got := cfg.UpstreamSecret("openai_direct"); got != "sk-test-openai" {
		t.Fatalf("openai secret: %q", got)
	}
	if got := cfg.UpstreamSecret("anthropic_direct"); got != "sk-test-anthropic" {
		t.Fatalf("anthropic secret: %q", got)
	}
	// Defaults applied.
	if cfg.Classify.ModelField != "model" {
		t.Fatalf("model_field default: %q", cfg.Classify.ModelField)
	}
	if cfg.Hostpick.UpstreamKey != "orange.upstream" {
		t.Fatalf("upstream_key default: %q", cfg.Hostpick.UpstreamKey)
	}
}

func TestLoadFile_missingEnvVar(t *testing.T) {
	// OPENAI_API_KEY deliberately unset.
	t.Setenv("OPENAI_API_KEY", "")
	os.Unsetenv("OPENAI_API_KEY")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-anthropic")

	_, err := LoadFile("../orange.yaml")
	if err == nil {
		t.Fatal("expected error when OPENAI_API_KEY is unset")
	}
}

func TestLookupModel(t *testing.T) {
	cfg := &Config{Models: []ModelMatch{
		{Match: "gpt-4o*", Upstream: "openai_direct"},
		{Match: "claude-*", Upstream: "anthropic_direct"},
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
		if got := cfg.LookupModel(tc.in); got != tc.want {
			t.Errorf("LookupModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
