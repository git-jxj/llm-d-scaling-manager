package slo

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// shortPromptEstimate is P1 on a short-prompt prefill below its queue gate:
// no queue, so no V_P of its own, and one prompt length above low load, so
// no a.
func shortPromptEstimate() prefill.Estimate {
	return prefill.Estimate{VPRefused: prefill.NoPrefillCapacity, ARefused: prefill.TooFewLowLoadMinutes}
}

// shortPromptRun is jointRows' 2 prefill + 3 decode fleet, whose prefill pods
// never queue, at 30 r/s against a 300 ms p90, after 20 cycles: its last
// reading, the result, and the inputs it was priced from. On jointRows' a₀
// (15 ms) and S (25 ms at 4000 tokens), two pods price p90 at 0.42 s there,
// three at 0.29 s.
func shortPromptRun(t *testing.T, on bool) (metrics.SLOTTFTReading, *domain.AnalyzerResult, ttftInputs) {
	t.Helper()
	r := jointRun(t, shortPromptEstimate(), true)
	require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchTTFTSizesPrefill: on}))
	r.in.ArrivalRate = 30
	r.in.Config = policy(0, 300*time.Millisecond)
	var last ttftInputs
	onTTFTPriced = func(in ttftInputs, _ float64, _ whatif.TTFTTarget) { last = in }
	t.Cleanup(func() { onTTFTPriced = nil })
	r.cycles(t, 0, 19)
	r.in.ReplicaMetrics = jointRows(19)
	res, readings := r.analyze(t)
	require.Len(t, readings, 1)
	return readings[0], res, last
}

// variantCapacity is the result's capacity of variant.
func variantCapacity(t *testing.T, res *domain.AnalyzerResult, variant string) domain.VariantCapacity {
	t.Helper()
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == variant {
			return vc
		}
	}
	require.Failf(t, "no variant", "%s", variant)
	return domain.VariantCapacity{}
}

// llm-d-incubation/llm-d-scaling-manager#131: a short-prompt prefill that
// never queues, on a fleet that misses TTFT. Off, the analyzer prices
// nothing (P1 has no a) and asks nothing for prefill. On, the joint a₀
// stands for a, the fleet is priced, and the prefill role's need is more
// replicas than the two that miss: three, which the model prices under the
// target.
func TestTTFT_TheSizesPrefillSwitch(t *testing.T) {
	off, offRes, _ := shortPromptRun(t, false)
	assert.Equal(t, constants.SLOTTFTReasonNoFixedLatency, off.Reason, "P1 refused a, and nothing stands for it")
	assert.NotContains(t, offRes.RoleDemand, domain.RolePrefill)
	pre := variantCapacity(t, offRes, "pre")
	assert.Equal(t, domain.ReasonNoData, pre.Reason)
	assert.Zero(t, pre.PerReplicaCapacity)

	on, res, in := shortPromptRun(t, true)
	require.Equal(t, constants.SLOTTFTReasonPriced, on.Reason)
	assert.True(t, on.Unmet, "two pods miss at 30 r/s")
	assert.True(t, in.aFromJoint)
	assert.InDelta(t, 0.015, in.est.A, 1e-12, "a is a₀")
	assert.Equal(t, constants.SLOTTFTVPInverted, on.VPSource)

	pre = variantCapacity(t, res, "pre")
	assert.Equal(t, reasonTTFTSized, pre.Reason)
	assert.InDelta(t, 10, pre.PerReplicaCapacity, 1e-12, "30 r/s over the 3 pods it asks for")
	assert.InDelta(t, 30, res.RoleDemand[domain.RolePrefill], 1e-12)
	assert.InDelta(t, 30, pre.TotalDemand, 1e-12)
	assert.InDelta(t, 1.5, pre.Utilization, 1e-12)
	need := res.RoleDemand[domain.RolePrefill] / pre.PerReplicaCapacity
	assert.InDelta(t, 3, need, 1e-12, "more than the fleet that missed")

	// The count it asks for meets the target, and the one below it does not.
	target := whatif.TTFTTarget{Seconds: 0.3, Percentile: 0.9}
	at := func(pods int) bool {
		fl, _ := ttftFleet(in, pods)
		met, known := whatif.TTFTMeets(whatif.TTFTAt(fl, 30, target.Percentile), target)
		require.True(t, known)
		return met
	}
	assert.False(t, at(2))
	assert.True(t, at(3))

	// The decode side is what it was.
	assert.Equal(t, variantCapacity(t, offRes, "dec"), variantCapacity(t, res, "dec"))
	assert.InDelta(t, offRes.RoleDemand[domain.RoleDecode], res.RoleDemand[domain.RoleDecode], 0)
	assert.Equal(t, offRes.TotalDemand, res.TotalDemand)
}

