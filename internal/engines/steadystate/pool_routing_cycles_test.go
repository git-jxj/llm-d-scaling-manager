package steadystate

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// routingCycle is one cycle as Route leaves it: the canary held when known,
// nothing established when the pools could not be read.
func routingCycle(known bool) ([]domain.VariantMetadata, []variantmeta.ModelRouting) {
	metas := []domain.VariantMetadata{
		{VariantName: "routed", Namespace: "ns", ModelID: "m", CurrentReplicas: 2},
		{VariantName: "canary", Namespace: "ns", ModelID: "m", CurrentReplicas: 3},
	}
	if !known {
		return metas, []variantmeta.ModelRouting{{Namespace: "ns", ModelID: "m", PoolReadFailed: "forbidden"}}
	}
	metas[0].RoutingKnown = true
	metas[1].RoutingKnown = true
	metas[1].HeldUnrouted, metas[1].UnroutedReplicas, metas[1].UnroutedReady = true, 3, 3
	metas[1].UnroutedPods = []string{"c-0", "c-1", "c-2"}
	return metas, []variantmeta.ModelRouting{{Namespace: "ns", ModelID: "m", Pools: 1, Routed: true, Known: true,
		Selected: []string{"routed"}, Held: []string{"canary"}}}
}

func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// A held variant stays held across a cycle whose reads failed, and the hold
// lapses to "counted as before" -- not to a release logged as one -- once the
// failure outlasts routingVerdictTTL. A later good read holds it again. The
// held Event is recorded on the change into the held state and then only
// every heldEventEvery, never once per cycle.
func TestHoldSurvivesAFailedReadForABoundedTime(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)
	rec := record.NewFakeRecorder(20)
	e := &Engine{Recorder: rec}
	va := &llmdvariant.Variant{ObjectMeta: metav1.ObjectMeta{Name: "canary", Namespace: "ns"}}
	variants := map[string]*llmdvariant.Variant{"ns/canary": va}
	t0 := time.Now()
	cycle := func(at time.Time, known bool) domain.VariantMetadata {
		metas, results := routingCycle(known)
		e.resolveRouting(context.Background(), "m", "ns", metas, results, variants, at)
		return metas[1]
	}

	if m := cycle(t0, true); !m.HeldUnrouted {
		t.Fatal("cycle 1: the canary is not held")
	}
	events := drainEvents(rec)
	if len(events) != 1 {
		t.Fatalf("cycle 1: %d Events, want 1 on the change into held", len(events))
	}
	for _, want := range []string{constants.K8SEventHeldNotInPool, "is held at 3", "no InferencePool", "routed", "To release it", "LLMSM_POOL_SELECTED_SUPPLY=false"} {
		if !strings.Contains(events[0], want) {
			t.Errorf("Event %q does not contain %q", events[0], want)
		}
	}

	m := cycle(t0.Add(time.Minute), false)
	if !m.HeldUnrouted || m.UnroutedReplicas != 3 || len(m.UnroutedPods) != 3 {
		t.Fatalf("cycle 2, read failed: %+v, want the last verdict kept", m)
	}
	at := t0.Add(time.Minute)
	if !decision.DefaultHeld.Held("ns", "m", "canary", 0, at) || decision.DefaultHeld.Covers("ns", "m", "canary", 0, at) {
		t.Error("cycle 2: scale from zero no longer sees the hold")
	}
	if !decision.DefaultHeld.Covers("ns", "m", "routed", 0, at) {
		t.Error("cycle 2: the routed variant no longer covers its model")
	}

	if m := cycle(t0.Add(2*time.Minute), true); !m.HeldUnrouted {
		t.Fatal("cycle 3: not held")
	}
	if n := len(drainEvents(rec)); n != 0 {
		t.Errorf("cycles 2-3: %d Events, want none: the hold did not change", n)
	}

	lapse := t0.Add(2*time.Minute + routingVerdictTTL + time.Second)
	m = cycle(lapse, false)
	if m.HeldUnrouted || m.UnroutedReplicas != 0 {
		t.Fatalf("read failing past the TTL: %+v, want counted as before", m)
	}
	if decision.DefaultHeld.Held("ns", "m", "canary", 0, lapse) || !decision.DefaultHeld.Covers("ns", "m", "canary", 0, lapse) {
		t.Error("a lapsed hold is still published to scale from zero")
	}

	if m := cycle(t0.Add(heldEventEvery+time.Hour), true); !m.HeldUnrouted {
		t.Fatal("a good read after the lapse does not hold again")
	}
	if n := len(drainEvents(rec)); n != 1 {
		t.Errorf("after heldEventEvery: %d Events, want the reminder", n)
	}
}

