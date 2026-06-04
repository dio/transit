package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/orange/internal/config"
)

func TestToolSelectorNilAllowsAll(t *testing.T) {
	var s *toolSelector
	assert.True(t, s.allows("anything"))
}

func TestToolSelectorDenyWinsExact(t *testing.T) {
	s, err := compileToolSelector("default", "github", config.MCPToolSelector{
		Include: []string{"search", "delete"},
		Exclude: []string{"delete"},
	})
	require.NoError(t, err)

	assert.True(t, s.allows("search"))
	assert.False(t, s.allows("delete"))
	assert.False(t, s.allows("other"))
}

func TestToolSelectorDenyWinsRegex(t *testing.T) {
	s, err := compileToolSelector("default", "aws-knowledge", config.MCPToolSelector{
		IncludeRegex: []string{"^aws____"},
		ExcludeRegex: []string{"delete|mutate"},
	})
	require.NoError(t, err)

	assert.True(t, s.allows("aws____read_documentation"))
	assert.False(t, s.allows("aws____delete_documentation"))
	assert.False(t, s.allows("github_search"))
}

func TestToolSelectorNoIncludesAllowsExceptExcludes(t *testing.T) {
	s, err := compileToolSelector("default", "kiwi", config.MCPToolSelector{
		Exclude: []string{"hidden"},
	})
	require.NoError(t, err)

	assert.True(t, s.allows("search-flight"))
	assert.False(t, s.allows("hidden"))
}

func TestToolSelectorBadRegex(t *testing.T) {
	_, err := compileToolSelector("default", "github", config.MCPToolSelector{
		IncludeRegex: []string{"("},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default")
	assert.Contains(t, err.Error(), "github")
}
