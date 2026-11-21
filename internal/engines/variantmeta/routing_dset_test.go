package variantmeta

import (
	"context"
	"slices"
	"strings"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// routeDSet is a DisaggregatedSet "glm" with a prefill and a decode role, each
// a group of two, their templates labelled only with llm-d.ai/role.
func routeDSet() *unstructured.Unstructured {
	role := func(name string) map[string]any {
		return map[string]any{
			"name":    name,
			"scaling": map[string]any{"mode": "External"},
			"spec": map[string]any{
				"leaderWorkerTemplate": map[string]any{
					"size": int64(2),
					"workerTemplate": map[string]any{
						"metadata": map[string]any{"labels": map[string]any{"llm-d.ai/role": name}},
						"spec": map[string]any{"containers": []any{map[string]any{
							"name": "vllm", "image": "registry.example/vllm:v1",
						}}},
					},
				},
			},
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.DisaggregatedSetAPIVersion,
		"kind":       constants.DisaggregatedSetKind,
		"metadata":   map[string]any{"name": "glm", "namespace": "ns", "uid": "uid-ds", "generation": int64(1)},
		"spec":       map[string]any{"roles": []any{role("prefill"), role("decode")}},
		"status": map[string]any{"observedGeneration": int64(1), "roleStatuses": []any{
			map[string]any{"name": "prefill", "replicas": int64(0), "readyReplicas": int64(0), "updatedReplicas": int64(0)},
			map[string]any{"name": "decode", "replicas": int64(1), "readyReplicas": int64(1), "updatedReplicas": int64(1)},
		}},
	}}
}

// routeDSetRole is the scale target of role, through its role scaler.
func routeDSetRole(t *testing.T, role string, replicas int64) scaletarget.ScaleTargetAccessor {
	t.Helper()
	return routeDSetRoleOf(t, routeDSet(), role, replicas)
}

// routeDSetRoleOf is the scale target of role of set, through its role scaler.
func routeDSetRoleOf(t *testing.T, set *unstructured.Unstructured, role string, replicas int64) scaletarget.ScaleTargetAccessor {
	t.Helper()
	scaler := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.DisaggregatedSetAPIVersion,
		"kind":       constants.DisaggregatedSetRoleScalerKind,
		"metadata": map[string]any{
			"name": "glm-" + role, "namespace": "ns", "uid": "uid-" + role,
			"labels": map[string]any{
				constants.DisaggregatedSetNameLabelKey: "glm",
				constants.DisaggregatedSetRoleLabelKey: role,
			},
		},
		"spec":   map[string]any{"replicas": replicas},
		"status": map[string]any{"replicas": replicas},
	}}
	acc, err := scaletarget.NewDisaggregatedSetRoleAccessor(scaler, set)
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

// routeDSetPod is a Pod of role's group 0: worker index worker, with the
// labels the DisaggregatedSet and its child LeaderWorkerSet stamp.
func routeDSetPod(role, worker string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "glm-" + role + "-0-" + worker, Namespace: "ns", Labels: map[string]string{
			constants.DisaggregatedSetNameLabelKey: "glm",
			constants.DisaggregatedSetRoleLabelKey: role,
			lwsv1.SetNameLabelKey:                  "glm-" + role + "-abc12",
			lwsv1.GroupIndexLabelKey:               "0",
			lwsv1.WorkerIndexLabelKey:              worker,
			"llm-d.ai/role":                        role,
		}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

func dsetVariant(role string) llmdvariant.Variant {
	return llmdvariant.Variant{
		ObjectMeta: metav1.ObjectMeta{Name: "v-" + role, Namespace: "ns"},
		Spec: llmdvariant.Spec{ModelID: "m", ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
			Kind: constants.DisaggregatedSetRoleScalerKind, Name: "glm-" + role,
		}},
	}
}

