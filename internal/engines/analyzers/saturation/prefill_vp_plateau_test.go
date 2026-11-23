package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// psPrefillCycle runs one cycle of a P/D model whose one prefill pod is a
// processor-sharing engine of 20k tok/s at utilization rho, never queued:
// it computes rho*20k tok/s of 2000-token prompts with rho/(1 - rho)
// requests in prefill. It returns the variant's estimate.
func (h *observeHarness) psPrefillCycle(rho float64) prefill.Estimate {
	h.t.Helper()
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	tokens := rho * 20000
	pre := makeReplicaMetrics("prefill-0", "prefill", 0, 16000, 0, 2000, 1)
	pre.RequestRate, pre.RequestRateKnown = tokens/2000, true
	pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = tokens, true
	pre.AvgPrefillTime, pre.AvgPrefillTimeKnown = rho/(1-rho)/pre.RequestRate, true
	rows := []domain.ReplicaMetrics{makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500), pre}
	_, err := h.a.Analyze(h.ctx, makeAnalyzerInput(rows, states))
	require.NoError(h.t, err)
	h.clock = h.clock.Add(time.Minute)
	est, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
	require.True(h.t, ok)
	return est
}

// SwitchPrefillVPOccupancyPlateau through Analyze: a prefill pod that never
// queues, driven from 50% to 95% of its 20k tok/s, eight minutes a step. On,
// its served rate flattens from 9 to 19 requests in prefill and V_P is the
// plateau, 19k, not a bound; off, V_P is refused for want of a queue.
func TestPrefillVPOccupancyPlateau_ThroughAnalyze(t *testing.T) {
	run := func(on bool) prefill.Estimate {
		h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
		require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillVPOccupancyPlateau: on}))
		var est prefill.Estimate
		for _, rho := range []float64{0.5, 0.6, 0.7, 0.8, 0.9, 0.95} {
			for range 8 {
				est = h.psPrefillCycle(rho)
			}
		}
		return est
	}
	on, off := run(true), run(false)
	require.Empty(t, on.VPRefused, "%+v", on)
	assert.Equal(t, prefill.SourceOccupancyPlateau, on.VPSource)
	assert.InDelta(t, 19000.0, on.VP, 1e-6)
	assert.False(t, on.VPLowerBound)
	assert.Equal(t, prefill.NoPrefillCapacity, off.VPRefused)
	assert.Zero(t, off.VPPlateau)
}
