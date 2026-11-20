package steadystate

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/controllerstatus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

var decodeLabels = map[string]string{"app": "decode", variantmeta.RoleLabel: "decode"}

// stormFixture is a decode Deployment asking for 2 replicas with 1 Ready, and
// rejected pods on gpu-node-7 created every 2 s up to now.
func stormFixture(t *testing.T, rejected int, now time.Time) (client.Client, *appsv1.Deployment) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "decode", Namespace: "ns"}}
	d.Spec.Replicas = ptr.To(int32(2))
	d.Spec.Template.Labels = decodeLabels
	d.Status.Replicas, d.Status.ReadyReplicas = 2, 1
	objs := make([]client.Object, 0, 1+rejected)
	objs = append(objs, d)
	for i := range rejected {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("decode-rejected-%03d", i), Namespace: "ns",
				Labels: decodeLabels, CreationTimestamp: metav1.NewTime(now.Add(-time.Duration(rejected-i) * 2 * time.Second))},
			Spec: corev1.PodSpec{NodeName: "gpu-node-7"},
			Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "UnexpectedAdmissionError",
				Message: "Allocate failed due to no healthy devices present; cannot allocate unhealthy devices rdma/ib"},
		})
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), d
}

// targetsOf is the cycle's active scale targets holding d alone.
func targetsOf(d *appsv1.Deployment) map[string]scaletarget.ScaleTargetAccessor {
	return map[string]scaletarget.ScaleTargetAccessor{
		utils.GetNamespacedKey(d.Namespace, d.Name): scaletarget.NewDeploymentAccessor(d),
	}
}

func decodeVariant() llmdvariant.Variant {
	return llmdvariant.Variant{
		ObjectMeta: metav1.ObjectMeta{Name: "decode", Namespace: "ns"},
		Spec: llmdvariant.Spec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "decode"},
			ModelID:        "m", MaxReplicas: 4,
		},
	}
}

// notArrivingSeries is the published llmsm_replicas_not_arriving series as
// "variant/role/reason=value".
func notArrivingSeries(t *testing.T, registry *prometheus.Registry) []string {
	t.Helper()
	mfs, err := registry.Gather()
	require.NoError(t, err)
	var out []string
	for _, mf := range mfs {
		if mf.GetName() != constants.LLMSMReplicasNotArriving {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, p := range m.GetLabel() {
				l[p.GetName()] = p.GetValue()
			}
			out = append(out, fmt.Sprintf("%s/%s/%s=%v", l[constants.LabelVariantName], l[constants.LabelRole],
				l[constants.LabelReason], m.GetGauge().GetValue()))
		}
	}
	return out
}

