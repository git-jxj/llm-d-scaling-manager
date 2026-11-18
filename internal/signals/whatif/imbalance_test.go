package whatif

import (
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// at is f at pods from P1's f by count alone.
func at(byPods map[int]float64, pods int) Imbalance {
	return ImbalanceReadings{ByPods: byPods}.At(pods)
}

// A count P1 learned keeps its own f, unflagged and unclamped, whatever the
// other counts or the raw share say.
func TestImbalanceAt_ALearnedCountKeepsItsOwn(t *testing.T) {
	assert.Equal(t, Imbalance{F: 1.01, Source: ImbalanceLearned}, at(map[int]float64{2: 1.3, 3: 1.01, 4: 1.6}, 3))
	r := ImbalanceReadings{ByPods: map[int]float64{3: 1.01}, Pods: 3, RawShare: 2.5}
	assert.Equal(t, Imbalance{F: 1.01, Source: ImbalanceLearned}, r.At(3), "the raw share does not override a learned f")
}

// One pod, or none, has no f.
func TestImbalanceAt_OnePod(t *testing.T) {
	for _, pods := range []int{0, 1} {
		assert.Equal(t, Imbalance{Source: ImbalanceOnePod}, at(map[int]float64{2: 1.2}, pods))
	}
}

// With no count learned and no raw share, f is FreshImbalance, flagged.
func TestImbalanceAt_NoCountIsTheDefault(t *testing.T) {
	for _, byPods := range []map[int]float64{nil, {}, {1: 1}, {2: 0.9}, {3: math.NaN()}, {4: math.Inf(1)}} {
		assert.Equal(t, Imbalance{F: FreshImbalance, Assumed: true, Source: ImbalanceDefault}, at(byPods, 3), "%v", byPods)
	}
}

// One count learned fits c = (f - 1)/g(n) exactly, and the curve reads it at
// every other count, flagged and named.
func TestImbalanceAt_OneCountFitsTheCurve(t *testing.T) {
	byPods := map[int]float64{2: 1.2}
	c := 0.2 / prefill.NormalMaxMean(2)
	for _, pods := range []int{3, 4, 8} {
		got := at(byPods, pods)
		assert.InDelta(t, 1+c*prefill.NormalMaxMean(pods), got.F, 1e-12, "%d pods", pods)
		assert.True(t, got.Assumed)
		assert.Equal(t, "fitted from counts {2}", got.Source)
		assert.Equal(t, []int{2}, got.Counts)
		assert.InDelta(t, c, got.Spread, 1e-12)
	}
	assert.Less(t, at(byPods, 3).F, at(byPods, 8).F, "the busiest of more pods reads higher")
}

// Counts on one curve give its c back; counts off it give the least-squares
// c through f = 1, each count weighted by its minutes.
func TestImbalanceAt_SeveralCountsWeightedByMinutes(t *testing.T) {
	g := prefill.NormalMaxMean
	on := map[int]float64{2: 1 + 0.15*g(2), 4: 1 + 0.15*g(4), 6: 1 + 0.15*g(6)}
	got := at(on, 3)
	assert.InDelta(t, 0.15, got.Spread, 1e-12)
	assert.InDelta(t, 1+0.15*g(3), got.F, 1e-12)
	assert.Equal(t, "fitted from counts {2, 4, 6}", got.Source)

	off := map[int]float64{2: 1.3, 4: 1.05}
	equal := ImbalanceReadings{ByPods: off, Minutes: map[int]int{2: 5, 4: 5}}.At(3)
	want := (0.3*g(2) + 0.05*g(4)) / (g(2)*g(2) + g(4)*g(4))
	assert.InDelta(t, want, equal.Spread, 1e-12)
	assert.Equal(t, []int{2, 4}, equal.Counts)
	// Five minutes at two pods against 300 at four: the fit leans to four.
	weighted := ImbalanceReadings{ByPods: off, Minutes: map[int]int{2: 5, 4: 300}}.At(3)
	wantW := (5*0.3*g(2) + 300*0.05*g(4)) / (5*g(2)*g(2) + 300*g(4)*g(4))
	assert.InDelta(t, wantW, weighted.Spread, 1e-12)
	assert.Less(t, weighted.F, equal.F)
	assert.InDelta(t, 0.05/g(4), weighted.Spread, 0.005, "near four pods' own c")
	// No minutes given weighs a count as prefill.MinImbalanceMinutes.
	assert.InDelta(t, equal.Spread, at(off, 3).Spread, 1e-12)

	// A count of one, or an f no reading of the curve gives, is not fitted.
	assert.Equal(t, []int{2}, at(map[int]float64{1: 1, 2: 1.1, 5: 0.5, 7: math.NaN()}, 3).Counts)
}

// Equal pods learned at every count give f = 1 at any other: never below 1.
func TestImbalanceAt_EqualPodsReadOne(t *testing.T) {
	got := at(map[int]float64{2: 1, 3: 1}, 8)
	assert.Equal(t, 1.0, got.F)
	assert.True(t, got.Assumed)
}

// A hot pod read now reaches the other counts (review of #177): three pods,
// nothing learned, a raw share of 2.5. Read at three it is 2.5; at four it
// is the curve through it, not FreshImbalance.
func TestImbalanceAt_AHotPodReachesTheOtherCounts(t *testing.T) {
	g := prefill.NormalMaxMean
	r := ImbalanceReadings{Pods: 3, RawShare: 2.5}
	now := r.At(3)
	assert.Equal(t, Imbalance{F: 2.5, Assumed: true, Source: ImbalanceRawShare}, now)
	next := r.At(4)
	c := 1.5 / g(3)
	assert.InDelta(t, 1+c*g(4), next.F, 1e-12)
	assert.Greater(t, next.F, 2.5, "N+1 is priced at least as hot as N")
	assert.True(t, next.Assumed)
	assert.Equal(t, "fitted from the raw share at 3", next.Source)

	// A raw share under the default does not lower it.
	low := ImbalanceReadings{Pods: 3, RawShare: 1.02}
	assert.Equal(t, Imbalance{F: FreshImbalance, Assumed: true, Source: ImbalanceDefault}, low.At(4))
	assert.Equal(t, Imbalance{F: FreshImbalance, Assumed: true, Source: ImbalanceDefault}, low.At(3))

	// With a learned count, the raw share raises the curve, never lowers it.
	learned := map[int]float64{2: 1.05}
	hot := ImbalanceReadings{ByPods: learned, Minutes: map[int]int{2: 30}, Pods: 3, RawShare: 2.5}.At(4)
	assert.Greater(t, hot.F, at(learned, 4).F)
	assert.Equal(t, "fitted from counts {2} and the raw share at 3", hot.Source)
	calm := ImbalanceReadings{ByPods: learned, Pods: 3, RawShare: 1.0}.At(4)
	assert.Equal(t, at(learned, 4), calm, "a raw share at 1 is no point")

	// Capped at its count, and not read at a count P1 learned.
	capped := ImbalanceReadings{Pods: 2, RawShare: 3.5}.At(2)
	assert.Equal(t, 2.0, capped.F)
	assert.Equal(t, at(map[int]float64{3: 1.1}, 4),
		ImbalanceReadings{ByPods: map[int]float64{3: 1.1}, Pods: 3, RawShare: 2.5}.At(4),
		"the raw share is no point where P1 learned its count")
}

// f read from rows of several engine ranks stays flagged, even where it was
// learned, and says so.
func TestImbalanceAt_MultiRankRowsStayFlagged(t *testing.T) {
	r := ImbalanceReadings{ByPods: map[int]float64{2: 1.04}, MultiRank: true}
	got := r.At(2)
	assert.Equal(t, 1.04, got.F)
	assert.True(t, got.Assumed)
	assert.Equal(t, "learned (per row of several engines)", got.Source)
	assert.True(t, r.At(3).Assumed)
	assert.Contains(t, r.At(3).Source, "per row of several engines")
}

// The fit sums in count order: the same counts give the same c to the bit,
// whatever order the map yields them in.
func TestImbalanceAt_Deterministic(t *testing.T) {
	r := ImbalanceReadings{ByPods: map[int]float64{2: 1.07, 3: 1.11, 4: 1.13, 5: 1.2, 6: 1.17, 7: 1.3, 8: 1.25},
		Minutes: map[int]int{2: 7, 3: 11, 4: 5, 5: 40, 6: 9, 7: 6, 8: 120}, Pods: 9, RawShare: 1.6}
	want := r.At(12)
	for range 200 {
		require.Equal(t, want, r.At(12))
	}
}

// The measured case behind the curve and the default (Qwen3-32B TP2 prefill
// pods on H200, 8000-token prompts). P1 learned f = 1.0691 over 29 minutes at
// two pods on the 2+2 runs; at three pods (3+1) the busiest pod's share of
// prefill busy time read 1.12 to 1.14 at the five steps below saturation,
// 1.12 at 0.85 of the knee (busy 0.90, 0.94, 0.67), and its share of prompt
// tokens 1.08 to 1.10. Those are raw shares; P1's f is de-biased, and a step
// of about 500 requests a pod carries a chance excess of about 0.04 at three
// pods. P1 kept too few minutes at three pods to learn f there: at
// 8000-token prompts the pods saw under 100 requests a minute each.
func TestImbalanceAt_ThreePodsFromTwo(t *testing.T) {
	const learnedAtTwo = 1.0691094943912582
	busyAtThree := []float64{0.90, 0.94, 0.67}
	raw := 3 * slices.Max(busyAtThree) / (busyAtThree[0] + busyAtThree[1] + busyAtThree[2])
	require.InDelta(t, 1.12, raw, 0.005)
	chance := prefill.NormalMaxMean(3) / math.Sqrt(500)
	debiased := raw - chance

	got := ImbalanceReadings{ByPods: map[int]float64{2: learnedAtTwo}, Minutes: map[int]int{2: 29}}.At(3)
	t.Logf("f at 3 pods fitted from f(2) = %.4f: %.4f (c %.4f); measured raw %.4f, de-biased about %.4f",
		learnedAtTwo, got.F, got.Spread, raw, debiased)
	assert.InDelta(t, 1.104, got.F, 0.001)
	assert.InEpsilon(t, raw, got.F, 0.02, "within 2%% of the raw busy share at three pods")
	assert.GreaterOrEqual(t, got.F, debiased, "at or above its de-biased reading")
	assert.Less(t, got.F, ImbalanceFloor, "below the floor the planner assumed")
}

// FreshImbalance stands above every share measured on the Qwen3-32B runs, and
// inside the 1.15 to 1.2 the issue asked for.
func TestFreshImbalance_StandsAboveTheMeasuredRuns(t *testing.T) {
	// f learned at two pods (two runs), and the busiest pod's raw busy share
	// at three pods at each step below saturation.
	measured := []float64{1.0, 1.0691, 1.138, 1.142, 1.123, 1.138, 1.139}
	for _, m := range measured {
		assert.Greater(t, FreshImbalance, m)
	}
	assert.GreaterOrEqual(t, FreshImbalance, 1.15)
	assert.LessOrEqual(t, FreshImbalance, 1.2)
	assert.Less(t, FreshImbalance, ImbalanceFloor)
	assert.False(t, DefaultImbalanceAcrossCounts, "opt-in: off by default")
}
