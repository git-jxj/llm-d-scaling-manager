package whatif

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// heavyTailLaw is a computed-token law whose top bucket holds 1e-4 of the
// requests at up to a million tokens: most of the service's second moment.
func heavyTailLaw() map[float64]float64 {
	return map[float64]float64{1000: 0.4, 2000: 0.8, 5000: 0.97, 20000: 0.9999, 1000000: 1, math.Inf(1): 1}
}

// With the switch every rank reads one utilization, at the law's mean
// service, and its cs^2 within the grid's own error; without it the ranks
// read their own draws and scatter on the same law.
func TestRankMixture_StratifiedMoments(t *testing.T) {
	shares := []float64{0.25, 0.25, 0.25, 0.25}
	off := RankMixture{S0: 0.1, V: 20000, Shares: shares}
	on := off
	on.Stratified = true
	hx, ok := newCumHist(heavyTailLaw())
	require.True(t, ok)
	es, cs2 := on.serviceMoments(hx)
	var want, want2 float64
	const n = 1 << 20
	for i := range n {
		v := 0.1 + hx.ppf((float64(i)+0.5)/n)/20000
		want += v
		want2 += v * v
	}
	want /= n
	assert.InEpsilon(t, want, es, 1e-3)
	assert.InEpsilon(t, want2/n/(want*want)-1, cs2, 0.06)
	d, u := on.Draws(4, heavyTailLaw(), pointHist(80000), DefaultMixtureDraws)
	require.Nil(t, u)
	for _, r := range d.Checks.Rho {
		assert.InDelta(t, d.Checks.Rho[0], r, 1e-12, "one law, one utilization")
	}
	assert.InEpsilon(t, es, d.Checks.Rho[0], 1e-9)
	d2, u := off.Draws(4, heavyTailLaw(), pointHist(80000), DefaultMixtureDraws)
	require.Nil(t, u)
	spread := 0.0
	for _, r := range d2.Checks.Rho {
		spread = max(spread, math.Abs(r-d2.Checks.Rho[0]))
	}
	assert.Greater(t, spread, 1e-3, "without the switch the ranks scatter")
}

// FitCore carries the switch onto the model; off by default.
func TestFitCore_StratifiedMoments(t *testing.T) {
	st := syntheticSteps(t, RankMixture{S0: 0.05, V: 20000, Shares: []float64{0.6, 0.4}, Spread: 1}, 12)
	m, _, err := FitCore(st, RankMixtureOptions{})
	require.NoError(t, err)
	assert.False(t, m.Stratified)
	m, _, err = FitCore(st, RankMixtureOptions{StratifiedMoments: true})
	require.NoError(t, err)
	assert.True(t, m.Stratified)
}