// DisaggregatedSet roles: a pool that selects the set by the label its
// controller stamps on every Pod selects both roles -- the parked prefill by
// its template, with the group labels added, and the running decode by its
// leader. Neither is held, so scale from zero wakes the parked prefill. The
// same holds for a pool keyed on the child LeaderWorkerSet's name, which no
// template can carry: it is taken to match, as pod-template-hash is. A pool
// that selects one role only holds the other, running or parked: a role the
// router does not reach is held like any variant. Workers are never counted.
func TestRouteDisaggregatedSetRoles(t *testing.T) {
	targets := map[string]scaletarget.ScaleTargetAccessor{
		"ns/glm-prefill": routeDSetRole(t, "prefill", 0),
		"ns/glm-decode":  routeDSetRole(t, "decode", 1),
	}
	c := heldClient(t, routeDSetPod("decode", "0"), routeDSetPod("decode", "1"))
	running := []llmdvariant.Variant{dsetVariant("decode")}
	parked := []llmdvariant.Variant{dsetVariant("prefill")}

	for _, tc := range []struct {
		name               string
		pool               map[string]string
		selected, held     []string
		decodeHeld         bool
		decodeUnroutedPods int
	}{
		{name: "pool selects the set", pool: map[string]string{constants.DisaggregatedSetNameLabelKey: "glm"},
			selected: []string{"v-decode", "v-prefill"}},
		{name: "pool selects by the child LeaderWorkerSet name", pool: map[string]string{lwsv1.SetNameLabelKey: "glm-decode-abc12"},
			selected: []string{"v-decode", "v-prefill"}},
		{name: "pool selects decode only", pool: map[string]string{"llm-d.ai/role": "decode"},
			selected: []string{"v-decode"}, held: []string{"v-prefill"}},
		{name: "pool selects prefill only", pool: map[string]string{"llm-d.ai/role": "prefill"},
			selected: []string{"v-prefill"}, held: []string{"v-decode"}, decodeHeld: true, decodeUnroutedPods: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metas := Discover(context.Background(), running, targets, c, DeclaredOnly)
			res := Route(context.Background(), metas, running, parked, targets, c, lister([]map[string]string{tc.pool}, nil))
			if len(res) != 1 || !res[0].Known || !res[0].Routed {
				t.Fatalf("model routing %+v, want routed and known", res)
			}
			sel, held := slices.Sorted(slices.Values(res[0].Selected)), slices.Sorted(slices.Values(res[0].Held))
			if !slices.Equal(sel, tc.selected) || !slices.Equal(held, tc.held) {
				t.Fatalf("selected %v held %v, want %v and %v", sel, held, tc.selected, tc.held)
			}
			m := metas[0]
			if m.HeldUnrouted != tc.decodeHeld || len(m.UnroutedPods) != tc.decodeUnroutedPods {
				t.Fatalf("decode held=%t pods=%v, want held=%t and %d leader Pod(s)", m.HeldUnrouted, m.UnroutedPods, tc.decodeHeld, tc.decodeUnroutedPods)
			}
		})
	}
}

// releaseLabel is a Pod label the pool selector requires besides the role's.
// The template keeps it "true"; a rollout or drain tool sets it "false" on one
// group's Pods, which keeps that group Ready and out of traffic.
const releaseLabel = "example.com/production-released"

// releasedDSet is routeDSet with releaseLabel "true" on every role's template.
func releasedDSet() *unstructured.Unstructured {
	set := routeDSet()
	roles, _, _ := unstructured.NestedSlice(set.Object, "spec", "roles")
	for _, r := range roles {
		path := []string{"spec", "leaderWorkerTemplate", "workerTemplate", "metadata", "labels"}
		labels, _, _ := unstructured.NestedStringMap(r.(map[string]any), path...)
		labels[releaseLabel] = "true"
		if err := unstructured.SetNestedStringMap(r.(map[string]any), labels, path...); err != nil {
			panic(err)
		}
	}
	if err := unstructured.SetNestedSlice(set.Object, roles, "spec", "roles"); err != nil {
		panic(err)
	}
	return set
}

// releasedDSetPod is the Pod at the given worker index in the given decode
// group of the child LeaderWorkerSet lws (one per revision), Ready, with
// releaseLabel set to released.
func releasedDSetPod(lws, group, worker, released string) *corev1.Pod {
	p := routeDSetPod("decode", worker)
	p.Name = lws + "-" + group + "-" + worker
	p.Labels[lwsv1.SetNameLabelKey] = lws
	p.Labels[lwsv1.GroupIndexLabelKey] = group
	p.Labels[releaseLabel] = released
	return p
}

// releasedGroup is both Pods of one decode group, leader and worker, with
// releaseLabel released.
func releasedGroup(lws, group, released string) []*corev1.Pod {
	return []*corev1.Pod{releasedDSetPod(lws, group, "0", released), releasedDSetPod(lws, group, "1", released)}
}

