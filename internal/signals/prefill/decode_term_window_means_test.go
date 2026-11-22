package prefill

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// noisyCycle is one cycle of a production-like fleet: the rate, the prompt and
// both engines' TTFTs move every cycle from noise. trueHop is the router's
// remainder past both engines.
type noisyCycle struct {
	at                    time.Time
	rate, prompt, pre, de float64
}

// noisyRun feeds a window a fleet of n cycles, 15 s apart, from cycle(i),
// with the router's five-minute mean the request-weighted mean of the
// engines' over its window plus trueHop, as a router reads it. edit, when not
// nil, changes each cycle before it is observed.
func noisyRun(windowMeans bool, n int, trueHop float64, cycle func(i int) noisyCycle, edit func(i int, c *DecodeTermCycle)) HopFit {
	w := NewDecodeTermWindow()
	var past []noisyCycle
	for i := range n {
		nc := cycle(i)
		nc.at = termT0.Add(time.Duration(i) * 15 * time.Second)
		past = append(past, nc)
		var sum, count float64
		for _, p := range past {
			if nc.at.Sub(p.at) < 300*time.Second {
				sum += p.rate * (p.pre + p.de + trueHop)
				count += p.rate
			}
		}
		pre := domain.ReplicaMetrics{PodName: "p", Ready: true, AvgTTFT: nc.pre, AvgTTFTKnown: true, RequestRate: nc.rate, RequestRateKnown: true,
			CapacityQueueKnown: true}
		dec := domain.ReplicaMetrics{PodName: "d", Ready: true, AvgTTFT: nc.de, AvgTTFTKnown: true, RequestRate: nc.rate, RequestRateKnown: true}
		c := DecodeTermCycle{Now: nc.at, Prefill: []domain.ReplicaMetrics{pre}, Decode: []domain.ReplicaMetrics{dec}, PromptTokens: nc.prompt,
			Router:      &domain.RouterLatency{WindowSeconds: 300, TTFTSumRate: sum / 300, TTFTCountRate: count / 300},
			HopUnqueued: true, HopWindowMeans: windowMeans}
		if edit != nil {
			edit(i, &c)
		}
		w.Observe(c)
	}
	return w.Estimate().Hop
}

// production is a level fleet with production's noise: the one-minute rate
// within 30% of its level, the prompt within 25%, each engine's TTFT within
// 20%, drawn fresh every cycle.
func production(seed uint64) func(i int) noisyCycle {
	rng := rand.New(rand.NewPCG(seed, 1))
	u := func(spread float64) float64 { return 1 + spread*(2*rng.Float64()-1) }
	return func(int) noisyCycle {
		return noisyCycle{rate: 8 * u(0.3), prompt: 80000 * u(0.25), pre: 1.4 * u(0.2), de: 0.48 * u(0.2)}
	}
}

// The case the switch is for: per-cycle noise defeats the per-cycle gates in
// every window, so off R is not learned and the assumed hop stands; on, R is
// read from the windows' means and is the true remainder, flagged.
func TestHop_WindowMeansRecoverRUnderPerCycleNoise(t *testing.T) {
	const trueHop = 0.6
	off := noisyRun(false, 6*240, trueHop, production(7), nil)
	assert.Equal(t, TooFewHopWindows, off.Refused, "off: no window passes the per-cycle gates")
	assert.Zero(t, off.Windows)

	on := noisyRun(true, 6*240, trueHop, production(7), nil)
	require.Empty(t, on.Refused)
	assert.True(t, on.FromWindowMeans)
	assert.GreaterOrEqual(t, on.Windows, MinHopMeanWindows)
	assert.InDelta(t, trueHop, on.Base, 1e-9, "each window's R is the router's mean less the engines' request-weighted means")
	assert.Zero(t, on.PerToken)
	assert.InDelta(t, 80000, on.HeldPromptTokens, 0.05*80000, "held at the median prompt, not the noise's low tail")
}

