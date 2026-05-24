package wsproxy

import (
	"context"
	"log/slog"
	"time"
)

// recordActorSession logs a structured session-end record.
//
// When otel_endpoint is configured, this should also emit:
//   - ws_proxy_sessions_total (counter, labels: model, result)
//   - ws_proxy_session_duration_ms (distribution, labels: model, result)
//
// Full OTLP metric export requires github.com/dio/logging +
// github.com/tetratelabs/telemetry; use newActorObservability to wire them in.
// See OBSERVABILITY.md for the wiring pattern.
func recordActorSession(
	ctx context.Context,
	log *slog.Logger,
	path, model string,
	inputTokens, outputTokens uint32,
	elapsed time.Duration,
	err error,
) {
	if log == nil {
		log = slog.Default()
	}
	result := "ok"
	reason := ""
	if err != nil {
		result = "error"
		reason = err.Error()
	}
	log.InfoContext(ctx, "ws-proxy: session ended",
		"path", path,
		"model", model,
		"input_tokens", inputTokens,
		"output_tokens", outputTokens,
		"duration", elapsed.Round(time.Millisecond).String(),
		"result", result,
		"reason", reason,
	)
}
