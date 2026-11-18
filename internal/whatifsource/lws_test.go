package whatifsource

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// lwsRole is one role of a DisaggregatedSet m on LeaderWorkerSets, as
// kube-state-metrics exports it: the role's LeaderWorkerSet m-abc-<role>
// runs groups of size Pods, each requesting gpus, the leader
// m-abc-<role>-<g> and the workers m-abc-<role>-<g>-<w>, all owned by
// StatefulSets. Only the label keys and the shape follow a real fleet.
type lwsRole struct {
	role             string
	groups, size     int
	gpus             float64
	blocks, kvTokens int
	// reporting says whether a Pod has engine series: every Pod when each
	// runs its own engine, the leader alone under Ray.
	reporting func(group, worker int) bool
	// notReady names the Pods that are not Ready.
	notReady map[string]bool
	// labels says whether kube-state-metrics exports the LWS and
	// DisaggregatedSet labels; the llm-d.ai/role label is always exported.
	noGroupLabels bool
	// itl says whether a reporting Pod has an ITL mean; nil for none.
	itl func(group, worker int) bool
	// fill is a reporting Pod's KV fill, read by every fill query, and the
	// ITL its engine reads there is lineITL's; nil reads 0.25 by the
	// utilization query alone, and 20 ms.
	fill func(group, worker int) float64
}

// lineITL is the ITL an engine reads at KV fill k: 6 ms at no load and
// 20 ms more at a full cache.
func lineITL(k float64) float64 { return 0.006 + 0.02*k }

func (r lwsRole) lws() string { return "m-abc-" + r.role }

func (r lwsRole) pod(group, worker int) string {
	if worker == 0 {
		return fmt.Sprintf("%s-%d", r.lws(), group)
	}
	return fmt.Sprintf("%s-%d-%d", r.lws(), group, worker)
}

// add sets every series of every Pod of the role on srv.
func (r lwsRole) add(t *testing.T, srv *promtest.Server) {
	t.Helper()
	k := KSMQueries(testNS)
	for g := range r.groups {
		for w := range r.size {
			name := r.pod(g, w)
			l := func(kv ...string) map[string]string {
				out := map[string]string{"namespace": testNS, "pod": name}
				for i := 0; i+1 < len(kv); i += 2 {
					out[kv[i]] = kv[i+1]
				}
				return out
			}
			labels := []string{ksmLabelName("llm-d.ai/role"), r.role}
			if !r.noGroupLabels {
				labels = append(labels,
					ksmLabelName(lwsv1.SetNameLabelKey), r.lws(),
					ksmLabelName(lwsv1.GroupIndexLabelKey), strconv.Itoa(g),
					ksmLabelName(lwsv1.WorkerIndexLabelKey), strconv.Itoa(w),
					ksmLabelName(constants.DisaggregatedSetNameLabelKey), "m",
					ksmLabelName(constants.DisaggregatedSetRoleLabelKey), r.role)
			}
			srv.Add(k.PodLabels, promtest.Const(l(labels...), 1))
			srv.Add(k.PodNode, promtest.Const(l("node", "n1"), 1))
			owner := fmt.Sprintf("%s-%d", r.lws(), g)
			if w == 0 {
				owner = r.lws()
			}
			srv.Add(k.PodOwner, promtest.Const(l("owner_kind", "StatefulSet", "owner_name", owner), 1))
			srv.Add(k.PodRunning, promtest.Const(l(), 1))
			ready := 1.0
			if r.notReady[name] {
				ready = 0
			} else {
				srv.Add(k.PodReadyTime, promtest.Const(l(), float64(t0.Unix()-3600)))
			}
			srv.Add(k.PodReady, promtest.Const(l(), ready))
			srv.Add(k.PodGPUs, promtest.Const(map[string]string{"pod": name}, r.gpus))
			if !r.reporting(g, w) {
				continue
			}
			series := func(kv ...string) map[string]string {
				return podLabels(name, append([]string{"instance", fmt.Sprintf("10.%d.%d.1:8000", g, w)}, kv...)...)
			}
			kv, itl := 0.25, 0.02
			if r.fill != nil {
				kv = r.fill(g, w)
				itl = lineITL(kv)
				for _, q := range []string{registration.QueryKvUsageInstant, registration.QueryKvUsageMean,
					registration.QueryMaxRankKvCacheUsage} {
					srv.Add(promQL(t, q), promtest.Const(series(), kv))
				}
				running := math.Round(kv * 64)
				srv.Add(promQL(t, registration.QueryRunningRequests), promtest.Const(series(), running))
				srv.Add(promQL(t, registration.QueryGenerationTokenRate), promtest.Const(series(), running/itl))
				srv.Add(promQL(t, registration.QueryRequestRate), promtest.Const(series(), running/itl/1000))
				for _, q := range []string{registration.QueryAvgOutputTokens, registration.QueryAvgOutputTokensRecent,
					registration.QueryAvgInputTokens, registration.QueryAvgInputTokensRecent} {
					srv.Add(promQL(t, q), promtest.Const(series(), 1000))
				}
			}
			srv.Add(promQL(t, registration.QueryKvCacheUsage), promtest.Const(series(), kv))
			srv.Add(promQL(t, registration.QueryQueueLength), promtest.Const(series(), 0))
			if r.itl != nil && r.itl(g, w) {
				srv.Add(promQL(t, registration.QueryAvgITL), promtest.Const(series(), itl))
			}
			srv.Add(promQL(t, registration.QueryCacheConfigInfo), promtest.Const(series(
				"num_gpu_blocks", strconv.Itoa(r.blocks), "block_size", "64"), 1))
			if q, ok := registeredPromQL(t, "cache_config_sizes"); ok {
				srv.Add(q, promtest.Const(series(
					"kv_cache_size_tokens", strconv.Itoa(r.kvTokens)), 1))
			}
		}
	}
	addKSMNode(srv, "n1", "NVIDIA-H100-80GB-HBM3")
}

