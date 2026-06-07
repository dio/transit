package resources

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"
)

const (
	defaultFlushInterval = 60 * time.Second
	defaultOfflineAfter  = 90 * time.Second // 3 missed 30s heartbeats
)

// HeartbeatRegistry tracks live egress instances in memory and flushes
// online_status + last_seen_at to the DB in a single batch UPDATE per flush
// cycle. Heartbeats themselves do no I/O — only the background goroutine does.
//
// singleflight deduplicates the first-contact DB validation per egress_id so
// that a burst of simultaneous heartbeats for an unknown egress_id only issues
// one SELECT against the DB.
type HeartbeatRegistry struct {
	pool   *pgxpool.Pool
	logger *slog.Logger

	flushInterval time.Duration
	offlineAfter  time.Duration

	mu   sync.RWMutex
	seen map[string]time.Time // egress_id → last heartbeat time

	known sync.Map           // egress_id → struct{} (DB-validated)
	sf    singleflight.Group // per-egress_id DB validation dedup

	done chan struct{}
	wg   sync.WaitGroup
}

// NewHeartbeatRegistry creates a registry. Call Start to begin the flush loop.
func NewHeartbeatRegistry(pool *pgxpool.Pool, logger *slog.Logger) *HeartbeatRegistry {
	return &HeartbeatRegistry{
		pool:          pool,
		logger:        logger,
		flushInterval: defaultFlushInterval,
		offlineAfter:  defaultOfflineAfter,
		seen:          make(map[string]time.Time),
		done:          make(chan struct{}),
	}
}

// Start begins the background flush goroutine.
func (r *HeartbeatRegistry) Start() {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(r.flushInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				r.flush(context.Background())
			case <-r.done:
				r.flush(context.Background()) // final flush on shutdown
				return
			}
		}
	}()
}

// Stop signals the flush goroutine to do a final flush and exit.
func (r *HeartbeatRegistry) Stop() {
	close(r.done)
	r.wg.Wait()
}

// Record registers a heartbeat for egressID. If egressID has not been seen
// before, it is validated against the DB (deduplicated via singleflight).
// On success the timestamp is stored in memory; no DB write occurs here.
func (r *HeartbeatRegistry) Record(ctx context.Context, egressID string) error {
	if _, ok := r.known.Load(egressID); ok {
		r.store(egressID)
		return nil
	}

	// First contact: validate egress exists, then add to known set.
	_, err, _ := r.sf.Do(egressID, func() (any, error) {
		var exists bool
		if err := r.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM egresses WHERE egress_id = $1)`,
			egressID,
		).Scan(&exists); err != nil {
			return nil, fmt.Errorf("validate egress: %w", err)
		}
		if !exists {
			return nil, connect.NewError(connect.CodeNotFound,
				fmt.Errorf("egress %q not found", egressID))
		}
		r.known.Store(egressID, struct{}{})
		r.store(egressID)
		return nil, nil
	})
	return err
}

func (r *HeartbeatRegistry) store(egressID string) {
	r.mu.Lock()
	r.seen[egressID] = time.Now().UTC()
	r.mu.Unlock()
}

// flush snapshots the seen map and batch-updates the DB. It is safe to run
// concurrently across multiple CP replicas because:
//
//   - Online entries: written freely; last-write-wins on last_seen_at. Every
//     replica that received a heartbeat writes its timestamp — all writes are
//     correct and the most recent one wins.
//
//   - Offline entries: written conditionally. A replica only marks an egress
//     offline if the DB's last_seen_at is also stale (older than offlineAfter).
//     This prevents replica A from marking an egress offline that replica B has
//     been receiving heartbeats for.
func (r *HeartbeatRegistry) flush(ctx context.Context) {
	r.mu.RLock()
	if len(r.seen) == 0 {
		r.mu.RUnlock()
		return
	}
	snapshot := make(map[string]time.Time, len(r.seen))
	for k, v := range r.seen {
		snapshot[k] = v
	}
	r.mu.RUnlock()

	now := time.Now().UTC()

	var (
		onlineIDs   []string
		onlineTimes []time.Time
		offlineIDs  []string
	)
	for id, lastSeen := range snapshot {
		if now.Sub(lastSeen) > r.offlineAfter {
			offlineIDs = append(offlineIDs, id)
		} else {
			onlineIDs = append(onlineIDs, id)
			onlineTimes = append(onlineTimes, lastSeen)
		}
	}

	// Online entries: unconditional except for the 5s skip-write tolerance to
	// avoid hammering pages when nothing has changed.
	if len(onlineIDs) > 0 {
		const q = `
UPDATE egresses
SET    online_status = 'online',
       last_seen_at  = v.ts,
       updated_at    = now()
FROM   unnest($1::text[], $2::timestamptz[]) AS v(eid, ts)
WHERE  egresses.egress_id = v.eid
  AND  (egresses.online_status != 'online'
        OR egresses.last_seen_at IS NULL
        OR egresses.last_seen_at < v.ts - interval '5 seconds')`
		if _, err := r.pool.Exec(ctx, q, onlineIDs, onlineTimes); err != nil {
			r.logger.Error("heartbeat flush (online) failed", "err", err)
		}
	}

	// Offline entries: conditional on DB state so that a replica that missed
	// heartbeats doesn't overwrite a fresher timestamp written by another replica.
	if len(offlineIDs) > 0 {
		const q = `
UPDATE egresses
SET    online_status = 'offline',
       updated_at    = now()
WHERE  egress_id = ANY($1)
  AND  online_status != 'offline'
  AND  (last_seen_at IS NULL OR last_seen_at < now() - $2::interval)`
		if _, err := r.pool.Exec(ctx, q, offlineIDs, r.offlineAfter); err != nil {
			r.logger.Error("heartbeat flush (offline) failed", "err", err)
		}
	}

	r.logger.Debug("heartbeat flush", "online", len(onlineIDs), "offline", len(offlineIDs))
}