// Off from the constructor, and a replay switch.
func TestTTFT_TheSizesPrefillOffFromTheConstructor(t *testing.T) {
	assert.False(t, New(nil, nil).ttftSizesPrefill)
	assert.Contains(t, ReplaySwitchNames(), SwitchTTFTSizesPrefill)
}

// a₀ stands only where P1 refused a, and only a learned one. A curve P1
// fitted and then refused the intercept of goes with the refused a.
func TestTTFT_WithJointFixedLatency(t *testing.T) {
	learned := &jointCycle{est: prefill.JointEstimate{Overhead: 0.015, OverheadSamples: 40}}
	p1 := ttftEstimate()
	got, taken := withJointFixedLatency(p1, learned)
	assert.False(t, taken)
	assert.Equal(t, p1, got, "P1's a stands")
	assert.Equal(t, aSourceP1, aSource(got, taken))

	refused := shortPromptEstimate()
	got, taken = withJointFixedLatency(refused, learned)
	assert.True(t, taken)
	assert.InDelta(t, 0.015, got.A, 0)
	assert.Empty(t, got.ARefused)
	assert.Equal(t, aSourceJoint, aSource(got, taken))

	afterFit := prefill.Estimate{VP: 100000, VPSource: prefill.SourceBusyMinutes, VPPromptTokens: 2000,
		ARefused: prefill.AUncertain, AModel: prefill.AModelQuadratic, ASlope: 1e-5, ACurve: 1e-9}
	got, taken = withJointFixedLatency(afterFit, learned)
	require.True(t, taken)
	assert.Empty(t, got.AModel)
	assert.Zero(t, got.ASlope)
	assert.Zero(t, got.ACurve)
	base, u := whatif.PrefillFromEstimate(got)
	require.Nil(t, u)
	assert.Zero(t, base.CurveSlope, "V_P is not rescaled by a refused curve")
	assert.Zero(t, base.CurveCurvature)

	for _, j := range []*jointCycle{nil,
		{est: prefill.JointEstimate{OverheadRefused: prefill.NoOverheadReadings}},
		{est: prefill.JointEstimate{Overhead: 0}},
		{est: prefill.JointEstimate{Overhead: math.NaN()}},
		{est: prefill.JointEstimate{Overhead: math.Inf(1)}},
	} {
		got, taken = withJointFixedLatency(refused, j)
		assert.False(t, taken)
		assert.Equal(t, refused, got)
		assert.Equal(t, aSourceNone, aSource(got, taken))
	}
}

