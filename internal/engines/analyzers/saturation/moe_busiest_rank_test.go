package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The fixtures below are synthetic: no recording holds a data-parallel
// engine. Eight ranks, the busiest 1.25 times as full as their mean: the
// spread the review computed for DP8 at 32 sequences a rank and a request KV
// cv of 1 (issue llm-d-incubation/llm-d-scaling-manager#312).

// moeDP8 is an expert-parallel MoE engine over eight data-parallel ranks.
func moeDP8() *capacity.EngineParams {
	p := deploymentParams()
	p.ExpertParallel, p.DataParallelSize = true, 8
	return p
}

// The switch is off in a new analyzer, so the controller reads the mean.
func TestMoEBusiestRank_OffFromTheConstructor(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	assert.False(t, a.moeBusiestRank)
	require.Contains(t, ReplaySwitchNames(), SwitchMoEBusiestRank)
	assert.False(t, a.BusiestRankFillFor(moeDP8()), "off: no variant is read at its busiest rank")
}

// On, only an expert-parallel engine over more than one data-parallel rank is
// read at its busiest rank: dense ranks step independently.
func TestMoEBusiestRank_OnlyExpertParallelDataParallel(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMoEBusiestRank: true}))
	assert.True(t, a.BusiestRankFillFor(moeDP8()))
	dense := moeDP8()
	dense.ExpertParallel = false
	assert.False(t, a.BusiestRankFillFor(dense), "dense data parallelism")
	one := moeDP8()
	one.DataParallelSize = 1
	assert.False(t, a.BusiestRankFillFor(one), "expert parallelism over one rank")
	assert.False(t, a.BusiestRankFillFor(nil), "no arguments recorded")
}

// Through Analyze: the ITL line's reading of a DP8 MoE decode replica is at
// its busiest rank with the switch on, at the mean off.
func TestMoEBusiestRank_LineReadAtTheBusiestRank(t *testing.T) {
	assert.InDelta(t, 0.4, busiestRankReadingK(t, false), 1e-12)
	assert.InDelta(t, 0.5, busiestRankReadingK(t, true), 1e-12)
}

// busiestRankReadingK runs one Analyze of a DP8 MoE decode replica at a mean
// fill of 0.4, its busiest rank at 0.5, and returns the k of the reading its
// ITL window took.
func busiestRankReadingK(t *testing.T, on bool) float64 {
	t.Helper()
	ctx, _ := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		AcceleratorName: "H200", GpuCount: 8, EngineParams: moeDP8(),
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMoEBusiestRank: on}))
	rm := makeReplicaMetrics("pod-1", "variant-d", 40_000, 100_000, 0, 100, 50)
	rm.Ready, rm.EngineRanks, rm.MaxRankKvCacheUsage = true, 8, 0.5
	rm.KvUsageMean, rm.KvUsageMeanKnown, rm.AvgITL, rm.AvgITLKnown = 0.4, true, 0.02, true
	in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H200",
			CurrentReplicas: 1, GPUsPerReplica: 8},
	})
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	all := a.itlTracker.Readings(a.itlWindowKey("test-ns", "test-model", "variant-d", "H200", 8)).All
	require.Len(t, all, 1, "the replica's one reading")
	return all[0].K
}
