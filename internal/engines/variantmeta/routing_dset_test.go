package variantmeta

import (
	"context"
	"slices"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	acc, err := scaletarget.NewDisaggregatedSetRoleAccessor(scaler, routeDSet())
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
// leader. Neither is held, so scale from zero wakes the parked prefill. A pool
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
