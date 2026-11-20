package variantmeta

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/aggregation"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// stormMessage is the kubelet's words when a device plugin reports every
// device of a resource unhealthy while the node still advertises them.
const stormMessage = "Pod was rejected: Allocate failed due to no healthy devices present; " +
	"cannot allocate unhealthy devices rdma/ib, which is unexpected"

var stormLabels = map[string]string{"app": "decode", RoleLabel: "decode"}

// badNode is the node whose kubelet rejects every pod.
const badNode = "gpu-node-7"

// admissionStorm is the incident behind issue #224: a decode Deployment asks
// for 2 replicas; one is Ready, and the scheduler keeps placing the other on a
// node whose kubelet rejects it at admission. The ReplicaSet replaces each
// rejected pod at once, so rejected pods pile up beside one fresh replacement
// that is Pending and placed on the same node.
func admissionStorm(rejected int, now time.Time) []corev1.Pod {
	pod := func(name string, created time.Time) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns", Labels: stormLabels,
			CreationTimestamp: metav1.NewTime(created),
		}}
	}
	ready := pod("decode-ready", now.Add(-2*time.Hour))
	ready.Spec.NodeName = "gpu-node-1"
	ready.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}}
	pods := make([]corev1.Pod, 0, rejected+2)
	pods = append(pods, ready)
	for i := range rejected {
		p := pod(fmt.Sprintf("decode-rejected-%03d", i), now.Add(-time.Duration(rejected-i)*5*time.Second))
		p.Spec.NodeName = badNode
		p.Status = corev1.PodStatus{Phase: corev1.PodFailed, Reason: "UnexpectedAdmissionError", Message: stormMessage}
		pods = append(pods, p)
	}
	next := pod("decode-replacement", now)
	next.Spec.NodeName = badNode
	next.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
	}}
	return append(pods, next)
}

// The storm's cause is the rejection, not the Pending replacement: that pod is
// its symptom and will be rejected next.
func TestExplainMissingAdmissionStorm(t *testing.T) {
	now := time.Now()
	got := ExplainMissing(admissionStorm(169, now), now.Add(-time.Hour))
	if got.Reason != NotArrivingAdmissionRejected {
		t.Fatalf("reason = %q, want %q", got.Reason, NotArrivingAdmissionRejected)
	}
	if got.Pods != 169 {
		t.Errorf("pods = %d, want 169: every rejected pod, not the Ready one or the replacement", got.Pods)
	}
	if got.Node != badNode {
		t.Errorf("node = %q, want gpu-node-7", got.Node)
	}
	if !strings.HasPrefix(got.Detail, "UnexpectedAdmissionError: Pod was rejected: Allocate failed") {
		t.Errorf("detail = %q, want the kubelet's reason and message", got.Detail)
	}
	if len(got.Detail) > maxCauseMessage+len("...") {
		t.Errorf("detail is %d bytes, over the %d bound", len(got.Detail), maxCauseMessage)
	}
}

// A pod that ended before the shortfall began explains nothing about it.
func TestExplainMissingIgnoresOldTerminalPods(t *testing.T) {
	now := time.Now()
	old := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "evicted",
		CreationTimestamp: metav1.NewTime(now.Add(-24 * time.Hour))},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}}
	if got := ExplainMissing([]corev1.Pod{old}, now.Add(-time.Hour)); got.Reason != NotArrivingNoPods {
		t.Fatalf("reason = %q, want %q", got.Reason, NotArrivingNoPods)
	}
}

