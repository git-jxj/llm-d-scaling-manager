package prefill

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// plateauVP is the synthetic prefill engine's capacity, tok/s, and
// plateauPrompt its computed tokens per request.
const (
	plateauVP     = 20000.0
	plateauPrompt = 2000.0
)

// psMinute is a minute of p-0 at utilization rho of a processor-sharing
// engine of plateauVP: it computes rho*plateauVP tok/s with no queue, and
// holds rho/(1 - rho) requests in prefill (Little's law), so its mean
// prefill time is that over its request rate.
func psMinute(rho float64) domain.ReplicaMetrics {
	rm := base("p-0")
	rate := rho * plateauVP
	rm.PrefillComputedTokenRate, rm.RequestRate = rate, rate/plateauPrompt
	rm.AvgPrefillTime, rm.AvgPrefillTimeKnown = rho/(1-rho)/rm.RequestRate, true
	rm.AvgServiceTime, rm.AvgTTFT = rm.AvgPrefillTime, rm.AvgPrefillTime+0.1
	return rm
}

// rampCycles is per minutes at each utilization of rhos, in order, the
// plateau switch on.
func rampCycles(per int, rhos ...float64) []Cycle {
	out := make([]Cycle, 0, per*len(rhos))
	for _, rho := range rhos {
		for range per {
			c := cycle(time.Time{}, psMinute(rho))
			c.VPOccupancyPlateau = true
			out = append(out, c)
		}
	}
	return out
}

// flatRamp is a staircase that drives the engine from 30% to 95%
// utilization, eight minutes a step: requests in prefill 0.43 to 19 per
// engine. From x = 9 to 19 the served rate moves from 18k to 19k tok/s.
func flatRamp() []Cycle {
	return rampCycles(8, 0.3, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95)
}

// A curve that flattens in the window is V_P: the served rate at its
// plateau, not a bound. Off, the fleet never queued and V_P is refused.
func TestPlateau_AFlatCurveIsVP(t *testing.T) {
	est := run(NewObserver(), flatRamp()...)
	require.Empty(t, est.VPRefused, "%+v", est)
	assert.Equal(t, SourceOccupancyPlateau, est.VPSource)
	assert.InDelta(t, 19000.0, est.VP, 1e-6, "the highest bin's median: 0.95 of the engine's 20k")
	assert.False(t, est.VPLowerBound)
	assert.True(t, est.VPPlateauFlat)
	assert.Equal(t, 16, est.VPPlateauMinutes, "the 90% and 95% steps, within 10% of the top")
	assert.InDelta(t, 19.0/9.0, est.VPPlateauSpan, 1e-9)
	assert.InDelta(t, plateauPrompt, est.VPPromptTokens, 1e-9)

	cs := flatRamp()
	for i := range cs {
		cs[i].VPOccupancyPlateau = false
	}
	off := run(NewObserver(), cs...)
	assert.Equal(t, NoPrefillCapacity, off.VPRefused)
	assert.Zero(t, off.VPPlateau, "off, the plateau is not read")
}

// A curve still rising at the highest occupancy seen is a lower bound: its
// highest bin, flagged.
func TestPlateau_ARisingCurveIsALowerBound(t *testing.T) {
	est := run(NewObserver(), rampCycles(8, 0.3, 0.5, 0.6, 0.7)...)
	require.Empty(t, est.VPRefused, "%+v", est)
	assert.Equal(t, SourceOccupancyPlateau, est.VPSource)
	assert.InDelta(t, 14000.0, est.VP, 1e-6)
	assert.True(t, est.VPLowerBound)
	assert.False(t, est.VPPlateauFlat)
	assert.InDelta(t, 1.0, est.VPPlateauSpan, 1e-9, "one bin: 12k at x 1.5 is more than 10% under 14k")
}

// Too few minutes is no plateau: the 90% and 95% steps of two minutes each
// are too few for their bins to count, so the highest counted bin, the 80%
// step, is a lower bound.
func TestPlateau_TooFewMinutesIsALowerBound(t *testing.T) {
	cs := append(rampCycles(8, 0.3, 0.5, 0.6, 0.7, 0.8), rampCycles(2, 0.9, 0.95)...)
	est := run(NewObserver(), cs...)
	assert.InDelta(t, 16000.0, est.VP, 1e-6)
	assert.True(t, est.VPLowerBound)
	assert.False(t, est.VPPlateauFlat)
}

