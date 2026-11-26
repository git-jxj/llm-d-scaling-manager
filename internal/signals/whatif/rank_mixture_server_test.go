package whatif

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serverTruth is the remainder server the synthetic fit windows are drawn
// from: render 0.2 s + 1 us a token, frontend 10 ms + 0.5 us a token, the
// router's work outside render 70 ms, the rest 50 ms.
var serverTruth = ZServer{R0: 0.2, R1: 1e-6, F0: 0.01, F1: 5e-7, Other: 0.07, N0: 0.05, HasRender: true}

// serverSteps is n 5-minute steps of four prefill pods, each pod at its own
// prompt and rate, their API servers drawn from truth with a queue of share
// truth.K, and the router's remainder their share-weighted sum with O and N.
func serverSteps(truth ZServer, n int) []MixtureStep {
	at := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	steps := make([]MixtureStep, n)
	for i := range steps {
		st := MixtureStep{At: at.Add(time.Duration(i) * 5 * time.Minute), PrefillTTFT: 1, DecodeTTFT: 0.5}
		var lsum, ren, fp float64
		for j := range 4 {
			l := 2000 + 4000*float64((i*7+j*3)%11)
			lam := 0.4 + 0.3*float64(j) + 0.05*float64(i%5)
			sr, sg := truth.service(l)
			rho := truth.K * (lam*sr + lam*sg)
			r := MixtureRank{Rate: lam, Prompt: l, Frontend: sg / (1 - rho), Render: sr / (1 - rho), RenderRate: lam}
			st.Ranks = append(st.Ranks, r)
			st.Rate += lam
			lsum += lam * l
			ren += lam * r.Render
			fp += lam * r.Frontend
		}
		st.PromptMean = lsum / st.Rate
		ren, fp = ren/st.Rate, fp/st.Rate
		st.RouterRenderMean = ren
		st.RouterProcessingMean = truth.Other + ren
		st.RouterMean = st.PrefillTTFT + st.DecodeTTFT + truth.Other + ren + fp + truth.N0
		steps[i] = st
	}
	return steps
}

// With no queue, the fit reads back the lines the pods measured, the
// router's work outside its render call and the rest, and prices the
// remainder each step measured.
func TestFitZServer_ReadsTheLinesBack(t *testing.T) {
	steps := serverSteps(serverTruth, 24)
	z, off := FitZServer(steps, 300, false)
	require.Empty(t, off)
	require.NotNil(t, z)
	assert.True(t, z.HasRender)
	assert.Zero(t, z.K, "no queue unless asked")
	assert.InDelta(t, 0.2, z.R0, 1e-6)
	assert.InDelta(t, 1e-6, z.R1, 1e-10)
	assert.InDelta(t, 0.01, z.F0, 1e-6)
	assert.InDelta(t, 5e-7, z.F1, 1e-10)
	assert.InDelta(t, 0.07, z.Other, 1e-9)
	assert.InDelta(t, 0.05, z.N0, 1e-6)
	assert.InDelta(t, 0, z.N1, 1e-9)
	assert.InDelta(t, 1, z.G, 1e-9)
	assert.InDelta(t, 1, z.RenderPerRequest, 1e-9)
	require.Len(t, z.Shares, 4)
	assert.InDelta(t, 1, z.Shares[0]+z.Shares[1]+z.Shares[2]+z.Shares[3], 1e-9)
	assert.Greater(t, z.Shares[0], z.Shares[3], "busiest first")
	assert.Equal(t, 24, z.Steps)
	assert.Equal(t, 96, z.PodSteps)
	for _, st := range steps {
		// The frontend is read at each pod's prompt and priced at the
		// step's mean: a line, so the share-weighted mean is the same.
		assert.InDelta(t, st.RouterMean-st.PrefillTTFT-st.DecodeTTFT, z.Mean(st.PromptMean, st.Rate), 2e-3)
	}
}

// A pod-step far off its line moves the soft-L1 fit little; least squares
// would move it by its whole weight.
func TestFitZServer_AnOutlierPodStepMovesItLittle(t *testing.T) {
	steps := serverSteps(serverTruth, 24)
	steps[3].Ranks[1].Frontend = 5
	z, off := FitZServer(steps, 300, false)
	require.Empty(t, off)
	assert.InDelta(t, 0.01, z.F0, 2e-3)
	assert.InDelta(t, 5e-7, z.F1, 1e-7)
}

