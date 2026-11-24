package whatif

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// attentionSteps is rank-steps whose prefill time is the PS sojourn of the
// attention service S(x, c) = s0 + x/v + b x (c + x/2), over prompts and
// computed tokens that spread.
func attentionSteps(s0, v, b float64) []MixtureStep {
	steps := make([]MixtureStep, 0, 12)
	for i := range 12 {
		ranks := make([]MixtureRank, 0, 4)
		for j := range 4 {
			x := 800 + 500*float64((i+j)%6)
			l := x + 4000 + 9000*float64((i*3+j)%5)
			lam := 0.2 + 0.08*float64(j)
			es := s0 + x/v + b*x*attentionContext(x, l)
			ranks = append(ranks, MixtureRank{Rate: lam, PrefillTime: es / (1 - lam*es), QueueTime: 0.01, Computed: x, Prompt: l})
		}
		steps = append(steps, MixtureStep{Ranks: ranks})
	}
	return steps
}

// The attention law is recovered from PS-inflated prefill times; the line
// fitted to the same rank-steps misses it at the longest context.
func TestFitAttention_RecoversTheLaw(t *testing.T) {
	s0, v, b := 0.05, 15000.0, 3e-9
	steps := attentionSteps(s0, v, b)
	gs0, ga, gb, ok := FitAttention(steps)
	require.True(t, ok)
	assert.InDelta(t, s0, gs0, 1e-6)
	assert.InDelta(t, 1/v, ga, 1e-10)
	assert.InDelta(t, b, gb, 1e-13)
	// Negative control: the line (v2's FitPSInversion) on the same steps
	// prices a 3000-token chunk on 40000 cached tokens well under the law.
	ls0, lv, _, err := FitPSInversion(steps)
	require.NoError(t, err)
	law := s0 + 3000/v + b*3000*attentionContext(3000, 43000)
	assert.Less(t, ls0+3000/lv, 0.9*law)
}

// The fit's coefficients are held at 0 or above: a falling cost in the
// context is read as none, not as a negative B.
func TestFitAttention_NonNegative(t *testing.T) {
	steps := attentionSteps(0.05, 15000, 3e-9)
	for i := range steps {
		for j := range steps[i].Ranks {
			r := &steps[i].Ranks[j]
			es := 0.3 + r.Computed/15000 - 2e-6*(r.Prompt-r.Computed)
			r.PrefillTime = es / (1 - r.Rate*es)
		}
	}
	gs0, ga, gb, ok := FitAttention(steps)
	require.True(t, ok)
	assert.GreaterOrEqual(t, gs0, 0.0)
	assert.GreaterOrEqual(t, ga, 0.0)
	assert.Zero(t, gb)
	// At one computed length the law is not identified.
	for i := range steps {
		for j := range steps[i].Ranks {
			steps[i].Ranks[j].Computed = 2000
		}
	}
	_, _, _, ok = FitAttention(steps)
	assert.False(t, ok)
}

// nnls holds a coefficient OLS reads negative at 0 and refits the rest.
func TestNNLS(t *testing.T) {
	X := [][]float64{{1, 1}, {1, 2}, {1, 3}, {1, 4}}
	y := []float64{0.5, 2.5, 4.5, 6.5} // OLS: -1.5 + 2 x
	c, ok := nnls(X, y)
	require.True(t, ok)
	assert.Zero(t, c[0])
	assert.InDelta(t, (0.5+5+13.5+26)/30, c[1], 1e-12) // through the origin
	// An exact non-negative solution is returned as is.
	c, ok = nnls(X, []float64{3, 5, 7, 9})
	require.True(t, ok)
	assert.InDelta(t, 1, c[0], 1e-12)
	assert.InDelta(t, 2, c[1], 1e-12)
	_, ok = nnls(nil, nil)
	assert.False(t, ok)
}

