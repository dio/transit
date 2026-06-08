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

func TestToolSelectorIncludeAllowsListed(t *testing.T) {
	s, err := compileToolSelector("default", "github", config.ToolFilter{
		ServerID: "github",
		Include:  []string{"search", "create"},
	})
	require.NoError(t, err)

	assert.True(t, s.allows("search"))
	assert.True(t, s.allows("create"))
	assert.False(t, s.allows("delete"))
	assert.False(t, s.allows("other"))
}

func TestToolSelectorEmptyIncludeAllowsAll(t *testing.T) {
	s, err := compileToolSelector("default", "kiwi", config.ToolFilter{
		ServerID: "kiwi",
	})
	require.NoError(t, err)

	assert.True(t, s.allows("search-flight"))
	assert.True(t, s.allows("hidden"))
	assert.True(t, s.allows("anything"))
}

func TestToolSelectorSingleInclude(t *testing.T) {
	s, err := compileToolSelector("default", "aws-knowledge", config.ToolFilter{
		ServerID: "aws-knowledge",
		Include:  []string{"read_documentation"},
	})
	require.NoError(t, err)

	assert.True(t, s.allows("read_documentation"))
	assert.False(t, s.allows("delete_documentation"))
	assert.False(t, s.allows("github_search"))
}