// With the queue asked for, the fit reads back the share K of the handlers'
// time that holds the server.
func TestFitZServer_QueueReadsBackK(t *testing.T) {
	truth := serverTruth
	truth.K = 0.5
	z, off := FitZServer(serverSteps(truth, 24), 300, true)
	require.Empty(t, off)
	assert.InDelta(t, 0.5, z.K, 0.03)
	assert.InDelta(t, 0.2, z.R0, 0.02)
	assert.InDelta(t, 0.01, z.F0, 2e-3)
	noQ, _ := FitZServer(serverSteps(truth, 24), 300, false)
	assert.Zero(t, noQ.K, "without the queue K is held at 0 on the same data")
	quiet, busy := z.Mean(10000, 0.5), z.Mean(10000, 8)
	assert.Greater(t, busy, quiet, "with a queue the remainder rises with the rate")
	assert.Equal(t, noQ.Mean(10000, 0.5), noQ.Mean(10000, 8), "without one it does not")
}

// No pod frontend recorded, too few steps with one, or no router
// processing: nothing is fitted, and the reason says so.
func TestFitZServer_AbstainsWithoutTheSeries(t *testing.T) {
	steps := serverSteps(serverTruth, 24)
	none := make([]MixtureStep, len(steps))
	for i, st := range steps {
		st.Ranks = append([]MixtureRank(nil), st.Ranks...)
		for j := range st.Ranks {
			st.Ranks[j].Frontend = 0
		}
		none[i] = st
	}
	z, off := FitZServer(none, 300, false)
	assert.Nil(t, z)
	assert.Contains(t, off, "fewer than 12 fit steps")

	z, _ = FitZServer(steps[:MinRemainderServerSteps-1], 300, false)
	assert.Nil(t, z, "one step short")
	z, _ = FitZServer(steps[:MinRemainderServerSteps], 300, false)
	assert.NotNil(t, z)

	noProc := make([]MixtureStep, len(steps))
	for i, st := range steps {
		st.RouterProcessingMean = 0
		noProc[i] = st
	}
	z, _ = FitZServer(noProc, 300, false)
	assert.Nil(t, z, "no router processing: no O")

	// A pod-step under minServerPodRequests is not read.
	z, _ = FitZServer(steps, 1, false)
	assert.Nil(t, z, "a second-long step reads no pod at its requests")
}

// Without the render handler on most pod-steps the render part is 0, and O
// is the router's whole processing as a line in the prompt.
func TestFitZServer_NoRender(t *testing.T) {
	steps := serverSteps(serverTruth, 24)
	for i := range steps {
		for j := range steps[i].Ranks {
			steps[i].Ranks[j].Render, steps[i].Ranks[j].RenderRate = 0, 0
		}
		steps[i].RouterRenderMean = 0
	}
	z, off := FitZServer(steps, 300, false)
	require.Empty(t, off)
	assert.False(t, z.HasRender)
	assert.Zero(t, z.R0)
	assert.Zero(t, z.RenderPerRequest)
	ren, _, _ := z.pods(10000, 4)
	assert.Equal(t, []float64{0, 0, 0, 0}, ren)
	assert.Positive(t, z.Other+z.Other1*10000)
}

// The draws keep the server's mean on a mean-1 shape, the router part
// carries the shape's tail, and the same inputs draw the same requests.
func TestZServer_DrawsKeepTheMean(t *testing.T) {
	z, off := FitZServer(serverSteps(serverTruth, 24), 300, false)
	require.Empty(t, off)
	shape, _ := vincentShape([]map[float64]float64{processingHist()})
	l := make([]float64, 40000)
	for i := range l {
		l[i] = 2000 + float64(i%9)*2500
	}
	var lb float64
	for _, x := range l {
		lb += x
	}
	lb /= float64(len(l))
	d := z.draws(l, 4, shape)
	var mean float64
	for _, x := range d {
		mean += x
	}
	mean /= float64(len(d))
	assert.InDelta(t, z.Mean(lb, 4), mean, 0.01*z.Mean(lb, 4))
	p95 := sampleQuantile(d, 0.95)
	_, router := z.meanParts(lb, 4)
	assert.Greater(t, p95, z.Mean(lb, 4)+router, "the router part is drawn on the shape's tail")
	assert.Equal(t, d, z.draws(l, 4, shape))
}

