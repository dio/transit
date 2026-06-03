package config

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
// [TIMESTAMP][PID][LEVEL][COMPONENT] [source/file:LINE] message
type EnvoyHandler struct {
	w io.Writer
}

// NewEnvoyHandler creates a handler that writes Envoy-formatted logs to w.
func NewEnvoyHandler(w io.Writer) *EnvoyHandler {
	return &EnvoyHandler{w: w}
}

// Handle formats and writes a log record.
func (h *EnvoyHandler) Handle(_ context.Context, r slog.Record) error {
	// Format: [TIMESTAMP][PID][LEVEL][COMPONENT] [source/file:LINE] message
	ts := r.Time.Format("2006-01-02 15:04:05.000")
	pid := os.Getpid()
	level := r.Level.String()
	if level == "INFO" {
		level = "info"
	} else if level == "WARN" {
		level = "warn"
	} else if level == "ERROR" {
		level = "error"
	} else if level == "DEBUG" {
		level = "debug"
	}

	// Collect attributes: extract component, build message suffix.
	component := "orange"
	var attrs []string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "component" {
			component = a.Value.String()
		} else {
			attrs = append(attrs, fmt.Sprintf("%s=%v", a.Key, a.Value))
		}
		return true
	})

	// Get source location.
	var file string
	var line int
	fs := runtime.CallersFrames([]uintptr{r.PC})
	f, _ := fs.Next()
	if f.File != "" {
		file = filepath.Base(f.File)
		line = f.Line
	}

	// Build final message: message + attrs.
	msg := r.Message
	for _, attr := range attrs {
		msg = fmt.Sprintf("%s %s", msg, attr)
	}

	// Write: [TIMESTAMP][PID][LEVEL][COMPONENT] [source/file:LINE] message
	fmt.Fprintf(h.w, "[%s][%d][%s][%s]", ts, pid, level, component)
	if file != "" {
		fmt.Fprintf(h.w, " [%s:%d]", file, line)
	}
	fmt.Fprintf(h.w, " %s\n", msg)
	return nil
}

// Enabled reports whether the handler handles the given level.
func (h *EnvoyHandler) Enabled(_ context.Context, level slog.Level) bool {
	return true
}

// WithAttrs returns a handler with the given attributes attached.
func (h *EnvoyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// For simplicity, ignore attrs; they're typically metadata.
	return h
}

// WithGroup returns a handler with the given group name.
func (h *EnvoyHandler) WithGroup(name string) slog.Handler {
	return h
}
