package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Issue llm-d-incubation/llm-d-scaling-manager#201, the second reading. The
// shape: a P/D decode pod of four ranks, a stated cap of 64 per rank and 9
// sequences running over all four, its busiest rank at 24 % KV. Five
// requests whose KV has landed wait on that rank for the next step; none
// waits for capacity. The landed five met the queue threshold of 5, the
// saturation test recorded a k2 at the busiest rank's 24 %, scaled to the
// pod, and the variant was priced there from then on.

// landedRow is that pod: n landed requests on its busiest rank, running
// sequences over its ranks, the busiest rank at busiestKV.
func landedRow(n, running int, busiestKV float64) domain.ReplicaMetrics {
	total := budgetRanks * budgetKVPerRank
	rm := makeReplicaMetrics("decode-0", decodeVariant, total*15/100, total, n, budgetIn, budgetOut)
	rm.EngineRanks = budgetRanks
	rm.MaxRankQueueLength = n
	rm.MaxRankKvCacheUsage = busiestKV
	rm.RunningRequests, rm.RunningRequestsKnown = running, true
	rm.RequestRate, rm.RequestRateKnown = budgetReqRate, true
	return withLanded(withReason(rm, 0), n)
}

// analyzeLanded runs four cycles of row beside a prefill replica with the
// switch on or off, and reports whether decode recorded a k2.
func analyzeLanded(t *testing.T, row domain.ReplicaMetrics, on bool) bool {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", decodeVariant, capacity.Record{
		GpuCount: 32, EngineParams: budgetParams(), LearnedFrom: "deployment",
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchDecodeLandedNeedsABound: on}))
	clock := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	in := makeAnalyzerInput([]domain.ReplicaMetrics{row, pdPrefillRow()}, pdStates)
	for range 4 {
		_, err := a.Analyze(context.Background(), in)
		require.NoError(t, err)
		clock = clock.Add(15 * time.Second)
	}
	return recordedK2(a)
}

func TestDecodeLandedNeedsABound_ARoomyDecodeRecordsNoK2(t *testing.T) {
	row := landedRow(5, 9, 0.24)
	require.True(t, analyzeLanded(t, row, false),
		"the defect: five landed requests on a rank at 24 % KV, 9 of 256 sequences, record a k2")
	assert.False(t, analyzeLanded(t, row, true), "on: a decode with room has no backlog to record")
}

// Controls: where the replica can be at a bound, the landed requests are its
// backlog with the switch on, as before.
func TestDecodeLandedNeedsABound_ABoundReplicaStillRecords(t *testing.T) {
	for name, row := range map[string]domain.ReplicaMetrics{
		"a rank may be at its cap": landedRow(5, 64, 0.24),
		"the busiest rank is full": landedRow(5, 9, 0.85),
		"a request waits for capacity": func() domain.ReplicaMetrics {
			return withReason(landedRow(5, 9, 0.24), 5)
		}(),
		"the running count is not read": func() domain.ReplicaMetrics {
			rm := landedRow(5, 9, 0.24)
			rm.RunningRequestsKnown = false
			return rm
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			assert.True(t, analyzeLanded(t, row, true))
		})
	}
}

// The rule on one row: a stated cap is needed to rule the cap out.
func TestDecodeLandedNeedsABound_TheRule(t *testing.T) {
	raw := landedRow(5, 9, 0.24)
	view := decodeCapacityView(raw, domain.RoleDecode, 64)
	require.Equal(t, 5, view.BusiestRankQueue(), "precondition: the view counts the landed five")
	assert.Equal(t, 0, landedWithoutBound(view, raw, domain.RoleDecode, 64, 0.8).BusiestRankQueue())
	assert.Equal(t, 0, landedWithoutBound(view, raw, domain.RoleDecode, 64, 0.8).QueueLength)
	assert.Equal(t, view, landedWithoutBound(view, raw, domain.RoleDecode, 0, 0.8), "no stated cap: as before")
	assert.Equal(t, view, landedWithoutBound(view, raw, domain.RolePrefill, 64, 0.8), "not a decode: as before")
}

func TestDecodeBudgetSwitches_OffFromTheConstructor(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	assert.False(t, a.k2DecodeBudget)
	assert.False(t, a.decodeLandedNeedsBound)
}
