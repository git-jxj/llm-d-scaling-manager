package whatifapi

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// loadPricer is the stub's fleets with phi x load added to every TTFT, phi
// the knob's (Knobs.CachedTokenCost) or, with none, its own: a fleet whose
// cached tokens cost load seconds a request at phi 1.
type loadPricer struct {
	r         *stubResolver
	load, phi float64
	// pastKnee prices a TTFT of pastKneeSeconds where the stub refuses one
	// as saturated: a step past the knee with a figure.
	pastKnee bool
}

// pastKneeSeconds is the TTFT loadPricer prices past the knee.
const pastKneeSeconds = 30.0

func (p *loadPricer) Fleet(k Knobs) (Fleet, error) {
	fl, err := p.r.Fleet(k)
	if err != nil {
		return nil, err
	}
	phi := p.phi
	if k.CachedTokenCost != nil {
		phi = *k.CachedTokenCost
	}
	return &loadFleet{Fleet: fl, add: phi * p.load, pastKnee: p.pastKnee}, nil
}

// loadFleet is a fleet with add seconds on every TTFT.
type loadFleet struct {
	Fleet
	add      float64
	pastKnee bool
}

func (f *loadFleet) shifted(r whatif.Result) whatif.Result {
	if u, ok := r.(whatif.Unanswerable); ok && f.pastKnee && u.Reason == whatif.Saturated {
		r = whatif.TTFT{Seconds: pastKneeSeconds, Floor: 0.1, Flags: []whatif.Flag{whatif.UnvalidatedTTFT}}
	}
	t, ok := r.(whatif.TTFT)
	if !ok {
		return r
	}
	t.Seconds += f.add
	if t.Sensitivity != nil {
		s := *t.Sensitivity
		s.Lo, s.Hi = s.Lo+f.add, s.Hi+f.add
		t.Sensitivity = &s
	}
	return t
}

func (f *loadFleet) TTFTAt(rate float64) whatif.Result { return f.shifted(f.Fleet.TTFTAt(rate)) }

func (f *loadFleet) TTFTPoint(rate float64) (whatif.Result, []string) {
	r, notes := f.Fleet.TTFTPoint(rate)
	return f.shifted(r), notes
}

// costLoad is the seconds a request the stub's fleets load at phi 1.
const costLoad = 0.2

// costRate is the rate of costSeries, a quarter of the stub's knee at 2
// prefill pods and 1000 tokens, 20 r/s.
const costRate = 5.0

// costSeries is n cycles at costRate whose measured mean TTFT is the stub's
// price there plus phi x costLoad: the TTFT a fleet whose cached tokens cost
// phi would measure.
func costSeries(n int, phi float64) ([]SeriesSample, []LatencySample) {
	s, l := btSeries(n, 0, 1)
	m := 0.1/(1-costRate/20) + phi*costLoad
	for i := range s {
		s[i].Rate = costRate
		// btRouter's mean is 1.2 x its scale.
		l[i].Router = btRouter(costRate, m/1.2)
	}
	return s, l
}

// fitOn is FitCachedTokenCost on series s and l, priced by the stub with
// costLoad seconds a request at phi 1.
func fitOn(t *testing.T, s []SeriesSample, l []LatencySample, share float64) CachedTokenCost {
	t.Helper()
	return fitWith(t, s, l, &loadPricer{r: &stubResolver{}, load: costLoad}, share)
}

// fitWith is fitOn priced by p.
func fitWith(t *testing.T, s []SeriesSample, l []LatencySample, p *loadPricer, share float64) CachedTokenCost {
	t.Helper()
	base, err := p.r.Resolve(context.Background(), InputsRequest{})
	require.NoError(t, err)
	res := &Resolved{Inputs: base.Inputs, Content: "cost", Pricer: p, Series: s, Latency: l,
		Config: btConfig("H100")}
	c, err := FitCachedTokenCost(context.Background(), res, s[len(s)-1].At, share)
	require.NoError(t, err)
	return c
}

// A fleet whose cached tokens cost a known phi measures its TTFT that much
// above the price at phi 0: the fit recovers phi, on each half of the steps
// too, and a fleet whose TTFT the price at phi 0 already reads high fits
// phi 0, never under it.
func TestFitCachedTokenCost_RecoversAKnownCost(t *testing.T) {
	for _, want := range []float64{0.3, 0.75} {
		s, l := costSeries(4*240, want)
		c := fitOn(t, s, l, 0.9)
		require.True(t, c.Fitted, c.Why)
		assert.InDelta(t, want, c.Phi, 0.01)
		assert.GreaterOrEqual(t, c.Steps, MinCachedTokenCostSteps)
		require.NotNil(t, c.FirstHalf)
		require.NotNil(t, c.SecondHalf)
		assert.InDelta(t, want, *c.FirstHalf, 0.01)
		assert.InDelta(t, want, *c.SecondHalf, 0.01)
		assert.Contains(t, c.Words(), "fitted on")
	}
	// The median holds against a minority of steps read far off.
	s, l := costSeries(4*240, 0.5)
	for i := 0; i < len(l); i += 7 * 20 {
		for j := i; j < min(i+20, len(l)); j++ {
			l[j].Router = btRouter(5, 3)
		}
	}
	c := fitOn(t, s, l, 0.9)
	require.True(t, c.Fitted, c.Why)
	assert.InDelta(t, 0.5, c.Phi, 0.01)
	// Measured under the price at phi 0: phi is held at 0.
	s, l = costSeries(4*240, -0.5)
	c = fitOn(t, s, l, 0.9)
	require.True(t, c.Fitted, c.Why)
	assert.Zero(t, c.Phi)
}

// Too few steps, or too little of the prompt cached, leave phi at 0 with the
// reason.
func TestFitCachedTokenCost_TooFewSteps(t *testing.T) {
	s, l := costSeries(30, 0.5)
	c := fitOn(t, s, l, 0.9)
	assert.False(t, c.Fitted)
	assert.Zero(t, c.Phi)
	assert.Contains(t, c.Why, CachedCostTooFewSteps)
	assert.Contains(t, c.Words(), "not fitted")
	s, l = costSeries(4*240, 0.5)
	c = fitOn(t, s, l, 0.05)
	assert.False(t, c.Fitted)
	assert.Zero(t, c.Phi)
	assert.Contains(t, c.Why, CachedCostNoShare)
	assert.Zero(t, c.Steps)
}

// A step past the knee is not fitted on, even where it is priced: its
// TTFT is the queue's, not the load's. Here two of every three 15-minute
// blocks run one decode pod, whose knee, 12 r/s, the 15 r/s they carry is
// past (the prefill knee, 20 r/s, is not, so no backlog carries into the
// steps after them), and measure far under their price there; the steps
// below the knee alone set phi.
func TestFitCachedTokenCost_SkipsStepsPastTheKnee(t *testing.T) {
	const want = 0.5
	s, l := costSeries(8*240, want)
	past := 0
	for i := range s {
		if (i/60)%3 != 0 {
			s[i].Rate, s[i].Decode = 15, 1
			l[i].Router = btRouter(15, 1/1.2)
			past++
		}
	}
	require.Positive(t, past)
	c := fitWith(t, s, l, &loadPricer{r: &stubResolver{}, load: costLoad, pastKnee: true}, 0.9)
	require.True(t, c.Fitted, c.Why)
	assert.InDelta(t, want, c.Phi, 0.01)
}
