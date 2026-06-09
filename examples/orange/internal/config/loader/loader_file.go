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
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/dio/transit/examples/orange/internal/client"
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
	resolver := config.NewDefaultResolver(bundleHTTPClient(), os.Getenv("ORANGE_SERVER_URL"), 5*time.Minute)

	if err := loadFile(state, path); err != nil {
		log.Error("failed to load ORANGE_CONFIG", "path", path, "err", err)
		os.Exit(1)
	}

	distribute(state, resolver)
	log.Info("config loaded and distributed to all packages", "path", path)

	go config.WatchFile(context.Background(), path, func() {
		if err := loadFile(state, path); err != nil {
			log.Warn("reload failed", "path", path, "err", err)
			return
		}
		distribute(state, resolver)
		log.Info("config reloaded", "path", path)
	})
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

// bundleHTTPClient loads the egress bundle from ORANGE_EGRESS_BUNDLE and
// returns an assertion-authenticated HTTP client for the orange:// resolver.
// Returns nil when the env var is unset or the bundle cannot be loaded.
func bundleHTTPClient() *http.Client {
	bundlePath := os.Getenv("ORANGE_EGRESS_BUNDLE")
	if bundlePath == "" {
		return nil
	}
	bundle, err := client.LoadBundle(bundlePath)
	if err != nil {
		log.Warn("orange:// resolver disabled: cannot load bundle", "path", bundlePath, "err", err)
		return nil
	}
	if serverURL := os.Getenv("ORANGE_SERVER_URL"); serverURL != "" {
		bundle.ServerURL = serverURL
	}
	hc, err := client.NewHTTPClient(bundle)
	if err != nil {
		log.Warn("orange:// resolver disabled: cannot build HTTP client", "err", err)
		return nil
	}
	log.Info("orange:// resolver enabled", "server", bundle.ServerURL)
	return hc
}