// Under one request in prefill per engine the engine idled part of every
// minute, so no plateau is read there, however flat: a steady light load
// whose occupancy wanders from 0.25 to 0.8 stays refused.
func TestPlateau_UnderOneRequestInPrefillIsNoPlateau(t *testing.T) {
	cs := make([]Cycle, 0, 40)
	for i := range 40 {
		rm := psMinute(0.4)
		rm.AvgPrefillTime = []float64{0.25, 0.5, 0.8}[i%3] / rm.RequestRate
		c := cycle(time.Time{}, rm)
		c.VPOccupancyPlateau = true
		cs = append(cs, c)
	}
	est := run(NewObserver(), cs...)
	assert.Equal(t, NoPrefillCapacity, est.VPRefused)
	assert.Zero(t, est.VPPlateau)
}

// A V_P learned from the queue wins: busy minutes of a single-rank pod are
// V_P itself, and a lower bound at or above the plateau stands. The plateau
// is read beside them.
func TestPlateau_AQueueLearnedVPWins(t *testing.T) {
	t.Run("busy minutes", func(t *testing.T) {
		cs := flatRamp()
		for range 5 {
			c := cycle(time.Time{}, queued("p-0", 10000, 3))
			c.QueueMin = map[string]float64{"p-0": 2}
			c.VPOccupancyPlateau = true
			cs = append(cs, c)
		}
		est := run(NewObserver(), cs...)
		assert.Equal(t, SourceBusyMinutes, est.VPSource)
		assert.Equal(t, 10000.0, est.VP)
		assert.False(t, est.VPLowerBound)
		assert.InDelta(t, 19000.0, est.VPPlateau, 1e-6)
	})
	t.Run("a lower bound above the plateau", func(t *testing.T) {
		cs := queuedMinutes(25000, 25000, 25000)
		for i := range cs {
			cs[i].VPOccupancyPlateau = true
		}
		est := run(NewObserver(), append(cs, flatRamp()...)...)
		assert.Equal(t, SourcePeakUnderQueue, est.VPSource)
		assert.Equal(t, 25000.0, est.VP)
		assert.True(t, est.VPLowerBound)
		assert.True(t, est.VPPlateauFlat)
	})
}

// A lower bound under a flattened plateau is raised to it, and is no longer
// a bound: the light queued minutes of issue
// llm-d-incubation/llm-d-scaling-manager#280 under a fleet seen at its limit.
func TestPlateau_RaisesALowerBoundUnderIt(t *testing.T) {
	cs := queuedMinutes(5000, 5000, 5000)
	for i := range cs {
		cs[i].VPOccupancyPlateau = true
	}
	est := run(NewObserver(), append(cs, flatRamp()...)...)
	assert.Equal(t, SourceOccupancyPlateau, est.VPSource)
	assert.InDelta(t, 19000.0, est.VP, 1e-6)
	assert.False(t, est.VPLowerBound)
	assert.False(t, est.VPFlooredAtServed)
}

// A rising plateau composes with the served-peak floor: the larger lower
// bound stands, flagged.
func TestPlateau_ARisingPlateauIsFlooredAtTheServedPeak(t *testing.T) {
	cs := rampCycles(8, 0.3, 0.5, 0.6, 0.7)
	for _, rate := range []float64{17000, 17000, 17000} {
		rm := psMinute(0.3)
		rm.PrefillComputedTokenRate = rate
		c := cycle(time.Time{}, rm)
		c.VPOccupancyPlateau = true
		cs = append(cs, c)
	}
	for i := range cs {
		cs[i].VPFloorServed = true
	}
	est := run(NewObserver(), cs...)
	assert.Equal(t, SourceOccupancyPlateau, est.VPSource)
	assert.Equal(t, 17000.0, est.VP)
	assert.True(t, est.VPLowerBound)
	assert.True(t, est.VPFlooredAtServed)
	assert.InDelta(t, 14000.0, est.VPPlateau, 1e-6)
}

// The completions are the computed-token histogram's +Inf bucket where the
// row carries one, the request rate otherwise; the occupancy is per engine.
func TestPlateau_OccupancyByLittlesLaw(t *testing.T) {
	rm := base("p-0")
	rm.RequestRate, rm.AvgPrefillTime, rm.AvgPrefillTimeKnown = 4, 0.5, true
	x, ok := rowOccupancy(rm)
	require.True(t, ok)
	assert.InDelta(t, 2.0, x, 1e-12)

	rm.ComputedTokensBuckets = []domain.LengthBucket{{UpperBound: 1000, Rate: 3}, {UpperBound: math.Inf(1), Rate: 6}}
	rm.EngineRanks = 4
	x, ok = rowOccupancy(rm)
	require.True(t, ok)
	assert.InDelta(t, 0.75, x, 1e-12, "6 completions/s x 0.5 s over 4 engines")

	rm.AvgPrefillTimeKnown = false
	_, ok = rowOccupancy(rm)
	assert.False(t, ok, "no prefill time, no occupancy")
}
