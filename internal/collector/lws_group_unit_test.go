package collector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

func TestCollapseToGroups_PassesThroughRowsOutsideAGroup(t *testing.T) {
	rows := []domain.ReplicaMetrics{
		{PodName: "a", ModelID: "m", TotalKvCapacityTokens: 1},
		{PodName: "b", ModelID: "m", TotalKvCapacityTokens: 2},
	}
	got := collapseToGroups(rows, func(string) (string, int, bool) { return "", 0, false })
	assert.Equal(t, rows, got)
}

func TestLWSMemberOf(t *testing.T) {
	for name, tc := range map[string]struct {
		labels map[string]string
		group  string
		worker int
		ok     bool
	}{
		"leader": {labels: map[string]string{lwsv1.SetNameLabelKey: "s", lwsv1.GroupIndexLabelKey: "3",
			lwsv1.WorkerIndexLabelKey: "0"}, group: "s/3", worker: 0, ok: true},
		"worker": {labels: map[string]string{lwsv1.SetNameLabelKey: "s", lwsv1.GroupIndexLabelKey: "3",
			lwsv1.WorkerIndexLabelKey: "2"}, group: "s/3", worker: 2, ok: true},
		"no group index": {labels: map[string]string{lwsv1.SetNameLabelKey: "s", lwsv1.WorkerIndexLabelKey: "2"}},
		"no labels":      {},
	} {
		t.Run(name, func(t *testing.T) {
			group, worker, ok := lwsMemberOf(tc.labels)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.group, group)
				assert.Equal(t, tc.worker, worker)
			}
		})
	}
}

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
