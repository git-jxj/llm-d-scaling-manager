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
			ready := corev1.ConditionTrue
			if f.notReady[name] {
				ready = corev1.ConditionFalse
			}
			objs = append(objs, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "models", Labels: labels[name]},
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

// The leader not reporting leaves its workers' engines, which are still the
// group's replica: one row, named for the lowest worker that reports.
func TestCollectReplicaMetrics_LWSLeaderMissingIsStillOneReplica(t *testing.T) {
	rows := lwsFleet{groups: 1, size: 4, reporting: func(_, w int) bool { return w != 0 }}.collect(t)

	require.Len(t, rows, 1)
	assert.Equal(t, "m-decode-0-1", rows[0].PodName)
	assert.Equal(t, int64(9000*64), rows[0].TotalKvCapacityTokens)
}

// The labels are read off cache_config_info as the engine writes them, and
// "None" (vLLM's unset field) is no value.
func TestCollectReplicaMetrics_ReadsTheEngineKVCacheSize(t *testing.T) {
	require.NoError(t, metrics.InitMetrics(prometheus.NewRegistry()))
	ts := time.Now()
	series := func(pod string) map[string]string {
		return namespaceSeriesLabels("m", pod, "10.0.0."+pod[len(pod)-1:]+":8000")
	}
	cfg := func(pod, effective, size string) map[string]string {
		l := series(pod)
		l["num_gpu_blocks"], l["block_size"] = "4695", "64"
		l["effective_attention_block_size"], l["kv_cache_size_tokens"] = effective, size
		return l
	}
	src := &mockMetricsSource{
		refreshFunc: func(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				registration.QueryKvCacheUsage: {Values: []source.MetricValue{
					{Labels: series("p1"), Value: 0.5, Timestamp: ts},
					{Labels: series("p2"), Value: 0.5, Timestamp: ts},
					{Labels: series("p3"), Value: 0.5, Timestamp: ts},
				}},
				registration.QueryCacheConfigInfo: {Values: []source.MetricValue{
					{Labels: cfg("p1", "512", "2403840"), Value: 1, Timestamp: ts},
					{Labels: cfg("p2", "512", "None"), Value: 1, Timestamp: ts},
					{Labels: cfg("p3", "None", "None"), Value: 1, Timestamp: ts},
				}},
			}, nil
		},
	}
	c := NewReplicaMetricsCollector(src, nil, nil, nil, allPodsLocator("m-prefill"))
	rows, err := c.CollectReplicaMetrics(context.Background(), "m", "models",
		map[string]scaletarget.ScaleTargetAccessor{}, map[string]*llmdvariant.Variant{}, nil)
	require.NoError(t, err)
	by := rowsByPod(rows)
	require.Len(t, by, 3)
	assert.Equal(t, int64(2403840), by["p1"].TotalKvCapacityTokens)
	assert.Equal(t, int64(1201920), by["p1"].TokensInUse)
	assert.Equal(t, int64(4695*512), by["p2"].TotalKvCapacityTokens)
	assert.Equal(t, int64(4695*64), by["p3"].TotalKvCapacityTokens)
}