// Z's parts are lines with non-negative intercepts and slopes; a part whose
// prompts do not spread is its median, not identified.
func TestFitZParts(t *testing.T) {
	steps := make([]MixtureStep, 0, 10)
	for i := range 10 {
		l := 20000 + 5000*float64(i)
		rs := make([]MixtureRank, 0, 3)
		for j := range 3 {
			lr := l + 1000*float64(j)
			rs = append(rs, MixtureRank{Rate: 1, PrefillTime: 0.5, QueueTime: 0.1, Computed: 2000, Prompt: lr,
				TTFT: 0.5 + 0.1 + 0.3 + 4e-6*lr})
		}
		dec := 0.4 + 3e-6*l
		pre := 0.9 + 4e-6*l
		// The remainder's line through the data would cross 0 below the
		// prompts read: -0.05 + 2e-6 l.
		steps = append(steps, MixtureStep{Ranks: rs, PromptMean: l, DecodeTTFT: dec, PrefillTTFT: pre,
			RouterMean: pre + dec - 0.05 + 2e-6*l})
	}
	p, un, err := FitZParts(steps)
	require.NoError(t, err)
	assert.Empty(t, un)
	assert.InDelta(t, 0.3, p.Overhead.A, 1e-9)
	assert.InDelta(t, 4e-6, p.Overhead.B, 1e-14)
	assert.InDelta(t, 0.4, p.Decode.A, 1e-9)
	assert.InDelta(t, 3e-6, p.Decode.B, 1e-14)
	assert.Zero(t, p.Remainder.A, "a negative intercept is held at 0")
	assert.Positive(t, p.Remainder.B)
	for i := range steps {
		steps[i].PromptMean = 50000
		for j := range steps[i].Ranks {
			steps[i].Ranks[j].Prompt = 50000
		}
	}
	p, un, err = FitZParts(steps)
	require.NoError(t, err)
	assert.Equal(t, []string{"overhead", "decode", "remainder"}, un)
	ov := make([]float64, 0, 30)
	for _, st := range steps {
		for _, r := range st.Ranks {
			ov = append(ov, r.TTFT-r.PrefillTime-r.QueueTime)
		}
	}
	assert.InDelta(t, medianOf(ov), p.Overhead.A, 1e-12, "the median of the part")
	assert.Zero(t, p.Overhead.B)
	_, _, err = FitZParts([]MixtureStep{{}})
	assert.Error(t, err)
}

// The clean steps leave out a step whose busiest rank is at or over
// MaxCleanRho, unless too few would be left.
func TestCleanSteps(t *testing.T) {
	m := RankMixture{S0: 0.1, V: 10000}
	mk := func(rate float64) MixtureStep {
		return MixtureStep{Ranks: []MixtureRank{{Rate: rate, Computed: 4000, Prompt: 4000}}}
	}
	// es = 0.5 s: rho = rate/2.
	steps := []MixtureStep{mk(1), mk(1.2), mk(1.6), mk(1.79), mk(1.8), mk(1.9)}
	assert.Len(t, m.cleanSteps(steps), 4)
	assert.Len(t, m.cleanSteps(steps[2:]), 4, "with fewer than minCleanSteps clean, every step")
}

// FitRankMixtureNL: the attention law, Z as the sum of its parts at spread
// 1, and a fit window that prices its own router mean.
func TestFitRankMixtureNL(t *testing.T) {
	truth := RankMixture{S0: 0.1, V: 8000, Shares: []float64{0.3, 0.25, 0.25, 0.2}, Spread: 1, Slots: 4,
		BetaOverhead: 3e-6, BetaDecode: 2e-6, Beta: 5e-6}
	steps := syntheticSteps(t, truth, 48)
	// syntheticSteps prices Z at A0 0: its parts' constants (0.3 s
	// overhead, 0.4 s decode) are in the engines' means, not the router's,
	// so the router mean here gets them added.
	for i := range steps {
		steps[i].RouterMean += 0.7
		steps[i].RouterP90 += 0.7
	}
	m, diag, err := FitRankMixtureNL(steps, RankMixtureOptions{Slots: 4})
	require.NoError(t, err)
	assert.Equal(t, ServiceAttention, m.Law)
	assert.Equal(t, 1.0, m.Spread)
	require.NotNil(t, m.Parts)
	assert.InDelta(t, m.Parts.Overhead.A+m.Parts.Decode.A+m.Parts.Remainder.A, m.A0, 1e-12)
	assert.InDelta(t, m.Parts.Overhead.B+m.Parts.Decode.B+m.Parts.Remainder.B, m.Beta, 1e-15)
	assert.GreaterOrEqual(t, m.A0, 0.0)
	assert.InDelta(t, 0.3, m.Parts.Overhead.A, 0.02)
	assert.InDelta(t, 0.4, m.Parts.Decode.A, 0.02)
	assert.InDelta(t, 1, diag.FitMean, 0.1)
	assert.Equal(t, 48, diag.Steps)
	assert.Contains(t, m.Assumed(), string(QueueRankMixtureNL))
	// The service it prices at the synthetic prompt is the truth's.
	assert.InDelta(t, truth.S0+1800/truth.V, m.service(1800, 48000), 0.02)
	_, _, err = FitRankMixtureNL(steps[:7], RankMixtureOptions{})
	assert.Error(t, err)
}

