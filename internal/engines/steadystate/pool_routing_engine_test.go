package steadystate

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/prometheus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/controllerstatus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// These drive the engine's own per-cycle path -- optimize(), and
// prepareModelData with a replica source -- so that removing readParked, a
// routeParkedModels call or the assembleModelData call fails a test.

const routeNS = "ns"

var routeGuideLabels = map[string]string{"llm-d.ai/guide": "g"}

// routeDeploy is Deployment d-<variant> with app=<variant> and, when routed,
// the pool label on its template.
func routeDeploy(variant string, replicas int32, routed bool) *appsv1.Deployment {
	tpl := map[string]string{"app": variant}
	if routed {
		for k, v := range routeGuideLabels {
			tpl[k] = v
		}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "d-" + variant, Namespace: routeNS},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": variant}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: tpl}}},
		Status: appsv1.DeploymentStatus{Replicas: replicas, ReadyReplicas: replicas},
	}
}

// routeVA is variant <name> of model, scaled by d-<name>.
func routeVA(name, model string) llmdvariant.Variant {
	return llmdvariant.Variant{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: routeNS},
		Spec: llmdvariant.Spec{ModelID: model,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "d-" + name}},
	}
}

func routePool() *v1.InferencePool {
	return &v1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: routeNS},
		Spec:       v1.InferencePoolSpec{Selector: v1.LabelSelector{MatchLabels: map[v1.LabelKey]v1.LabelValue{"llm-d.ai/guide": "g"}}},
	}
}

func routeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// routeRegistry registers each variant of a model with its scale target, as
// KEDA calling about its ScaledObject does.
func routeRegistry(models map[string][]string) *registry.Registry {
	reg := registry.New(0)
	for model, variants := range models {
		for _, v := range variants {
			reg.Observe(routeNS, v, map[string]string{registry.ModelIDKey: model})
			reg.SetTarget(routeNS, v, registry.Target{APIVersion: "apps/v1", Kind: "Deployment", Name: "d-" + v})
		}
	}
	return reg
}

func routeConfig() *config.Config {
	cfg := config.NewTestConfig()
	config.SetPoolSelectedSupplyForTest(cfg, true)
	config.SetPoolGroupForTest(cfg, v1.GroupName)
	return cfg
}

// routeEngine is a whole engine over c, as the controller builds it.
func routeEngine(t *testing.T, c client.Client, reg *registry.Registry) *Engine {
	t.Helper()
	ctx := context.Background()
	sources := source.NewSourceRegistry()
	if err := sources.Register("prometheus", prometheus.NewPrometheusSource(ctx, nil, prometheus.DefaultPrometheusSourceConfig())); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(c, c, c.Scheme(), nil, sources, routeConfig(), allocation.NewNoOpLimiter("test"))
	e.Variants = reg
	e.Status = controllerstatus.New(c, c, "scaling-manager-system")
	return e
}

func runCycle(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.optimize(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("optimize: %v", err)
	}
}

// Every variant of the model parked: optimize() takes the no-active-model path,
// and must still publish the model's routing, so scale from zero wakes the
// routed variant and never the held one.
func TestOptimizePublishesTheRoutingOfAnAllParkedModel(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)
	c := routeClient(t, routePool(), routeDeploy("routed", 0, true), routeDeploy("canary", 0, false))
	e := routeEngine(t, c, routeRegistry(map[string][]string{"m": {"routed", "canary"}}))
	runCycle(t, e)
	now := time.Now()
	if !decision.DefaultHeld.Held(routeNS, "m", "canary", 0, now) {
		t.Error("the parked canary is not held: scale from zero could wake it")
	}
	if decision.DefaultHeld.Held(routeNS, "m", "routed", 0, now) || !decision.DefaultHeld.Covers(routeNS, "m", "routed", 0, now) {
		t.Error("the parked routed variant is not published as selected")
	}
}

