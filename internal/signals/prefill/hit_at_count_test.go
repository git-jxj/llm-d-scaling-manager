package prefill

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The lab run of issue #384 (64 shared documents, Qwen3-32B, one prefill
// pod's GPU cache under the documents' working set), its clean windows
// pooled: on 2 prefill pods a request computed 7,532 of 15,962 prompt tokens;
// on 1 pod, 10,454; on 4, 3,205.
const (
	labWhole       = 15962.0
	labComputedAt1 = 10454.0
	labComputedAt2 = 7532.0
	labComputedAt4 = 3205.0
)

// Fitted on 2 pods and priced on 1, the floor reads the computed tokens at
// or above what 1 pod computed (about 11,750 against 10,454), where holding
// the 2-pod hit read 7,532, 28% under. On 4 pods the hit is held: 7,532,
// over the 3,205 measured, which is safe for capacity.
func TestHitAtCount_TheLabRunFittedOnTwoPods(t *testing.T) {
	h := 1 - labComputedAt2/labWhole
	at1, ok := HitAtCountOf(h, labComputedAt2, 2, 1, nil)
	require.True(t, ok)
	assert.Equal(t, HitFloor, at1.Rule)
	assert.InDelta(t, h/2, at1.Hit, 1e-12)
	assert.InDelta(t, labWhole*(1-h/2), at1.Computed, 1e-6, "the whole prompt does not move with the count")
	t.Logf("1 pod: hit %.3f, computed %.0f a request (measured %.0f, held %.0f)", at1.Hit, at1.Computed, labComputedAt1, labComputedAt2)
	assert.GreaterOrEqual(t, at1.Computed, labComputedAt1, "never under what 1 pod computed")
	assert.InDelta(t, 11750, at1.Computed, 100)
	assert.True(t, at1.Moved())
	assert.InDelta(t, h/2, at1.Lo, 1e-12)
	assert.Equal(t, h, at1.Hi)

	at4, ok := HitAtCountOf(h, labComputedAt2, 2, 4, nil)
	require.True(t, ok)
	assert.Equal(t, HitHeld, at4.Rule)
	assert.Equal(t, h, at4.Hit)
	assert.Equal(t, labComputedAt2, at4.Computed)
	assert.GreaterOrEqual(t, at4.Computed, labComputedAt4, "held is pessimistic at more pods")
	assert.False(t, at4.Moved())
	assert.Equal(t, h, at4.Lo)
	assert.Equal(t, 1.0, at4.Hi, "min(1, h x 2)")
}

// At the fit's count nothing moves.
func TestHitAtCount_TheSameCountMovesNothing(t *testing.T) {
	a, ok := HitAtCountOf(0.4, 6000, 3, 3, nil)
	require.True(t, ok)
	assert.Equal(t, HitAtCount{FitHit: 0.4, Hit: 0.4, FitCount: 3, Count: 3, Rule: HitSameCount, Lo: 0.4, Hi: 0.4,
		Computed: 6000, ComputedScale: 1}, a)
	assert.False(t, a.Moved())
}

// The edges: no hit stays no hit at any count; a hit of 1 or more, a NaN or
// negative hit, no pods on either side, and a computed prompt that is not a
// positive finite number price nothing.
func TestHitAtCount_Edges(t *testing.T) {
	for _, n := range []int{1, 2, 8} {
		a, ok := HitAtCountOf(0, 5000, 4, n, nil)
		require.True(t, ok, "h = 0 at %d", n)
		assert.Equal(t, 0.0, a.Hit)
		assert.Equal(t, 5000.0, a.Computed)
		assert.False(t, a.Moved())
	}
	for name, c := range map[string]struct {
		h, computed     float64
		fitCount, count int
	}{
		"N' = 0":            {0.5, 5000, 2, 0},
		"N = 0":             {0.5, 5000, 0, 2},
		"N' < 0":            {0.5, 5000, 2, -1},
		"h = 1":             {1, 5000, 2, 1},
		"h above 1":         {1.2, 5000, 2, 1},
		"h negative":        {-0.1, 5000, 2, 1},
		"h NaN":             {math.NaN(), 5000, 2, 1},
		"h +Inf":            {math.Inf(1), 5000, 2, 1},
		"computed 0":        {0.5, 0, 2, 1},
		"computed NaN":      {0.5, math.NaN(), 2, 1},
		"computed +Inf":     {0.5, math.Inf(1), 2, 1},
		"computed negative": {0.5, -1, 2, 1},
	} {
		_, ok := HitAtCountOf(c.h, c.computed, c.fitCount, c.count, nil)
		assert.False(t, ok, name)
	}
}

