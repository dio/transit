package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// EnvoyHandler is an slog.Handler that formats logs like Envoy does:
// [TIMESTAMP][PID][LEVEL][COMPONENT] [file.go:LINE] message key=value
type EnvoyHandler struct {
	w        io.Writer
	minLevel slog.Level
	attrs    []slog.Attr
}

const LevelTrace = slog.LevelDebug - 4

// NewEnvoyHandler creates a handler that writes Envoy-formatted logs to w.
func NewEnvoyHandler(w io.Writer) *EnvoyHandler {
	return NewEnvoyHandlerWithLevel(w, slog.LevelInfo)
}

// NewEnvoyHandlerWithLevel creates a handler with an explicit minimum level.
func NewEnvoyHandlerWithLevel(w io.Writer, minLevel slog.Level) *EnvoyHandler {
	return &EnvoyHandler{w: w, minLevel: minLevel}
}

// Logger returns an Orange component logger using Envoy-style formatting.
// Use this for background/config/cluster/sidecar code. Request-path filters
// should use up.Writer.Slog so stream-scoped attrs stay attached.
func Logger(component string) *slog.Logger {
	if component == "" {
		component = "orange"
	}
	return slog.New(NewEnvoyHandlerWithLevel(os.Stderr, envLogLevel())).With("component", component)
}

// Handle formats and writes a log record.
func (h *EnvoyHandler) Handle(_ context.Context, r slog.Record) error {
	ts := r.Time.Format("2006-01-02 15:04:05.000")
	pid := os.Getpid()
	level := r.Level.String()
	if r.Level == LevelTrace {
		level = "trace"
	} else if level == "INFO" {
		level = "info"
	} else if level == "WARN" {
		level = "warn"
	} else if level == "ERROR" {
		level = "error"
	} else if level == "DEBUG" {
		level = "debug"
	}

	component := "orange"
	var attrs []string
	collect := func(a slog.Attr) {
		a.Value = a.Value.Resolve()
		if a.Key == "component" {
			component = a.Value.String()
		} else {
			attrs = append(attrs, fmt.Sprintf("%s=%v", a.Key, a.Value))
		}
	}
	for _, a := range h.attrs {
		collect(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		collect(a)
		return true
	})

	var file string
	var line int
	fs := runtime.CallersFrames([]uintptr{r.PC})
	f, _ := fs.Next()
	if f.File != "" {
		file = filepath.Base(f.File)
		line = f.Line
	}

	msg := r.Message
	for _, attr := range attrs {
		msg = fmt.Sprintf("%s %s", msg, attr)
	}

	_, _ = fmt.Fprintf(h.w, "[%s][%d][%s][%s]", ts, pid, level, component)
	if file != "" {
		_, _ = fmt.Fprintf(h.w, " [%s:%d]", file, line)
	}
	_, _ = fmt.Fprintf(h.w, " %s\n", msg)
	return nil
}

// Enabled reports whether the handler handles the given level.
func (h *EnvoyHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.minLevel
}

// WithAttrs returns a handler with the given attributes attached.
func (h *EnvoyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &EnvoyHandler{
		w:        h.w,
		minLevel: h.minLevel,
		attrs:    make([]slog.Attr, 0, len(h.attrs)+len(attrs)),
	}
	next.attrs = append(next.attrs, h.attrs...)
	next.attrs = append(next.attrs, attrs...)
	return next
}

func (h *EnvoyHandler) WithGroup(name string) slog.Handler {
	return h
}

func envLogLevel() slog.Level {
	switch os.Getenv("ORANGE_LOG_LEVEL") {
	case "trace", "TRACE":
		return LevelTrace
	case "debug", "DEBUG":
		return slog.LevelDebug
	case "warn", "WARN", "warning", "WARNING":
		return slog.LevelWarn
	case "error", "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
