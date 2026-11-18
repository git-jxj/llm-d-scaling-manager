package locator_test

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/locator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// dsetPod builds the chain a DisaggregatedSet controller makes for one leader
// pod: Pod -> StatefulSet -> LeaderWorkerSet (child, one per role and
// revision) -> DisaggregatedSet. The DisaggregatedSet object itself is not
// created: the walk must not need to read it.
func dsetPod(labels map[string]string) (*lwsv1.LeaderWorkerSet, *appsv1.StatefulSet, *corev1.Pod) {
	ns := testNamespace
	lws := &lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{
		Name: "glm-0-abc12345-decode", Namespace: ns, UID: "uid-lws",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: constants.DisaggregatedSetAPIVersion, Kind: constants.DisaggregatedSetKind,
			Name: "glm", UID: "uid-ds", Controller: ptr.To(true),
		}},
	}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "glm-0-abc12345-decode", Namespace: ns, UID: "uid-sts",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: constants.LeaderWorkerSetAPIVersion, Kind: constants.LeaderWorkerSetKind,
			Name: "glm-0-abc12345-decode", UID: "uid-lws", Controller: ptr.To(true),
		}},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "glm-0-abc12345-decode-0", Namespace: ns, Labels: labels,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: constants.DeploymentAPIVersion, Kind: constants.StatefulSetKind,
			Name: "glm-0-abc12345-decode", UID: "uid-sts", Controller: ptr.To(true),
		}},
	}}
	return lws, sts, pod
}

func TestResolveScaleTarget_DisaggregatedSetRole(t *testing.T) {
	ns := testNamespace
	lws, sts, pod := dsetPod(map[string]string{
		constants.DisaggregatedSetNameLabelKey: "glm",
		constants.DisaggregatedSetRoleLabelKey: "decode",
		lwsv1.SetNameLabelKey:                  "glm-0-abc12345-decode",
	})
	loc, _ := locator.New(newClients(t, lws, sts, pod), nil)

	ref, ok, err := loc.ResolveScaleTarget(context.Background(), ns, pod.Name)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ref.Kind != constants.DisaggregatedSetRoleScalerKind || ref.Name != "glm-decode" ||
		ref.APIVersion != constants.DisaggregatedSetAPIVersion {
		t.Errorf("target = %+v, want the role scaler glm-decode, not the child LeaderWorkerSet", ref)
	}
}

func TestLocate_DisaggregatedSetRoleRegistered(t *testing.T) {
	ns := testNamespace
	lws, sts, pod := dsetPod(map[string]string{constants.DisaggregatedSetRoleLabelKey: "decode"})
	loc, _ := locator.New(newClients(t, lws, sts, pod),
		variantsOf(registered(ns, constants.DisaggregatedSetRoleScalerKind, "glm-decode")))
	got, err := loc.Locate(context.Background(), ns, pod.Name)
	if err != nil || got == nil || got.Name != "h" {
		t.Fatalf("got=%v err=%v, want the ScaledObject on glm-decode", got, err)
	}
}

// A ScaledObject on the child LeaderWorkerSet is not followed: that object
// is replaced on the next rollout, and the set's controller owns its replicas.
func TestLocate_DisaggregatedSetChildLWSNotATarget(t *testing.T) {
	ns := testNamespace
	lws, sts, pod := dsetPod(map[string]string{constants.DisaggregatedSetRoleLabelKey: "decode"})
	loc, _ := locator.New(newClients(t, lws, sts, pod),
		variantsOf(registered(ns, constants.LeaderWorkerSetKind, lws.Name)))
	got, err := loc.Locate(context.Background(), ns, pod.Name)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
	ref, ok, _ := loc.ResolveScaleTarget(context.Background(), ns, pod.Name)
	if !ok || ref.Kind != constants.DisaggregatedSetRoleScalerKind {
		t.Errorf("target = %+v ok=%v, want the role scaler", ref, ok)
	}
}

func TestResolveScaleTarget_DisaggregatedSetUnattributable(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"no role label": {constants.DisaggregatedSetNameLabelKey: "glm"},
		"sub-role pod": {
			constants.DisaggregatedSetRoleLabelKey:    "decode",
			constants.DisaggregatedSetSubRoleLabelKey: "batch",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ns := testNamespace
			lws, sts, pod := dsetPod(labels)
			loc, _ := locator.New(newClients(t, lws, sts, pod), nil)
			ref, ok, err := loc.ResolveScaleTarget(context.Background(), ns, pod.Name)
			if err != nil {
				t.Fatalf("ResolveScaleTarget: %v", err)
			}
			if ok {
				t.Errorf("target = %+v, want none", ref)
			}
			// Positive anchor: the pod was read, and its labels are what made
			// the answer.
			if got := loc.GetPodLabels(context.Background(), ns, pod.Name); len(got) == 0 {
				t.Errorf("pod labels not read")
			}
		})
	}
}
