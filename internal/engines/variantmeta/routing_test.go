package variantmeta

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

const routeGuide = "llm-d.ai/guide"

// routeDeployment is a Deployment whose selector is app=<name> and whose
// template adds the given pool labels, as an llm-d workload's does.
func routeDeployment(name string, replicas, ready int32, poolLabels map[string]string) *appsv1.Deployment {
	tpl := map[string]string{"app": name}
	maps.Copy(tpl, poolLabels)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: tpl}},
		},
		Status: appsv1.DeploymentStatus{Replicas: replicas, ReadyReplicas: ready},
	}
}

// routePod is a Pod of Deployment dep with the given labels on top of app=dep.
func routePod(name, dep string, ready bool, extra map[string]string) *corev1.Pod {
	labels := map[string]string{"app": dep}
	maps.Copy(labels, extra)
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if ready {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return ownedBy(p, dep)
}

func routeVariant(name, kind, target string) llmdvariant.Variant {
	return llmdvariant.Variant{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: llmdvariant.Spec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: kind, Name: target},
			ModelID:        "m",
		},
	}
}

func lister(sels []map[string]string, err error) PoolLister {
	return func(context.Context, string) ([]map[string]string, error) { return sels, err }
}

type routeWant struct {
	unrouted, ready int
	pods            []string
	held, known     bool
}

func gotRoute(m domain.VariantMetadata) routeWant {
	got := routeWant{unrouted: m.UnroutedReplicas, ready: m.UnroutedReady, held: m.HeldUnrouted, known: m.RoutingKnown}
	if len(m.UnroutedPods) > 0 {
		got.pods = slices.Sorted(slices.Values(m.UnroutedPods))
	}
	return got
}

// runRoute discovers and routes the given Deployments' variants.
func runRoute(t *testing.T, c client.Client, deps []*appsv1.Deployment, pools PoolLister) ([]domain.VariantMetadata, []ModelRouting) {
	t.Helper()
	targets := map[string]scaletarget.ScaleTargetAccessor{}
	vas := make([]llmdvariant.Variant, 0, len(deps))
	for _, d := range deps {
		targets["ns/"+d.Name] = scaletarget.NewDeploymentAccessor(d)
		vas = append(vas, routeVariant("v"+d.Name, "Deployment", d.Name))
	}
	metas := Discover(context.Background(), vas, targets, c, DeclaredOnly)
	return metas, Route(context.Background(), metas, vas, targets, c, pools)
}