// The attention law changes the draws only through the service: a law with
// B 0 prices as the line does, and a positive B raises the mean.
func TestRankMixture_AttentionDraws(t *testing.T) {
	line := RankMixture{S0: 0.1, V: 10000, Shares: []float64{1}}
	att := line
	att.Law = ServiceAttention
	comp, prom := expHist(2000), pointHist(40000)
	dl, u := line.Draws(0.5, comp, prom, 4000)
	require.Nil(t, u)
	da, u := att.Draws(0.5, comp, prom, 4000)
	require.Nil(t, u)
	ml, _ := dl.Stats(0, 0)
	ma, _ := da.Stats(0, 0)
	assert.InDelta(t, ml, ma, 0.03*ml)
	att.B = 2e-9
	db, u := att.Draws(0.5, comp, prom, 4000)
	require.Nil(t, u)
	mb, _ := db.Stats(0, 0)
	assert.Greater(t, mb, 1.5*ml)
	assert.False(t, math.IsNaN(mb))
}

// sharesAt averages the profiles of the fit steps nearest the priced rate;
// with too few rows it prices the window's mean profile.
func TestRankMixture_SharesAtTheLoad(t *testing.T) {
	m := RankMixture{Shares: []float64{0.7, 0.3}}
	assert.Equal(t, m.Shares, m.sharesAt(10))
	for i := range 10 {
		top := 0.8 - 0.03*float64(i) // more even as the rate rises
		m.ShareRows = append(m.ShareRows, ShareRow{Rate: float64(i + 1), Shares: []float64{top, 1 - top}})
	}
	got := m.sharesAt(10)
	// The five nearest 10 are rates 6-10: tops 0.65 .. 0.53, mean 0.59.
	assert.InDelta(t, 0.59, got[0], 1e-9)
	assert.InDelta(t, 1, got[0]+got[1], 1e-9)
	assert.InDelta(t, 0.74, m.sharesAt(1)[0], 1e-9)
	rows := shareRowsOf([]MixtureStep{{Rate: 2, Ranks: []MixtureRank{{Rate: 0.5}, {Rate: 1.5}}}, {Rate: 1, Ranks: []MixtureRank{{Rate: 1}}}}, 2)
	require.Len(t, rows, 1)
	assert.Equal(t, []float64{0.75, 0.25}, rows[0].Shares)
}

// A lognormal's sigma is read back from its mean and p90; the histogram's
// quantile interpolates inside a bucket.
func TestLnSigmaAndHistQuantile(t *testing.T) {
	sigma := 0.6
	p90 := math.Exp(-sigma*sigma/2 + z90*sigma) // mean 1
	got, ok := lnSigma(1, p90)
	require.True(t, ok)
	assert.InDelta(t, sigma, got, 1e-9)
	_, ok = lnSigma(0, 1)
	assert.False(t, ok)
	h := map[float64]float64{1: 0.5, 2: 0.8, 4: 1, math.Inf(1): 1}
	q, ok := histP90(h)
	require.True(t, ok)
	assert.InDelta(t, 3, q, 1e-12)
	_, ok = histP90(map[float64]float64{})
	assert.False(t, ok)
}

// The parts' spreads keep Z's mean and lengthen its tail; with no sigma the
// draws carry no Z and price a0 + beta L as before.
func TestZParts_SpreadIsMeanPreserving(t *testing.T) {
	m := RankMixture{S0: 0.1, V: 10000, Shares: []float64{1}, A0: 0.9, Beta: 0, Parts: &ZParts{
		Overhead: ZPart{A: 0.5}, Decode: ZPart{A: 0.4}}}
	d, u := m.Draws(0.5, expHist(2000), pointHist(40000), 8000)
	require.Nil(t, u)
	assert.Nil(t, d.Z)
	m0, q0 := d.Stats(m.A0, 0, 0.9)
	m.Parts.Overhead.Sigma, m.Parts.Decode.Sigma = 0.8, 0.5
	d, u = m.Draws(0.5, expHist(2000), pointHist(40000), 8000)
	require.Nil(t, u)
	require.NotNil(t, d.Z)
	m1, q1 := d.Stats(m.A0, 0, 0.9)
	assert.InDelta(t, m0, m1, 0.02*m0)
	assert.Greater(t, q1[0], q0[0]*1.05)
}