// A variant never seen before, with a failed read, is counted as before.
func TestAnUnknownVerdictWithNoHistoryChangesNothing(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)
	e := &Engine{}
	metas, results := routingCycle(false)
	e.resolveRouting(context.Background(), "m", "ns", metas, results, nil, time.Now())
	if metas[1].HeldUnrouted || metas[1].UnroutedReplicas != 0 {
		t.Fatalf("%+v: a variant with no history was held on a failed read", metas[1])
	}
}

func gatherValue(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) (float64, int) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		total, series := 0.0, 0
		for _, m := range f.GetMetric() {
			match := true
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					match = false
				}
			}
			if match {
				series++
				total += m.GetCounter().GetValue() + m.GetGauge().GetValue()
			}
		}
		return total, series
	}
	return 0, 0
}

// Every model of a namespace reads one InferencePool listing per cycle, from
// the uncached reader, and a failed one is counted once per namespace per
// cycle with its reason.
func TestPoolListerReadsOncePerNamespacePerCycle(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := v1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	var lists atomic.Int32
	refuse := atomic.Bool{}
	reader := interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).Build(), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			lists.Add(1)
			if refuse.Load() {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "inferencepools"}, "", errors.New("no"))
			}
			return c.List(ctx, l, opts...)
		},
	})
	cfg := config.NewTestConfig()
	config.SetPoolGroupForTest(cfg, v1.GroupName)
	e := &Engine{Config: cfg, apiReader: reader}
	pools := e.poolLister()
	for range 3 {
		if _, err := pools(context.Background(), "ns"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pools(context.Background(), "other"); err != nil {
		t.Fatal(err)
	}
	if n := lists.Load(); n != 2 {
		t.Fatalf("%d listings, want 2: one per namespace", n)
	}

	refuse.Store(true)
	e.resetPoolReads()
	for range 3 {
		if _, err := pools(context.Background(), "ns"); err == nil {
			t.Fatal("a refused listing returned no error")
		}
	}
	if n := lists.Load(); n != 3 {
		t.Fatalf("%d listings, want 3: a new cycle reads again, once", n)
	}
	if v, _ := gatherValue(t, registry, constants.LLMSMMetricsCollectionErrorsTotal,
		map[string]string{constants.LabelQueryType: constants.QueryTypePoolList, constants.LabelReason: "forbidden"}); v != 1 {
		t.Fatalf("pool_list forbidden = %v, want 1 per namespace per cycle", v)
	}
}

// LLMSM_POOL_SELECTED_SUPPLY=false: rows and metadata are left as before, and
// whatever an earlier cycle held or published is cleared, the gauge included.
func TestPoolSelectedSupplyOff(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewTestConfig()
	config.SetPoolSelectedSupplyForTest(cfg, false)
	e := &Engine{Config: cfg}
	decision.DefaultHeld.Publish("ns", "m", []string{"canary"}, []string{"routed"}, time.Now())
	metrics.SetUnroutedReplicas("ns", "m", map[string]int{"canary": 1})
	if _, n := gatherValue(t, registry, constants.LLMSMUnroutedReplicas, map[string]string{constants.LabelModelName: "m"}); n != 1 {
		t.Fatalf("precondition: %d gauge series, want 1", n)
	}

	rows := []domain.ReplicaMetrics{{PodName: "c-0", VariantName: "canary"}}
	metas := []domain.VariantMetadata{{VariantName: "canary", Namespace: "ns", ModelID: "m", CurrentReplicas: 1}}
	got := e.routeModel(context.Background(), "m", "ns", metas, nil, nil, nil, rows, nil, time.Now())
	if len(got) != 1 || metas[0].HeldUnrouted || metas[0].RoutingKnown {
		t.Fatalf("rows=%v meta=%+v, want both untouched", got, metas[0])
	}
	if decision.DefaultHeld.Held("ns", "m", "canary", 0, time.Now()) {
		t.Error("an earlier hold survives turning the feature off")
	}
	if _, n := gatherValue(t, registry, constants.LLMSMUnroutedReplicas, map[string]string{constants.LabelModelName: "m"}); n != 0 {
		t.Errorf("%d llmsm_unrouted_replicas series left with the feature off", n)
	}
}

// ownedPod is a running, Ready Pod of Deployment dep.
func ownedPod(name, dep string, labels map[string]string) *corev1.Pod {
	l := map[string]string{"app": dep, appsv1.DefaultDeploymentUniqueLabelKey: "h1"}
	for k, v := range labels {
		l[k] = v
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: l,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: dep + "-h1", UID: "rs", Controller: ptr.To(true)}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

func routedDeployment(name string, replicas int32, labels map[string]string) *appsv1.Deployment {
	tpl := map[string]string{"app": name}
	for k, v := range labels {
		tpl[k] = v
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: tpl}}},
		Status: appsv1.DeploymentStatus{Replicas: replicas, ReadyReplicas: replicas},
	}
}

