package variantmeta

import (
	"context"
	"maps"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

func heldPod(name string, labels map[string]string, node string, phase corev1.PodPhase, deleting bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: maps.Clone(labels)},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: phase},
	}
	// Owned by Deployment "m", as its ReplicaSet's pods are; ownedBy changes it.
	ownedBy(p, "m")
	if deleting {
		// The fake client accepts a deletion timestamp only on an object that
		// still has a finalizer, which is exactly a pod still draining.
		p.Finalizers = []string{"drain"}
		p.DeletionTimestamp = &metav1.Time{}
	}
	return p
}

func heldClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// A terminating pod still holds its GPUs; status.replicas has already dropped
// it. Counting it is the point: a release judged without it hands the donor's
// GPUs out while the pod is still draining.
func TestHeldReplicasCountsTerminatingPods(t *testing.T) {
	sel := map[string]string{"app": "m"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](2),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: sel}},
		},
		Status: appsv1.DeploymentStatus{Replicas: 2},
	}
	c := heldClient(t,
		heldPod("running", sel, "n1", corev1.PodRunning, false),
		heldPod("draining", sel, "n1", corev1.PodRunning, true),
		heldPod("starting", sel, "n2", corev1.PodPending, false),
		heldPod("unscheduled", sel, "", corev1.PodPending, false),
		heldPod("finished", sel, "n2", corev1.PodSucceeded, false),
		heldPod("other", map[string]string{"app": "x"}, "n1", corev1.PodRunning, false),
	)
	held, _, known := heldReplicaCount(context.Background(), c, "ns", scaletarget.NewDeploymentAccessor(d))
	if !known || held != 3 {
		t.Fatalf("held = %d (known %t), want 3: running, draining and scheduled-starting", held, known)
	}
}

// On a LeaderWorkerSet a replica is a group: four pods of one group are one
// held replica, and a group with only its draining leader left still holds.
func TestHeldReplicasCountsLWSGroups(t *testing.T) {
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "pd", Namespace: "ns"},
		Spec: lwsv1.LeaderWorkerSetSpec{
			Replicas:             ptr.To[int32](2),
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{Size: ptr.To[int32](4)},
		},
	}
	group := func(idx string) map[string]string {
		return map[string]string{lwsv1.SetNameLabelKey: "pd", lwsv1.GroupIndexLabelKey: idx}
	}
	c := heldClient(t,
		heldPod("pd-0", group("0"), "n1", corev1.PodRunning, false),
		heldPod("pd-0-1", group("0"), "n1", corev1.PodRunning, false),
		heldPod("pd-0-2", group("0"), "n2", corev1.PodRunning, false),
		heldPod("pd-0-3", group("0"), "n2", corev1.PodRunning, false),
		heldPod("pd-1", group("1"), "n3", corev1.PodRunning, true),
		heldPod("pd-2", group("2"), "", corev1.PodPending, false),
	)
	held, filled, known := heldReplicaCount(context.Background(), c, "ns", scaletarget.NewLWSAccessor(lws))
	if !known || held != 2 {
		t.Fatalf("held = %d (known %t), want 2 groups", held, known)
	}
	// Filled is the groups whose every pod is scheduled: group 0 only. Group 1's
	// leader alone holds GPUs; a receiver's fill must not end on it.
	if filled != 1 {
		t.Fatalf("filled = %d, want 1: only group 0 has all four pods", filled)
	}
}

// A DisaggregatedSet role's pods are listed by the set and role labels: they
// belong to child LeaderWorkerSets named <set>-<slice>-<revision>-<role>, never
// to anything named like the role scaler, and the other role's pods are not
// this role's.
func TestHeldReplicasCountsDisaggregatedSetRoleGroups(t *testing.T) {
	scaler := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "glm-decode", "namespace": "ns", "labels": map[string]any{
			constants.DisaggregatedSetNameLabelKey: "glm", constants.DisaggregatedSetRoleLabelKey: "decode"}},
		"spec": map[string]any{"replicas": int64(2)},
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
	pod := func(role, idx string) map[string]string {
		return map[string]string{
			constants.DisaggregatedSetNameLabelKey: "glm",
			constants.DisaggregatedSetRoleLabelKey: role,
			lwsv1.SetNameLabelKey:                  "glm-0-abc12345-" + role,
			lwsv1.GroupIndexLabelKey:               idx,
		}
	}
	c := heldClient(t,
		heldPod("d-0", pod("decode", "0"), "n1", corev1.PodRunning, false),
		heldPod("d-0-1", pod("decode", "0"), "n1", corev1.PodRunning, false),
		heldPod("d-1", pod("decode", "1"), "n2", corev1.PodRunning, false),
		heldPod("p-0", pod("prefill", "0"), "n3", corev1.PodRunning, false),
		heldPod("p-0-1", pod("prefill", "0"), "n3", corev1.PodRunning, false),
	)
	held, filled, known := heldReplicaCount(context.Background(), c, "ns", acc)
	if !known || held != 2 || filled != 1 {
		t.Fatalf("held = %d filled = %d (known %t), want 2 decode groups, 1 filled", held, filled, known)
	}
}

func TestHeldReplicasUnknownWithoutAClient(t *testing.T) {
	if _, _, known := heldReplicaCount(context.Background(), nil, "ns", nil); known {
		t.Fatal("a missing client must report the count as unknown, not zero")
	}
}

// ownedBy makes p a pod of the named Deployment's ReplicaSet.
func ownedBy(p *corev1.Pod, deployment string) *corev1.Pod {
	if p.Labels == nil {
		p.Labels = map[string]string{}
	}
	p.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "h1"
	p.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "ReplicaSet", Name: deployment + "-h1", UID: "rs", Controller: ptr.To(true),
	}}
	return p
}

// Two variants of one model share pod template labels; a variant's pods are
// the ones its own ReplicaSet controls. Negative control: the sibling's pods,
// matched by label, must not be counted (or, by the same lister, marked as
// donors).
func TestHeldReplicasCountsOnlyTheDeploymentsOwnPods(t *testing.T) {
	sel := map[string]string{"app": "m"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: sel}},
		},
	}
	c := heldClient(t,
		heldPod("mine", sel, "n1", corev1.PodRunning, false),
		ownedBy(heldPod("sibling-1", sel, "n1", corev1.PodRunning, false), "m-other"),
		ownedBy(heldPod("sibling-2", sel, "n1", corev1.PodRunning, false), "m-other"),
	)
	held, _, known := heldReplicaCount(context.Background(), c, "ns", scaletarget.NewDeploymentAccessor(d))
	if !known || held != 1 {
		t.Fatalf("held = %d (known %t), want 1: the sibling variant's pods share labels but not the owner", held, known)
	}
}
