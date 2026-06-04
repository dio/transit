package observability

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvoyHandlerWithAttrsPreservesComponent(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewEnvoyHandler(&buf)).With("component", "orange/responsesws")

	logger.Info("sidecar ready", "addr", "127.0.0.1:10002")

	got := buf.String()
	require.Contains(t, got, "[info][orange/responsesws]")
	require.Contains(t, got, "sidecar ready addr=127.0.0.1:10002")
	require.False(t, strings.Contains(got, "component=orange/responsesws"))
}

func TestLoggerUsesOrangeComponent(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewEnvoyHandler(&buf)).With("component", "orange/pick")

	logger.Warn("upstream skipped", "upstream", "openai")

	got := buf.String()
	require.Contains(t, got, "[warn][orange/pick]")
	require.Contains(t, got, "upstream skipped upstream=openai")
}

func TestEnvoyHandlerFiltersBelowMinimumLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewEnvoyHandlerWithLevel(&buf, slog.LevelInfo)).With("component", "orange/responsesws")

	logger.Debug("frame forwarded")
	logger.Info("session started")

	got := buf.String()
	require.NotContains(t, got, "frame forwarded")
	require.Contains(t, got, "[info][orange/responsesws]")
	require.Contains(t, got, "session started")
}

func TestEnvoyHandlerTraceLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewEnvoyHandlerWithLevel(&buf, LevelTrace)).With("component", "orange/responsesws")

	logger.LogAttrs(t.Context(), LevelTrace, "waiting for frame", slog.String("session_id", "sid"))

	got := buf.String()
	require.Contains(t, got, "[trace][orange/responsesws]")
	require.Contains(t, got, "waiting for frame session_id=sid")
}
