package rls

import (
	"context"
	"fmt"
	"math"
	"sort"

	rlsconfig "github.com/envoyproxy/ratelimit/src/config"
	rlsstats "github.com/envoyproxy/ratelimit/src/stats"
	"github.com/shopspring/decimal"

	orangeconfig "github.com/dio/transit/examples/orange/internal/config"
)

const (
	orangeDomain     = "orange"
	orangeConfigName = "orange-ratelimit"

	microUSDPerUSD = 1_000_000
)

var microUSDScale = decimal.NewFromInt(microUSDPerUSD)

// SnapshotFunc returns the current decoded orange config.
//
// For deployed rls: wrap egress.Client.Fetch.
// For in-process use (e.g. tests): return a RawConfig directly.
type SnapshotFunc func() (*orangeconfig.RawConfig, error)

// OrangeLoader returns a Loader backed by orange CP. On each poll it calls fn,
// translates the RawConfig into a RateLimitConfig (no YAML serialization),
// and atomically swaps the PollProvider's snapshot.
//
// The descriptor layout is:
//
//	key_id=<scope>
//	  dim=<dim>                ← wildcard-model rules (Models=["*"])
//	  model_id=<model>
//	    dim=<dim>              ← model-specific rules
func OrangeLoader(fn SnapshotFunc) Loader {
	nm := newNoopManager()
	return func(ctx context.Context) (rlsconfig.RateLimitConfig, error) {
		raw, err := fn()
		if err != nil {
			return nil, fmt.Errorf("orange CP snapshot: %w", err)
		}
		return translateSnapshot(raw, nm)
	}
}

func translateSnapshot(raw *orangeconfig.RawConfig, sm rlsstats.Manager) (cfg rlsconfig.RateLimitConfig, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("rls: translate snapshot: %v", r)
		}
	}()

	root := &rlsconfig.YamlRoot{Domain: orangeDomain}

	if raw != nil && len(raw.RateLimit.Policies) > 0 {
		scopes := make([]string, 0, len(raw.RateLimit.Policies))
		for k := range raw.RateLimit.Policies {
			scopes = append(scopes, k)
		}
		sort.Strings(scopes)

		for _, scope := range scopes {
			entries := raw.RateLimit.Policies[scope]
			// Expand tier references so scopeDescriptor sees fully resolved limits.
			expanded := make([]orangeconfig.RawRateLimitPolicyEntry, 0, len(entries))
			for _, e := range entries {
				if e.Rule != "" {
					if t, ok := raw.RateLimit.Tiers[e.Rule]; ok {
						e = applyTierToEntry(e, t)
					}
				}
				expanded = append(expanded, e)
			}
			if d := scopeDescriptor(scope, expanded); d != nil {
				root.Descriptors = append(root.Descriptors, *d)
			}
		}
	}

	cfg = rlsconfig.NewRateLimitConfigImpl(
		[]rlsconfig.RateLimitConfigToLoad{{Name: orangeConfigName, ConfigYaml: root}},
		sm,
		true,
	)
	return cfg, nil
}

// ── descriptor builder ────────────────────────────────────────────────────────

type dimKey struct {
	model string
	dim   string
}

type dimVal struct {
	unit string // "MINUTE" | "HOUR" | "DAY"
	rpu  uint32 // requests_per_unit; 0 means skip
}

func scopeDescriptor(scope string, rules []orangeconfig.RawRateLimitPolicyEntry) *rlsconfig.YamlDescriptor {
	dims := make(map[dimKey]dimVal)

	setMin := func(model, dim, unit string, rpu uint32) {
		if rpu == 0 {
			return
		}
		k := dimKey{model, dim}
		if prev, ok := dims[k]; !ok || rpu < prev.rpu {
			dims[k] = dimVal{unit, rpu}
		}
	}

	for _, r := range rules {
		if isWildcard(r.Models) {
			emitDims("", r, setMin)
		} else {
			for _, m := range r.Models {
				emitDims(m, r, setMin)
			}
		}
	}

	if len(dims) == 0 {
		return nil
	}

	byModel := make(map[string][]dimKey)
	for k := range dims {
		byModel[k.model] = append(byModel[k.model], k)
	}

	var children []rlsconfig.YamlDescriptor

	// Wildcard dims sit directly under key_id.
	if wkeys, ok := byModel[""]; ok {
		sort.Slice(wkeys, func(i, j int) bool { return wkeys[i].dim < wkeys[j].dim })
		for _, dk := range wkeys {
			children = append(children, dimDescriptor(dk.dim, dims[dk]))
		}
	}

	// Model-specific dims sit under an intermediate model_id descriptor.
	models := make([]string, 0, len(byModel))
	for m := range byModel {
		if m != "" {
			models = append(models, m)
		}
	}
	sort.Strings(models)

	for _, model := range models {
		mkeys := byModel[model]
		sort.Slice(mkeys, func(i, j int) bool { return mkeys[i].dim < mkeys[j].dim })
		var modelChildren []rlsconfig.YamlDescriptor
		for _, dk := range mkeys {
			modelChildren = append(modelChildren, dimDescriptor(dk.dim, dims[dk]))
		}
		children = append(children, rlsconfig.YamlDescriptor{
			Key:         "model_id",
			Value:       model,
			Descriptors: modelChildren,
		})
	}

	return &rlsconfig.YamlDescriptor{
		Key:         "key_id",
		Value:       scope,
		Descriptors: children,
	}
}

