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

// A busy fleet of LWS groups: a row is a group of four engines, every
// minute busy, so no minute is low load and a comes only from the engines'
// own prefill timing (timing.go).

const (
	busyRanks    = 4
	busyA0       = 0.250 // the fixed part of a request's prefill, seconds
	busyPerToken = 80e-6 // a lone request's seconds per computed token
	busyOverhead = 0.400 // TTFT outside the prefill and the queue, seconds
	busyQueue    = 0.050 // the queue wait, seconds
)

// busyRow is one minute of group pod: rr requests/s over the group, ieff
// computed tokens each. The per-token part stretches with the per-engine
// occupancy L, as a shared pod's does; the fixed part does not:
// T = a0 + b*ieff*(1 + L), solved for T since L = rr*T/ranks.
func busyRow(pod string, rr, ieff float64) domain.ReplicaMetrics {
	b, l := busyPerToken*ieff, rr/busyRanks
	// T = a0 + b(1 + l T)  =>  T = (a0 + b) / (1 - b l)
	prefill := (busyA0 + b) / (1 - b*l)
	rm := base(pod)
	rm.EngineRanks = busyRanks
	rm.RequestRate, rm.PrefillComputedTokenRate = rr, rr*ieff
	rm.AvgServiceTime = prefill
	rm.AvgPrefillTime, rm.AvgPrefillTimeKnown = prefill, true
	rm.AvgQueueTime, rm.AvgQueueTimeKnown = busyQueue, true
	rm.AvgTTFT, rm.AvgTTFTKnown = prefill+busyQueue+busyOverhead, true
	rm.QueueLength = 2
	return rm
}

// busyMinutes is n minutes of two groups at loads and prompts drawn from a
// seeded source: a queue on every minute, so never low load
// whether the row is read whole or per rank.
func busyMinutes(n int, on bool) []Cycle {
	rng := rand.New(rand.NewPCG(7, 11))
	out := make([]Cycle, n)
	for i := range out {
		c := cycle(t0.Add(time.Duration(i)*minute), busyRow("g-0", 1+3*rng.Float64(), 1000+5000*rng.Float64()),
			busyRow("g-1", 1+3*rng.Float64(), 1000+5000*rng.Float64()))
		c.AFromTiming, c.ALowLoadPerRank = on, on
		out[i] = c
	}
	return out
}

// On, a is the intercept of the quieter half's line plus the median
// overhead, flagged by its source; off, it is refused for too few low-load
// minutes, as P1 shipped. The fitted intercept reads a0 within 10%: the
// quieter half still stretches a little, which the line takes into its
// slope.
func TestTiming_ABusyFleetLearnsAFromEngineTiming(t *testing.T) {
	on := run(NewObserver(), busyMinutes(120, true)...)
	require.Equal(t, Refusal(""), on.ARefused, "on: %+v", on)
	assert.Equal(t, ASourceEngineTiming, on.ASource)
	assert.InEpsilon(t, busyA0, on.AIntercept, 0.10)
	assert.True(t, on.AOverheadKnown)
	assert.InDelta(t, busyOverhead, on.AOverhead, 1e-9)
	assert.InDelta(t, on.AIntercept+busyOverhead, on.A, 1e-12)
	assert.GreaterOrEqual(t, on.ATimingMinutes, MinTimingMinutes)
	assert.Equal(t, on.ATimingMinutes, on.ASamples)
	assert.Greater(t, on.ATimingOccupancy, LowLoadOccupancy, "no minute was low load")
	assert.Zero(t, on.SlopeBound, "a shared pod's slope bounds nothing")

	off := run(NewObserver(), busyMinutes(120, false)...)
	assert.Equal(t, TooFewLowLoadMinutes, off.ARefused)
	assert.Zero(t, off.A)
	assert.Empty(t, off.ASource)
}

// Every minute in the fit (the low-load one too, had there been one) shares
// one model, so a fit over every minute, not the quieter half, reads the
// intercept low: the half is what keeps a near a0.
func TestTiming_TheQuieterHalfKeepsTheIntercept(t *testing.T) {
	var xs, ys []float64
	for _, c := range busyMinutes(120, true) {
		for _, rm := range c.Rows {
			xs, ys = append(xs, rm.PrefillComputedTokenRate/rm.RequestRate), append(ys, rm.AvgPrefillTime)
		}
	}
	all, _ := ols(xs, ys)
	est := run(NewObserver(), busyMinutes(120, true)...)
	require.Equal(t, ASourceEngineTiming, est.ASource)
	assert.Less(t, math.Abs(est.AIntercept-busyA0), math.Abs(all-busyA0))
}

