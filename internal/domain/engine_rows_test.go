package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// EngineRows puts a group's engines in place of its row, each with its
// group's identity, readiness and warm-pool mark and its own
// readings, and leaves every other row as it is; with no group it is the
// input itself.
func TestEngineRows(t *testing.T) {
	plain := []ReplicaMetrics{{PodName: "a", VariantName: "v", Ready: true}}
	got := EngineRows(plain)
	assert.Equal(t, plain, got)
	assert.Same(t, &plain[0], &got[0], "no group: the input, not a copy")

	since := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	group := ReplicaMetrics{PodName: "g-0", VariantName: "v", Namespace: "ns", ModelID: "m", Revision: "r1",
		ReadySince: since, Ready: false, FromWarmPool: true, AvgITL: 0.02, AvgITLKnown: true,
		KvUsageMean: 0.3, KvUsageMeanKnown: true, AvgInputTokens: 900,
		GroupEngines: []EngineReading{
			{PodName: "g-0", AvgITL: 0.01, AvgITLKnown: true, KvUsageMean: 0.2, KvUsageMeanKnown: true, RunningRequests: 3},
			{PodName: "g-0-1", AvgITL: 0.03, KvUsageInstant: 0.4, KvUsageInstantKnown: true}}}
	rows := []ReplicaMetrics{plain[0], group}
	got = EngineRows(rows)
	require.Len(t, got, 3)
	assert.Equal(t, plain[0], got[0])
	assert.Equal(t, ReplicaMetrics{PodName: "g-0", VariantName: "v", Namespace: "ns", ModelID: "m", Revision: "r1",
		ReadySince: since, FromWarmPool: true, AvgITL: 0.01, AvgITLKnown: true,
		KvUsageMean: 0.2, KvUsageMeanKnown: true, RunningRequests: 3}, got[1],
		"the group's identity and the engine's readings, nothing of the group's own readings")
	e := got[2]
	assert.Equal(t, "g-0-1", e.PodName)
	assert.Equal(t, ITLAbsent, e.ITLState(), "the engine's own ITL state, not the group's")
	assert.InDelta(t, 0.4, e.KvUsageForITL(), 0, "the engine's own fill")
	assert.False(t, e.Ready, "a group not Ready gives no engine Ready")
	assert.Equal(t, map[string]string{"g-0": "g-0", "g-0-1": "g-0"}, EngineGroups(rows), "each engine settles as its group")
	assert.Nil(t, EngineGroups(plain))
	assert.Nil(t, e.GroupEngines)

	// Every reading the ITL line reads goes through EngineReadingOf and back.
	full := ReplicaMetrics{PodName: "p", EngineRanks: 2, KvUsageMean: 0.1, KvUsageMeanKnown: true, KvUsageInstant: 0.2,
		KvUsageInstantKnown: true, AvgITL: 0.03, AvgITLKnown: true, AvgITLEmpty: true, SpecAcceptedPerStep: 1.5,
		SpecAcceptedPerStepKnown: true, RunningRequests: 7, RunningRequestsKnown: true, QueueLength: 2, MaxRankQueueLength: 1,
		PreemptionRate: 0.1, PreemptionRateKnown: true, GenerationTokenRate: 50, GenerationTokenRateKnown: true,
		TokensInUse: 100, TotalKvCapacityTokens: 1000}
	back := EngineReadingOf(full).rowOf(ReplicaMetrics{PodName: "p"})
	assert.Equal(t, full, back)
}