// The pool states, plus the held variant.
func TestRouteMarksWhatThePoolDoesNotSelect(t *testing.T) {
	inPool := map[string]string{routeGuide: "g"}
	pool := []map[string]string{inPool}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "inference.networking.k8s.io", Resource: "inferencepools"}, "", errors.New("no"))
	notServed := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "inference.networking.k8s.io", Kind: "InferencePool"}}

	// Variant a: 3 replicas, template in the pool; pod a-2 lost the pool label.
	// Variant b: 2 replicas, template outside every pool (a canary).
	a := routeDeployment("a", 3, 3, inPool)
	b := routeDeployment("b", 2, 2, nil)
	pods := []*corev1.Pod{
		routePod("a-0", "a", true, inPool),
		routePod("a-1", "a", true, inPool),
		routePod("a-2", "a", true, nil),
		routePod("b-0", "b", true, nil),
		routePod("b-1", "b", true, nil),
	}
	allSelected := []*corev1.Pod{
		routePod("a-0", "a", true, inPool),
		routePod("a-1", "a", true, inPool),
		routePod("a-2", "a", true, inPool),
	}
	asBefore := routeWant{known: true}

	cases := []struct {
		name       string
		pools      PoolLister
		dep        []*appsv1.Deployment
		pods       []*corev1.Pod
		want       map[string]routeWant
		routed     bool
		modelKnown bool
	}{
		{
			name: "pool selects every pod", pools: lister(pool, nil),
			dep: []*appsv1.Deployment{a}, pods: allSelected,
			want: map[string]routeWant{"va": asBefore}, routed: true, modelKnown: true,
		},
		{
			name: "pool selects a subset", pools: lister(pool, nil),
			dep: []*appsv1.Deployment{a}, pods: pods[:3],
			want:   map[string]routeWant{"va": {unrouted: 1, ready: 1, pods: []string{"a-2"}, known: true}},
			routed: true, modelKnown: true,
		},
		{
			name: "pool selects none of a variant: it is held", pools: lister(pool, nil),
			dep: []*appsv1.Deployment{a, b}, pods: pods,
			want: map[string]routeWant{
				"va": {unrouted: 1, ready: 1, pods: []string{"a-2"}, known: true},
				"vb": {unrouted: 2, ready: 2, pods: []string{"b-0", "b-1"}, held: true, known: true},
			},
			routed: true, modelKnown: true,
		},
		{
			name: "pools unreadable: verdict unknown", pools: lister(nil, forbidden),
			dep: []*appsv1.Deployment{a, b}, pods: pods,
			want: map[string]routeWant{"va": {}, "vb": {}},
		},
		{
			name: "no pool in the namespace", pools: lister(nil, nil),
			dep: []*appsv1.Deployment{a, b}, pods: pods,
			want: map[string]routeWant{"va": asBefore, "vb": asBefore}, modelKnown: true,
		},
		{
			name: "pool kind not served", pools: lister(nil, notServed),
			dep: []*appsv1.Deployment{a, b}, pods: pods,
			want: map[string]routeWant{"va": asBefore, "vb": asBefore}, modelKnown: true,
		},
		{
			name: "no pool selects any variant of the model", pools: lister([]map[string]string{{routeGuide: "other"}}, nil),
			dep: []*appsv1.Deployment{a, b}, pods: pods,
			want: map[string]routeWant{"va": asBefore, "vb": asBefore}, modelKnown: true,
		},
		{
			// A pool keyed on pod-template-hash, which no template carries: the
			// template still matches, so nothing is held.
			name: "selector on a label the Deployment adds", pools: lister([]map[string]string{{routeGuide: "g", appsv1.DefaultDeploymentUniqueLabelKey: "h1"}}, nil),
			dep: []*appsv1.Deployment{a}, pods: allSelected,
			want: map[string]routeWant{"va": asBefore}, routed: true, modelKnown: true,
		},
		{
			// The template lacks the pool label, its live Pods carry it (added
			// by a controller): selected through the Pods, not held.
			name: "selected by its live Pods", pools: lister([]map[string]string{{"pool": "x"}}, nil),
			dep:  []*appsv1.Deployment{b},
			pods: []*corev1.Pod{routePod("b-0", "b", true, map[string]string{"pool": "x"})},
			want: map[string]routeWant{"vb": asBefore}, routed: true, modelKnown: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]client.Object, 0, len(tc.pods))
			for _, p := range tc.pods {
				objs = append(objs, p.DeepCopy())
			}
			metas, res := runRoute(t, heldClient(t, objs...), tc.dep, tc.pools)
			if len(metas) != len(tc.want) {
				t.Fatalf("got %d metas, want %d", len(metas), len(tc.want))
			}
			for _, m := range metas {
				if got, w := gotRoute(m), tc.want[m.VariantName]; !routeEqual(got, w) {
					t.Errorf("%s: got %+v, want %+v", m.VariantName, got, w)
				}
			}
			if len(res) != 1 || res[0].Routed != tc.routed || res[0].Known != tc.modelKnown {
				t.Errorf("model routing = %+v, want routed=%t known=%t", res, tc.routed, tc.modelKnown)
			}
		})
	}
}

func routeEqual(a, b routeWant) bool {
	return a.unrouted == b.unrouted && a.ready == b.ready && a.held == b.held && a.known == b.known && slices.Equal(a.pods, b.pods)
}

// A Pod list that fails leaves the verdict unknown wherever it decides it, and
// never turns into a hold: here the canary's template is outside the pool and
// only its Pods could have said otherwise.
func TestRouteAFailedPodListIsUnknownNotHeld(t *testing.T) {
	inPool := map[string]string{routeGuide: "g"}
	c := heldClient(t, routePod("a-0", "a", true, inPool))
	failing := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return apierrors.NewServiceUnavailable("down")
		},
	})
	metas, res := runRoute(t, failing, []*appsv1.Deployment{routeDeployment("a", 1, 1, inPool), routeDeployment("b", 2, 2, nil)},
		lister([]map[string]string{inPool}, nil))
	for _, m := range metas {
		if m.HeldUnrouted || m.RoutingKnown {
			t.Errorf("%s: held=%t known=%t, want neither: a failed read establishes nothing", m.VariantName, m.HeldUnrouted, m.RoutingKnown)
		}
	}
	if len(res) != 1 || !res[0].Routed || res[0].Known {
		t.Errorf("model routing = %+v, want routed and not known", res)
	}
}