// A DisaggregatedSet group taken out of the pool by a release label is Ready
// and serves nothing, so it is not supply: the pool selects the role's
// template (the label is "true" there), so the role is selected and not held,
// and each relabelled group is counted once, through its leader -- workers are
// never counted, only named, so the group's row goes whichever Pod the
// collector named it for. During a rollout two revisions each have a group 0; they are
// told apart by their Pods, not by group index, so holding one revision's
// group 0 out leaves the other's counted.
func TestRouteDisaggregatedSetReleaseLabel(t *testing.T) {
	pool := map[string]string{"llm-d.ai/role": "decode", releaseLabel: "true"}
	for _, tc := range []struct {
		name     string
		replicas int64
		groups   [][]*corev1.Pod
		unrouted []string
	}{
		{name: "every group released", replicas: 2,
			groups: [][]*corev1.Pod{releasedGroup("glm-decode-old", "0", "true"), releasedGroup("glm-decode-old", "1", "true")}},
		{name: "group 1 held out", replicas: 2,
			groups:   [][]*corev1.Pod{releasedGroup("glm-decode-old", "0", "true"), releasedGroup("glm-decode-old", "1", "false")},
			unrouted: []string{"glm-decode-old-1-0"}},
		{name: "only a worker relabelled: workers are not consulted", replicas: 2,
			groups: [][]*corev1.Pod{releasedGroup("glm-decode-old", "0", "true"), {
				releasedDSetPod("glm-decode-old", "1", "0", "true"), releasedDSetPod("glm-decode-old", "1", "1", "false")}}},
		{name: "rollout: the new revision's group 0 held out, the old one released", replicas: 3,
			groups: [][]*corev1.Pod{releasedGroup("glm-decode-old", "0", "true"), releasedGroup("glm-decode-old", "1", "true"),
				releasedGroup("glm-decode-new", "0", "false")},
			unrouted: []string{"glm-decode-new-0-0"}},
		{name: "rollout: the old revision's group 0 drained out, the new one released", replicas: 3,
			groups: [][]*corev1.Pod{releasedGroup("glm-decode-old", "0", "false"), releasedGroup("glm-decode-new", "0", "true"),
				releasedGroup("glm-decode-new", "1", "true")},
			unrouted: []string{"glm-decode-old-0-0"}},
		{name: "rollout: both revisions' group 0 held out", replicas: 3,
			groups: [][]*corev1.Pod{releasedGroup("glm-decode-old", "0", "false"), releasedGroup("glm-decode-old", "1", "true"),
				releasedGroup("glm-decode-new", "0", "false")},
			unrouted: []string{"glm-decode-new-0-0", "glm-decode-old-0-0"}},
		{name: "every group held out: the role is still selected by its template", replicas: 2,
			groups:   [][]*corev1.Pod{releasedGroup("glm-decode-old", "0", "false"), releasedGroup("glm-decode-old", "1", "false")},
			unrouted: []string{"glm-decode-old-0-0", "glm-decode-old-1-0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]client.Object, 0, 2*len(tc.groups))
			for _, g := range tc.groups {
				for _, p := range g {
					objs = append(objs, p)
				}
			}
			c := heldClient(t, objs...)
			targets := map[string]scaletarget.ScaleTargetAccessor{"ns/glm-decode": routeDSetRoleOf(t, releasedDSet(), "decode", tc.replicas)}
			running := []llmdvariant.Variant{dsetVariant("decode")}
			metas := Discover(context.Background(), running, targets, c, DeclaredOnly)
			res := Route(context.Background(), metas, running, nil, targets, c, lister([]map[string]string{pool}, nil))
			if len(res) != 1 || !res[0].Known || !res[0].Routed {
				t.Fatalf("model routing %+v, want routed and known", res)
			}
			if !slices.Equal(res[0].Selected, []string{"v-decode"}) || len(res[0].Held) != 0 {
				t.Fatalf("selected %v held %v, want the role selected by its template and not held", res[0].Selected, res[0].Held)
			}
			// tc.unrouted are the leaders (worker index 0); each group's
			// worker (index 1) is named beside it.
			named := make([]string, 0, 2*len(tc.unrouted))
			for _, leader := range tc.unrouted {
				named = append(named, leader, strings.TrimSuffix(leader, "-0")+"-1")
			}
			slices.Sort(named)
			want := routeWant{unrouted: len(tc.unrouted), ready: len(tc.unrouted), pods: named, known: true}
			if got := gotRoute(metas[0]); !routeEqual(got, want) {
				t.Fatalf("decode routing %+v, want %+v", got, want)
			}
		})
	}
}
