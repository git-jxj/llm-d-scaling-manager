package whatifapi

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// bkRouter is a router window of rate r: TTFT buckets ttft (cumulative
// shares at each edge) around mean ttftMean, TPOT buckets tpot around
// tpotMean.
func bkRouter(r float64, ttft [][2]float64, ttftMean float64, tpot [][2]float64, tpotMean float64) *domain.RouterLatency {
	hist := func(xs [][2]float64) []domain.LatencyBucket {
		b := make([]domain.LatencyBucket, 0, len(xs)+1)
		for _, x := range xs {
			b = append(b, domain.LatencyBucket{UpperBound: x[0], Rate: x[1] * r})
		}
		return append(b, domain.LatencyBucket{UpperBound: math.Inf(1), Rate: r})
	}
	return &domain.RouterLatency{WindowSeconds: 300, TTFTBuckets: hist(ttft), TTFTSumRate: ttftMean * r, TTFTCountRate: r,
		TPOTBuckets: hist(tpot), TPOTSumRate: tpotMean * r, TPOTCountRate: r}
}

// statIndex is q's index in backtestStats.
func statIndex(t *testing.T, q float64) int {
	t.Helper()
	for i, x := range backtestStats {
		if x == q {
			return i
		}
	}
	t.Fatalf("no statistic %g", q)
	return -1
}

// A measured percentile is scored only where the bucket it fell in is
// narrow against the measured mean (#291). The 0.6B staircase's B phase:
// TTFT buckets 0.2 to 0.4 s around a mean of 0.16 s, TPOT 5 to 10 ms
// around 7.5 ms. Interpolated, the p90 reads the grid; it is shown, with
// its bucket, and not scored. Where the buckets are fine it is scored.
func TestBacktest_APercentileInACoarseBucketIsUnresolvable(t *testing.T) {
	p90 := statIndex(t, 0.9)
	coarse := measuredOf(LatencySample{Router: bkRouter(10, [][2]float64{{0.2, 0.7}, {0.4, 1}}, 0.16,
		[][2]float64{{0.005, 0.3}, {0.01, 1}}, 0.0075)}, MeasuredRouter, 300, true)
	require.NotNil(t, coarse.ttftBucket[p90], "TTFT p90 in 0.2 to 0.4 s around 0.16 s")
	assert.Equal(t, Band{Lo: 0.2, Hi: 0.4}, *coarse.ttftBucket[p90])
	require.NotNil(t, coarse.tpotBucket[p90], "TPOT p90 in 5 to 10 ms around 7.5 ms")
	assert.Equal(t, Band{Lo: 0.005, Hi: 0.01}, *coarse.tpotBucket[p90])
	assert.Nil(t, coarse.ttftBucket[statIndex(t, 0)], "the mean is no bucket's")

	pred := BacktestPoint{Statistic: statP90, Predicted: Figure{Value: ptr(0.3)}}
	p := scoredPoint(pred, coarse.ttft[p90], coarse.requests, coarse.ttftTop, coarse.ttftWhy[p90], coarse.ttftBucket[p90])
	assert.Equal(t, UnscoredUnresolvable, p.Unscored)
	assert.Nil(t, p.Ratio, "not scored")
	require.NotNil(t, p.Measured, "the interpolated figure is still shown")
	assert.InDelta(t, 0.2+0.2*(0.9-0.7)/0.3, *p.Measured, 1e-12)
	assert.Equal(t, &Band{Lo: 0.2, Hi: 0.4}, p.MeasuredBucket)

	// Buckets 10 ms wide around a 115 ms mean place the p90: scored.
	fine := measuredOf(LatencySample{Router: bkRouter(10, [][2]float64{{0.1, 0.1}, {0.11, 0.5}, {0.12, 0.95}, {0.2, 1}}, 0.115,
		[][2]float64{{0.010, 0.5}, {0.012, 0.95}, {0.05, 1}}, 0.011)}, MeasuredRouter, 300, true)
	assert.Nil(t, fine.ttftBucket[p90])
	assert.Nil(t, fine.tpotBucket[p90])
	p = scoredPoint(pred, fine.ttft[p90], fine.requests, fine.ttftTop, fine.ttftWhy[p90], fine.ttftBucket[p90])
	assert.Empty(t, p.Unscored)
	require.NotNil(t, p.Ratio)
	assert.Nil(t, p.MeasuredBucket)

	// Above the top bucket is a miss, scored as before, not unresolvable.
	top := measuredOf(LatencySample{Router: bkRouter(10, [][2]float64{{0.2, 0.5}, {0.4, 0.8}}, 0.3,
		[][2]float64{{0.01, 1}}, 0.005)}, MeasuredRouter, 300, true)
	assert.Nil(t, top.ttftBucket[p90])
	p = scoredPoint(pred, top.ttft[p90], top.requests, top.ttftTop, top.ttftWhy[p90], top.ttftBucket[p90])
	assert.True(t, p.AboveHistogram)
}

// With no measured mean the bucket's lower edge stands for it: a bucket is
// scored when its upper edge is at most 1.4 times its lower.
func TestBacktest_ABucketWithNoMeanIsJudgedOnItsEdges(t *testing.T) {
	h, ok := histOf([]domain.LatencyBucket{{UpperBound: 0.010, Rate: 0.5}, {UpperBound: 0.014, Rate: 0.95},
		{UpperBound: 0.025, Rate: 1}, {UpperBound: math.Inf(1), Rate: 1}})
	require.True(t, ok)
	assert.Nil(t, h.coarseBucket(0.9, nil), "10 to 14 ms: 1.4x")
	assert.Equal(t, &Band{Lo: 0.014, Hi: 0.025}, h.coarseBucket(0.97, nil), "14 to 25 ms: 1.8x")
	assert.Equal(t, &Band{Lo: 0, Hi: 0.010}, h.coarseBucket(0.3, nil), "a first bucket from 0 never places one")
}

