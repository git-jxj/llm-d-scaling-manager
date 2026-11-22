package whatifapi

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// upperBoundPoint is a mean TTFT predicted at v seconds from a V_P that is
// only a lower bound, as figureOf puts it on the wire.
func upperBoundPoint(v float64) BacktestPoint {
	f := figureOf(whatif.TTFT{Seconds: v, Flags: []whatif.Flag{whatif.VPLowerBound}}, unitSeconds, true)
	return BacktestPoint{Statistic: statisticMean, Predicted: f}
}

// A TTFT priced from a lower bound of V_P is an upper bound on the wire;
// one priced from a stated V_P is not (#369).
func TestFigure_ATTFTFromALowerBoundOfVPIsAnUpperBound(t *testing.T) {
	f := figureOf(whatif.TTFT{Seconds: 0.5, Flags: []whatif.Flag{whatif.VPLowerBound}}, unitSeconds, true)
	assert.True(t, f.UpperBound)
	b, err := json.Marshal(f)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"upperBound":true`)

	assumed := figureOf(whatif.TTFT{Seconds: 0.5, Flags: []whatif.Flag{whatif.VPLowerBound, whatif.AssumedVP}}, unitSeconds, true)
	assert.False(t, assumed.UpperBound, "a stated V_P is no bound")
	plain := figureOf(whatif.TTFT{Seconds: 0.5}, unitSeconds, true)
	assert.False(t, plain.UpperBound)
	b, err = json.Marshal(plain)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "upperBound", "add-only: absent where false")
	rate := figureOf(whatif.Answer{Value: 4, Flags: []whatif.Flag{whatif.VPLowerBound}}, unitRate, false)
	assert.False(t, rate.UpperBound, "a rate from a lower bound of V_P is a lower bound, not an upper one")
}

// An upper bound the fleet measured under, or at most 20% over, is
// consistent with it and not scored: its ratio says nothing of the model's
// accuracy. One the fleet measured more than 20% over is scored, read low,
// so a figure under what was measured is never hidden (#369).
func TestBacktest_AnUpperBoundIsScoredOnlyWhereItBroke(t *testing.T) {
	m := 1.0
	for _, c := range []struct {
		name      string
		predicted float64
		unscored  string
	}{
		{"1525x high: the bound held", 1525, UnscoredUpperBound},
		{"1.2x high: the bound held", 1.2, UnscoredUpperBound},
		{"0.9x: within 20% under it", 0.9, UnscoredUpperBound},
		{"0.5x: the bound broke", 0.5, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := scoredPoint(upperBoundPoint(c.predicted), &m, 1000, 100, "", nil)
			assert.Equal(t, c.unscored, p.Unscored)
			if c.unscored != "" {
				assert.Nil(t, p.Ratio, "not scored")
				return
			}
			require.NotNil(t, p.Ratio, "scored")
			assert.InDelta(t, c.predicted, *p.Ratio, 1e-9)
			s := statsOf([]BacktestPoint{p})
			assert.Equal(t, 1, s.Low, "read low")
		})
	}
	// A plain figure 1525x high is scored as it always was.
	plain := scoredPoint(BacktestPoint{Statistic: statisticMean, Predicted: Figure{Value: ptr(1525), Unit: unitSeconds}}, &m, 1000, 100, "", nil)
	require.NotNil(t, plain.Ratio)
	assert.Empty(t, plain.Unscored)

	// Measured above the histogram's top bucket: the bound broke.
	inf := math.Inf(1)
	top := scoredPoint(upperBoundPoint(5), &inf, 1000, 10, "", nil)
	assert.True(t, top.AboveHistogram)
	require.NotNil(t, top.Ratio, "scored, read low")
	// In a coarse bucket wholly above it: broke; wholly under it: held.
	under := scoredPoint(upperBoundPoint(0.1), &m, 1000, 100, "", &Band{Lo: 0.8, Hi: 1.6})
	assert.Equal(t, OutsideUnder, under.OutsideBucket)
	require.NotNil(t, under.Ratio)
	over := scoredPoint(upperBoundPoint(5), &m, 1000, 100, "", &Band{Lo: 0.8, Hi: 1.6})
	assert.Equal(t, UnscoredUpperBound, over.Unscored)
	assert.Nil(t, over.Ratio)
	assert.Empty(t, over.OutsideBucket)
}

// The scores count the held bounds under upperBound, and the verdict says
// so rather than that the figure was never scored for want of one.
func TestBacktest_HeldUpperBoundsAreCountedNotScored(t *testing.T) {
	m := 1.0
	steps := make([]BacktestStep, 0, 3)
	for _, v := range []float64{1.3, 2, 3} {
		steps = append(steps, BacktestStep{TTFT: []BacktestPoint{scoredPoint(upperBoundPoint(v), &m, 1000, 100, "", nil)}})
	}
	sc := scoreOf(0, steps, func(st BacktestStep) []BacktestPoint { return st.TTFT }, 0)
	assert.Equal(t, 0, sc.All.Steps)
	assert.Equal(t, 3, sc.Unscored[UnscoredUpperBound])
	line, quoted := verdictPart("TTFT", []BacktestScore{sc}, "steps")
	assert.Nil(t, quoted)
	assert.Contains(t, line, "In 3 steps its TTFT was priced from a lower bound of V_P")
	assert.Contains(t, line, "consistent, and not scored for accuracy")

	// The self-check reads the p90's score where none was scored.
	p90 := sc
	p90.Statistic = statP90
	f := selfCheckFigure([]BacktestScore{sc, p90}, 3)
	require.NotNil(t, f)
	assert.Equal(t, 3, f.UpperBound)
	assert.Zero(t, f.Steps, "no rule reads a held bound")
}

// A predicted TTFT over its target that is only an upper bound is no
// predicted miss: the target agreement counts it not judged (#369).
func TestBacktest_AnUpperBoundOverTheTargetIsNoPredictedMiss(t *testing.T) {
	i := statIndex(t, 0)
	points := make([]BacktestPoint, len(backtestStats))
	m := measured{ttft: make([]*float64, len(backtestStats)), ttftWhy: make([]string, len(backtestStats)),
		ttftBucket: make([]*Band, len(backtestStats)), requests: 1000}
	m.ttft[i] = ptr(0.1)
	target := &ReportTarget{Percentile: 0, Seconds: 0.2}

	points[i] = upperBoundPoint(0.5)
	j := judgeTarget(target, points, m)
	assert.Equal(t, judgedUnknown, j.Predicted, "at most 0.5 s against 0.2 s: not known")
	assert.Equal(t, judgedMet, j.Measured)

	points[i] = upperBoundPoint(0.15)
	assert.Equal(t, judgedMet, judgeTarget(target, points, m).Predicted, "at most 0.15 s meets 0.2 s")

	points[i] = BacktestPoint{Statistic: statisticMean, Predicted: Figure{Value: ptr(0.5), Unit: unitSeconds}}
	assert.Equal(t, judgedMissed, judgeTarget(target, points, m).Predicted, "a plain figure over the target is a miss")
}