// The ask is the least count the model prices under the target, each count
// at its own f: on one pod f is 1, on more at least 1.5, so one pod meeting
// says nothing of two. Past maxPrefillAskFactor times the Ready pods there is
// none.
func TestTTFT_PrefillAsk(t *testing.T) {
	in := ttftInputsAt(2, 0)
	target := whatif.TTFTTarget{Seconds: 3, Percentile: 0.9}
	meets := func(n int, rate float64) bool {
		fl, _ := ttftFleet(in, n)
		met, known := whatif.TTFTMeets(whatif.TTFTAt(fl, rate, target.Percentile), target)
		return known && met
	}
	for _, rate := range []float64{1, 20, 60, 100, 150} {
		n, ok := prefillAsk(in, rate, target)
		if !ok {
			for c := 1; c <= maxPrefillAskFactor*in.pods; c++ {
				assert.False(t, meets(c, rate), "%g r/s: %d pods meets, and no ask", rate, c)
			}
			continue
		}
		assert.True(t, meets(n, rate), "%g r/s: the ask %d meets", rate, n)
		for c := 1; c < n; c++ {
			assert.False(t, meets(c, rate), "%g r/s: %d pods, fewer than the ask %d, meets", rate, c, n)
		}
	}
	n, ok := prefillAsk(in, 100, target)
	require.True(t, ok)
	assert.Greater(t, n, 2, "100 r/s is past what 2 pods serve")

	_, ok = prefillAsk(in, 1000, target)
	assert.False(t, ok, "past 4x the pods: the bound")
	_, ok = prefillAsk(ttftInputsAt(0, 0), 10, target)
	assert.False(t, ok, "no Ready pod")
	for _, rate := range []float64{0, math.NaN(), math.Inf(1)} {
		_, ok = prefillAsk(in, rate, target)
		assert.False(t, ok, "rate %g", rate)
	}
}

// The ask sizes the model's one serving prefill variant: its capacity is the
// arrival rate over the ask. Two serving prefill variants size none, a
// variant with no Ready pod does not count, and the decode side is left as
// it was.
func TestTTFT_SizePrefill(t *testing.T) {
	fleet := fleetReading{
		variants: []domain.VariantReplicaState{
			{VariantName: "a", Role: domain.RolePrefill}, {VariantName: "idle", Role: domain.RolePrefill},
			{VariantName: "d", Role: domain.RoleDecode},
		},
		ready: map[string]int{"a": 3, "d": 2},
	}
	newResult := func() *domain.AnalyzerResult {
		return &domain.AnalyzerResult{
			RoleDemand: map[string]float64{domain.RoleDecode: 500},
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "a", Role: domain.RolePrefill, ObservedReplicas: 3, Reason: domain.ReasonNoData},
				{VariantName: "idle", Role: domain.RolePrefill, Reason: domain.ReasonNoData},
				{VariantName: "d", Role: domain.RoleDecode, ObservedReplicas: 2, PerReplicaCapacity: 100, Reason: "slo-own"},
			},
		}
	}

	res := newResult()
	sizePrefill(res, map[string]int{"a": 5}, fleet, 40)
	assert.InDelta(t, 40, res.RoleDemand[domain.RolePrefill], 0)
	assert.InDelta(t, 500, res.RoleDemand[domain.RoleDecode], 0, "decode keeps its own")
	a := res.VariantCapacities[0]
	assert.Equal(t, reasonTTFTSized, a.Reason)
	assert.InDelta(t, 8, a.PerReplicaCapacity, 1e-12, "40 r/s over the 5 it asks for")
	assert.InDelta(t, 40, a.TotalDemand, 1e-12)
	assert.InDelta(t, 40/(3*8.0), a.Utilization, 1e-12)
	assert.Equal(t, newResult().VariantCapacities[1:], res.VariantCapacities[1:], "no Ready pod, and decode, as they were")

	for _, asks := range []map[string]int{nil, {"a": 0}, {"idle": 5}} {
		res = newResult()
		sizePrefill(res, asks, fleet, 40)
		assert.Equal(t, newResult(), res, "asks %v", asks)
	}
	for _, arrival := range []float64{0, math.NaN(), math.Inf(1)} {
		res = newResult()
		sizePrefill(res, map[string]int{"a": 5}, fleet, arrival)
		assert.Equal(t, newResult(), res, "arrival %g", arrival)
	}

	two := fleet
	two.ready = map[string]int{"a": 3, "idle": 1, "d": 2}
	res = newResult()
	sizePrefill(res, map[string]int{"a": 5, "idle": 1}, two, 40)
	assert.Equal(t, newResult(), res, "two serving prefill variants: their asks were never priced together")

	// No role demand yet (no output length): the prefill role still gets its own.
	res = newResult()
	res.RoleDemand = nil
	sizePrefill(res, map[string]int{"a": 5}, fleet, 40)
	assert.Equal(t, map[string]float64{domain.RolePrefill: 40}, res.RoleDemand)
}
