package server

// egress_serve_local_remote.go — connected-mode support for "orange egress serve --local".
//
// When ORANGE_SERVER_URL / --server-url is set, the local stack (redis + RLS +
// Envoy) runs exactly as in standalone mode, but instead of watching a local
// orange.yaml the config is fetched from the remote orange server via the
// egress bundle credentials. A temp file bridges the remote snapshot to the
// existing file-watcher hot-reload path.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dio/transit/examples/orange/internal/client"
	orangeconfig "github.com/dio/transit/examples/orange/internal/config"
)

// setupRemoteSnapshot loads the egress bundle, fetches the initial config
// snapshot from the orange server, writes it to a temp yaml file, sets
// opts.configPath to that file, and starts a background poller that rewrites
// the file whenever the server publishes a new snapshot.
//
// Returns the temp file path so the caller can defer os.Remove(tmpPath).
func setupRemoteSnapshot(ctx context.Context, opts *localServeOpts) (string, error) {
	bundle, err := client.LoadBundle(opts.bundlePath)
	if err != nil {
		return "", fmt.Errorf("load bundle %q: %w", opts.bundlePath, err)
	}
	// ORANGE_SERVER_URL overrides the server URL baked into the bundle.
	bundle.ServerURL = opts.serverURL

	clnt, err := client.NewClient(bundle)
	if err != nil {
		return "", fmt.Errorf("create orange client: %w", err)
	}

	_, raw, ok, err := clnt.Fetch(ctx)
	if err != nil {
		return "", fmt.Errorf("initial snapshot fetch from %s: %w", opts.serverURL, err)
	}
	if !ok {
		return "", fmt.Errorf("server %s returned no snapshot for workspace %s; ensure config is published", opts.serverURL, bundle.WorkspaceID)
	}

	// Write snapshot as yaml to a temp file that Envoy and RLS will read.
	tmpFile, err := os.CreateTemp("", "orange-snapshot-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create temp snapshot file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if err := marshalRawConfigYAML(tmpFile, raw); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write initial snapshot: %w", err)
	}
	tmpFile.Close()

	// Redirect opts to the temp file; callers (RLS snapshotFn, Envoy env var)
	// read from opts.configPath and will now use the remote-backed temp file.
	opts.configPath = tmpPath

	fmt.Fprintf(os.Stderr, "egress:local  mode=connected  server=%s\n", opts.serverURL)
	fmt.Fprintf(os.Stderr, "egress:local  snapshot=%s\n", filepath.Base(tmpPath))

	// Background poller: rewrite temp file and let the existing WatchFile
	// goroutine (started later) detect the change and trigger provider.Reload.
	go func() {
		ticker := time.NewTicker(opts.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, newRaw, changed, err := clnt.Fetch(ctx)
				if err != nil || !changed {
					continue
				}
				if err := rewriteRawConfigYAML(tmpPath, newRaw); err != nil {
					fmt.Fprintf(os.Stderr, "[remote] snapshot write failed: %v\n", err)
					continue
				}
				fmt.Fprintf(os.Stderr, "\n[remote] snapshot updated from %s\n", opts.serverURL)
			}
		}
	}()

	return tmpPath, nil
}

// marshalRawConfigYAML writes raw as YAML to w.
func marshalRawConfigYAML(w *os.File, raw *orangeconfig.RawConfig) error {
	data, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// rewriteRawConfigYAML atomically replaces path with the YAML encoding of raw.
func rewriteRawConfigYAML(path string, raw *orangeconfig.RawConfig) error {
	data, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	// Write to a sibling temp file then rename for atomic replacement.
	tmp, err := os.CreateTemp(filepath.Dir(path), "orange-snap-*.yaml")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), path)
}
