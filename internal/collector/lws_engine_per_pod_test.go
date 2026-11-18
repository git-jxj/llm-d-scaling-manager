package collector

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/locator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// lwsFleet is a decode role of a DisaggregatedSet on a LeaderWorkerSet,
// m-decode: groups replicas of size Pods each, the leader m-decode-<g> and
// the workers m-decode-<g>-<w>, labelled as the LWS and DisaggregatedSet
// controllers label them.
type lwsFleet struct {
	groups, size int
	// reporting says whether a Pod reports engine series: every Pod when
	// each runs its own engine, the leader alone under Ray.
	reporting func(group, worker int) bool
	// notReady names the Pods whose Ready condition is false.
	notReady map[string]bool
	// absent names the Pods that do not exist (not yet recreated).
	absent map[string]bool
}

func lwsPodName(group, worker int) string {
	if worker == 0 {
		return fmt.Sprintf("m-decode-%d", group)
	}
	return fmt.Sprintf("m-decode-%d-%d", group, worker)
}

func (f lwsFleet) labels(group, worker int) map[string]string {
	return map[string]string{
		lwsv1.SetNameLabelKey:                  "m-decode",
		lwsv1.GroupIndexLabelKey:               strconv.Itoa(group),
		lwsv1.WorkerIndexLabelKey:              strconv.Itoa(worker),
		constants.DisaggregatedSetNameLabelKey: "m",
		constants.DisaggregatedSetRoleLabelKey: "decode",
	}
}

// collect runs one collection over the fleet. Pod (g, w) reports KV usage
// 0.1*(w+1) on a cache of (w+1)*1000 blocks of 64 tokens, a request rate of
// w+1 and w+1 running requests, so every merge rule is visible.
func (f lwsFleet) collect(t *testing.T) []domain.ReplicaMetrics {
	t.Helper()
	require.NoError(t, metrics.InitMetrics(prometheus.NewRegistry()))
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	ts := time.Now()
	labels := map[string]map[string]string{}
	var objs []client.Object
	var kv, cfg, rate, running []source.MetricValue
	for g := range f.groups {
		for w := range f.size {
			name := lwsPodName(g, w)
			labels[name] = f.labels(g, w)
			if f.absent[name] {
				continue
			}
			ready := corev1.ConditionTrue
			if f.notReady[name] {
				ready = corev1.ConditionFalse
			}
			objs = append(objs, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "models", Labels: labels[name],
					Annotations: map[string]string{lwsv1.SizeAnnotationKey: strconv.Itoa(f.size)}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{
					Type: corev1.PodReady, Status: ready, LastTransitionTime: metav1.NewTime(ts.Add(-time.Hour)),
				}}},
			})
			if !f.reporting(g, w) {
				continue
			}
			series := func() map[string]string {
				return namespaceSeriesLabels("m", name, fmt.Sprintf("10.0.%d.%d:8000", g, w))
			}
			kv = append(kv, source.MetricValue{Labels: series(), Value: 0.1 * float64(w+1), Timestamp: ts})
			c := series()
			c["num_gpu_blocks"] = strconv.Itoa((w + 1) * 1000)
			c["block_size"] = "64"
			cfg = append(cfg, source.MetricValue{Labels: c, Value: 1, Timestamp: ts})
			rate = append(rate, source.MetricValue{Labels: series(), Value: float64(w + 1), Timestamp: ts})
			running = append(running, source.MetricValue{Labels: series(), Value: float64(w + 1), Timestamp: ts})
		}
	}
	src := &mockMetricsSource{
		refreshFunc: func(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				registration.QueryKvCacheUsage:    {Values: kv},
				registration.QueryCacheConfigInfo: {Values: cfg},
				registration.QueryRequestRate:     {Values: rate},
				registration.QueryRunningRequests: {Values: running},
			}, nil
		},
	}
	loc := &mockLocator{
		locateFunc: func(context.Context, string, string) (*locator.ManagedScaler, error) {
			return &locator.ManagedScaler{Namespace: "models", Name: "m-decode"}, nil
		},
		getPodLabelsFunc: func(_ context.Context, _, name string) map[string]string { return labels[name] },
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	c := NewReplicaMetricsCollector(src, nil, reader, nil, loc)
	rows, err := c.CollectReplicaMetrics(context.Background(), "m", "models",
		map[string]scaletarget.ScaleTargetAccessor{}, map[string]*llmdvariant.Variant{}, nil)
	require.NoError(t, err)
	return rows
}

func rowsByPod(rows []domain.ReplicaMetrics) map[string]domain.ReplicaMetrics {
	out := map[string]domain.ReplicaMetrics{}
	for _, r := range rows {
		out[r.PodName] = r
	}
	return out
}

