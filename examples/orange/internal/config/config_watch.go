package config

// config_watch.go — generic file-change watcher used by the Envoy module
// (config/loader) and the RLS local serve mode.
//
// WatchFile watches a single file for writes and calls onChange each time a
// change is detected. It tries fsnotify first; if that fails it falls back to
// 5 s mtime polling so the caller always gets notifications regardless of OS or
// filesystem limitations.
//
// The watcher runs until ctx is cancelled. onChange is called synchronously on
// the watcher goroutine; it must not block for long.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchFile watches path for writes (fsnotify + 5 s mtime fallback) and calls
// onChange each time the file is modified. Blocks until ctx is cancelled.
func WatchFile(ctx context.Context, path string, onChange func()) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("config: fsnotify init failed, falling back to polling", "path", path, "err", err)
		watchFilePolling(ctx, path, onChange)
		return
	}
	defer watcher.Close()

	// Watch the directory so renames/atomic writes (editor temp-file swaps) are
	// caught, not just in-place writes.
	dir := filepath.Dir(path)
	if err := watcher.Add(dir); err != nil {
		slog.Warn("config: fsnotify watch failed, falling back to polling", "dir", dir, "err", err)
		watchFilePolling(ctx, path, onChange)
		return
	}

	var lastMod time.Time
	if fi, err := os.Stat(path); err == nil {
		lastMod = fi.ModTime()
	}

	slog.Debug("config: watching with fsnotify", "path", path)

	// 5 s heartbeat catches any events fsnotify might miss.
	heartbeat := time.NewTicker(5 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case event, ok := <-watcher.Events:
			if !ok {
				slog.Warn("config: fsnotify events closed, switching to polling", "path", path)
				watchFilePolling(ctx, path, onChange)
				return
			}
			if event.Name != path {
				continue
			}
			if event.Op&fsnotify.Write == 0 && event.Op&fsnotify.Create == 0 {
				continue
			}
			fireIfModified(path, &lastMod, onChange)

		case err, ok := <-watcher.Errors:
			if !ok {
				slog.Warn("config: fsnotify errors closed, switching to polling", "path", path)
				watchFilePolling(ctx, path, onChange)
				return
			}
			slog.Warn("config: fsnotify error", "path", path, "err", err)

		case <-heartbeat.C:
			fireIfModified(path, &lastMod, onChange)
		}
	}
}

// watchFilePolling is the fallback when fsnotify is unavailable.
func watchFilePolling(ctx context.Context, path string, onChange func()) {
	var lastMod time.Time
	if fi, err := os.Stat(path); err == nil {
		lastMod = fi.ModTime()
	}

	slog.Debug("config: watching with polling", "path", path, "interval", "5s")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fireIfModified(path, &lastMod, onChange)
		}
	}
}

// fireIfModified calls onChange when path's mtime has advanced past lastMod.
func fireIfModified(path string, lastMod *time.Time, onChange func()) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if !fi.ModTime().After(*lastMod) {
		return
	}
	*lastMod = fi.ModTime()
	onChange()
}