// Negative control: a ramp, the reason the steadiness gates exist, is still
// kept out on window means. The rate doubles every five minutes, so a
// window's later half carries about 1.4 times its earlier half's rate.
func TestHop_WindowMeansKeepARampOut(t *testing.T) {
	ramp := func(i int) noisyCycle {
		return noisyCycle{rate: 0.5 * math.Pow(2, float64(i)/20), prompt: 8000, pre: 0.4, de: 0.05}
	}
	f := noisyRun(true, 240, 0.02, ramp, nil)
	assert.Equal(t, TooFewHopWindows, f.Refused)
	assert.Zero(t, f.Windows)

	// A prompt that climbs 30% a window, about 14% between its halves, is kept
	// out the same way.
	climb := func(i int) noisyCycle {
		return noisyCycle{rate: 5, prompt: 8000 * math.Pow(1.3, float64(i)/20), pre: 0.4, de: 0.05}
	}
	f = noisyRun(true, 240, 0.02, climb, nil)
	assert.Equal(t, TooFewHopWindows, f.Refused)
	assert.Zero(t, f.Windows)
}

// Negative controls: the held and queued gates hold on window means, the
// queue's whatever HopUnqueued says, and too few windows give no R.
func TestHop_WindowMeansKeepTheHeldAndQueuedGates(t *testing.T) {
	queued := noisyRun(true, 6*240, 0.6, production(3), func(_ int, c *DecodeTermCycle) {
		c.Prefill[0].CapacityQueueLength, c.Prefill[0].QueueLength = 2, 2
		c.HopUnqueued = false
	})
	assert.Equal(t, TooFewHopWindows, queued.Refused)
	assert.Zero(t, queued.Windows)

	held := noisyRun(true, 6*240, 0.6, production(3), func(_ int, c *DecodeTermCycle) { c.RouterHeld = true })
	assert.Equal(t, TooFewHopWindows, held.Refused)
	assert.Zero(t, held.Windows)

	// Every fourth minute queued: the windows over it are out, the rest in.
	some := noisyRun(true, 6*240, 0.6, production(3), func(i int, c *DecodeTermCycle) {
		if i%16 < 4 {
			c.Prefill[0].CapacityQueueLength = 2
		}
	})
	all := noisyRun(true, 6*240, 0.6, production(3), nil)
	assert.Less(t, some.Windows, all.Windows)

	short := noisyRun(true, 4*30, 0.6, production(3), nil)
	assert.Equal(t, TooFewHopWindows, short.Refused, "half an hour of windows is the least")
	assert.Positive(t, short.Windows)
	assert.Less(t, short.Windows, MinHopMeanWindows)
	assert.True(t, short.FromWindowMeans)
}

// The fit is a median: a minority of windows far off moves it little, and a
// median not above zero is refused.
func TestHop_WindowMeansFitIsAMedian(t *testing.T) {
	var samples []hopSample
	for i := range 40 {
		r := 0.6
		if i%4 == 0 {
			r = 5
		}
		samples = append(samples, hopSample{r: r, prompt: 1000 + float64(i)})
	}
	f := fitHopMeans(samples)
	require.Empty(t, f.Refused)
	assert.InDelta(t, 0.6, f.Base, 1e-12)
	assert.InDelta(t, 1019.5, f.HeldPromptTokens, 1e-9)
	assert.Equal(t, 40, f.Windows)

	for i := range samples {
		samples[i].r = -0.01
	}
	assert.Equal(t, NonPositiveHop, fitHopMeans(samples).Refused)
}

// On a steady fleet both reads agree: the switch changes nothing a per-cycle
// window already read, but the source and the hold.
func TestHop_WindowMeansAgreeOnASteadyFleet(t *testing.T) {
	steady := func(int) noisyCycle { return noisyCycle{rate: 10, prompt: 8000, pre: 0.05, de: 0.03} }
	off := noisyRun(false, 4*60, 0.007, steady, nil)
	on := noisyRun(true, 4*60, 0.007, steady, nil)
	require.Empty(t, off.Refused)
	require.Empty(t, on.Refused)
	assert.InDelta(t, off.Base, on.Base, 1e-9)
	assert.Equal(t, off.HeldPromptTokens, on.HeldPromptTokens)
	assert.False(t, off.FromWindowMeans)
	assert.True(t, on.FromWindowMeans)
}