// The remainder server on a fitted variant: drawn from the pods where the fit
// window recorded them, the spread itself where it did not (with the reason
// in the assumed line), and nothing at all without the spread. The fit
// window's checks never move.
func TestFitRankMixtureNL_RemainderServer(t *testing.T) {
	steps := nlSteps(t)
	srv := serverSteps(serverTruth, len(steps))
	for i := range steps {
		steps[i].RouterProcessingHist = processingHist()
		steps[i].RouterProcessingMean = srv[i].RouterProcessingMean
		steps[i].RouterRenderMean = srv[i].RouterRenderMean
	}
	o := nlOptions()
	o.RemainderSpread = true
	spread, spreadDiag, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	require.NotNil(t, spread.Parts.Remainder.Shape)

	// No pod frontend recorded: the plain spread, figure for figure.
	o.RemainderServer = true
	absent, absentDiag, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	assert.Nil(t, absent.Parts.Remainder.Server)
	assert.Contains(t, absent.RemainderServerOff, "fewer than 12 fit steps")
	assert.Contains(t, absent.Assumed(), "router remainder server not applied")
	assert.Equal(t, spreadDiag, absentDiag)
	comp, prom := expHist(2000), expHist(40000)
	ds, u := spread.Draws(steps[0].Rate, comp, prom, 4000)
	require.Nil(t, u)
	da, u := absent.Draws(steps[0].Rate, comp, prom, 4000)
	require.Nil(t, u)
	assert.Equal(t, ds.Z, da.Z, "abstained, the remainder is the spread's draws")

	// The pods' frontends recorded: the server draws the remainder.
	for i := range steps {
		for j := range steps[i].Ranks {
			if j < len(srv[i].Ranks) {
				r := srv[i].Ranks[j]
				steps[i].Ranks[j].Frontend, steps[i].Ranks[j].Render, steps[i].Ranks[j].RenderRate = r.Frontend, r.Render, r.RenderRate
			}
		}
	}
	on, onDiag, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	require.NotNil(t, on.Parts.Remainder.Server, on.RemainderServerOff)
	assert.Empty(t, on.RemainderServerOff)
	assert.Zero(t, on.Parts.Remainder.Server.K)
	assert.Contains(t, on.Assumed(), "router remainder from the prefill pods' API servers")
	assert.Equal(t, spreadDiag, onDiag, "the mode never moves the fit window's checks")
	dn, u := on.Draws(steps[0].Rate, comp, prom, 4000)
	require.Nil(t, u)
	assert.NotEqual(t, ds.Z, dn.Z)
	assert.Equal(t, ds.QP, dn.QP, "the queue part is the same draws")

	// Asked without the spread: nothing.
	o.RemainderSpread = false
	alone, _, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	assert.False(t, alone.RemainderServer)
	assert.Nil(t, alone.Parts.Remainder.Server)
	assert.NotContains(t, alone.Assumed(), "remainder server")
}

// The step pools each rank's API-server figures: the frontend weighted by
// rate, the render mean by the render rate, the render rate as the rate is
// (absent counts as none); the router's render mean is the mean of the
// cycles that read one.
func TestMixtureStepOf_PoolsTheAPIServer(t *testing.T) {
	// The render rates run against the request rates, so a render mean
	// weighted by the request rate reads 0.35, not 0.28.
	c1 := MixtureCycle{Ranks: []MixtureRankReading{{Key: "p0", MixtureRank: MixtureRank{Rate: 1, Frontend: 0.01, Render: 0.2,
		RenderRate: 3}}}, RouterRenderMean: 0.2}
	c2 := MixtureCycle{Ranks: []MixtureRankReading{{Key: "p0", MixtureRank: MixtureRank{Rate: 3, Frontend: 0.03, Render: 0.4,
		RenderRate: 2}}}}
	s := MixtureStepOf([]MixtureCycle{c1, c2}, time.Time{})
	require.Len(t, s.Ranks, 1)
	r := s.Ranks[0]
	assert.InDelta(t, 0.025, r.Frontend, 1e-12, "the frontend by request rate")
	assert.InDelta(t, 0.28, r.Render, 1e-12, "the render mean by render rate")
	assert.InDelta(t, 2.5, r.RenderRate, 1e-12)
	assert.Equal(t, 0.2, s.RouterRenderMean)

	s = MixtureStepOf([]MixtureCycle{{Ranks: []MixtureRankReading{{Key: "p0", MixtureRank: MixtureRank{Rate: 1}}}}}, time.Time{})
	assert.Zero(t, s.Ranks[0].Frontend)
	assert.Zero(t, s.Ranks[0].Render)
	assert.Zero(t, s.Ranks[0].RenderRate)
	assert.Zero(t, s.RouterRenderMean)
}

// nnLine is the constrained optimum: the free line where it is
// non-negative, else the better boundary.
func TestNNLine(t *testing.T) {
	a, b := nnLine([]float64{1, 2, 3}, []float64{3, 5, 7}, nil)
	assert.InDelta(t, 1, a, 1e-12)
	assert.InDelta(t, 2, b, 1e-12)
	a, b = nnLine([]float64{1, 2, 3}, []float64{3, 2, 1}, nil)
	assert.InDelta(t, 2, a, 1e-12, "a falling line: the mean")
	assert.Zero(t, b)
	a, b = nnLine([]float64{1, 2, 3}, []float64{-1, 1, 3}, nil)
	assert.Zero(t, a, "a negative intercept: through the origin")
	assert.InDelta(t, 10.0/14, b, 1e-12)
	a, b = nnLine([]float64{1, 2}, []float64{-1, -2}, nil)
	assert.Zero(t, a)
	assert.Zero(t, b)
	a, b = nnLine([]float64{1, 2, 3}, []float64{1, 1, 10}, []float64{1, 1, 0})
	assert.InDelta(t, 1, a, 1e-12, "a zero weight reads nothing")
	assert.Zero(t, b)
}
