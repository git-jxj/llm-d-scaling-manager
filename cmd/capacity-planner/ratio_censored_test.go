package main

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// R_q is read by interval-censored maximum likelihood where the replay's
// switch says so (saturation.SwitchTPOTRatioCensored), at the q bucket's
// edge otherwise (#279): on the router's 10 / 25 ms edges around a 15 ms
// mean, about 1.27 against the edge read's higher figure.
func TestTPOTRatio_TheCensoredReadOnTheSwitch(t *testing.T) {
	// 1000 requests, lognormal at a mean of 15 ms and a spread of 0.2: 2.7%
	// under 10 ms, 99.6% under 25 ms; its p90 over its mean is 1.267.
	w := whatif.TPOTWindow{Sum: 15, Count: 1000, SumMeasured: true, Buckets: []whatif.HistogramBucket{
		{UpperEdge: 0.010, Count: 27}, {UpperEdge: 0.025, Count: 996}, {UpperEdge: 0.050, Count: 1000}, {UpperEdge: math.Inf(1), Count: 1000}}}
	st := &learnreplay.State{TPOTWindows: []whatif.TPOTWindow{w, w, w}}
	f := answerFlags{set: map[string]bool{}}
	edge, _, u := f.tpotRatioFor(st, 0.9)
	require.Nil(t, u)
	st.TPOTRatioCensored = true
	censored, _, u := f.tpotRatioFor(st, 0.9)
	require.Nil(t, u)
	assert.Less(t, censored.Value, edge.Value, "the censored read is under the edge read")
	assert.InDelta(t, 1.267, censored.Value, 0.05)
}

// The censored read is not replaced by the engine's edge reading where the
// router's buckets are coarse: the fit reads inside the coarse bucket, which
// is what the switch is for (#279 round 2). Without the switch the engine's
// finer edge reading stands, as before.
func TestTPOTRatio_TheCensoredReadIsNotSwappedForTheEngines(t *testing.T) {
	w := whatif.TPOTWindow{Sum: 15, Count: 1000, SumMeasured: true, Buckets: []whatif.HistogramBucket{
		{UpperEdge: 0.010, Count: 27}, {UpperEdge: 0.025, Count: 996}, {UpperEdge: 0.050, Count: 1000}, {UpperEdge: math.Inf(1), Count: 1000}}}
	router := []whatif.TPOTWindow{w, w, w}
	st := &learnreplay.State{TPOTWindows: router, EngineTPOTWindows: []whatif.TPOTWindow{fineWindow, fineWindow, fineWindow}}
	f := answerFlags{set: map[string]bool{}}
	_, flags, u := f.tpotRatioFor(st, 0.9)
	require.Nil(t, u)
	require.Contains(t, flags, whatif.EngineTPOTHistogram, "off: the coarse router falls back to the engine")
	st.TPOTRatioCensored = true
	f = answerFlags{set: map[string]bool{}}
	got, flags, u := f.tpotRatioFor(st, 0.9)
	require.Nil(t, u)
	want, _, _ := whatif.TPOTRatioCensored(router, 0.9)
	assert.NotContains(t, flags, whatif.EngineTPOTHistogram)
	assert.False(t, f.engineTPOT)
	assert.InDelta(t, want.Value, got.Value, 1e-12)
}