// A group whose every Pod runs its own engine is one replica, and that
// replica is all four engines: their KV capacity, batch, traffic and ranks
// summed, utilization weighed by capacity. Read from the leader alone it was
// a quarter of the replica.
func TestCollectReplicaMetrics_LWSEnginePerPodIsOneReplicaPerGroup(t *testing.T) {
	rows := lwsFleet{groups: 2, size: 4, reporting: func(int, int) bool { return true }}.collect(t)

	require.Len(t, rows, 2, "two groups are two replicas, not eight")
	by := rowsByPod(rows)
	for _, leader := range []string{"m-decode-0", "m-decode-1"} {
		r, ok := by[leader]
		require.True(t, ok, "the group's row is named for its leader; got %v", rows)
		// Blocks 1000+2000+3000+4000 of 64 tokens.
		assert.Equal(t, int64(10000*64), r.TotalKvCapacityTokens, leader+": KV capacity is the group's")
		assert.Equal(t, int64(10000), r.NumGpuBlocks, leader)
		assert.Equal(t, 4, r.EngineRanks, leader+": one rank per Pod")
		// Weighed by capacity: (0.1*1 + 0.2*2 + 0.3*3 + 0.4*4) / 10.
		assert.InDelta(t, 0.3, r.KvCacheUsage, 1e-9, leader+": utilization is weighed by capacity")
		assert.InDelta(t, 0.4, r.MaxRankKvCacheUsage, 1e-9, leader+": the busiest engine is the fullest Pod")
		assert.InDelta(t, 10, r.RequestRate, 1e-9, leader+": request rate is the group's")
		assert.Equal(t, 10, r.RunningRequests, leader+": the batch is the group's")
		assert.True(t, r.Ready, leader)
	}
}

// Under Ray only the leader reports, for the whole group. Its row is passed
// through as it was: one replica per group, the leader's own figures.
func TestCollectReplicaMetrics_LWSLeaderOnlyIsUnchanged(t *testing.T) {
	rows := lwsFleet{groups: 2, size: 4, reporting: func(_, w int) bool { return w == 0 }}.collect(t)

	require.Len(t, rows, 2)
	for _, r := range rows {
		assert.Equal(t, int64(1000*64), r.TotalKvCapacityTokens, r.PodName)
		assert.Equal(t, 1, r.EngineRanks, r.PodName)
		assert.InDelta(t, 0.1, r.KvCacheUsage, 1e-9, r.PodName)
		assert.InDelta(t, 1, r.RequestRate, 1e-9, r.PodName)
	}
}

// A group with a worker not Ready is not a Ready replica: part of its
// traffic and queue sits on a Pod nothing routes to yet. Its capacity still
// counts, as a starting Pod's does.
func TestCollectReplicaMetrics_LWSGroupIsReadyOnlyWhenEveryPodIs(t *testing.T) {
	rows := lwsFleet{groups: 2, size: 4, reporting: func(int, int) bool { return true },
		notReady: map[string]bool{"m-decode-1-2": true}}.collect(t)

	by := rowsByPod(rows)
	require.Len(t, by, 2)
	assert.True(t, by["m-decode-0"].Ready)
	assert.False(t, by["m-decode-1"].Ready, "a worker not Ready makes its group not Ready")
	assert.Equal(t, int64(10000*64), by["m-decode-1"].TotalKvCapacityTokens)
}

// A worker that is not Ready and reports no series, or one that does not
// exist yet, leaves the rows that do exist all Ready: the group is still not
// a Ready replica, from the Pod listing and the LWS size annotation. Under
// Ray the leader's readiness stands, whatever its workers do.
func TestCollectReplicaMetrics_LWSGroupMissingAPodIsNotReady(t *testing.T) {
	silentWorker := func(g, w int) bool { return g != 1 || w != 2 }
	rows := lwsFleet{groups: 2, size: 4, reporting: silentWorker,
		notReady: map[string]bool{"m-decode-1-2": true}}.collect(t)
	by := rowsByPod(rows)
	require.Len(t, by, 2)
	assert.True(t, by["m-decode-0"].Ready)
	assert.False(t, by["m-decode-1"].Ready, "a worker not Ready, with no row of its own, makes its group not Ready")

	rows = lwsFleet{groups: 2, size: 4, reporting: silentWorker,
		absent: map[string]bool{"m-decode-1-2": true}}.collect(t)
	by = rowsByPod(rows)
	assert.True(t, by["m-decode-0"].Ready)
	assert.False(t, by["m-decode-1"].Ready, "a group with fewer Pods than its size is not Ready")

	rows = lwsFleet{groups: 2, size: 4, reporting: func(_, w int) bool { return w == 0 },
		notReady: map[string]bool{"m-decode-1-2": true}}.collect(t)
	by = rowsByPod(rows)
	assert.True(t, by["m-decode-1"].Ready, "leader-only: the leader's own readiness, as before")
}

