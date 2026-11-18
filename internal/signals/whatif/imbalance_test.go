package whatif

import (
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// A count P1 learned keeps its own f, unflagged, whatever the other counts
// say.
func TestImbalanceAcrossCounts_ALearnedCountKeepsItsOwn(t *testing.T) {
	got := ImbalanceAcrossCounts(map[int]float64{2: 1.3, 3: 1.01, 4: 1.6}, 3)
	assert.Equal(t, Imbalance{F: 1.01, Source: ImbalanceLearned}, got)
}

// With no count learned, f is FreshImbalance, flagged, and never above the
// pods.
func TestImbalanceAcrossCounts_NoCountIsTheDefault(t *testing.T) {
	for _, byPods := range []map[int]float64{nil, {}, {1: 1}, {2: 0.9}, {3: math.NaN()}, {4: math.Inf(1)}} {
		got := ImbalanceAcrossCounts(byPods, 3)
		assert.Equal(t, Imbalance{F: FreshImbalance, Assumed: true, Source: ImbalanceDefault}, got, "%v", byPods)
	}
	assert.Equal(t, 1.0, ImbalanceAcrossCounts(nil, 1).F, "never above the pods")
}

// One count learned fits c = (f - 1)/g(n) exactly, and the curve reads it at
// every other count, flagged and named.
func TestImbalanceAcrossCounts_OneCountFitsTheCurve(t *testing.T) {
	byPods := map[int]float64{2: 1.2}
	c := 0.2 / prefill.NormalMaxMean(2)
	for _, pods := range []int{3, 4, 8} {
		got := ImbalanceAcrossCounts(byPods, pods)
		assert.InDelta(t, 1+c*prefill.NormalMaxMean(pods), got.F, 1e-12, "%d pods", pods)
		assert.True(t, got.Assumed)
		assert.Equal(t, "fitted from counts {2}", got.Source)
		assert.Equal(t, []int{2}, got.Counts)
		assert.InDelta(t, c, got.Spread, 1e-12)
	}
	// It rises with the pods, as the busiest of more pods does.
	assert.Less(t, ImbalanceAcrossCounts(byPods, 3).F, ImbalanceAcrossCounts(byPods, 8).F)
	// Read back at the count it was fitted from, it gives that count's f.
	assert.InDelta(t, 1.2, 1+c*prefill.NormalMaxMean(2), 1e-12)
}

// Counts on one curve give its c back; counts off it give the least-squares
// c through f = 1.
func TestImbalanceAcrossCounts_SeveralCounts(t *testing.T) {
	g := prefill.NormalMaxMean
	on := map[int]float64{2: 1 + 0.15*g(2), 4: 1 + 0.15*g(4), 6: 1 + 0.15*g(6)}
	got := ImbalanceAcrossCounts(on, 3)
	assert.InDelta(t, 0.15, got.Spread, 1e-12)
	assert.InDelta(t, 1+0.15*g(3), got.F, 1e-12)
	assert.Equal(t, "fitted from counts {2, 4, 6}", got.Source)

	off := map[int]float64{2: 1.1, 4: 1.1}
	want := (0.1*g(2) + 0.1*g(4)) / (g(2)*g(2) + g(4)*g(4))
	got = ImbalanceAcrossCounts(off, 3)
	assert.InDelta(t, want, got.Spread, 1e-12)
	assert.Equal(t, []int{2, 4}, got.Counts)

	// A count of one, or an f no reading of the curve gives, is not fitted.
	got = ImbalanceAcrossCounts(map[int]float64{1: 1, 2: 1.1, 5: 0.5, 7: math.NaN()}, 3)
	assert.Equal(t, []int{2}, got.Counts)
}

// Equal pods learned at every count give f = 1 at any other: never below 1.
func TestImbalanceAcrossCounts_EqualPodsReadOne(t *testing.T) {
	got := ImbalanceAcrossCounts(map[int]float64{2: 1, 3: 1}, 8)
	assert.Equal(t, 1.0, got.F)
	assert.True(t, got.Assumed)
}

// The fit sums in count order: the same counts give the same c to the bit,
// whatever order the map yields them in.
func TestImbalanceAcrossCounts_Deterministic(t *testing.T) {
	byPods := map[int]float64{2: 1.07, 3: 1.11, 4: 1.13, 5: 1.2, 6: 1.17, 7: 1.3, 8: 1.25}
	want := ImbalanceAcrossCounts(byPods, 12)
	for range 200 {
		require.Equal(t, want, ImbalanceAcrossCounts(byPods, 12))
	}
}

// The measured case behind the curve and the default (Qwen3-32B TP2 prefill
// pods on H200, 8000-token prompts). P1 learned f = 1.0691 over 29 minutes at
// two pods on the 2+2 runs; at three pods (3+1) the busiest pod's share of
// prefill busy time read 1.12 to 1.14 at the five steps below saturation,
// 1.12 at 0.85 of the knee (busy 0.90, 0.94, 0.67), and its share of prompt
// tokens 1.08 to 1.10. P1 kept too few minutes at three pods to learn f
// there: at 8000-token prompts the pods saw under 100 requests a minute each.
func TestImbalanceAcrossCounts_ThreePodsFromTwo(t *testing.T) {
	const learnedAtTwo = 1.0691094943912582
	busyAtThree := []float64{0.90, 0.94, 0.67}
	measured := 3 * slices.Max(busyAtThree) / (busyAtThree[0] + busyAtThree[1] + busyAtThree[2])
	require.InDelta(t, 1.12, measured, 0.005)

	got := ImbalanceAcrossCounts(map[int]float64{2: learnedAtTwo}, 3)
	t.Logf("f at 3 pods fitted from f(2) = %.4f: %.4f (c %.4f); measured %.4f", learnedAtTwo, got.F, got.Spread, measured)
	assert.InDelta(t, 1.104, got.F, 0.001)
	assert.InEpsilon(t, measured, got.F, 0.02, "within 2%% of the busy share measured at three pods")
	assert.GreaterOrEqual(t, got.F, 1.08, "at or above the prompt-token share measured there")
	assert.Less(t, got.F, ImbalanceFloor, "below the floor the planner assumed")
}

// FreshImbalance stands above every share measured on the Qwen3-32B runs, and
// inside the 1.15 to 1.2 the issue asked for.
func TestFreshImbalance_StandsAboveTheMeasuredRuns(t *testing.T) {
	// f learned at two pods (two runs), and the busiest pod's busy share at
	// three pods at each step below saturation.
	measured := []float64{1.0, 1.0691, 1.138, 1.142, 1.123, 1.138, 1.139}
	for _, m := range measured {
		assert.Greater(t, FreshImbalance, m)
	}
	assert.GreaterOrEqual(t, FreshImbalance, 1.15)
	assert.LessOrEqual(t, FreshImbalance, 1.2)
	assert.Less(t, FreshImbalance, ImbalanceFloor)
	assert.False(t, DefaultImbalanceAcrossCounts, "opt-in: off by default")
}

// The raw share at its own count raises an f not learned, at most to the
// pods, and never a learned one.
func TestImbalance_WithRawShare(t *testing.T) {
	fresh := ImbalanceAcrossCounts(nil, 3)
	assert.Equal(t, fresh, fresh.WithRawShare(1.1, 3), "under the default")
	assert.Equal(t, fresh, fresh.WithRawShare(math.NaN(), 3), "no reading")
	assert.Equal(t, Imbalance{F: 1.4, Assumed: true, Source: ImbalanceRawShare}, fresh.WithRawShare(1.4, 3))
	assert.Equal(t, Imbalance{F: 3, Assumed: true, Source: ImbalanceRawShare}, fresh.WithRawShare(3.5, 3), "at most the pods")
	learned := ImbalanceAcrossCounts(map[int]float64{3: 1.05}, 3)
	assert.Equal(t, learned, learned.WithRawShare(2.5, 3), "a learned f is kept")
}