func TestExplainMissingCauses(t *testing.T) {
	now := time.Now()
	waiting := func(reason, message string) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "vllm", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
		}}}
	}
	crash := corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "vllm", RestartCount: 4,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
	}}}
	cases := []struct {
		name   string
		status corev1.PodStatus
		node   string
		reason string
		detail string
	}{
		{"out of gpus at admission", corev1.PodStatus{Phase: corev1.PodFailed, Reason: "OutOfnvidia.com/gpu",
			Message: "Node didn't have enough resource"}, "n1", NotArrivingAdmissionRejected, "OutOfnvidia.com/gpu: Node didn't"},
		{"node affinity at admission", corev1.PodStatus{Phase: corev1.PodFailed, Reason: "NodeAffinity"}, "n1",
			NotArrivingAdmissionRejected, "NodeAffinity"},
		{"evicted", corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: "low on memory"}, "n1",
			NotArrivingFailed, "Evicted: low on memory"},
		{"unschedulable", corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/12 nodes are available: 12 Insufficient nvidia.com/gpu."}}}, "",
			NotArrivingUnschedulable, "0/12 nodes are available: 12 Insufficient nvidia.com/gpu."},
		{"kueue gate", corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonSchedulingGated,
			Message: "Scheduling is blocked due to non-empty scheduling gates"}}}, "",
			NotArrivingSchedulingGated, "Scheduling is blocked"},
		{"image pull", waiting("ImagePullBackOff", "Back-off pulling image"), "n2", NotArrivingImagePull,
			"ImagePullBackOff: Back-off pulling image"},
		{"config error", waiting("CreateContainerConfigError", `secret "hf-token" not found`), "n2",
			NotArrivingContainerConfig, `CreateContainerConfigError: secret "hf-token" not found`},
		{"crash loop", crash, "n3", NotArrivingCrashLoop,
			"container vllm: CrashLoopBackOff, last exit OOMKilled (code 137), 4 restarts"},
		{"running not ready", corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionFalse, Message: "containers with unready status: [vllm]"}}},
			"n4", NotArrivingNotReady, "containers with unready status: [vllm]"},
		{"still pulling", waiting("ContainerCreating", ""), "n5", NotArrivingStarting, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", CreationTimestamp: metav1.NewTime(now)},
				Spec: corev1.PodSpec{NodeName: tc.node}, Status: tc.status}
			got := ExplainMissing([]corev1.Pod{p}, now.Add(-time.Minute))
			if got.Reason != tc.reason || got.Pods != 1 || got.Node != tc.node {
				t.Fatalf("got %+v, want reason %q, 1 pod, node %q", got, tc.reason, tc.node)
			}
			if !strings.HasPrefix(got.Detail, tc.detail) {
				t.Errorf("detail = %q, want it to start with %q", got.Detail, tc.detail)
			}
		})
	}
}

// Ready pods and pods being deleted explain nothing; with nothing left the
// cause is that no pod exists.
func TestExplainMissingNoPods(t *testing.T) {
	now := time.Now()
	deleting := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "d", DeletionTimestamp: ptr.To(metav1.NewTime(now))},
		Status: corev1.PodStatus{Phase: corev1.PodPending}}
	ready := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "r"}, Status: corev1.PodStatus{Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	if got := ExplainMissing([]corev1.Pod{deleting, ready}, now); got.Reason != NotArrivingNoPods {
		t.Fatalf("reason = %q, want %q", got.Reason, NotArrivingNoPods)
	}
}

// Scope (1) of issue #224, on the discovery side: through an admission storm,
// supply counts the one Ready replica, and the Pending replacement that the
// node is about to reject again is not counted as capacity on its way.
func TestDiscoverAdmissionStormAddsNoSupply(t *testing.T) {
	now := time.Now()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "decode", Namespace: "ns"}}
	d.Spec.Replicas = ptr.To(int32(2))
	d.Spec.Template.Labels = stormLabels
	// A ReplicaSet counts no Failed pod: status.replicas is the Ready pod and
	// the replacement.
	d.Status.Replicas, d.Status.ReadyReplicas = 2, 1
	pods := admissionStorm(169, now)
	objs := make([]client.Object, 0, 1+len(pods))
	objs = append(objs, d)
	for i := range pods {
		objs = append(objs, &pods[i])
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	va := llmdvariant.Variant{
		ObjectMeta: metav1.ObjectMeta{Name: "decode", Namespace: "ns"},
		Spec: llmdvariant.Spec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "decode"},
			ModelID:        "m", MaxReplicas: 4,
		},
	}
	metas := Discover(context.Background(), []llmdvariant.Variant{va}, nil, c, DeclaredOnly)
	if len(metas) != 1 {
		t.Fatalf("discovered %d variants, want 1", len(metas))
	}
	m := metas[0]
	if m.ReadyReplicas != 1 || m.PendingReplicas != 1 {
		t.Fatalf("ready %d pending %d, want 1 and 1", m.ReadyReplicas, m.PendingReplicas)
	}
	vc := domain.VariantCapacity{ReplicaCount: m.ReadyReplicas, PendingReplicas: m.PendingReplicas,
		StuckReplicas: m.StuckReplicas, PerReplicaCapacity: 1}
	if got := aggregation.Supply(vc); got != 1 {
		t.Errorf("supply = %v, want 1: only the Ready replica serves", got)
	}
	if got := aggregation.AnticipatedSupply(vc); got != 1 {
		t.Errorf("anticipated supply = %v, want 1: nothing is arriving while the node rejects every pod "+
			"(stuck %d)", got, m.StuckReplicas)
	}
}
