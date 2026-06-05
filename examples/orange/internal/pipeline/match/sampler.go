package match

import (
	"math/rand/v2"

	"github.com/dio/transit/examples/orange/internal/config"
)

// sampleSplit picks a child index from s.Children using the global
// math/rand/v2 source and the configured weight distribution. Panics
// if weights do not sum to a positive integer (caller must guarantee
// valid config).
func sampleSplit(s *config.SplitNode) int {
	return sampleSplitFrom(s.Children, rand.IntN)
}

// sampleSplitFrom is the testable core. randN(n) must return a
// uniformly distributed value in [0, n).
func sampleSplitFrom(children []config.SplitChild, randN func(n int) int) int {
	total := 0
	for _, c := range children {
		total += c.Weight
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