// The summary counts an unresolvable percentile apart: out of the steps
// scored, the median ratio and within 20%.
func TestBacktest_UnresolvableIsOutOfTheScore(t *testing.T) {
	pt := func(ratio float64, unscored string) BacktestPoint {
		p := BacktestPoint{Statistic: statP90, Predicted: Figure{Value: ptr(ratio)}, Measured: ptr(1.0), Unscored: unscored}
		if unscored == "" {
			p.Ratio = ptr(ratio)
		} else {
			p.MeasuredBucket = &Band{Lo: 0.5, Hi: 2}
		}
		return p
	}
	steps := []BacktestStep{
		{TPOT: []BacktestPoint{{}, {}, pt(1.0, ""), {}}},
		{TPOT: []BacktestPoint{{}, {}, pt(1.1, ""), {}}},
		{TPOT: []BacktestPoint{{}, {}, pt(0.5, UnscoredUnresolvable), {}}},
		{TPOT: []BacktestPoint{{}, {}, pt(0.4, UnscoredUnresolvable), {}}},
	}
	_, tpot := scoresOf(steps)
	sc := tpot[statIndex(t, 0.9)]
	assert.Equal(t, 2, sc.All.Steps)
	assert.Equal(t, 2, sc.Unscored[UnscoredUnresolvable])
	assert.InDelta(t, 1.05, *sc.All.Bias, 1e-12)
	assert.InDelta(t, 1.0, *sc.All.Within, 1e-12)
	assert.Zero(t, sc.All.Low, "the steps a coarse bucket read low are not counted low")
}

// scoresAt is a figure's scores: the mean scored in meanSteps steps with
// median ratio meanBias, the p90 in p90Steps with p90Bias and unresolvable
// in the rest of n.
func scoresAt(meanSteps int, meanBias float64, p90Steps int, p90Bias float64, n int) []BacktestScore {
	score := func(stat string, steps int, bias float64) BacktestScore {
		s := BacktestScore{Statistic: stat, Unscored: map[string]int{}, Refused: map[string]int{}, Outside: map[string]int{}}
		s.All.Steps = steps
		if steps > 0 {
			s.All.Bias, s.All.Within = ptr(bias), ptr(1.0)
		}
		return s
	}
	out := []BacktestScore{score(statisticMean, meanSteps, meanBias), score("p50", 0, 0), score(statP90, p90Steps, p90Bias), score("p95", 0, 0)}
	out[2].Unscored[UnscoredUnresolvable] = n - p90Steps
	return out
}

// The verdict and the self-check quote the mean, which no bucket sets, and
// the verdict says beside it how many p90s a bucket could not place.
func TestBacktest_TheMeanIsTheQuotedFigure(t *testing.T) {
	scores := scoresAt(10, 0.92, 0, 0, 10)
	line, quoted := verdictPart("TPOT", scores, "five-minute steps")
	require.NotNil(t, quoted)
	assert.Equal(t, statisticMean, quoted.Statistic)
	assert.True(t, strings.HasPrefix(line, "mean TPOT: within 20% of measured in 10 of the 10 five-minute steps scored"), line)
	assert.Contains(t, line, "p90 TPOT not scored in 10: the measured p90 fell in a histogram bucket wider than 40% of the measured mean")

	line, _ = verdictPart("TTFT", scoresAt(10, 1.02, 4, 0.85, 10), "five-minute steps")
	assert.Contains(t, line, "p90 TTFT: within 20% in 4 of 4, median ratio 0.85.")
	assert.Contains(t, line, "p90 TTFT not scored in 6")

	// With no mean scored the p90 leads, as before.
	_, quoted = verdictPart("TPOT", scoresAt(0, 0, 8, 1.1, 8), "five-minute steps")
	require.NotNil(t, quoted)
	assert.Equal(t, statP90, quoted.Statistic)

	f := selfCheckFigure(scoresAt(10, 0.92, 0, 0, 10), 10)
	require.NotNil(t, f)
	assert.Equal(t, statisticMean, f.Statistic)
	assert.Equal(t, 10, f.Steps)
	assert.InDelta(t, 0.92, *f.Bias, 1e-12)
	f = selfCheckFigure(scoresAt(0, 0, 0, 0, 10), 10)
	require.NotNil(t, f, "nothing scored: the p90's refusals still count")
	assert.Equal(t, statP90, f.Statistic)
	assert.Zero(t, f.Steps)
}

// A TTFT target inside the bucket the measured percentile fell in is not
// judged on the measured side; outside it, the bucket decides.
func TestBacktest_ATargetInsideACoarseBucketIsUnknown(t *testing.T) {
	m := measuredOf(LatencySample{Router: bkRouter(1000, [][2]float64{{0.2, 0.7}, {0.4, 1}}, 0.16,
		[][2]float64{{0.01, 1}}, 0.005)}, MeasuredRouter, 300, true)
	points := []BacktestPoint{{}, {}, {Predicted: Figure{Value: ptr(0.25)}}, {}}
	for target, want := range map[float64]string{0.3: judgedUnknown, 0.15: judgedMissed, 0.4: judgedMet, 0.5: judgedMet} {
		j := judgeTarget(&ReportTarget{Percentile: 0.9, Seconds: target}, points, m)
		assert.Equal(t, want, j.Measured, "target %g", target)
	}
}
