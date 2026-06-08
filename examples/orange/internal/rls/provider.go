// Package rls is a minimal Envoy RateLimitService implementation that runs
// embedded inside egress. It polls orange CP for rate limit config (same client
// relationship as egress itself) and enforces limits against Redis.
package rls

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	gostats "github.com/lyft/gostats"

	rlsconfig "github.com/envoyproxy/ratelimit/src/config"
	rlsstats "github.com/envoyproxy/ratelimit/src/stats"
)

// Loader fetches the current rate limit config from any source.
// For production, wrap a gRPC call to orange CP. For in-process use, wrap
// appState.Snapshot directly. See OrangeLoader and HTTPLoader.
type Loader func(ctx context.Context) (rlsconfig.RateLimitConfig, error)

// PollProvider atomically swaps the current config on a ticker.
// Callers never block on a reload — they always read the last good snapshot.
type PollProvider struct {
	loader   Loader
	interval time.Duration
	current  atomic.Pointer[rlsconfig.RateLimitConfig]
}

// NewPollProvider creates a PollProvider. Call LoadOnce before Start.
func NewPollProvider(loader Loader, interval time.Duration) *PollProvider {
	return &PollProvider{loader: loader, interval: interval}
}

// LoadOnce performs one synchronous load. Must succeed before serving traffic.
func (p *PollProvider) LoadOnce(ctx context.Context) error {
	cfg, err := p.loader(ctx)
	if err != nil {
		return fmt.Errorf("rls: initial load: %w", err)
	}
	p.current.Store(&cfg)
	return nil
}

// Start runs the background reload loop until ctx is cancelled.
func (p *PollProvider) Start(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cfg, err := p.loader(ctx)
			if err != nil {
				slog.Error("rls: config reload failed", "err", err)
				continue
			}
			p.current.Store(&cfg)
			slog.Debug("rls: config reloaded")
		}
	}
}

// Current returns the most recently loaded config, or nil before LoadOnce.
func (p *PollProvider) Current() rlsconfig.RateLimitConfig {
	ptr := p.current.Load()
	if ptr == nil {
		return nil
	}
	return *ptr
}

// ── noop stats ─────────────────────────────────────────────────────────────

// noopManager satisfies rlsstats.Manager with a null sink. Used when building
// configs from code — we don't need metrics on the config parsing path.
type noopManager struct {
	store   gostats.Store
	counter gostats.Counter
}

func newNoopManager() rlsstats.Manager {
	store := gostats.NewStore(gostats.NewNullSink(), false)
	return &noopManager{store: store, counter: store.NewCounter("noop")}
}

func (m *noopManager) GetStatsStore() gostats.Store { return m.store }

func (m *noopManager) NewStats(key string) rlsstats.RateLimitStats {
	return rlsstats.RateLimitStats{
		Key:                     key,
		TotalHits:               m.counter,
		OverLimit:               m.counter,
		NearLimit:               m.counter,
		OverLimitWithLocalCache: m.counter,
		WithinLimit:             m.counter,
		ShadowMode:              m.counter,
	}
}

func (m *noopManager) NewDomainStats(domain string) rlsstats.DomainStats {
	return rlsstats.DomainStats{Key: domain, NotFound: m.counter}
}

func (m *noopManager) NewShouldRateLimitStats() rlsstats.ShouldRateLimitStats {
	return rlsstats.ShouldRateLimitStats{RedisError: m.counter, ServiceError: m.counter}
}

func (m *noopManager) NewServiceStats() rlsstats.ServiceStats {
	return rlsstats.ServiceStats{
		ConfigLoadSuccess: m.counter,
		ConfigLoadError:   m.counter,
		ShouldRateLimit:   m.NewShouldRateLimitStats(),
		GlobalShadowMode:  m.counter,
	}
}