// The wiring: prepareModelData's assembly runs routing, so the canary's rows
// are gone and it is held, against a real (fake) InferencePool. A second
// cycle with the routed variant parked keeps the canary held -- the model is
// still routed through the parked variant -- and publishes it so scale from
// zero wakes the routed one.
func TestAssembleModelDataRoutesTheModel(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	guide := map[string]string{"llm-d.ai/guide": "g"}
	pool := &v1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       v1.InferencePoolSpec{Selector: v1.LabelSelector{MatchLabels: map[v1.LabelKey]v1.LabelValue{"llm-d.ai/guide": "g"}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool,
		ownedPod("a-0", "a", guide), ownedPod("b-0", "b", nil), ownedPod("b-1", "b", nil)).Build()
	cfg := config.NewTestConfig()
	config.SetPoolSelectedSupplyForTest(cfg, true)
	config.SetPoolGroupForTest(cfg, v1.GroupName)
	e := &Engine{Config: cfg, client: c, apiReader: c}
	va := func(name, dep string) llmdvariant.Variant {
		return llmdvariant.Variant{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Spec: llmdvariant.Spec{ModelID: "m", ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: dep}}}
	}
	vas := []llmdvariant.Variant{va("va", "a"), va("vb", "b")}
	targets := map[string]scaletarget.ScaleTargetAccessor{
		"ns/a": scaletarget.NewDeploymentAccessor(routedDeployment("a", 1, guide)),
		"ns/b": scaletarget.NewDeploymentAccessor(routedDeployment("b", 2, nil)),
	}
	rows := []domain.ReplicaMetrics{{PodName: "a-0", VariantName: "va"}, {PodName: "b-0", VariantName: "vb"}, {PodName: "b-1", VariantName: "vb"}}
	e.parked = parkedVariants{known: true}
	data := e.assembleModelData(context.Background(), "m", "ns", vas, targets, nil, rows, c)
	if len(data.replicaMetrics) != 1 || data.replicaMetrics[0].PodName != "a-0" {
		t.Fatalf("rows %v, want only the routed variant's", data.replicaMetrics)
	}
	if len(data.variantMetadata) != 2 || !data.variantMetadata[1].HeldUnrouted {
		t.Fatalf("metadata %+v, want vb held", data.variantMetadata)
	}

	// Next cycle: scale-to-zero parked va; only vb runs.
	e.resetPoolReads()
	targets["ns/a"] = scaletarget.NewDeploymentAccessor(routedDeployment("a", 0, guide))
	e.parked = parkedVariants{byModel: map[string][]llmdvariant.Variant{"ns/m": {va("va", "a")}}, targets: targets, known: true}
	data = e.assembleModelData(context.Background(), "m", "ns", vas[1:], targets, nil, rows[1:], c)
	if len(data.variantMetadata) != 1 || !data.variantMetadata[0].HeldUnrouted || len(data.replicaMetrics) != 0 {
		t.Fatalf("parked cycle: metadata %+v rows %v, want vb still held and its rows dropped", data.variantMetadata, data.replicaMetrics)
	}
	now := time.Now()
	if decision.DefaultHeld.Covers("ns", "m", "vb", 0, now) || !decision.DefaultHeld.Covers("ns", "m", "va", 0, now) {
		t.Error("scale from zero would count the held canary as the model's decode, and never wake va")
	}
}
