package match

import (
	"crypto/rand"
	"math/big"

	"github.com/dio/transit/examples/orange/internal/config"
)

// sampleSplit picks a child index from s.Children using crypto/rand
// and the configured weight distribution. Panics if weights do not
// sum to a positive integer (caller must guarantee valid config).
func sampleSplit(s *config.SplitNode) int {
	return sampleSplitFrom(s.Children, cryptoIntn)
}

func cryptoIntn(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		panic("crypto/rand failure: " + err.Error())
	}
	return int(v.Int64())
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
