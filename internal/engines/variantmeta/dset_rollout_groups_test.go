package variantmeta

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// rolloutDecode is the decode role of DisaggregatedSet "glm", groups of two
// Pods, through its role scaler.
func rolloutDecode(t *testing.T) scaletarget.ScaleTargetAccessor {
	t.Helper()
	scaler := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "glm-decode", "namespace": "ns", "labels": map[string]any{
			constants.DisaggregatedSetNameLabelKey: "glm", constants.DisaggregatedSetRoleLabelKey: "decode"}},
		"spec":   map[string]any{"replicas": int64(3)},
		"status": map[string]any{"replicas": int64(3)},
	}}
	set := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "glm", "namespace": "ns"},
		"spec": map[string]any{"roles": []any{map[string]any{"name": "decode", "spec": map[string]any{
			"leaderWorkerTemplate": map[string]any{"size": int64(2), "workerTemplate": map[string]any{}}}}}},
	}}
	acc, err := scaletarget.NewDisaggregatedSetRoleAccessor(scaler, set)
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

// rolloutPod is the Pod at the given worker index in the given group of the
// child LeaderWorkerSet of revision rev, on node (none when ""), requesting 8 GPUs.
func rolloutPod(rev, group, worker, node string, phase corev1.PodPhase) *corev1.Pod {
	lws := "glm-0-" + rev + "-decode"
	p := heldPod(lws+"-"+group+"-"+worker, map[string]string{
		constants.DisaggregatedSetNameLabelKey: "glm",
		constants.DisaggregatedSetRoleLabelKey: "decode",
		lwsv1.SetNameLabelKey:                  lws,
		lwsv1.GroupIndexLabelKey:               group,
		lwsv1.WorkerIndexLabelKey:              worker,
	}, node, phase, false)
	p.Spec.Containers = []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}}}}
	return p
}

// Mid-rollout, a DisaggregatedSet role runs one child LeaderWorkerSet per
// revision, and each has a group 0. Each group is counted on its own: held and
// filled are the per-revision sum, not one group per index.
func TestHeldReplicasCountsEachRevisionsGroups(t *testing.T) {
	c := heldClient(t,
		rolloutPod("old", "0", "0", "n1", corev1.PodRunning), rolloutPod("old", "0", "1", "n1", corev1.PodRunning),
		rolloutPod("old", "1", "0", "n2", corev1.PodRunning), rolloutPod("old", "1", "1", "n2", corev1.PodRunning),
		// The new revision's group 0: its leader is bound, its worker not yet.
		rolloutPod("new", "0", "0", "n3", corev1.PodRunning), rolloutPod("new", "0", "1", "", corev1.PodPending),
	)
	held, filled, known := heldReplicaCount(context.Background(), c, "ns", rolloutDecode(t))
	if !known || held != 3 || filled != 2 {
		t.Fatalf("held = %d filled = %d (known %t), want 3 groups held (old 0, old 1, new 0), 2 filled", held, filled, known)
	}
}

// PlacedGroups counts each revision's groups apart: two revisions' groups 0,
// both placed, are two placed replicas; the new one half placed is a partial
// replica on its own node, not the third Pod of a placed group.
func TestPlacedGroupsCountsEachRevisionsGroups(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pods         []*corev1.Pod
		placed       int
		partialNodes []string
	}{
		{name: "both groups 0 placed", placed: 2, pods: []*corev1.Pod{
			rolloutPod("old", "0", "0", "n1", corev1.PodRunning), rolloutPod("old", "0", "1", "n1", corev1.PodRunning),
			rolloutPod("new", "0", "0", "n2", corev1.PodRunning), rolloutPod("new", "0", "1", "n2", corev1.PodRunning),
		}},
		{name: "the new group 0 half placed", placed: 1, partialNodes: []string{"n2"}, pods: []*corev1.Pod{
			rolloutPod("old", "0", "0", "n1", corev1.PodRunning), rolloutPod("old", "0", "1", "n1", corev1.PodRunning),
			rolloutPod("new", "0", "0", "n2", corev1.PodRunning), rolloutPod("new", "0", "1", "", corev1.PodPending),
		}},
		{name: "both groups 0 half placed", partialNodes: []string{"n2", "n1"}, pods: []*corev1.Pod{
			rolloutPod("old", "0", "0", "n1", corev1.PodRunning), rolloutPod("old", "0", "1", "", corev1.PodPending),
			rolloutPod("new", "0", "0", "n2", corev1.PodRunning), rolloutPod("new", "0", "1", "", corev1.PodPending),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := heldClient(t, podObjects(tc.pods)...)
			placed, partial, ok := PlacedGroups(context.Background(), c, "ns", rolloutDecode(t))
			nodes := make([]string, 0, len(partial))
			for _, p := range partial {
				nodes = append(nodes, p.Node)
			}
			if !ok || placed != tc.placed || !slices.Equal(nodes, tc.partialNodes) {
				t.Fatalf("placed = %d partial on %v (ok %t), want %d and %v", placed, nodes, ok, tc.placed, tc.partialNodes)
			}
		})
	}
}

// The placement timeout counts unplaced replicas: one Pod of each revision's
// group 0 unschedulable is two unplaced groups, each timed on its own, and
// both are at Kueue's gate when both wait there.
func TestCountUnplacedCountsEachRevisionsGroups(t *testing.T) {
	now := time.Unix(10000, 0)
	unschedulable := func(p *corev1.Pod, since time.Duration) corev1.Pod {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable, LastTransitionTime: metav1.NewTime(now.Add(-since))}}
		return *p
	}
	list := []corev1.Pod{
		*rolloutPod("old", "0", "0", "n1", corev1.PodRunning),
		unschedulable(rolloutPod("old", "0", "1", "", corev1.PodPending), 10*time.Minute),
		*rolloutPod("new", "0", "0", "n2", corev1.PodRunning),
		unschedulable(rolloutPod("new", "0", "1", "", corev1.PodPending), time.Minute),
	}
	n, _, pods, at := countUnplaced(list, true, now)
	if n != 2 || len(pods) != 2 || len(at) != 2 {
		t.Fatalf("unplaced = %d groups, pods %v, at %v; want 2 groups, one per revision", n, pods, at)
	}
	if !at["glm-0-old-decode/0"].Equal(now.Add(-10*time.Minute)) || !at["glm-0-new-decode/0"].Equal(now.Add(-time.Minute)) {
		t.Fatalf("at = %v, want each revision's group 0 timed from its own unplaced Pod", at)
	}

	// Both workers held at Kueue's gate instead: two gated groups.
	gatedPod := func(rev string) corev1.Pod {
		p := rolloutPod(rev, "0", "1", "", corev1.PodPending)
		p.CreationTimestamp = metav1.NewTime(now.Add(-5 * time.Minute))
		p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: kueue.AdmissionGate}}
		return *p
	}
	list = []corev1.Pod{list[0], gatedPod("old"), list[2], gatedPod("new")}
	_, _, _, at = countUnplaced(list, true, now)
	if gated := countGated(list, true, at, nil); len(gated) != 2 {
		t.Fatalf("gated = %v, want both revisions' group 0", gated)
	}
}

// podObjects converts Pods for heldClient.
func podObjects(pods []*corev1.Pod) []client.Object {
	out := make([]client.Object, 0, len(pods))
	for _, p := range pods {
		out = append(out, p)
	}
	return out
}