// The floor scales the hit by N'/N at every pair below the fit's count, and
// the computed tokens by (1 - h')/(1 - h).
func TestHitAtCount_TheFloor(t *testing.T) {
	a, ok := HitAtCountOf(0.6, 4000, 4, 1, nil)
	require.True(t, ok)
	assert.InDelta(t, 0.15, a.Hit, 1e-12)
	assert.InDelta(t, 4000*0.85/0.4, a.Computed, 1e-9)
	assert.InDelta(t, 0.85/0.4, a.ComputedScale, 1e-12)
	a, ok = HitAtCountOf(0.6, 4000, 4, 3, nil)
	require.True(t, ok)
	assert.InDelta(t, 0.45, a.Hit, 1e-12)
}

// uniformGaps is a reuse-gap histogram of hits gaps spread evenly over
// [0, 100] s, in ten buckets, and an idle histogram of misses evictions
// whose median is 100 s.
func uniformGaps(hits, misses float64) *ReuseHistograms {
	r := &ReuseHistograms{GapSum: hits * 50}
	for i := 1; i <= 10; i++ {
		r.Gap = append(r.Gap, ReuseBucket{UpperEdge: float64(10 * i), Count: hits * float64(i) / 10})
	}
	r.Gap = append(r.Gap, ReuseBucket{UpperEdge: math.Inf(1), Count: hits})
	r.Idle = []ReuseBucket{{UpperEdge: 50, Count: 0}, {UpperEdge: 150, Count: misses}, {UpperEdge: math.Inf(1), Count: misses}}
	return r
}

// The reuse-gap rule solves the occupancy at N': with hit gaps even over
// [0, 100] s, as many misses as hits and a 100 s horizon, half the pods give
// T'^2 - 400 T' + 15000 = 0, so T' = 200 - 50 sqrt(10) and the hits under it
// are T'/100 of them.
func TestHitAtCount_TheReuseGapRule(t *testing.T) {
	a, ok := HitAtCountOf(0.5, 4000, 2, 1, uniformGaps(1000, 1000))
	require.True(t, ok)
	assert.Equal(t, HitReuseGap, a.Rule)
	tp := 200 - 50*math.Sqrt(10)
	assert.InDelta(t, 0.5*tp/100, a.Hit, 1e-6)
	assert.InDelta(t, 4000*(1-a.Hit)/0.5, a.Computed, 1e-6)
	assert.Equal(t, 0.25, a.Lo, "the bracket keeps the floor")
	assert.Equal(t, 0.5, a.Hi)

	// More pods hold the hit, whatever the histograms say.
	up, ok := HitAtCountOf(0.5, 4000, 2, 3, uniformGaps(1000, 1000))
	require.True(t, ok)
	assert.Equal(t, HitHeld, up.Rule)
	assert.Equal(t, 0.5, up.Hit)
}

// The rule never prices above the fit's hit, and the floor stands in where
// the histograms do not give it.
func TestHitAtCount_TheReuseGapRuleFallsBackToTheFloor(t *testing.T) {
	for name, edit := range map[string]func(r *ReuseHistograms){
		"no gap histogram":    func(r *ReuseHistograms) { r.Gap = nil },
		"no idle histogram":   func(r *ReuseHistograms) { r.Idle = nil },
		"no hit counted":      func(r *ReuseHistograms) { r.Gap = []ReuseBucket{{10, 0}, {math.Inf(1), 0}} },
		"no eviction counted": func(r *ReuseHistograms) { r.Idle = []ReuseBucket{{10, 0}, {math.Inf(1), 0}} },
		"edges not ascending": func(r *ReuseHistograms) { r.Gap[3].UpperEdge = 5 },
		"counts falling":      func(r *ReuseHistograms) { r.Gap[3].Count = 0 },
		"a NaN count":         func(r *ReuseHistograms) { r.Idle[1].Count = math.NaN() },
		"only an +Inf edge":   func(r *ReuseHistograms) { r.Idle = []ReuseBucket{{math.Inf(1), 10}} },
	} {
		r := uniformGaps(1000, 1000)
		edit(r)
		a, ok := HitAtCountOf(0.5, 4000, 2, 1, r)
		require.True(t, ok, name)
		assert.Equal(t, HitFloor, a.Rule, name)
		assert.Equal(t, 0.25, a.Hit, name)
	}
	// With the gaps far shorter than the horizon, the rule would keep
	// every hit: capped at the fit's.
	short := uniformGaps(1000, 10)
	for i := range short.Gap {
		short.Gap[i].UpperEdge /= 100
	}
	short.GapSum /= 100
	a, ok := HitAtCountOf(0.5, 4000, 2, 1, short)
	require.True(t, ok)
	assert.Equal(t, HitReuseGap, a.Rule)
	assert.LessOrEqual(t, a.Hit, 0.5)
}