// Too few minutes, prompts of one length, and a fit with no TTFT: each as
// the low-load fit treats them.
func TestTiming_Refusals(t *testing.T) {
	est := run(NewObserver(), busyMinutes(25, true)...)
	assert.Equal(t, TooFewLowLoadMinutes, est.ARefused, "25 minutes of two groups: 25 in the quieter half, under 30")
	assert.Equal(t, TooFewTimedMinutes, est.ATimingRefused)

	one := make([]Cycle, 120)
	for i := range one {
		c := cycle(t0.Add(time.Duration(i)*minute), busyRow("g-0", 2+float64(i%7)/2, 3000))
		c.AFromTiming = true
		one[i] = c
	}
	est = run(NewObserver(), one...)
	assert.Equal(t, TooFewLowLoadMinutes, est.ARefused)
	assert.Equal(t, NoPromptSpread, est.ATimingRefused)

	noTTFT := busyMinutes(120, true)
	for i := range noTTFT {
		for j := range noTTFT[i].Rows {
			noTTFT[i].Rows[j].AvgTTFTKnown = false
		}
	}
	est = run(NewObserver(), noTTFT...)
	require.Equal(t, ASourceEngineTiming, est.ASource)
	assert.False(t, est.AOverheadKnown)
	assert.Equal(t, est.AIntercept, est.A)

	noTiming := busyMinutes(120, true)
	for i := range noTiming {
		for j := range noTiming[i].Rows {
			noTiming[i].Rows[j].AvgPrefillTimeKnown = false
		}
	}
	est = run(NewObserver(), noTiming...)
	assert.Equal(t, TooFewLowLoadMinutes, est.ARefused)
	assert.Equal(t, TooFewTimedMinutes, est.ATimingRefused)
}

// Where the low-load fit has its minutes, it stands, and the timing fit is
// not read.
func TestTiming_TheLowLoadFitStandsWhereItHasTheMinutes(t *testing.T) {
	cs := lowLoadMinutes(0.010, 1e-6, steadyPrompts(1000, 2000, 4000)...)
	for i := range cs {
		cs[i].AFromTiming = true
	}
	est := run(NewObserver(), cs...)
	require.Equal(t, Refusal(""), est.ARefused)
	assert.Equal(t, ASourceLowLoad, est.ASource)
	assert.Zero(t, est.ATimingMinutes)
}

// A group of four engines each at occupancy 0.08 reads 0.32 whole: refused
// as P1 shipped, admitted per rank.
func TestLowLoadPerRank_AGroupOfQuietEnginesIsLowLoad(t *testing.T) {
	group := func(perRank bool) Estimate {
		o := NewObserver()
		var est Estimate
		for i, ieff := range steadyPrompts(1000, 2000, 4000) {
			svc := 0.010 + 1e-6*ieff
			rr := busyRanks * 0.08 / svc
			rm := lowLoad(ieff, svc, rr, 0.002)
			rm.EngineRanks = busyRanks
			c := cycle(t0.Add(time.Duration(i)*minute), rm)
			c.ALowLoadPerRank = perRank
			est = o.Observe(c)
		}
		return est
	}
	on := group(true)
	require.Equal(t, Refusal(""), on.ARefused, "%+v", on)
	assert.InDelta(t, 0.012, on.A, 1e-9)
	off := group(false)
	assert.Equal(t, TooFewLowLoadMinutes, off.ARefused)
	assert.Zero(t, off.ASamples)
}

// A fleet that only computes long prompts still gets a, marked extrapolated:
// its intercept is read from prompts past MaxAShortestPrompt and may read
// low. The default fleet's shorter prompts are not.
func TestTiming_LongPromptsAreFlaggedNotRefused(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 17))
	long := make([]Cycle, 120)
	for i := range long {
		c := cycle(t0.Add(time.Duration(i)*minute), busyRow("g-0", 0.5+rng.Float64(), 4000+8000*rng.Float64()),
			busyRow("g-1", 0.5+rng.Float64(), 4000+8000*rng.Float64()))
		c.AFromTiming = true
		long[i] = c
	}
	est := run(NewObserver(), long...)
	require.Equal(t, ASourceEngineTiming, est.ASource, "%+v", est)
	assert.Empty(t, est.ARefused)
	assert.True(t, est.AExtrapolated)
	assert.Greater(t, est.AShortestPrompt, MaxAShortestPrompt)

	short := run(NewObserver(), busyMinutes(120, true)...)
	require.Equal(t, ASourceEngineTiming, short.ASource)
	assert.False(t, short.AExtrapolated)
	assert.Positive(t, short.AShortestPrompt)
	assert.LessOrEqual(t, short.AShortestPrompt, MaxAShortestPrompt)
}