// On a LeaderWorkerSet a replica is a group, reached through its leader:
// workers are never counted, and a selector on the leader's worker index (a
// label the LWS controller adds) still selects the template.
func TestRouteLWSCountsLeadersOnly(t *testing.T) {
	sel := []map[string]string{{routeGuide: "g", lwsv1.WorkerIndexLabelKey: "0"}}
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "pd", Namespace: "ns"},
		Spec: lwsv1.LeaderWorkerSetSpec{
			Replicas: ptr.To[int32](2),
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
				Size:           ptr.To[int32](2),
				LeaderTemplate: &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{routeGuide: "g"}}},
			},
		},
		Status: lwsv1.LeaderWorkerSetStatus{Replicas: 2, ReadyReplicas: 2},
	}
	pod := func(name, group, worker string, labels map[string]string) *corev1.Pod {
		l := map[string]string{lwsv1.SetNameLabelKey: "pd", lwsv1.GroupIndexLabelKey: group, lwsv1.WorkerIndexLabelKey: worker}
		maps.Copy(l, labels)
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: l},
			Status: corev1.PodStatus{Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
		}
	}
	c := heldClient(t,
		pod("pd-0", "0", "0", map[string]string{routeGuide: "g"}),
		pod("pd-0-1", "0", "1", nil), // a worker: no pool label, never counted
		pod("pd-1", "1", "0", nil),   // a leader relabelled out of the pool
		pod("pd-1-1", "1", "1", nil),
	)
	va := routeVariant("vpd", "LeaderWorkerSet", "pd")
	targets := map[string]scaletarget.ScaleTargetAccessor{"ns/pd": scaletarget.NewLWSAccessor(lws)}
	metas := Discover(context.Background(), []llmdvariant.Variant{va}, targets, c, DeclaredOnly)
	Route(context.Background(), metas, []llmdvariant.Variant{va}, targets, c, lister(sel, nil))
	got := gotRoute(metas[0])
	want := routeWant{unrouted: 1, ready: 1, pods: []string{"pd-1"}, known: true}
	if !routeEqual(got, want) {
		t.Fatalf("got %+v, want %+v: one group outside the pool, by its leader", got, want)
	}
}

// A stuck Pod outside the pool is already subtracted as stuck, so it is named
// (its rows go) but not counted again; a starting one is counted, not Ready.
func TestRouteCountsStartingNotStuck(t *testing.T) {
	inPool := map[string]string{routeGuide: "g"}
	a := routeDeployment("a", 3, 1, inPool)
	starting := routePod("a-1", "a", false, nil)
	starting.Status.Phase = corev1.PodPending
	stuck := routePod("a-2", "a", false, nil)
	stuck.Status.Phase = corev1.PodPending
	stuck.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
	}}
	c := heldClient(t, routePod("a-0", "a", true, inPool), starting, stuck)
	metas, _ := runRoute(t, c, []*appsv1.Deployment{a}, lister([]map[string]string{inPool}, nil))
	m := metas[0]
	if m.UnroutedReplicas != 1 || m.UnroutedReady != 0 || len(m.UnroutedPods) != 2 {
		t.Fatalf("unrouted=%d ready=%d pods=%v, want 1, 0 and both Pods named", m.UnroutedReplicas, m.UnroutedReady, m.UnroutedPods)
	}
}

// A failed InferencePool listing is counted with its reason.
func TestRouteCountsAFailedPoolListing(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	count := func(reason string) float64 {
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range families {
			if f.GetName() != constants.LLMSMMetricsCollectionErrorsTotal {
				continue
			}
			for _, m := range f.GetMetric() {
				labels := map[string]string{}
				for _, l := range m.GetLabel() {
					labels[l.GetName()] = l.GetValue()
				}
				if labels[constants.LabelQueryType] == constants.QueryTypePoolList && labels[constants.LabelReason] == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
		return 0
	}
	deps := []*appsv1.Deployment{routeDeployment("a", 1, 1, nil)}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "inferencepools"}, "", errors.New("no"))
	runRoute(t, heldClient(t), deps, lister(nil, forbidden))
	runRoute(t, heldClient(t), deps, lister(nil, apierrors.NewServiceUnavailable("down")))
	runRoute(t, heldClient(t), deps, lister(nil, &meta.NoKindMatchError{}))
	if count("forbidden") != 1 || count("unavailable") != 1 {
		t.Errorf("forbidden=%v unavailable=%v, want 1 each", count("forbidden"), count("unavailable"))
	}
	if count("other") != 0 {
		t.Errorf("a pool kind that is not served was counted as a failure")
	}
}

// Route must leave metadata untouched when it is given no lister.
func TestRouteWithoutListerChangesNothing(t *testing.T) {
	metas := []domain.VariantMetadata{{VariantName: "v", Namespace: "ns", ModelID: "m", CurrentReplicas: 2}}
	if res := Route(context.Background(), metas, nil, nil, nil, nil); res != nil {
		t.Fatalf("results without a lister: %+v", res)
	}
	if metas[0].HeldUnrouted || metas[0].UnroutedReplicas != 0 || metas[0].RoutingKnown {
		t.Fatalf("metadata changed without a lister: %+v", metas[0])
	}
}
