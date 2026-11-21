package whatif

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lognormalWindow is a window of n requests whose TPOT is lognormal with
// mean m and log-spread sigma, binned at edges (the last +Inf), its sum
// the distribution's mean times n.
func lognormalWindow(n, m, sigma float64, edges []float64) TPOTWindow {
	mu := math.Log(m) - sigma*sigma/2
	w := TPOTWindow{Sum: m * n, Count: n, SumMeasured: true}
	for _, e := range edges {
		c := n
		if !math.IsInf(e, 1) {
			c = n * 0.5 * math.Erfc(-(math.Log(e)-mu)/(sigma*math.Sqrt2))
		}
		w.Buckets = append(w.Buckets, HistogramBucket{UpperEdge: e, Count: c})
	}
	return w
}

// trueRatio is a lognormal's q-quantile over its mean.
func trueRatio(q, sigma float64) float64 {
	return math.Exp(sigma*math.Sqrt2*math.Erfinv(2*q-1) - sigma*sigma/2)
}

// The router's TPOT edges (10, 25, 50, 75 ms) around a 15 ms mean with a
// per-request spread of 0.2: the p90's bucket is 10 to 25 ms, and the edge
// read prices R_q at 25/15 = 1.67 where the truth is 1.26 (#279: predicted
// p90 over p50 1.55 against a measured 1.27 to 1.31). The censored fit
// reads the bucket's mass and lands within 5% of the truth, and never above
// the edge reading.
func TestTPOTRatioCensored_ReadsTheBucketMassNotItsEdge(t *testing.T) {
	edges := []float64{0.010, 0.025, 0.050, 0.075, math.Inf(1)}
	for _, c := range []struct{ mean, sigma float64 }{{0.015, 0.2}, {0.012, 0.15}, {0.022, 0.3}, {0.030, 0.5}} {
		w := lognormalWindow(1000, c.mean, c.sigma, edges)
		ws := []TPOTWindow{w, w, w}
		edge, _, u := TPOTRatioFromWindows(ws, 0.9)
		require.Nil(t, u)
		got, _, u := TPOTRatioCensored(ws, 0.9)
		require.Nil(t, u)
		want := trueRatio(0.9, c.sigma)
		assert.InEpsilon(t, max(1, want), got.Value, 0.05, "mean %g sigma %g: edge %.3f", c.mean, c.sigma, edge.Value)
		assert.LessOrEqual(t, got.Value, edge.Value, "never above the edge reading")
	}
}

// A refusal of the edge read is the censored read's too: no histogram, or
// a quantile in the +Inf bucket.
func TestTPOTRatioCensored_RefusesAsTheEdgeReadDoes(t *testing.T) {
	_, _, u := TPOTRatioCensored(nil, 0.9)
	require.NotNil(t, u)
	assert.Equal(t, NoTPOTHistogram, u.Reason)
	w := TPOTWindow{Sum: 1, Count: 100, SumMeasured: true, Buckets: []HistogramBucket{{UpperEdge: 0.005, Count: 50}, {UpperEdge: math.Inf(1), Count: 100}}}
	_, _, u = TPOTRatioCensored([]TPOTWindow{w}, 0.9)
	require.NotNil(t, u)
	assert.Equal(t, TPOTQuantileUnbounded, u.Reason)
}