func drain(r *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-r.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// The incident behind issue #224, cycle by cycle: nothing before the
// threshold, one Warning naming the cause and the node after it, no repeat
// every cycle, a repeat after notArrivingEventEvery, and a Normal event with
// every trace gone once the replica arrives.
func TestReplicasNotArrivingAdmissionStorm(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	c, d := stormFixture(t, 169, t0.Add(6*time.Minute))
	recorder := record.NewFakeRecorder(100)
	status := controllerstatus.New(nil, nil, "ctrl")
	e := &Engine{Recorder: recorder, client: c, Status: status, cycleScaleTargets: targetsOf(d)}
	groups := map[string][]llmdvariant.Variant{"m|ns": {decodeVariant()}}
	ctx := context.Background()

	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0)
	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0.Add(4*time.Minute))
	assert.Empty(t, drain(recorder), "inside the threshold (5m floor): a rollout, not a fault")
	assert.Empty(t, notArrivingSeries(t, registry))
	assert.NotContains(t, status.Data(), controllerstatus.KeyReplicasNotArriving)

	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0.Add(6*time.Minute))
	events := drain(recorder)
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(events[0], "Warning "+constants.K8SEventReplicasNotArriving+" "), events[0])
	for _, want := range []string{"1 of 2 replicas Ready since 2026-11-20T10:00:00Z", "169 pods rejected by the kubelet at admission",
		"on node gpu-node-7", "cannot allocate unhealthy devices rdma/ib", "platform"} {
		assert.Contains(t, events[0], want)
	}
	assert.Equal(t, []string{"decode/decode/admission-rejected=1"}, notArrivingSeries(t, registry))
	line := status.Data()[controllerstatus.KeyReplicasNotArriving]
	assert.Equal(t, "ns/decode (decode): 1/2 Ready since 2026-11-20T10:00:00Z: admission-rejected on node gpu-node-7: "+
		"UnexpectedAdmissionError: Allocate failed due to no healthy devices present; cannot allocate unhealthy devices rdma/ib", line)

	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0.Add(7*time.Minute))
	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0.Add(20*time.Minute))
	assert.Empty(t, drain(recorder), "same cause inside notArrivingEventEvery: no repeat")
	assert.Equal(t, line, status.Data()[controllerstatus.KeyReplicasNotArriving],
		"the status line does not move while the cause holds, so the ConfigMap is not rewritten every cycle")

	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0.Add(21*time.Minute))
	assert.Len(t, drain(recorder), 1, "a reminder once notArrivingEventEvery has passed")

	d.Status.ReadyReplicas = 2
	require.NoError(t, c.Status().Update(ctx, d))
	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0.Add(25*time.Minute))
	assert.Empty(t, drain(recorder), "arrived for less than notArrivingClearHold: not lifted yet")
	assert.NotEmpty(t, notArrivingSeries(t, registry))
	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0.Add(26*time.Minute))
	events = drain(recorder)
	require.Len(t, events, 1)
	assert.Equal(t, "Normal "+constants.K8SEventReplicasArrived+
		" 2 of 2 replicas Ready; they were short since 2026-11-20T10:00:00Z", events[0])
	assert.Empty(t, notArrivingSeries(t, registry))
	assert.NotContains(t, status.Data(), controllerstatus.KeyReplicasNotArriving)
}

// A new cause is reported at once, not after the reminder interval, and the
// metric moves to the new reason rather than keeping both.
func TestReplicasNotArrivingCauseChange(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	t0 := time.Now()
	c, d := stormFixture(t, 0, t0)
	recorder := record.NewFakeRecorder(100)
	e := &Engine{Recorder: recorder, client: c, cycleScaleTargets: targetsOf(d)}
	groups := map[string][]llmdvariant.Variant{"m|ns": {decodeVariant()}}
	ctx := context.Background()

	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0)
	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0.Add(6*time.Minute))
	events := drain(recorder)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "no pod exists for the missing replicas")
	assert.Equal(t, []string{"decode/decode/no-pods=1"}, notArrivingSeries(t, registry))

	require.NoError(t, c.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "decode-x", Namespace: "ns", Labels: decodeLabels,
			CreationTimestamp: metav1.NewTime(t0.Add(6 * time.Minute))},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/12 nodes are available: 12 Insufficient nvidia.com/gpu."}}},
	}))
	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0.Add(7*time.Minute))
	events = drain(recorder)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "1 pod unschedulable (0/12 nodes are available: 12 Insufficient nvidia.com/gpu.)")
	assert.Equal(t, []string{"decode/decode/unschedulable=1"}, notArrivingSeries(t, registry))

	// The variant goes away: its series goes with it.
	e.reportReplicasNotArriving(ctx, map[string][]llmdvariant.Variant{}, nil, nil, t0.Add(8*time.Minute))
	assert.Empty(t, notArrivingSeries(t, registry))
}

// The threshold is twice the role's known start time, never under the floor;
// an unknown role waits on the default start time, which the floor covers.
func TestNotArrivingThreshold(t *testing.T) {
	e := &Engine{}
	tr := e.notArrivingState()
	tr.noteRoleStarts([]allocation.ModelScalingRequest{{Namespace: "ns", ModelID: "m",
		CompositeSignal: allocation.NamedAnalyzerResult{Result: &domain.AnalyzerResult{
			RoleStartSeconds: map[string]float64{"decode": 500, "prefill": 30}}}}})
	d, known := tr.threshold("ns", "m", "decode")
	assert.True(t, known)
	assert.Equal(t, 1000*time.Second, d)
	d, known = tr.threshold("ns", "m", "prefill")
	assert.True(t, known)
	assert.Equal(t, notArrivingFloor, d)
	d, known = tr.threshold("ns", "other", "decode")
	assert.False(t, known)
	assert.Equal(t, notArrivingFloor, d)
}