// The leader not reporting leaves its workers' engines, which are still the
// group's replica: one row, named for the lowest worker that reports.
func TestCollectReplicaMetrics_LWSLeaderMissingIsStillOneReplica(t *testing.T) {
	rows := lwsFleet{groups: 1, size: 4, reporting: func(_, w int) bool { return w != 0 }}.collect(t)

	require.Len(t, rows, 1)
	assert.Equal(t, "m-decode-0-1", rows[0].PodName)
	assert.Equal(t, int64(9000*64), rows[0].TotalKvCapacityTokens)
}

// The engine's own KV cache size is read off cache_config_info as the engine
// writes it, "None" (vLLM's unset field) being no value, and it prices an
// instance of one rank. Behind an API server of several ranks the blocks,
// the server's sum over its engines, price it, at the effective block size
// where the ranks agree on one; the rank count is the blocks query's.
func TestCollectReplicaMetrics_ReadsTheEngineKVCacheSize(t *testing.T) {
	require.NoError(t, metrics.InitMetrics(prometheus.NewRegistry()))
	ts := time.Now()
	series := func(pod string) map[string]string {
		return namespaceSeriesLabels("m", pod, "10.0.0."+pod[len(pod)-1:]+":8000")
	}
	blocks := func(pod, n string, ranks float64) source.MetricValue {
		l := series(pod)
		delete(l, seriesModelLabel)
		l["num_gpu_blocks"], l["block_size"] = n, "64"
		return source.MetricValue{Labels: l, Value: ranks, Timestamp: ts}
	}
	sizes := func(pod, effective, size string, ranks float64) source.MetricValue {
		l := series(pod)
		delete(l, seriesModelLabel)
		l["effective_attention_block_size"], l["kv_cache_size_tokens"] = effective, size
		return source.MetricValue{Labels: l, Value: ranks, Timestamp: ts}
	}
	pods := []string{"p1", "p2", "p3", "p4", "p5"}
	kv := make([]source.MetricValue, 0, len(pods))
	for _, p := range pods {
		kv = append(kv, source.MetricValue{Labels: series(p), Value: 0.5, Timestamp: ts})
	}
	src := &mockMetricsSource{
		refreshFunc: func(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				registration.QueryKvCacheUsage: {Values: kv},
				registration.QueryCacheConfigInfo: {Values: []source.MetricValue{
					blocks("p1", "4695", 1), blocks("p2", "4695", 1), blocks("p3", "4695", 1),
					// Two ranks behind one API server: 9390 blocks is their sum.
					blocks("p4", "9390", 2), blocks("p5", "9390", 2),
				}},
				// Named as registration.QueryCacheConfigSizes registers it, spelled
				// out so the test builds on a tree without the query.
				"cache_config_sizes": {Values: []source.MetricValue{
					sizes("p1", "512", "2403840", 1),
					sizes("p2", "512", "None", 1),
					sizes("p3", "None", "None", 1),
					// The ranks agree on the block and not on the count.
					sizes("p4", "512", "2403840", 1), sizes("p4", "512", "2400000", 1),
					// The ranks agree on both: still one engine's count each.
					sizes("p5", "512", "2403840", 2),
				}},
			}, nil
		},
	}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, allPodsLocator("m-prefill"))
	rows, err := c.CollectReplicaMetrics(context.Background(), "m", "models",
		map[string]scaletarget.ScaleTargetAccessor{}, map[string]*llmdvariant.Variant{}, nil)
	require.NoError(t, err)
	by := rowsByPod(rows)
	require.Len(t, by, 5)
	assert.Equal(t, int64(2403840), by["p1"].TotalKvCapacityTokens, "one rank: the engine's own count")
	assert.Equal(t, int64(1201920), by["p1"].TokensInUse)
	assert.Equal(t, int64(4695*512), by["p2"].TotalKvCapacityTokens, "no count: blocks at the effective block size")
	assert.Equal(t, int64(4695*64), by["p3"].TotalKvCapacityTokens, "neither: blocks at block_size")
	for _, p := range []string{"p4", "p5"} {
		assert.Equal(t, 2, by[p].EngineRanks, p+": the rank count is the blocks query's")
		assert.Equal(t, int64(9390*512), by[p].TotalKvCapacityTokens, p+": two ranks: the summed blocks price it")
	}
}
