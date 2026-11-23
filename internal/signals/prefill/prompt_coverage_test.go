package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The prompts a V_P speaks for (Cycle.VPPromptCoverage; issue
// llm-d-incubation/llm-d-scaling-manager#405).

// coverageMinutes is one busy minute of p-0 per prompt, each computing 30k
// tok/s, with the coverage switch set to on.
func coverageMinutes(on bool, prompts ...float64) []Cycle {
	out := make([]Cycle, len(prompts))
	for i, p := range prompts {
		rm := queued("p-0", 30000, 3)
		rm.RequestRate = 30000 / p
		out[i] = cycle(t0.Add(minute*time.Duration(i)), rm)
		out[i].QueueMin = map[string]float64{"p-0": 1}
		out[i].VPPromptCoverage = on
	}
	return out
}

// Busy minutes carry the 10th and 90th percentiles of their prompts, by the
// nearest rank: of ten minutes at 1k to 10k, the first and the ninth.
func TestPromptCoverage_BusyMinutesCarryTheirRange(t *testing.T) {
	est := run(NewObserver(), coverageMinutes(true, 4000, 1000, 9000, 2000, 6000, 3000, 10000, 5000, 7000, 8000)...)
	require.Equal(t, SourceBusyMinutes, est.VPSource)
	assert.InDelta(t, 1000.0, est.VPPromptLow, 1e-6)
	assert.InDelta(t, 9000.0, est.VPPromptHigh, 1e-6)
	assert.InDelta(t, 5500.0, est.VPPromptTokens, 1e-6, "the mean is kept beside the range")

	// A peak under a queue carries its run's minutes: the last three, the
	// fastest run.
	cs := coverageMinutes(true, 1000, 2000, 8000, 8100, 7900)
	for i := range cs {
		cs[i].QueueMin = nil
		if i < 2 {
			cs[i].Rows[0].PrefillComputedTokenRate = 20000
			cs[i].Rows[0].RequestRate = 20000 / []float64{1000, 2000}[i]
		}
	}
	pk := run(NewObserver(), cs...)
	require.Equal(t, SourcePeakUnderQueue, pk.VPSource)
	assert.InDelta(t, 7900.0, pk.VPPromptLow, 1e-6)
	assert.InDelta(t, 8100.0, pk.VPPromptHigh, 1e-6)
}

// Off, no range is carried, and V_P is unchanged.
func TestPromptCoverage_OffCarriesNone(t *testing.T) {
	prompts := []float64{4000, 1000, 9000, 2000, 6000}
	on := run(NewObserver(), coverageMinutes(true, prompts...)...)
	off := run(NewObserver(), coverageMinutes(false, prompts...)...)
	assert.Zero(t, off.VPPromptLow)
	assert.Zero(t, off.VPPromptHigh)
	assert.Equal(t, on.VP, off.VP)
	assert.Equal(t, on.VPPromptTokens, off.VPPromptTokens)
	assert.Positive(t, on.VPPromptHigh)
}

// A range is a reading of every minute: one minute with no prompt read makes
// it unknown, as the mean is.
func TestPromptCoverage_AnUnreadPromptIsNoRange(t *testing.T) {
	lo, hi := promptRange([]float64{1000, 0, 3000})
	assert.Zero(t, lo)
	assert.Zero(t, hi)
	lo, hi = promptRange([]float64{3000})
	assert.Equal(t, 3000.0, lo)
	assert.Equal(t, 3000.0, hi)
}
