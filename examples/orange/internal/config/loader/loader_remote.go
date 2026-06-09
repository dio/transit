package loader

// loader_remote.go — CP-connected bootstrap for liborange.so.
//
// When ORANGE_EGRESS_BUNDLE and ORANGE_SERVER_URL are both set, this loader
// authenticates to the orange management plane using bundle credentials and
// polls SnapshotService.Fetch via client.Watcher. The first tick fires
// immediately so pipeline packages receive config before Envoy starts serving
// traffic. Subsequent ticks run at ORANGE_POLL_INTERVAL (default 30s).
//
// Interaction with loader_file.go:
//   If ORANGE_CONFIG is also set, loader_file.go handles the initial load;
//   this loader then supersedes it on the first successful Fetch and owns all
//   subsequent updates.
//   If ORANGE_CONFIG is not set, this loader is the sole config source.

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/dio/transit/examples/orange/internal/client"
	"github.com/dio/transit/examples/orange/internal/config"
)

func init() {
	bundlePath := os.Getenv("ORANGE_EGRESS_BUNDLE")
	serverURL := os.Getenv("ORANGE_SERVER_URL")
	if bundlePath == "" || serverURL == "" {
		return
	}

	bundle, err := client.LoadBundle(bundlePath)
	if err != nil {
		log.Error("remote loader: cannot load egress bundle", "path", bundlePath, "err", err)
		os.Exit(1)
	}
	bundle.ServerURL = serverURL

	clnt, err := client.NewClient(bundle)
	if err != nil {
		log.Error("remote loader: cannot create CP client", "err", err)
		os.Exit(1)
	}

	state := config.NewAppState()
	watcher := client.NewWatcher(clnt)

	notifyFn := func(version uint64) {
		st := watcher.ReadSnapshot()
		if st.Snap == nil {
			return
		}
		if err := state.LoadConfig(st.Snap.GetPayload()); err != nil {
			log.Warn("remote loader: snapshot load failed", "version", version, "err", err)
			return
		}
		distribute(state, clnt.Resolver)
		log.Info("remote loader: snapshot distributed", "server", serverURL, "version", version)
	}

	interval := remotePollInterval()
	log.Info("remote loader: starting background poll", "server", serverURL, "interval", interval)
	watcher.StartPoll(context.Background(), interval, notifyFn)
}

// remotePollInterval parses ORANGE_POLL_INTERVAL as a Go duration string.
// Falls back to 30s when unset or unparseable.
func remotePollInterval() time.Duration {
	if s := os.Getenv("ORANGE_POLL_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
		fmt.Fprintf(os.Stderr, "remote loader: invalid ORANGE_POLL_INTERVAL %q; using 30s\n", s)
	}
	return 30 * time.Second
}
