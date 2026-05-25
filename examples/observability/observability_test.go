package observability_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	observability "github.com/dio/transit/examples/observability"
)

// TestExtensionName verifies the filter name constant is stable.
func TestExtensionName(t *testing.T) {
	require.Equal(t, "observability", observability.ExtensionName)
}

// TestExtractModel_present verifies that a non-empty model value is returned as-is.
func TestExtractModel_present(t *testing.T) {
	model, ok := observability.ModelFromHeader("gpt-4o")
	require.True(t, ok)
	require.Equal(t, "gpt-4o", model)
}

// TestExtractModel_absent verifies that an empty string produces (_, false).
func TestExtractModel_absent(t *testing.T) {
	model, ok := observability.ModelFromHeader("")
	require.False(t, ok)
	require.Equal(t, "", model)
}

// TestExtractModel_whitespace verifies that a whitespace-only value is treated as absent.
func TestExtractModel_whitespace(t *testing.T) {
	_, ok := observability.ModelFromHeader("   ")
	require.False(t, ok)
}
