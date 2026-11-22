package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// With both switches, the router hop read on window means and the held
// speculating D, decode is counted once: R is the router's mean less both
// engines', so D + R is the router's mean less the prefill engine's.
func TestHop_WindowMeansAndTheHeldSpeculatingDCountDecodeOnce(t *testing.T) {
	const prefillTTFT, decodeTTFT, hop = 1.4, 0.48, 0.6
	run := func(spec bool) DecodeTermEstimate {
		w := NewDecodeTermWindow()
		at := termT0
		for i := range 60 * 4 {
			rate := 1.5
			if i%2 == 1 {
				rate = 1.9
			}
			c := hopCycle(at, prefillTTFT+decodeTTFT+hop, rate, 2600)
			c.Prefill[0].AvgTTFT = prefillTTFT
			dec := decodeRow("d", decodeTTFT, 0.04, 85000)
			dec.RequestRate = rate
			dec.SpecAcceptedPerStep, dec.SpecAcceptedPerStepKnown = 1.85, true
			c.Decode = []domain.ReplicaMetrics{dec}
			c.HopUnqueued, c.HopWindowMeans = true, true
			c.SpeculatingHeld, c.ITLPerToken = spec, true
			w.Observe(c)
			at = at.Add(15 * time.Second)
		}
		return w.Estimate()
	}
	off, on := run(false), run(true)
	require.Empty(t, on.Hop.Refused)
	assert.InDelta(t, hop, on.Hop.Base, 1e-6, "R on window means holds no decode TTFT")
	assert.Equal(t, off.Hop, on.Hop, "the held D does not touch R")
	assert.Zero(t, off.Held.Seconds)
	assert.InDelta(t, decodeTTFT+hop, on.Held.Seconds+on.Hop.Base, 1e-6, "decode counted once")
}
