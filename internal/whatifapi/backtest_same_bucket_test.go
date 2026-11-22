package whatifapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An unresolvable pair whose prediction falls in the measured bucket itself,
// (lower, le], is consistent at the bucket's resolution: the third outcome,
// beside within and out. A prediction on the bucket's upper bound belongs to
// it; one just above it, or on its lower bound, does not, and stays plain
// unresolvable. Neither is scored.
func TestBacktest_APredictionInTheMeasuredBucketIsSameBucket(t *testing.T) {
	p90 := statIndex(t, 0.9)
	// TPOT 10 to 20 ms, the router's own bounds: 2x wide.
	m := measuredOf(LatencySample{Router: bkRouter(10, [][2]float64{{0.2, 1}},
		0.1, [][2]float64{{0.01, 0.5}, {0.02, 1}}, 0.012)}, MeasuredRouter, 300, true)
	require.NotNil(t, m.tpotBucket[p90])
	require.Equal(t, Band{Lo: 0.01, Hi: 0.02}, *m.tpotBucket[p90])
	at := func(v float64) BacktestPoint {
		return scoredPoint(BacktestPoint{Statistic: statP90, Predicted: Figure{Value: ptr(v)}}, m.tpot[p90], m.requests,
			m.tpotTop, m.tpotWhy[p90], m.tpotBucket[p90])
	}
	for _, c := range []struct {
		v    float64
		same bool
	}{{0.0187, true}, {0.02, true}, {0.0101, true}, {0.01, false}, {0.0201, false}, {0.0235, false}, {0.0085, false}} {
		p := at(c.v)
		assert.Equal(t, UnscoredUnresolvable, p.Unscored, "%g s", c.v)
		assert.Nil(t, p.Ratio, "%g s is not scored", c.v)
		assert.Equal(t, c.same, p.SameBucket, "%g s in (10, 20] ms", c.v)
	}
	assert.False(t, at(0.03).SameBucket, "a certain miss is never in the bucket")
	assert.Equal(t, OutsideOver, at(0.03).OutsideBucket)

	// The score counts it apart, with the bucket and its width; it never
	// enters within, the median ratio or the scored steps.
	scored := BacktestPoint{Statistic: statP90, Predicted: Figure{Value: ptr(0.05)}, Measured: ptr(0.05), Ratio: ptr(1.0)}
	steps := []BacktestStep{
		{TPOT: []BacktestPoint{{}, {}, at(0.0187), {}}},
		{TPOT: []BacktestPoint{{}, {}, at(0.015), {}}},
		{TPOT: []BacktestPoint{{}, {}, at(0.022), {}}},
		{TPOT: []BacktestPoint{{}, {}, at(0.03), {}}},
		{TPOT: []BacktestPoint{{}, {}, scored, {}}},
		{Excluded: ExcludedWarmUp, TPOT: []BacktestPoint{{}, {}, at(0.015), {}}},
	}
	_, tpot := scoresOf(steps)
	sc := tpot[p90]
	assert.Equal(t, 3, sc.Unscored[UnscoredUnresolvable])
	assert.Equal(t, 2, sc.SameBucket)
	require.NotNil(t, sc.SameBucketBand)
	assert.Equal(t, Band{Lo: 0.01, Hi: 0.02}, *sc.SameBucketBand)
	assert.InDelta(t, 2.0, sc.SameBucketWidth, 1e-9)
	assert.Equal(t, 2, sc.All.Steps, "the scored point and the certain miss")
	assert.InDelta(t, 0.5, *sc.All.Within, 1e-12, "same-bucket steps are not within")
	w := sc.SameBucketWords()
	assert.Contains(t, w, "in 2, 10 ms to 20 ms (2.0x wide)")
	assert.Contains(t, w, "not within 20%")
}

// The words name a bucket from 0 as such, and buckets of several widths by
// the narrowest.
func TestBacktest_SameBucketWordsCarryTheWidth(t *testing.T) {
	var sc BacktestScore
	assert.Empty(t, sc.SameBucketWords())
	sc.noteSameBucket(Band{Lo: 0.01, Hi: 0.02})
	sc.noteSameBucket(Band{Lo: 0.02, Hi: 0.04})
	assert.Contains(t, sc.SameBucketWords(), "(buckets 2.0x wide or wider)")
	sc.noteSameBucket(Band{Lo: 0, Hi: 0.005})
	assert.Contains(t, sc.SameBucketWords(), "(from 0)")
	assert.Equal(t, 3, sc.SameBucket)
	assert.Equal(t, Band{Lo: 0, Hi: 0.04}, *sc.SameBucketBand)
}

// Beside a figure judged at its mean, the self-check says how often the p90
// it could not place fell in the measured bucket itself, with the bucket's
// width, and judges exactly as it does without it: the same-bucket count is
// information, never part of the trust.
func TestSelfCheck_SameBucketIsInformationOnly(t *testing.T) {
	e, _ := scEntry(t, 1, 1)
	scores := scoresAt(12, 0.99, 0, 0, 12)
	p90 := &scores[statIndex(t, 0.9)]
	for range 9 {
		p90.noteSameBucket(Band{Lo: 0.01, Hi: 0.02})
	}
	fig := func(sc []BacktestScore) *SelfCheckFigure {
		f := selfCheckFigure(sc, 12)
		f.Levels, f.WithinBound = selfCheckLevelsOK, ptr(1.0)
		return f
	}
	with := fig(scores)
	require.Equal(t, statisticMean, with.Statistic)
	assert.Equal(t, 9, with.P90SameBucket)
	assert.Equal(t, 12, with.P90Unresolvable)
	without := fig(scoresAt(12, 0.99, 0, 0, 12))
	assert.Zero(t, without.P90SameBucket)

	a := &SelfCheck{TPOT: with, Folds: []SelfCheckFold{}, Checks: []SelfCheckRule{}}
	a.judge(e, time.Hour, SelfCheckInfo{PlannerVersion: "vtest"})
	b := &SelfCheck{TPOT: without, Folds: []SelfCheckFold{}, Checks: []SelfCheckRule{}}
	b.judge(e, time.Hour, SelfCheckInfo{PlannerVersion: "vtest"})
	assert.Equal(t, b.Status, a.Status)
	assert.Equal(t, b.Checks, a.Checks)
	assert.Equal(t, b.Trust, a.Trust)
	assert.Contains(t, a.Line, "p90 TPOT unresolvable in 12, in the measured histogram bucket itself in 9, 10 ms to 20 ms (2.0x wide): "+
		"consistent at the bucket's resolution, not within 20%")
	assert.NotContains(t, b.Line, "measured histogram bucket")
}
