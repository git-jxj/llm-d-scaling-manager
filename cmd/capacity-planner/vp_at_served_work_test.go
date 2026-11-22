package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// servedFleetCase is a P/D fleet's prefill side whose V_P is a lower bound
// from a queue peak, with the prefill pods' sustained work above what it
// serves: the client fleet's shape, 5 pods at 3270 computed tokens a request,
// f 1.39, a V_P that gives a knee near 21.5 r/s, and 85536 tokens a second of
// served work (26.2 r/s).
func servedFleetCase(on bool) *pricedFleet {
	pf := &pricedFleet{vpAtServedOn: on, servedComputed: map[int]float64{5: 85536}}
	pf.fleet.Prefill = whatif.Prefill{VP: 19500, VPLowerBound: true, FixedLatency: 1.195, Imbalance: 1.39, Replicas: 5,
		PromptTokens: 3270, PromptCV: 0.5}
	return pf
}

// prefillOnlyTTFT is the prefill side's mean TTFT at rate.
func prefillOnlyTTFT(t *testing.T, pf *pricedFleet, rate float64) float64 {
	t.Helper()
	r := pf.priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, rate, 0) })
	v, ok := r.(whatif.TTFT)
	require.True(t, ok, "%#v", r)
	return v.Seconds
}

// With prefill-vp-at-served-work on, the TTFT is priced from the V_P the
// floored knee rests on: the knee is the served work's rate either way, and
// at 80% of it the TTFT no longer reads the prefill pods past their capacity.
// Off, as the served floor left it: the knee is floored and the TTFT is
// priced from the lower V_P, many times what the pods served at.
func TestTTFT_VPAtServedWorkPricesTheTTFTAtTheFlooredKnee(t *testing.T) {
	off, on := servedFleetCase(false), servedFleetCase(true)
	kOff, ok := off.knee().(whatif.Answer)
	require.True(t, ok)
	kOn, ok := on.knee().(whatif.Answer)
	require.True(t, ok)
	floor := 85536 / 3270.0
	assert.InDelta(t, floor, kOff.Value, 1e-9, "off: the knee is floored at the served work")
	assert.InDelta(t, floor, kOn.Value, 1e-6, "on: the raised V_P gives the same knee")
	assert.Contains(t, kOff.Flags, kneeAtServedWork)

	rate := 0.8 * floor
	tOff, tOn := prefillOnlyTTFT(t, off, rate), prefillOnlyTTFT(t, on, rate)
	t.Logf("mean TTFT at 80%% of the %.2f r/s knee: off %.2f s, on %.2f s", floor, tOff, tOn)
	assert.Greater(t, tOff, 3*tOn, "off: the lower V_P reads the pods near or past capacity")
	assert.Less(t, tOn, 10.0)

	ratio, ok := on.servedRatio(on.fleet.Prefill)
	require.True(t, ok)
	assert.InDelta(t, floor/whatif.PrefillKnee(on.fleet.Prefill).(whatif.Answer).Value, ratio, 1e-12)
	on.noteAtServedWork()
	assert.Contains(t, on.flags, vpAtServedWork)
	off.noteAtServedWork()
	assert.NotContains(t, off.flags, vpAtServedWork)
}

// Nothing moves where the knee is no lower bound, where the served work is
// below the knee, where V_P is stated, or with no served work at the pod
// count priced.
func TestTTFT_VPAtServedWorkLeavesOtherFleets(t *testing.T) {
	for name, edit := range map[string]func(pf *pricedFleet){
		"no lower bound": func(pf *pricedFleet) { pf.fleet.Prefill.VPLowerBound = false },
		"work below":     func(pf *pricedFleet) { pf.servedComputed[5] = 1000 },
		"stated V_P":     func(pf *pricedFleet) { pf.assumedVP = true },
		"other count":    func(pf *pricedFleet) { pf.fleet.Prefill.Replicas = 4 },
	} {
		pf := servedFleetCase(true)
		edit(pf)
		_, ok := pf.servedRatio(pf.fleet.Prefill)
		assert.False(t, ok, name)
		assert.Equal(t, pf.fleet, pf.atServedWork(pf.fleet), name)
	}
}
