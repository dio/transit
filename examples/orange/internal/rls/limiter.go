package rls

import (
	"context"
	"math/rand"
	"time"

	goredis "github.com/redis/go-redis/v9"

	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	rlsconfig "github.com/envoyproxy/ratelimit/src/config"
	rllimiter "github.com/envoyproxy/ratelimit/src/limiter"
	rlsstats "github.com/envoyproxy/ratelimit/src/stats"
	rlsutils "github.com/envoyproxy/ratelimit/src/utils"
)

// RateLimiter checks rate limits against Redis using pipelined INCRBY + EXPIRE.
// It uses envoyproxy/ratelimit's BaseRateLimiter for key generation and
// threshold logic, and go-redis for the actual pipeline execution.
type RateLimiter struct {
	client *goredis.Client
	base   *rllimiter.BaseRateLimiter
}

// NewRateLimiter creates a RateLimiter. sm is used only for internal counters
// that track hits/overlimit; pass newNoopManager() when metrics are not needed.
func NewRateLimiter(client *goredis.Client, sm rlsstats.Manager) *RateLimiter {
	return &RateLimiter{
		client: client,
		base: rllimiter.NewBaseRateLimit(
			stdTimeSource{},
			rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec
			0,   // no expiry jitter
			nil, // no local cache
			0.8, // near-limit ratio
			"",  // no cache key prefix
			sm,
		),
	}
}

// DoLimit evaluates all descriptors in the request against their configured
// limits and Redis counters. A nil entry in limits means no rule matched for
// that descriptor — always returns OK in that case.
func (r *RateLimiter) DoLimit(
	ctx context.Context,
	req *pb.RateLimitRequest,
	limits []*rlsconfig.RateLimit,
) []*pb.RateLimitResponse_DescriptorStatus {
	hitsAddends := rlsutils.GetHitsAddends(req)
	cacheKeys := r.base.GenerateCacheKeys(req, limits, hitsAddends)

	type entry struct {
		idx int
		cmd *goredis.IntCmd
	}
	entries := make([]entry, 0, len(cacheKeys))
	results := make([]uint64, len(cacheKeys))

	pipe := r.client.Pipeline()
	for i, key := range cacheKeys {
		if key.Key == "" {
			continue
		}
		ttl := time.Duration(rlsutils.UnitToDivider(limits[i].Limit.Unit)) * time.Second
		cmd := pipe.IncrBy(ctx, key.Key, int64(hitsAddends[i]))
		pipe.Expire(ctx, key.Key, ttl)
		entries = append(entries, entry{i, cmd})
	}
	if len(entries) > 0 {
		// Pipeline-level errors are non-fatal: a failed INCRBY leaves result at 0 → OK,
		// which is the safe fail-open behaviour for a rate limiter.
		_, _ = pipe.Exec(ctx)
	}
	for _, e := range entries {
		if v, err := e.cmd.Result(); err == nil {
			results[e.idx] = uint64(v)
		}
	}

	statuses := make([]*pb.RateLimitResponse_DescriptorStatus, len(limits))
	for i, key := range cacheKeys {
		after := results[i]
		info := rllimiter.NewRateLimitInfo(limits[i], after-hitsAddends[i], after, 0, 0)
		statuses[i] = r.base.GetResponseDescriptorStatus(key.Key, info, false, hitsAddends[i])
	}
	return statuses
}

type stdTimeSource struct{}

func (stdTimeSource) UnixNow() int64 { return time.Now().Unix() }
