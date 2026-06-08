// File-based bootstrap: loads orange config from ORANGE_CONFIG environment variable.
//
// When ORANGE_CONFIG is set, init() reads the YAML file at startup, compiles it
// into an AppState, and distributes it to all pipeline packages (pick, match, adapt,
// mcp, responsesws). This enables `make demo` to run Envoy without a control plane.
//
// Blank-import the loader package from cmd/module/main.go to enable the file-based
// path:
//
//	import _ "github.com/dio/transit/examples/orange/internal/config/loader"
//
// If ORANGE_CONFIG is not set the loader is silent; pipeline packages will
// block or return errors until a remote CP pushes a snapshot via Fetch.
//
// Reloads: a background goroutine watches the file using fsnotify for instant
// change detection, with a 5-second fallback to mtime polling if fsnotify fails.
// When a change is detected it reloads and redistributes to all packages so
// that `make demo` picks up edits without restarting Envoy.
package loader

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/observability"
	"github.com/dio/transit/examples/orange/internal/pipeline/adapt"
	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/examples/orange/internal/pipeline/mcp"
	"github.com/dio/transit/examples/orange/internal/pipeline/pick"
	"github.com/dio/transit/examples/orange/internal/pipeline/responsesws"
)

var log *slog.Logger = observability.Logger("orange/config")

func init() {
	path := os.Getenv("ORANGE_CONFIG")
	if path == "" {
		return
	}

	state := config.NewAppState()
	resolver := config.NewDefaultResolver(5 * time.Minute)

	if err := loadFile(state, path); err != nil {
		log.Error("failed to load ORANGE_CONFIG", "path", path, "err", err)
		os.Exit(1)
	}

	distribute(state, resolver)
	log.Info("config loaded and distributed to all packages", "path", path)

	go watchFile(path, state, resolver)
}

// distribute pushes state+resolver to every pipeline package.
func distribute(state *config.AppState, resolver config.SecretResolver) {
	pick.SetAppState(state)
	match.SetAppState(state)
	adapt.SetAppState(state, resolver)
	mcp.SetAppState(state, resolver)
	responsesws.SetAppState(state, resolver)
}

// loadFile reads path and applies it to state.
func loadFile(state *config.AppState, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return state.LoadConfig(data)
}

// watchFile watches path for changes and reloads state on each change.
// Uses fsnotify with a 5-second fallback to polling if fsnotify fails.
func watchFile(path string, state *config.AppState, resolver config.SecretResolver) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Warn("fsnotify init failed, falling back to polling", "err", err)
		watchFilePolling(path, state, resolver)
		return
	}
	defer watcher.Close()

	dir := filepath.Dir(path)
	if err := watcher.Add(dir); err != nil {
		log.Warn("fsnotify add dir failed, falling back to polling", "dir", dir, "err", err)
		watchFilePolling(path, state, resolver)
		return
	}

	var lastMod time.Time
	if fi, err := os.Stat(path); err == nil {
		lastMod = fi.ModTime()
	}

	log.Debug("watching config file with fsnotify", "path", path)

	// Fallback ticker in case fsnotify stops working
	fallbackTicker := time.NewTicker(5 * time.Second)
	defer fallbackTicker.Stop()

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				log.Warn("fsnotify events closed, switching to polling")
				watchFilePolling(path, state, resolver)
				return
			}
			// Only care about the config file itself
			if event.Name != path {
				continue
			}
			if event.Op&fsnotify.Write == 0 && event.Op&fsnotify.Create == 0 {
				continue
			}
			reloadIfModified(path, &lastMod, state, resolver)

		case err, ok := <-watcher.Errors:
			if !ok {
				log.Warn("fsnotify errors closed, switching to polling")
				watchFilePolling(path, state, resolver)
				return
			}
			log.Warn("fsnotify error, continuing", "err", err)

		case <-fallbackTicker.C:
			// Periodically check if file was modified (catch changes fsnotify might miss)
			reloadIfModified(path, &lastMod, state, resolver)
		}
	}
}

// watchFilePolling polls path for mtime changes and reloads state on each change.
func watchFilePolling(path string, state *config.AppState, resolver config.SecretResolver) {
	var lastMod time.Time
	if fi, err := os.Stat(path); err == nil {
		lastMod = fi.ModTime()
	}

	log.Debug("watching config file with polling", "path", path, "interval", "5s")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		reloadIfModified(path, &lastMod, state, resolver)
	}
}

// reloadIfModified checks if the file was modified and reloads if necessary.
func reloadIfModified(path string, lastMod *time.Time, state *config.AppState, resolver config.SecretResolver) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if !fi.ModTime().After(*lastMod) {
		return
	}
	*lastMod = fi.ModTime()
	if err := loadFile(state, path); err != nil {
		log.Warn("reload failed", "path", path, "err", err)
		return
	}
	distribute(state, resolver)
	log.Info("config reloaded", "path", path)
}
