package match

import (
	"math/rand/v2"

	"github.com/dio/transit/examples/orange/internal/config"
)

// sampleSplit picks a child index from s.Children using the global
// math/rand/v2 source and the configured weight distribution.
// Weights are sampled proportionally to their values; they need not
// sum to exactly 100. Returns a uniform random index when all weights
// are zero or no children are present (degenerate config that should
// have been caught by validateRoutingNode).
func sampleSplit(s *config.SplitNode) int {
	return sampleSplitFrom(s.Children, rand.IntN)
}

// sampleSplitFrom is the testable core. randN(n) must return a
// uniformly distributed value in [0, n).
// When the total weight is zero or children is empty it returns a
// uniform random index rather than panicking.
func sampleSplitFrom(children []config.SplitChild, randN func(n int) int) int {
	if len(children) == 0 {
		return 0
	}
	total := 0
	for _, c := range children {
		total += c.Weight
	}
	if total <= 0 {
		// Degenerate: all weights zero. Fall back to uniform selection.
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