func everyPod(int, int) bool   { return true }
func leaderOnly(_, w int) bool { return w == 0 }

// lwsFleetCapture captures one step of a fleet of 2 decode groups and 1
// prefill group, 4 Pods of 8 GPUs each, from kube-state-metrics alone.
func lwsFleetCapture(t *testing.T, decode, prefill lwsRole, edit ...func(*Options)) decoded {
	t.Helper()
	srv := promtest.New()
	t.Cleanup(srv.Close)
	decode.add(t, srv)
	prefill.add(t, srv)
	opts := Options{Prometheus: apiFor(t, srv), Namespace: testNS, From: t0, To: t0, Step: 15e9,
		KubeStateMetrics: true}
	for _, e := range edit {
		e(&opts)
	}
	d, _ := capture(t, opts)
	return d
}

func decodeRole(reporting func(int, int) bool) lwsRole {
	return lwsRole{role: domain.RoleDecode, groups: 2, size: 4, gpus: 8, blocks: 150000, kvTokens: 7900000,
		reporting: reporting}
}

func prefillRole(reporting func(int, int) bool) lwsRole {
	return lwsRole{role: domain.RolePrefill, groups: 1, size: 4, gpus: 8, blocks: 4700, kvTokens: 2406400,
		reporting: reporting}
}

func statesByName(states []domain.VariantReplicaState) map[string]domain.VariantReplicaState {
	out := map[string]domain.VariantReplicaState{}
	for _, s := range states {
		out[s.VariantName] = s
	}
	return out
}

// A DisaggregatedSet on LeaderWorkerSets whose every Pod runs its own engine,
// read from kube-state-metrics alone: each role is a variant named as the
// controller names its scaler, a group of 4 Pods is one replica of 32 GPUs,
// its KV cache all four engines', and no replica is pending.
func TestCapture_DisaggregatedSetOnLWSWithAnEnginePerPod(t *testing.T) {
	d := lwsFleetCapture(t, decodeRole(everyPod), prefillRole(everyPod))

	require.Len(t, d.window.Variants, 2)
	dec, pre := d.window.Variants[0], d.window.Variants[1]
	assert.Equal(t, "m-decode", dec.Name, "variant <set>-<role>, as the controller names the role's scaler")
	assert.Equal(t, domain.RoleDecode, dec.Role)
	assert.Equal(t, 32, dec.GpuCount, "GPUs per replica are the group's, 4 Pods of 8")
	assert.Equal(t, "m-prefill", pre.Name)
	assert.Equal(t, domain.RolePrefill, pre.Role)
	assert.Equal(t, 32, pre.GpuCount)
	for _, f := range d.window.Flags {
		assert.NotContains(t, f, "multi-Pod", "the groups are labelled, so nothing is read from one Pod")
	}

	require.Len(t, d.cycles, 1)
	in := d.cycles[0].Input
	rows := map[string]domain.ReplicaMetrics{}
	for _, r := range in.ReplicaMetrics {
		rows[r.PodName] = r
	}
	require.Len(t, rows, 3, "one row per group: %v", in.ReplicaMetrics)
	for _, leader := range []string{"m-abc-decode-0", "m-abc-decode-1"} {
		r := rows[leader]
		assert.Equal(t, "m-decode", r.VariantName, leader)
		assert.Equal(t, int64(4*7900000), r.TotalKvCapacityTokens, leader+": the group's four engines, as each reports its size")
		assert.Equal(t, 4, r.Ranks(), leader)
		assert.True(t, r.Ready, leader)
	}
	assert.Equal(t, int64(4*2406400), rows["m-abc-prefill-0"].TotalKvCapacityTokens)

	states := statesByName(in.VariantStates)
	assert.Equal(t, domain.VariantReplicaState{VariantName: "m-decode", CurrentReplicas: 2, DesiredReplicas: 2,
		GPUsPerReplica: 32, Role: domain.RoleDecode, AcceleratorName: "NVIDIA-H100-80GB-HBM3"}, states["m-decode"])
	assert.Equal(t, 1, states["m-prefill"].CurrentReplicas)
	assert.Zero(t, states["m-prefill"].PendingReplicas)
}

