package prefill

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// timedKey is a key whose one pod holds the timed minutes (ieff, prefill
// time), every one at the same occupancy, so the quieter half is all of
// them.
func timedKey(xs, ys []float64) *keyState {
	ps := &podState{}
	for i := range xs {
		ps.samples = append(ps.samples, sample{timedOK: true, timed: timedSample{ieff: xs[i], prefill: ys[i], occ: 0.5}})
	}
	return &keyState{pods: map[string]*podState{"p": ps}}
}

// line is n minutes on prefill = a0 + b*x, x from lo to hi, plus noise
// alternating +-jitter.
func line(n int, lo, hi, a0, b, jitter float64) (xs, ys []float64) {
	for i := range n {
		x := lo + (hi-lo)*float64(i)/float64(n-1)
		j := jitter
		if i%2 == 1 {
			j = -jitter
		}
		xs, ys = append(xs, x), append(ys, a0+b*x+j)
	}
	return xs, ys
}

// Each refusal of the timing fit, and the two boundaries: 30 minutes are
// enough and 29 are not; a p90 at 1.5 times the p10 spreads and one under
// it does not.
func TestTiming_TheRefusalsAndTheirBoundaries(t *testing.T) {
	spread := func(hi float64) (xs, ys []float64) {
		// 30 minutes: p10 (index 2) at 1000, p90 (index 26) at hi.
		for i := range 30 {
			x := 1000.0
			if i >= 10 {
				x = hi
			}
			xs, ys = append(xs, x), append(ys, 0.2+1e-4*x)
		}
		return xs, ys
	}
	fit := func(n int, lo, hi, a0, b, jitter float64) func() ([]float64, []float64) {
		return func() ([]float64, []float64) { return line(n, lo, hi, a0, b, jitter) }
	}
	for _, c := range []struct {
		name    string
		minutes func() ([]float64, []float64)
		want    Refusal
	}{
		{"a falling slope", fit(40, 1000, 3000, 1.0, -1e-4, 0), NonPositiveSlope},
		{"an intercept under zero", fit(40, 2000, 6000, -0.1, 1e-4, 0), NonPositiveIntercept},
		{"an intercept noise could put at zero", fit(40, 2000, 6000, 0.01, 1e-4, 0.2), AUncertain},
		{"30 minutes", fit(30, 1000, 3000, 0.2, 1e-4, 0.001), ""},
		{"29 minutes", fit(29, 1000, 3000, 0.2, 1e-4, 0.001), TooFewTimedMinutes},
		{"a spread of exactly 1.5", func() ([]float64, []float64) { return spread(1500) }, ""},
		{"a spread just under 1.5", func() ([]float64, []float64) { return spread(1499) }, NoPromptSpread},
	} {
		xs, ys := c.minutes()
		est := Estimate{ARefused: TooFewLowLoadMinutes}
		est.fixedFromTiming(timedKey(xs, ys))
		assert.Equal(t, c.want, est.ATimingRefused, c.name)
		if c.want == "" {
			assert.Equal(t, ASourceEngineTiming, est.ASource, c.name)
			assert.Empty(t, est.ARefused, c.name)
		} else {
			assert.Equal(t, TooFewLowLoadMinutes, est.ARefused, "%s: the low-load refusal stands", c.name)
			assert.Zero(t, est.A, c.name)
		}
	}
}
