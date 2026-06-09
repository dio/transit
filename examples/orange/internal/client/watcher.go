package client

// watcher.go — shared poll state for CP-connected components.
//
// Watcher holds the latest config snapshot, poll lifecycle state, and a
// short change-history ring buffer. A single background goroutine calls tick()
// on every interval tick; callers read state under the read lock.
//
// The design separates transport (Client) from state (Watcher): the loader
// package uses Watcher.StartPoll to keep liborange.so live-updated from the
// CP, while the egress emulator REPL uses it for interactive introspection.
// Components that need only raw Fetch semantics use Client directly.

import (
	"context"
	"sync"
	"time"

	configv1 "github.com/dio/transit/examples/orange/api/orange/config/v1"
	"github.com/dio/transit/examples/orange/internal/config"
)

const watcherMaxHistory = 20

// ChangeEntry records one config snapshot receipt in the history ring buffer.
type ChangeEntry struct {
	ReceivedAt time.Time
	Version    uint64
	Checksum   []byte
	Source     string // always "fetch" for now; "watch" once streaming lands
	Providers  int
	Servers    int
	Profiles   int
}

// Watcher holds live CP↔egress state shared between the poll goroutine
// and REPL commands.
type Watcher struct {
	mu sync.RWMutex

	snap         *configv1.SnapshotEnvelope
	raw          *config.RawConfig
	lastVersion  uint64
	lastChecksum []byte

	pollStatus    string // "idle", "polling", "error", "stopped"
	pollErr       error
	lastHeartbeat time.Time
	lastFetch     time.Time

	history []ChangeEntry

	client *Client
}

// NewWatcher creates a Watcher backed by client.
func NewWatcher(client *Client) *Watcher {
	return &Watcher{
		client:     client,
		pollStatus: "idle",
	}
}

// Client returns the underlying transport client (for direct Resolver access).
func (w *Watcher) Client() *Client { return w.client }

// StartPoll launches the background poll goroutine and returns immediately.
// notifyFn is called from the poll goroutine after each state swap when a new
// snapshot arrives; it receives the new version number. Pass nil to disable.
func (w *Watcher) StartPoll(ctx context.Context, interval time.Duration, notifyFn func(uint64)) {
	go w.pollLoop(ctx, interval, notifyFn)
}

func (w *Watcher) pollLoop(ctx context.Context, interval time.Duration, notifyFn func(uint64)) {
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

func (w *Watcher) tick(ctx context.Context, notifyFn func(uint64)) {
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
func (w *Watcher) Heartbeat(ctx context.Context) error {
	if _, err := w.client.Heartbeat(ctx); err != nil {
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
func (w *Watcher) Fetch(ctx context.Context) (bool, error) {
	return w.fetch(ctx, nil)
}

func (w *Watcher) fetch(ctx context.Context, notifyFn func(uint64)) (bool, error) {
	snap, raw, changed, err := w.client.Fetch(ctx)
	if err != nil {
		return false, err
	}

	if !changed {
		w.mu.Lock()
		w.lastFetch = time.Now()
		w.mu.Unlock()
		return false, nil
	}

	entry := ChangeEntry{
		ReceivedAt: time.Now(),
		Version:    snap.GetVersion(),
		Checksum:   snap.GetChecksum(),
		Source:     "fetch",
		Providers:  len(raw.LLM.Providers),
		Servers:    len(raw.MCP.Servers),
		Profiles:   len(raw.Profiles),
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
func (w *Watcher) appendHistory(e ChangeEntry) {
	if len(w.history) >= watcherMaxHistory {
		w.history = w.history[1:]
	}
	w.history = append(w.history, e)
}

func (w *Watcher) setStatus(status string, err error) {
	w.mu.Lock()
	w.pollStatus = status
	w.pollErr = err
	w.mu.Unlock()
}

// WatcherSnapshot is a read-locked copy of watcher state for use in REPL commands.
type WatcherSnapshot struct {
	Snap          *configv1.SnapshotEnvelope
	Raw           *config.RawConfig
	LastVersion   uint64
	LastChecksum  []byte
	PollStatus    string
	PollErr       error
	LastHeartbeat time.Time
	LastFetch     time.Time
}

// ReadSnapshot returns a consistent read-locked copy of the current watcher state.
func (w *Watcher) ReadSnapshot() WatcherSnapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return WatcherSnapshot{
		Snap:          w.snap,
		Raw:           w.raw,
		LastVersion:   w.lastVersion,
		LastChecksum:  w.lastChecksum,
		PollStatus:    w.pollStatus,
		PollErr:       w.pollErr,
		LastHeartbeat: w.lastHeartbeat,
		LastFetch:     w.lastFetch,
	}
}

// HistoryEntries returns a copy of the change history.
func (w *Watcher) HistoryEntries() []ChangeEntry {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]ChangeEntry, len(w.history))
	copy(out, w.history)
	return out
}

// PromptFields returns the minimal prompt state under a read lock.
func (w *Watcher) PromptFields() (version uint64, status string, hasSnap bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.lastVersion, w.pollStatus, w.snap != nil
}