// The same fleet with the roles from --role-of: the groups are replicas
// whoever names the variants.
func TestCapture_LWSGroupsWithRoleOfAreReplicas(t *testing.T) {
	d := lwsFleetCapture(t, decodeRole(everyPod), prefillRole(everyPod), func(o *Options) {
		for _, r := range []string{"decode=llm-d.ai/role=decode", "prefill=llm-d.ai/role=prefill"} {
			parsed, err := ParseRoleOf(r)
			require.NoError(t, err)
			o.RoleOf = append(o.RoleOf, parsed)
		}
	})

	require.Len(t, d.window.Variants, 2)
	assert.Equal(t, 32, d.window.Variants[0].GpuCount, "GPUs per replica are the group's")
	in := d.cycles[0].Input
	assert.Len(t, in.ReplicaMetrics, 3, "one row per group")
	states := statesByName(in.VariantStates)
	assert.Equal(t, 2, states["decode"].CurrentReplicas, "a group is one replica")
	assert.Zero(t, states["decode"].PendingReplicas)
	assert.Equal(t, 1, states["prefill"].CurrentReplicas)
}

// A worker not Ready makes its group pending, once.
func TestCapture_LWSGroupWithAWorkerNotReadyIsOnePendingReplica(t *testing.T) {
	dec := decodeRole(everyPod)
	dec.notReady = map[string]bool{"m-abc-decode-1-2": true, "m-abc-decode-1-3": true}
	d := lwsFleetCapture(t, dec, prefillRole(everyPod))

	states := statesByName(d.cycles[0].Input.VariantStates)
	assert.Equal(t, 2, states["m-decode"].CurrentReplicas)
	assert.Equal(t, 1, states["m-decode"].PendingReplicas)
}

// Under Ray the leader alone reports for its group: one row per group with
// the leader's figures, as before, and the group's GPUs.
func TestCapture_LWSLeaderOnlyIsOneReplicaPerGroup(t *testing.T) {
	d := lwsFleetCapture(t, decodeRole(leaderOnly), prefillRole(leaderOnly))

	assert.Equal(t, 32, d.window.Variants[0].GpuCount)
	in := d.cycles[0].Input
	require.Len(t, in.ReplicaMetrics, 3)
	for _, r := range in.ReplicaMetrics {
		assert.Equal(t, 1, r.Ranks(), r.PodName)
	}
	states := statesByName(in.VariantStates)
	assert.Equal(t, 2, states["m-decode"].CurrentReplicas)
	assert.Zero(t, states["m-decode"].PendingReplicas)
}

// A fleet of StatefulSet-owned Pods kube-state-metrics shows no workload
// labels for fails, and the error names the --role-of that would read it.
func TestCapture_NothingAttributedNamesTheRoleOfToPass(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	dec := decodeRole(everyPod)
	dec.noGroupLabels = true
	dec.add(t, srv)
	opts := Options{Prometheus: apiFor(t, srv), Namespace: testNS, From: t0, To: t0, Step: 15e9,
		KubeStateMetrics: true}
	_, err := Capture(context.Background(), opts, &bytes.Buffer{})
	require.ErrorIs(t, err, ErrNothingAttributed)
	assert.Contains(t, err.Error(), "8 Pods with one have no variant, m-abc-decode-0 among them, owned by a StatefulSet")
	assert.Contains(t, err.Error(), "labelled llm-d.ai/role=decode")
	assert.Contains(t, err.Error(), "pass --role-of decode=llm-d.ai/role=decode")
}

// A group whose engines all read an ITL but one gives the line no reading
// that cycle, and the capture says so, so a decode knee the planner then
// cannot price has a reason beside it.
func TestCapture_NotesAGroupWhoseITLAnEngineDidNotReport(t *testing.T) {
	run := func(itl func(int, int) bool) *Summary {
		t.Helper()
		srv := promtest.New()
		t.Cleanup(srv.Close)
		dec := decodeRole(everyPod)
		dec.itl = itl
		dec.add(t, srv)
		prefillRole(everyPod).add(t, srv)
		opts := Options{Prometheus: apiFor(t, srv), Namespace: testNS, From: t0, To: t0, Step: 15e9,
			KubeStateMetrics: true}
		_, sum := capture(t, opts)
		return sum
	}

	sum := run(func(g, w int) bool { return g != 1 || w != 2 })
	assert.Empty(t, sum.Notes, "the line reads a group engine by engine: the three that read an ITL are readings")

	assert.Empty(t, run(everyPod).Notes, "every engine read an ITL")
}

// registeredPromQL is promQL for a query the registry may not hold: false
// when it does not, so a fixture built for a newer tree still builds here.
func registeredPromQL(t testing.TB, name string) (string, bool) {
	t.Helper()
	reg := source.NewSourceRegistry()
	require.NoError(t, reg.Register("prometheus", source.NewNoOpSource()))
	registration.RegisterSaturationQueries(reg)
	if reg.Get("prometheus").QueryList().Get(name) == nil {
		return "", false
	}
	return promQL(t, name), true
}
