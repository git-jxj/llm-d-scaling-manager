package variantmeta

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
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

func routeVariant(name, dep string) llmdvariant.Variant {
	return llmdvariant.Variant{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: llmdvariant.Spec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: dep},
			ModelID:        "m",
		},
	}
}

func lister(sels []map[string]string, err error) PoolLister {
	return func(context.Context, string) ([]map[string]string, error) { return sels, err }
}

// The five pool states, plus the held variant. Each case states what supply
// must exclude; on the parent commit nothing is excluded, so every case that
// expects an exclusion fails there.
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

	type want struct {
		unrouted, ready int
		pods            []string
		held            bool
	}
	cases := []struct {
		name  string
		pools PoolLister
		dep   []*appsv1.Deployment
		pods  []*corev1.Pod
		want  map[string]want
	}{
		{
			name: "pool selects every pod", pools: lister(pool, nil),
			dep: []*appsv1.Deployment{a}, pods: allSelected,
			want: map[string]want{"va": {}},
		},
		{
			name: "pool selects a subset", pools: lister(pool, nil),
			dep: []*appsv1.Deployment{a}, pods: pods[:3],
			want: map[string]want{"va": {unrouted: 1, ready: 1, pods: []string{"a-2"}}},
		},
		{
			name: "pool selects none of a variant: it is held", pools: lister(pool, nil),
			dep: []*appsv1.Deployment{a, b}, pods: pods,
			want: map[string]want{
				"va": {unrouted: 1, ready: 1, pods: []string{"a-2"}},
				"vb": {unrouted: 2, ready: 2, held: true},
			},
		},
		{
			name: "pools unreadable: counted as before", pools: lister(nil, forbidden),
			dep: []*appsv1.Deployment{a, b}, pods: pods,
			want: map[string]want{"va": {}, "vb": {}},
		},
		{
			name: "no pool in the namespace", pools: lister(nil, nil),
			dep: []*appsv1.Deployment{a, b}, pods: pods,
			want: map[string]want{"va": {}, "vb": {}},
		},
		{
			name: "pool kind not served", pools: lister(nil, notServed),
			dep: []*appsv1.Deployment{a, b}, pods: pods,
			want: map[string]want{"va": {}, "vb": {}},
		},
		{
			name: "no pool selects any variant of the model", pools: lister([]map[string]string{{routeGuide: "other"}}, nil),
			dep: []*appsv1.Deployment{a, b}, pods: pods,
			want: map[string]want{"va": {}, "vb": {}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]client.Object, 0, len(tc.pods))
			targets := map[string]scaletarget.ScaleTargetAccessor{}
			vas := make([]llmdvariant.Variant, 0, len(tc.dep))
			for _, d := range tc.dep {
				targets["ns/"+d.Name] = scaletarget.NewDeploymentAccessor(d)
				vas = append(vas, routeVariant("v"+d.Name, d.Name))
			}
			for _, p := range tc.pods {
				objs = append(objs, p.DeepCopy())
			}
			c := heldClient(t, objs...)
			metas := Discover(context.Background(), vas, targets, c, DeclaredOnly)
			Route(context.Background(), metas, vas, targets, c, tc.pools)
			if len(metas) != len(tc.want) {
				t.Fatalf("got %d metas, want %d", len(metas), len(tc.want))
			}
			for _, m := range metas {
				w := tc.want[m.VariantName]
				got := want{unrouted: m.UnroutedReplicas, ready: m.UnroutedReady, held: m.HeldUnrouted}
				if len(m.UnroutedPods) > 0 {
					got.pods = slices.Sorted(slices.Values(m.UnroutedPods))
				}
				if got.unrouted != w.unrouted || got.ready != w.ready || got.held != w.held || !slices.Equal(got.pods, w.pods) {
					t.Errorf("%s: got %+v, want %+v", m.VariantName, got, w)
				}
			}
		})
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
	vas := []llmdvariant.Variant{routeVariant("va", "a")}
	targets := map[string]scaletarget.ScaleTargetAccessor{"ns/a": scaletarget.NewDeploymentAccessor(a)}
	metas := Discover(context.Background(), vas, targets, c, DeclaredOnly)
	Route(context.Background(), metas, vas, targets, c, lister([]map[string]string{inPool}, nil))
	m := metas[0]
	if m.UnroutedReplicas != 1 || m.UnroutedReady != 0 || len(m.UnroutedPods) != 2 {
		t.Fatalf("unrouted=%d ready=%d pods=%v, want 1, 0 and both Pods named", m.UnroutedReplicas, m.UnroutedReady, m.UnroutedPods)
	}
}

// Route must leave metadata untouched when it is given no lister.
func TestRouteWithoutListerChangesNothing(t *testing.T) {
	metas := []domain.VariantMetadata{{VariantName: "v", Namespace: "ns", ModelID: "m", CurrentReplicas: 2}}
	Route(context.Background(), metas, nil, nil, nil, nil)
	if metas[0].HeldUnrouted || metas[0].UnroutedReplicas != 0 {
		t.Fatalf("metadata changed without a lister: %+v", metas[0])
	}
}
