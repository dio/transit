package wsproxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"
)

// sessionLogPath, if set, causes recordActorSession to append a JSON line per
// session to that file in addition to the slog output. Used by e2e tests to
// assert on structured session fields without parsing human-readable log text.
var (
	sessionLogPath string
	sessionLogMu   sync.Mutex // guards file append
)

// InitSessionLog enables JSON-line session logging to path.
// Called from Register() when WSPROXY_SESSION_LOG is set.
func InitSessionLog(path string) { sessionLogPath = path }

// SessionRecord is the JSON shape written to sessionLogPath per session.
// Exported so e2e can unmarshal and assert without string matching.
type SessionRecord struct {
	Path         string `json:"path"`
	Model        string `json:"model"`
	InputTokens  uint32 `json:"input_tokens"`
	OutputTokens uint32 `json:"output_tokens"`
	DurationMS   int64  `json:"duration_ms"`
	Result       string `json:"result"`
	Reason       string `json:"reason"`
}

// recordActorSession logs a structured session-end record via slog and, when
// sessionLogPath is set, appends a JSON line for test assertions.
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

	if sessionLogPath == "" {
		return
	}
	rec := SessionRecord{
		Path:         path,
		Model:        model,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		DurationMS:   elapsed.Milliseconds(),
		Result:       result,
		Reason:       reason,
	}
	line, err2 := json.Marshal(rec)
	if err2 != nil {
		return
	}
	sessionLogMu.Lock()
	defer sessionLogMu.Unlock()
	f, err2 := os.OpenFile(sessionLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err2 != nil {
		return
	}
	f.Write(append(line, '\n')) //nolint:errcheck
	f.Close()                   //nolint:errcheck
}
