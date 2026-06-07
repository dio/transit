package server

// egress_watcher.go — shared poll state for the egress emulator REPL.
//
// egressWatcher holds the latest config snapshot, poll lifecycle state, and a
// short change-history ring buffer. A single background goroutine calls tick()
// on every interval tick; REPL commands read state under the read lock.
//
// This file has no readline dependency so it can be extracted into the Envoy
// dynamic module package when needed. The dynamic module will replace pollLoop
// with a Watch stream; the state struct, state-swap logic, and Heartbeat/Fetch
// one-shots are unchanged.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect"

	configv1 "github.com/dio/transit/examples/orange/api/orange/config/v1"
	configv1connect "github.com/dio/transit/examples/orange/api/orange/config/v1/configv1connect"
	egressv1 "github.com/dio/transit/examples/orange/api/orange/egress/v1"
	egressv1connect "github.com/dio/transit/examples/orange/api/orange/egress/v1/egressv1connect"
	"github.com/dio/transit/examples/orange/internal/config"
)

const watcherMaxHistory = 20

// egressChangeEntry records one config snapshot receipt in the history ring buffer.
type egressChangeEntry struct {
	receivedAt time.Time
	version    uint64
	checksum   []byte
	source     string // always "fetch" for now; "watch" once streaming lands
	providers  int
	servers    int
	profiles   int
}

// egressWatcher holds live CP↔egress state shared between the poll goroutine
// and REPL commands.
type egressWatcher struct {
	mu sync.RWMutex

	snap         *configv1.SnapshotEnvelope
	raw          *config.RawConfig
	lastVersion  uint64
	lastChecksum []byte

	pollStatus    string // "idle", "polling", "error", "stopped"
	pollErr       error
	lastHeartbeat time.Time
	lastFetch     time.Time

	history []egressChangeEntry

	heartbeatClient egressv1connect.EgressServiceClient
	snapshotClient  configv1connect.SnapshotServiceClient
	resolver        *config.CachedResolver
}

func newEgressWatcher(
	heartbeatClient egressv1connect.EgressServiceClient,
	snapshotClient configv1connect.SnapshotServiceClient,
	resolver *config.CachedResolver,
) *egressWatcher {
	return &egressWatcher{
		heartbeatClient: heartbeatClient,
		snapshotClient:  snapshotClient,
		resolver:        resolver,
		pollStatus:      "idle",
	}
}

// startPoll launches the background poll goroutine and returns immediately.
// notifyFn is called from the poll goroutine after each state swap when a new
// snapshot arrives; it receives the new version number. Pass nil to disable.
func (w *egressWatcher) startPoll(ctx context.Context, interval time.Duration, notifyFn func(uint64)) {
	go w.pollLoop(ctx, interval, notifyFn)
}

func (w *egressWatcher) pollLoop(ctx context.Context, interval time.Duration, notifyFn func(uint64)) {
	w.setStatus("polling", nil)
	w.tick(ctx, notifyFn) // immediate first tick before the timer fires

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.setStatus("stopped", nil)
			return
		case <-ticker.C:
			w.tick(ctx, notifyFn)
		}
	}
}

func (w *egressWatcher) tick(ctx context.Context, notifyFn func(uint64)) {
	if err := w.Heartbeat(ctx); err != nil {
		w.setStatus("error", err)
		// Heartbeat failure is non-fatal: proceed to Fetch so the snapshot
		// stays current even when the heartbeat endpoint is temporarily slow.
	}
	if _, err := w.fetch(ctx, notifyFn); err != nil {
		w.setStatus("error", err)
		return
	}
	w.setStatus("polling", nil)
}

// Heartbeat sends a single Heartbeat RPC. Updates lastHeartbeat on success.
// Called both by the poll goroutine (via tick) and directly from the REPL
// 'heartbeat' command, independent of the poll timer.
func (w *egressWatcher) Heartbeat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := w.heartbeatClient.Heartbeat(ctx, connect.NewRequest(&egressv1.HeartbeatRequest{})); err != nil {
		return err
	}
	w.mu.Lock()
	w.lastHeartbeat = time.Now()
	w.mu.Unlock()
	return nil
}

// Fetch performs a one-shot Fetch RPC. Returns (true, nil) when a new snapshot
// arrived, (false, nil) for Unchanged, or (false, err) on failure.
// Called directly from the REPL 'fetch' command.
func (w *egressWatcher) Fetch(ctx context.Context) (bool, error) {
	return w.fetch(ctx, nil)
}

func (w *egressWatcher) fetch(ctx context.Context, notifyFn func(uint64)) (bool, error) {
	w.mu.RLock()
	lastVersion := w.lastVersion
	lastChecksum := w.lastChecksum
	w.mu.RUnlock()

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	resp, err := w.snapshotClient.Fetch(ctx, connect.NewRequest(&configv1.FetchRequest{
		LastVersion:  lastVersion,
		LastChecksum: lastChecksum,
	}))
	if err != nil {
		return false, err
	}

	if resp.Msg.GetUnchanged() != nil {
		w.mu.Lock()
		w.lastFetch = time.Now()
		w.mu.Unlock()
		return false, nil
	}

	snap := resp.Msg.GetSnapshot()
	if snap == nil {
		return false, nil
	}

	raw, err := config.DecodeRawFromProtoEnvelope(snap)
	if err != nil {
		return false, fmt.Errorf("decode snapshot: %w", err)
	}

	entry := egressChangeEntry{
		receivedAt: time.Now(),
		version:    snap.GetVersion(),
		checksum:   snap.GetChecksum(),
		source:     "fetch",
		providers:  len(raw.LLM.Providers),
		servers:    len(raw.MCP.Servers),
		profiles:   len(raw.Profiles),
	}

	w.mu.Lock()
	w.snap = snap
	w.raw = raw
	w.lastVersion = snap.GetVersion()
	w.lastChecksum = snap.GetChecksum()
	w.lastFetch = time.Now()
	w.appendHistory(entry)
	w.mu.Unlock()

	if notifyFn != nil {
		notifyFn(snap.GetVersion())
	}
	return true, nil
}

// appendHistory appends e to the ring buffer. Must be called under mu.Lock.
func (w *egressWatcher) appendHistory(e egressChangeEntry) {
	if len(w.history) >= watcherMaxHistory {
		w.history = w.history[1:]
	}
	w.history = append(w.history, e)
}

func (w *egressWatcher) setStatus(status string, err error) {
	w.mu.Lock()
	w.pollStatus = status
	w.pollErr = err
	w.mu.Unlock()
}

// egressSnapshot is a read-locked copy of watcher state for use in REPL commands.
type egressSnapshot struct {
	snap          *configv1.SnapshotEnvelope
	raw           *config.RawConfig
	lastVersion   uint64
	lastChecksum  []byte
	pollStatus    string
	pollErr       error
	lastHeartbeat time.Time
	lastFetch     time.Time
}

func (w *egressWatcher) readSnapshot() egressSnapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return egressSnapshot{
		snap:          w.snap,
		raw:           w.raw,
		lastVersion:   w.lastVersion,
		lastChecksum:  w.lastChecksum,
		pollStatus:    w.pollStatus,
		pollErr:       w.pollErr,
		lastHeartbeat: w.lastHeartbeat,
		lastFetch:     w.lastFetch,
	}
}

func (w *egressWatcher) historyEntries() []egressChangeEntry {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]egressChangeEntry, len(w.history))
	copy(out, w.history)
	return out
}

// promptFields returns the minimal prompt state under a read lock.
func (w *egressWatcher) promptFields() (version uint64, status string, hasSnap bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.lastVersion, w.pollStatus, w.snap != nil
}
