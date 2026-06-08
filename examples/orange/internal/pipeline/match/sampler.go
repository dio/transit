package match

import (
	"math/rand/v2"

	"github.com/dio/transit/examples/orange/internal/config"
)

// sampleSplitNew picks a child index from a *config.SplitConfig using the global
// math/rand/v2 source and the configured weight distribution.
func sampleSplitNew(s *config.SplitConfig) int {
	return sampleSplitNewFrom(s.Children, rand.IntN)
}

// sampleSplitNewFrom is the testable core for the new-system SplitConfig.
// randN(n) must return a uniformly distributed value in [0, n).
// When the total weight is zero or children is empty it returns a
// uniform random index rather than panicking.
func sampleSplitNewFrom(children []config.WeightedRoutingConfig, randN func(n int) int) int {
	if len(children) == 0 {
		return 0
	}
	total := 0
	for _, c := range children {
		total += c.Weight
	}
	if total <= 0 {
		return randN(len(children))
	}
	draw := randN(total)
	cumulative := 0
	for i, c := range children {
		cumulative += c.Weight
		if draw < cumulative {
			return i
		}
	}
	return len(children) - 1
}
