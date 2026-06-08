package match

import (
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/orange/internal/config"
)

// TestSampleSplitNew_returnsValidIndex exercises sampleSplitNew itself (the
// production entry point) to verify it returns an index within bounds
// across many draws.
func TestSampleSplitNew_returnsValidIndex(t *testing.T) {
	s := &config.SplitConfig{Children: makeNewChildren(34, 33, 33)}
	for range 1000 {
		idx := sampleSplitNew(s)
		require.GreaterOrEqual(t, idx, 0)
		require.Less(t, idx, len(s.Children))
	}
}

// chiSquared computes the chi-squared statistic for the observed counts
// against the expected proportions given by weights/100.
func chiSquared(observed []int, weights []int, n int) float64 {
	var x2 float64
	for i, obs := range observed {
		expected := float64(n) * float64(weights[i]) / 100.0
		diff := float64(obs) - expected
		x2 += diff * diff / expected
	}
	return x2
}

func makeNewChildren(weights ...int) []config.WeightedRoutingConfig {
	children := make([]config.WeightedRoutingConfig, len(weights))
	for i, w := range weights {
		children[i] = config.WeightedRoutingConfig{Weight: w}
	}
	return children
}

// --- Degenerate / safety tests ---

func TestSampleSplitNewFrom_emptyChildren_returnsZero(t *testing.T) {
	// Should never happen after config validation, but must not panic.
	got := sampleSplitNewFrom(nil, func(int) int { return 0 })
	assert.Equal(t, 0, got)
}

func TestSampleSplitNewFrom_allZeroWeights_uniformFallback(t *testing.T) {
	// Should never happen after config validation, but must not panic.
	children := makeNewChildren(0, 0, 0)
	calls := 0
	sampleSplitNewFrom(children, func(n int) int {
		calls++
		assert.Equal(t, 3, n, "fallback must draw from len(children)")
		return 1
	})
	assert.Equal(t, 1, calls)
}

// --- Boundary tests (deterministic mock randN) ---

func TestSampleSplitNewFrom_boundary_zero(t *testing.T) {
	children := makeNewChildren(40, 35, 25)
	// draw=0 → arm 0 (first value inside first window [0,40))
	got := sampleSplitNewFrom(children, func(int) int { return 0 })
	assert.Equal(t, 0, got)
}

func TestSampleSplitNewFrom_boundary_lastInFirst(t *testing.T) {
	children := makeNewChildren(40, 35, 25)
	// draw=39 → arm 0 (last value inside first window [0,40))
	got := sampleSplitNewFrom(children, func(int) int { return 39 })
	assert.Equal(t, 0, got)
}

func TestSampleSplitNewFrom_boundary_firstOfSecond(t *testing.T) {
	children := makeNewChildren(40, 35, 25)
	// draw=40 → arm 1 (first value of second window [40,75))
	got := sampleSplitNewFrom(children, func(int) int { return 40 })
	assert.Equal(t, 1, got)
}

func TestSampleSplitNewFrom_boundary_last(t *testing.T) {
	children := makeNewChildren(40, 35, 25)
	// draw=99 → arm 2 (last value, total-1)
	got := sampleSplitNewFrom(children, func(int) int { return 99 })
	assert.Equal(t, 2, got)
}

func TestSampleSplitNewFrom_twoArms_boundary(t *testing.T) {
	children := makeNewChildren(60, 40)
	got := sampleSplitNewFrom(children, func(int) int { return 59 })
	assert.Equal(t, 0, got)
	got = sampleSplitNewFrom(children, func(int) int { return 60 })
	assert.Equal(t, 1, got)
}

// --- Distribution tests (seeded math/rand/v2 via sampleSplitNewFrom) ---

func runDistribution(t *testing.T, weights []int, n int, chiCrit float64, extraCheck func(counts []int)) {
	t.Helper()
	rng := rand.New(rand.NewPCG(42, 0))
	children := makeNewChildren(weights...)
	counts := make([]int, len(weights))
	for range n {
		idx := sampleSplitNewFrom(children, rng.IntN)
		require.Less(t, idx, len(counts))
		counts[idx]++
	}
	x2 := chiSquared(counts, weights, n)
	assert.Less(t, x2, chiCrit, "chi-squared %.4f exceeds critical value %.4f (weights=%v counts=%v)", x2, chiCrit, weights, counts)
	if extraCheck != nil {
		extraCheck(counts)
	}
}

func TestSampleSplitNewFrom_distribution_50_50(t *testing.T) {
	// 2 arms, df=1, χ²_crit=10.83 at α=0.001
	runDistribution(t, []int{50, 50}, 100_000, 10.83, nil)
}

func TestSampleSplitNewFrom_distribution_34_33_33(t *testing.T) {
	// 3 arms, df=2, χ²_crit=13.82 at α=0.001
	runDistribution(t, []int{34, 33, 33}, 100_000, 13.82, nil)
}

func TestSampleSplitNewFrom_distribution_1_1_98(t *testing.T) {
	// 3 arms skewed, df=2, χ²_crit=13.82 at α=0.001
	// Also assert the heavy arm really dominates (catches incorrect normalisation).
	runDistribution(t, []int{1, 1, 98}, 100_000, 13.82, func(counts []int) {
		assert.Greater(t, counts[2], 95_000, "arm 2 (weight=98) must dominate: got %d", counts[2])
	})
}

func TestSampleSplitNewFrom_distribution_uniform_8arm(t *testing.T) {
	// 8 arms (12/13/13/13/12/13/12/12=100), df=7, χ²_crit=24.32 at α=0.001
	runDistribution(t, []int{12, 13, 13, 13, 12, 13, 12, 12}, 100_000, 24.32, nil)
}
