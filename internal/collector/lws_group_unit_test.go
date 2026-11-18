package collector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The KV capacity is the engine's own count where it reports one, then its
// blocks times the effective block size, then blocks times block_size.
func TestKvCapacityTokens(t *testing.T) {
	for name, tc := range map[string]struct {
		data      podMetricData
		want      int64
		wantBlock int64
	}{
		// A prefill engine of a build that compresses attention: blocks of 64
		// configured, 512 effective, and the engine's own count.
		"engine count first": {data: podMetricData{numGpuBlocks: 4695, blockSize: 64, effectiveBlockSize: 512,
			kvCacheSizeTokens: 2403840}, want: 2403840, wantBlock: 512},
		// The engine's count is not always blocks times any block size.
		"engine count beats the product": {data: podMetricData{numGpuBlocks: 150097, blockSize: 64,
			effectiveBlockSize: 64, kvCacheSizeTokens: 7904184}, want: 7904184, wantBlock: 64},
		// Several ranks behind one API server: the blocks are their sum, the
		// count one engine's, so the blocks price it.
		"several ranks: the blocks": {data: podMetricData{numGpuBlocks: 9390, blockSize: 64, effectiveBlockSize: 512,
			kvCacheSizeTokens: 2403840, engineRanks: 2}, want: 9390 * 512, wantBlock: 512},
		"effective block size": {data: podMetricData{numGpuBlocks: 4695, blockSize: 64, effectiveBlockSize: 512},
			want: 4695 * 512, wantBlock: 512},
		"block size alone":       {data: podMetricData{numGpuBlocks: 4695, blockSize: 64}, want: 4695 * 64, wantBlock: 64},
		"overflow saturates":     {data: podMetricData{numGpuBlocks: 1 << 40, blockSize: 1 << 40}, want: 1<<63 - 1, wantBlock: 1 << 40},
		"nothing is no capacity": {data: podMetricData{}, want: 0, wantBlock: 0},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, kvCapacityTokens(&tc.data))
			assert.Equal(t, tc.wantBlock, tokensPerBlock(&tc.data))
		})
	}
}

func TestCollapseToGroups_PassesThroughRowsOutsideAGroup(t *testing.T) {
	rows := []domain.ReplicaMetrics{
		{PodName: "a", ModelID: "m", TotalKvCapacityTokens: 1},
		{PodName: "b", ModelID: "m", TotalKvCapacityTokens: 2},
	}
	got := collapseToGroups([]placedRow{{row: rows[0]}, {row: rows[1]}}, nil)
	assert.Equal(t, rows, got)
}

func TestLWSPlaceOf(t *testing.T) {
	for name, tc := range map[string]struct {
		labels map[string]string
		want   lwsPlace
	}{
		"leader": {labels: map[string]string{lwsv1.SetNameLabelKey: "s", lwsv1.GroupIndexLabelKey: "3",
			lwsv1.WorkerIndexLabelKey: "0"}, want: lwsPlace{group: "s/3"}},
		"worker": {labels: map[string]string{lwsv1.SetNameLabelKey: "s", lwsv1.GroupIndexLabelKey: "3",
			lwsv1.WorkerIndexLabelKey: "2"}, want: lwsPlace{group: "s/3", worker: 2, isWorker: true}},
		"worker with no group index": {labels: map[string]string{lwsv1.SetNameLabelKey: "s",
			lwsv1.WorkerIndexLabelKey: "2"}, want: lwsPlace{isWorker: true}},
		"no labels": {},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, lwsPlaceOf(tc.labels))
		})
	}
}

// A group whose workers report is engine-per-Pod: it is Ready only while no
// Pod of it is missing or starting. A leader-only group (Ray) keeps the
// leader's own readiness, whatever its workers do.
func TestCollapseToGroups_ImpairedGroupIsNotReady(t *testing.T) {
	row := func(pod string, worker int) placedRow {
		return placedRow{row: domain.ReplicaMetrics{PodName: pod, ModelID: "m", Ready: true, EngineRanks: 1},
			place: lwsPlace{group: "s/0", worker: worker, isWorker: worker != 0}}
	}
	impaired := func(string) bool { return true }

	got := collapseToGroups([]placedRow{row("s-0", 0), row("s-0-1", 1)}, impaired)
	assert.Len(t, got, 1)
	assert.False(t, got[0].Ready, "an engine-per-Pod group lacking a serving Pod is not Ready")

	got = collapseToGroups([]placedRow{row("s-0", 0), row("s-0-1", 1)}, func(string) bool { return false })
	assert.True(t, got[0].Ready)

	got = collapseToGroups([]placedRow{row("s-0", 0)}, impaired)
	assert.Len(t, got, 1)
	assert.True(t, got[0].Ready, "a leader-only group keeps the leader's readiness")
}

func TestLWSRankMismatch(t *testing.T) {
	member := func(worker, ranks int) placedRow {
		return placedRow{row: domain.ReplicaMetrics{EngineRanks: ranks}, place: lwsPlace{group: "s/0", worker: worker}}
	}
	group := func(m ...placedRow) lwsGroup { return lwsGroup{key: "s/0", members: m} }
	assert.Empty(t, lwsRankMismatch(group(member(0, 4)), 4), "leader-only: the leader reports for the group")
	assert.Empty(t, lwsRankMismatch(group(member(0, 1), member(1, 1), member(2, 1), member(3, 1)), 4))
	assert.Empty(t, lwsRankMismatch(group(member(0, 1), member(1, 1)), 4), "a Pod missing is not judged on its sum")
	assert.NotEmpty(t, lwsRankMismatch(group(member(0, 4), member(1, 1), member(2, 1), member(3, 1)), 4),
		"a leader of every rank beside workers that report too")
	assert.NotEmpty(t, lwsRankMismatch(group(member(0, 1), member(1, 2), member(2, 1), member(3, 1)), 4))
}