// A starting pod past twice its role's known start time is not counted as on
// its way; with no known start time nothing changes, so a slow model never
// measured is not ordered twice.
func TestWithholdOverdueArrivals(t *testing.T) {
	e := &Engine{}
	e.notArrivingState().noteRoleStarts([]allocation.ModelScalingRequest{{Namespace: "ns", ModelID: "m",
		CompositeSignal: allocation.NamedAnalyzerResult{Result: &domain.AnalyzerResult{
			RoleStartSeconds: map[string]float64{"decode": 200}}}}})
	metas := []domain.VariantMetadata{
		{VariantName: "decode", Namespace: "ns", Role: "decode", PendingReplicas: 2, PendingAges: []float64{100, 900}},
		{VariantName: "prefill", Namespace: "ns", Role: "prefill", PendingReplicas: 1, PendingAges: []float64{900}},
	}
	e.withholdOverdueArrivals(context.Background(), "m", metas)

	assert.Equal(t, []float64{100}, metas[0].PendingAges)
	assert.Equal(t, 1, metas[0].StuckReplicas, "900 s is past max(5m, 2x200s)")
	assert.Equal(t, []float64{900}, metas[1].PendingAges, "prefill's start time is not known")
	assert.Equal(t, 0, metas[1].StuckReplicas)
}

// A rollout or a scale-up that lands a replica every few minutes is never
// reported, however long it takes as a whole: the clock restarts when a
// replica arrives. A replica that then stops arriving is, once its own pod is
// older than the threshold.
func TestReplicasNotArrivingProgressIsNotAFault(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	t0 := time.Now()
	c, d := stormFixture(t, 0, t0)
	d.Spec.Replicas = ptr.To(int32(4))
	d.Status.ReadyReplicas = 0
	recorder := record.NewFakeRecorder(100)
	e := &Engine{Recorder: recorder, client: c, cycleScaleTargets: targetsOf(d)}
	groups := map[string][]llmdvariant.Variant{"m|ns": {decodeVariant()}}
	ctx := context.Background()

	for minute := 0; minute <= 16; minute++ {
		d.Status.ReadyReplicas = int32(min(minute/4, 3))
		e.reportReplicasNotArriving(ctx, groups, nil, nil, t0.Add(time.Duration(minute)*time.Minute))
	}
	assert.Empty(t, drain(recorder), "one replica every 4 minutes for 12 minutes: progress, not a fault")

	require.NoError(t, c.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "decode-last", Namespace: "ns", Labels: decodeLabels,
			CreationTimestamp: metav1.NewTime(t0.Add(12 * time.Minute))},
		Spec: corev1.PodSpec{NodeName: "gpu-node-3"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}},
	}))
	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0.Add(18*time.Minute))
	events := drain(recorder)
	require.Len(t, events, 1, "the fourth replica made no progress for 6 minutes")
	assert.Contains(t, events[0], "1 pod still starting on node gpu-node-3")
	assert.Equal(t, []string{"decode/decode/starting=1"}, notArrivingSeries(t, registry))
}

// A scale target that is no longer among the cycle's active ones takes its
// report with it, rather than leaving a series that alerts forever.
func TestReplicasNotArrivingScaleTargetGone(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	t0 := time.Now()
	c, d := stormFixture(t, 0, t0)
	e := &Engine{Recorder: record.NewFakeRecorder(100), client: c, cycleScaleTargets: targetsOf(d)}
	groups := map[string][]llmdvariant.Variant{"m|ns": {decodeVariant()}}
	ctx := context.Background()
	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0)
	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0.Add(6*time.Minute))
	require.NotEmpty(t, notArrivingSeries(t, registry))

	e.cycleScaleTargets = nil
	e.reportReplicasNotArriving(ctx, groups, nil, nil, t0.Add(7*time.Minute))
	assert.Empty(t, notArrivingSeries(t, registry))
	assert.Empty(t, e.notArriving.variants)
}
