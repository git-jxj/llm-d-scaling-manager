package collector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A group whose Pods each run an engine is one merged row, and keeps each
// engine's row as read for the ITL line; a leader-only group, and a row
// outside any group, carry none.
func TestCollapseToGroups_KeepsTheEnginesOfAGroup(t *testing.T) {
	engine := func(pod string, k float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, ModelID: "m", VariantName: "d", Ready: true, EngineRanks: 1,
			KvCacheUsage: k, KvUsageMean: k, KvUsageMeanKnown: true, AvgITL: 0.006 + 0.02*k, AvgITLKnown: true,
			TotalKvCapacityTokens: 100, GenerationTokenRate: 10, GenerationTokenRateKnown: true}
	}
	rows := []placedRow{
		{row: engine("s-0", 0.2), place: lwsPlace{group: "s/0"}},
		{row: engine("s-0-1", 0.4), place: lwsPlace{group: "s/0", worker: 1}},
		{row: engine("t-0", 0.3), place: lwsPlace{group: "t/0"}},
		{row: engine("p", 0.5)},
	}
	got := collapseToGroups(rows, nil)
	require.Len(t, got, 3)

	group := got[0]
	assert.Equal(t, "s-0", group.PodName)
	assert.InDelta(t, 0.3, group.KvUsageMean, 1e-9, "the group's fill is its engines' mean")
	require.Len(t, group.GroupEngines, 2)
	assert.Equal(t, []string{"s-0", "s-0-1"}, []string{group.GroupEngines[0].PodName, group.GroupEngines[1].PodName})
	assert.InDelta(t, 0.4, group.GroupEngines[1].KvUsageMean, 0, "each engine as read")
	assert.InDelta(t, 0.014, group.GroupEngines[1].AvgITL, 1e-12)

	assert.Nil(t, got[1].GroupEngines, "a leader-only group: its leader is its engine")
	assert.Nil(t, got[2].GroupEngines, "a row outside any group")
}