// The decode part's sigma is read from the decode engines' TTFT histogram.
func TestFitSigmas_DecodeFromItsHistogram(t *testing.T) {
	sigma := 0.5
	mean := 0.4
	p90 := mean * math.Exp(-sigma*sigma/2+z90*sigma)
	steps := make([]MixtureStep, 0, 6)
	for range 6 {
		steps = append(steps, MixtureStep{Rate: 1, DecodeTTFT: mean,
			DecodeTTFTHist: map[float64]float64{p90 * 0.5: 0.4, p90: 0.9, p90 * 2: 1, math.Inf(1): 1}})
	}
	m := RankMixture{S0: 0.1, V: 10000, Shares: []float64{1}, Parts: &ZParts{Decode: ZPart{A: mean}}}
	m.fitSigmas(steps)
	assert.InDelta(t, sigma, m.Parts.Decode.Sigma, 1e-9)
	assert.Zero(t, m.Parts.Overhead.Sigma, "no prefill TTFT histogram: no overhead spread")
}

// LineService keeps v2's service line under the variant's fit: no attention
// term, the line FitPSInversion reads, and the rest of the variant as is.
func TestFitRankMixtureNL_LineService(t *testing.T) {
	truth := RankMixture{S0: 0.1, V: 8000, Shares: []float64{0.3, 0.25, 0.25, 0.2}, Spread: 1, Slots: 4,
		BetaOverhead: 3e-6, BetaDecode: 2e-6, Beta: 5e-6}
	steps := syntheticSteps(t, truth, 48)
	for i := range steps {
		steps[i].RouterMean += 0.7
		steps[i].RouterP90 += 0.7
	}
	m, _, err := FitRankMixtureNL(steps, RankMixtureOptions{Slots: 4, LineService: true})
	require.NoError(t, err)
	assert.Equal(t, ServiceLine, m.Law)
	assert.Zero(t, m.B)
	s0, v, _, err := FitPSInversion(steps)
	require.NoError(t, err)
	assert.InDelta(t, s0, m.S0, 1e-12)
	assert.InDelta(t, v, m.V, 1e-9)
	require.NotNil(t, m.Parts)
	att, _, err := FitRankMixtureNL(steps, RankMixtureOptions{Slots: 4})
	require.NoError(t, err)
	assert.Equal(t, ServiceAttention, att.Law)
}

// heavyTail is a computed-token law whose top bucket holds 1e-4 of the
// requests at up to a million tokens: most of the service's second moment.
func heavyTail() map[float64]float64 {
	return map[float64]float64{1000: 0.4, 2000: 0.8, 5000: 0.97, 20000: 0.9999, 1000000: 1, math.Inf(1): 1}
}

// The variant reads every rank's mean service once, from the stratified
// grid: equal shares read equal utilizations, at the law's mean. v2 reads
// each rank's from its own draws, and on a heavy tail they scatter.
func TestRankMixture_StratifiedMoments(t *testing.T) {
	shares := []float64{0.25, 0.25, 0.25, 0.25}
	v2 := RankMixture{S0: 0.1, V: 20000, Shares: shares}
	nlm := v2
	nlm.Parts = &ZParts{}
	hx, ok := newCumHist(heavyTail())
	require.True(t, ok)
	hl, ok := newCumHist(pointHist(80000))
	require.True(t, ok)
	es, cs2 := nlm.serviceMoments(hx, hl)
	// The law's mean by a fine quadrature of its quantile function.
	var want, want2 float64
	const n = 1 << 20
	for i := range n {
		v := 0.1 + hx.ppf((float64(i)+0.5)/n)/20000
		want += v
		want2 += v * v
	}
	want /= n
	wantCS2 := want2/n/(want*want) - 1
	assert.InEpsilon(t, want, es, 1e-3)
	assert.InEpsilon(t, wantCS2, cs2, 0.06, "a random sample of the same size reads the tail's cs^2 off by more")
	d, u := nlm.Draws(4, heavyTail(), pointHist(80000), DefaultMixtureDraws)
	require.Nil(t, u)
	for _, r := range d.Checks.Rho {
		assert.InDelta(t, d.Checks.Rho[0], r, 1e-12, "one law, one utilization")
	}
	assert.InEpsilon(t, 4*0.25*es, d.Checks.Rho[0], 1e-9)
	// Negative control: v2's ranks read their own draws and differ.
	d2, u := v2.Draws(4, heavyTail(), pointHist(80000), DefaultMixtureDraws)
	require.Nil(t, u)
	spread := 0.0
	for _, r := range d2.Checks.Rho {
		spread = max(spread, math.Abs(r-d2.Checks.Rho[0]))
	}
	assert.Greater(t, spread, 1e-3)
}