func dimDescriptor(dim string, v dimVal) rlsconfig.YamlDescriptor {
	return rlsconfig.YamlDescriptor{
		Key:   "dim",
		Value: dim,
		RateLimit: &rlsconfig.YamlRateLimit{
			RequestsPerUnit: v.rpu,
			Unit:            v.unit,
		},
	}
}

func isWildcard(models []string) bool {
	for _, m := range models {
		if m == "*" {
			return true
		}
	}
	return false
}

func emitDims(model string, r orangeconfig.RawRateLimitPolicyEntry, setMin func(model, dim, unit string, rpu uint32)) {
	setMin(model, "rpm", "MINUTE", intToUint32(r.RPM))
	setMin(model, "rph", "HOUR", intToUint32(r.RPH))
	setMin(model, "rpd", "DAY", intToUint32(r.RPD))
	setMin(model, "input_tpm", "MINUTE", intToUint32(r.InputTokensPerMinute))
	setMin(model, "input_tph", "HOUR", intToUint32(r.InputTokensPerHour))
	setMin(model, "input_tpd", "DAY", intToUint32(r.InputTokensPerDay))
	setMin(model, "output_tpm", "MINUTE", intToUint32(r.OutputTokensPerMinute))
	setMin(model, "output_tph", "HOUR", intToUint32(r.OutputTokensPerHour))
	setMin(model, "output_tpd", "DAY", intToUint32(r.OutputTokensPerDay))
	setMin(model, "cache_read_tph", "HOUR", intToUint32(r.CacheReadTokensPerHour))
	setMin(model, "cache_read_tpd", "DAY", intToUint32(r.CacheReadTokensPerDay))
	setMin(model, "cache_write_tph", "HOUR", intToUint32(r.CacheWriteTokensPerHour))
	setMin(model, "cache_write_tpd", "DAY", intToUint32(r.CacheWriteTokensPerDay))
	setMin(model, "usd_per_min", "MINUTE", usdToMicroUSD(r.USDPerMinute))
	setMin(model, "usd_per_hour", "HOUR", usdToMicroUSD(r.USDPerHour))
	setMin(model, "usd_per_day", "DAY", usdToMicroUSD(r.USDPerDay))
}

// applyTierToEntry returns entry with any zero/unset field filled from tier.
// Non-zero entry fields take precedence — entry overrides tier.
func applyTierToEntry(entry orangeconfig.RawRateLimitPolicyEntry, tier orangeconfig.RawRateLimitTier) orangeconfig.RawRateLimitPolicyEntry {
	if entry.USDPerMinute.IsZero() {
		entry.USDPerMinute = tier.USDPerMinute
	}
	if entry.USDPerHour.IsZero() {
		entry.USDPerHour = tier.USDPerHour
	}
	if entry.USDPerDay.IsZero() {
		entry.USDPerDay = tier.USDPerDay
	}
	if entry.RPM == 0 {
		entry.RPM = tier.RPM
	}
	if entry.RPH == 0 {
		entry.RPH = tier.RPH
	}
	if entry.RPD == 0 {
		entry.RPD = tier.RPD
	}
	if entry.InputTokensPerMinute == 0 {
		entry.InputTokensPerMinute = tier.InputTokensPerMinute
	}
	if entry.InputTokensPerHour == 0 {
		entry.InputTokensPerHour = tier.InputTokensPerHour
	}
	if entry.InputTokensPerDay == 0 {
		entry.InputTokensPerDay = tier.InputTokensPerDay
	}
	if entry.OutputTokensPerMinute == 0 {
		entry.OutputTokensPerMinute = tier.OutputTokensPerMinute
	}
	if entry.OutputTokensPerHour == 0 {
		entry.OutputTokensPerHour = tier.OutputTokensPerHour
	}
	if entry.OutputTokensPerDay == 0 {
		entry.OutputTokensPerDay = tier.OutputTokensPerDay
	}
	if entry.CacheReadTokensPerHour == 0 {
		entry.CacheReadTokensPerHour = tier.CacheReadTokensPerHour
	}
	if entry.CacheReadTokensPerDay == 0 {
		entry.CacheReadTokensPerDay = tier.CacheReadTokensPerDay
	}
	if entry.CacheWriteTokensPerHour == 0 {
		entry.CacheWriteTokensPerHour = tier.CacheWriteTokensPerHour
	}
	if entry.CacheWriteTokensPerDay == 0 {
		entry.CacheWriteTokensPerDay = tier.CacheWriteTokensPerDay
	}
	if entry.OnExceed == "" {
		entry.OnExceed = tier.OnExceed
	}
	return entry
}

func intToUint32(v int) uint32 {
	if v <= 0 {
		return 0
	}
	if v > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}

func usdToMicroUSD(usd decimal.Decimal) uint32 {
	if usd.IsZero() || usd.IsNegative() {
		return 0
	}
	mu := usd.Mul(microUSDScale)
	if mu.GreaterThan(decimal.NewFromInt(math.MaxUint32)) {
		return math.MaxUint32
	}
	return uint32(mu.IntPart())
}