// A cycle that analyzes some model still publishes the routing of a model
// whose variants are all parked.
func TestOptimizePublishesAParkedModelBesideAnActiveOne(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)
	c := routeClient(t, routePool(),
		routeDeploy("routed", 0, true), routeDeploy("canary", 0, false),
		routeDeploy("other", 1, true))
	e := routeEngine(t, c, routeRegistry(map[string][]string{"m": {"routed", "canary"}, "m2": {"other"}}))
	runCycle(t, e)
	if !decision.DefaultHeld.Held(routeNS, "m", "canary", 0, time.Now()) {
		t.Error("a parked model beside an active one has no routing published")
	}
}

// prepareModelData with the engine's own readParked, as optimize() calls it:
// after a restart with the routed variant already at zero, the running canary
// is held on the first cycle and its rows never reach the analyzers. With the
// parked variants unreadable, nothing is held and nothing is published.
func TestPrepareModelDataHoldsTheCanaryAfterARestart(t *testing.T) {
	for _, tc := range []struct {
		name         string
		parkedKnown  bool
		wantHeld     bool
		wantRows     int
		wantRouting  bool
		wantRoutKnow bool
	}{
		{name: "parked variants read", parkedKnown: true, wantHeld: true, wantRows: 0, wantRouting: true, wantRoutKnow: true},
		{name: "parked variants unreadable", parkedKnown: false, wantHeld: false, wantRows: 1, wantRouting: false, wantRoutKnow: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(decision.DefaultHeld.Reset)
			canaryPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "canary" + podSuffix, Namespace: routeNS,
					Labels: map[string]string{"app": "canary", appsv1.DefaultDeploymentUniqueLabelKey: "h1"},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet",
						Name: "d-canary-h1", UID: "rs", Controller: ptr.To(true)}}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			}
			c := routeClient(t, routePool(), routeDeploy("routed", 0, true), routeDeploy("canary", 1, false), canaryPod)
			e, _ := engineConfigEngine(t)
			e.Config, e.client, e.apiReader = routeConfig(), c, c
			if tc.parkedKnown {
				e.Variants = routeRegistry(map[string][]string{"m": {"routed", "canary"}})
			}
			e.ReplicaMetricsCollector = collector.NewReplicaMetricsCollector(
				replicaSource{modelID: "m", pods: []string{"canary" + podSuffix}}, c, nil, nil, variantOfPodLocator{})
			e.parked = e.readParked(context.Background())

			md, err := e.prepareModelData(context.Background(), "m", []llmdvariant.Variant{routeVA("canary", "m")}, c)
			if err != nil || md == nil {
				t.Fatalf("prepareModelData: %v, %v", md, err)
			}
			if len(md.variantMetadata) != 1 || md.variantMetadata[0].HeldUnrouted != tc.wantHeld ||
				md.variantMetadata[0].RoutingKnown != tc.wantRoutKnow {
				t.Fatalf("metadata %+v, want held=%t known=%t", md.variantMetadata, tc.wantHeld, tc.wantRoutKnow)
			}
			if len(md.replicaMetrics) != tc.wantRows {
				t.Fatalf("%d rows reach the analyzers, want %d", len(md.replicaMetrics), tc.wantRows)
			}
			now := time.Now()
			published := decision.DefaultHeld.Held(routeNS, "m", "canary", 0, now) ||
				!decision.DefaultHeld.Covers(routeNS, "m", "canary", 0, now)
			if published != tc.wantRouting {
				t.Fatalf("routing published = %t, want %t", published, tc.wantRouting)
			}
		})
	}
}

// The model-level verdict lapses routingVerdictTTL after the last good read,
// like the per-variant ones: reused, it keeps the time it was established.
func TestModelVerdictLapsesWhileReadsKeepFailing(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)
	e := &Engine{}
	t0 := time.Now()
	metas, results := routingCycle(true)
	e.resolveRouting(context.Background(), "m", "ns", metas, results, nil, t0)
	for i := 1; i <= 12; i++ {
		at := t0.Add(time.Duration(i) * 30 * time.Second)
		metas, results = routingCycle(false)
		e.resolveRouting(context.Background(), "m", "ns", metas, results, nil, at)
		fresh := at.Sub(t0) <= routingVerdictTTL
		if got := decision.DefaultHeld.Held("ns", "m", "canary", 0, at); got != fresh {
			t.Fatalf("after %s of failed reads, held=%t, want %t", at.Sub(t0), got, fresh)
		}
	}
}
