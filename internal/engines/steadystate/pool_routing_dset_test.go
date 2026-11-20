package steadystate

import (
	"context"
	"slices"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// dsetReleaseLabel is a Pod label the pool selector requires: the template
// keeps it "true", a drain tool sets it "false" on one group's Pods.
const dsetReleaseLabel = "example.com/production-released"

// dsetDecodeRole is the decode role of DisaggregatedSet "glm" at two groups of
// two, its template carrying dsetReleaseLabel "true".
func dsetDecodeRole(t *testing.T) scaletarget.ScaleTargetAccessor {
	t.Helper()
	set := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.DisaggregatedSetAPIVersion,
		"kind":       constants.DisaggregatedSetKind,
		"metadata":   map[string]any{"name": "glm", "namespace": "ns", "uid": "uid-ds", "generation": int64(1)},
		"spec": map[string]any{"roles": []any{map[string]any{
			"name":    "decode",
			"scaling": map[string]any{"mode": "External"},
			"spec": map[string]any{"leaderWorkerTemplate": map[string]any{
				"size": int64(2),
				"workerTemplate": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"llm-d.ai/role": "decode", dsetReleaseLabel: "true"}},
					"spec": map[string]any{"containers": []any{map[string]any{
						"name": "vllm", "image": "registry.example/vllm:v1",
					}}},
				},
			}},
		}}},
		"status": map[string]any{"observedGeneration": int64(1), "roleStatuses": []any{
			map[string]any{"name": "decode", "replicas": int64(2), "readyReplicas": int64(2), "updatedReplicas": int64(2)},
		}},
	}}
	scaler := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.DisaggregatedSetAPIVersion,
		"kind":       constants.DisaggregatedSetRoleScalerKind,
		"metadata": map[string]any{
			"name": "glm-decode", "namespace": "ns", "uid": "uid-decode",
			"labels": map[string]any{
				constants.DisaggregatedSetNameLabelKey: "glm",
				constants.DisaggregatedSetRoleLabelKey: "decode",
			},
		},
		"spec":   map[string]any{"replicas": int64(2)},
		"status": map[string]any{"replicas": int64(2)},
	}}
	acc, err := scaletarget.NewDisaggregatedSetRoleAccessor(scaler, set)
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

// dsetDecodePod is the Pod at the given worker index in the given decode
// group, Ready, with dsetReleaseLabel set to released.
func dsetDecodePod(group, worker, released string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "glm-decode-abc12-" + group + "-" + worker, Namespace: "ns", Labels: map[string]string{
			constants.DisaggregatedSetNameLabelKey: "glm",
			constants.DisaggregatedSetRoleLabelKey: "decode",
			lwsv1.SetNameLabelKey:                  "glm-decode-abc12",
			lwsv1.GroupIndexLabelKey:               group,
			lwsv1.WorkerIndexLabelKey:              worker,
			"llm-d.ai/role":                        "decode",
			dsetReleaseLabel:                       released,
		}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

// A DisaggregatedSet group held out of the pool by a release label is not
// supply, and stays out across a failed pool read: the verdict is kept for
// routingVerdictTTL, as for any variant, so one failed read does not hand the
// optimizer a group the router does not reach. Past the TTL it lapses to
// "counted as before". Supply here is the rows left after dropUnroutedRows,
// and ReplicaCount and PendingReplicas after applyRouting for the two ways an
// analyzer fills them: from those rows, and from scale-target status when no
// row reported. A group's row is named for its leader, or for its lowest
// worker when the leader did not report; either way it goes.
func TestDisaggregatedSetReleaseLabelSurvivesAFailedPoolRead(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		dsetDecodePod("0", "0", "true"), dsetDecodePod("0", "1", "true"),
		dsetDecodePod("1", "0", "false"), dsetDecodePod("1", "1", "false"),
	).Build()
	targets := map[string]scaletarget.ScaleTargetAccessor{"ns/glm-decode": dsetDecodeRole(t)}
	vas := []llmdvariant.Variant{{
		ObjectMeta: metav1.ObjectMeta{Name: "v-decode", Namespace: "ns"},
		Spec: llmdvariant.Spec{ModelID: "m", ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
			Kind: constants.DisaggregatedSetRoleScalerKind, Name: "glm-decode",
		}},
	}}
	pool := []map[string]string{{"llm-d.ai/role": "decode", dsetReleaseLabel: "true"}}
	unavailable := apierrors.NewServiceUnavailable("down")

	e := &Engine{}
	t0 := time.Now()
	// cycle routes one cycle and returns the decode role's supply: its routed
	// rows (one per group, named for the leader) and its capacity after
	// applyRouting, as an analyzer counting from rows would leave it.
	cycle := func(at time.Time, readFails bool, group1Row string) (rows []string, vc, status domain.VariantCapacity, m domain.VariantMetadata) {
		t.Helper()
		pools := func(context.Context, string) ([]map[string]string, error) {
			if readFails {
				return nil, unavailable
			}
			return pool, nil
		}
		metas := variantmeta.Discover(context.Background(), vas, targets, c, variantmeta.DeclaredOnly)
		results := variantmeta.Route(context.Background(), metas, vas, nil, targets, c, pools)
		e.resolveRouting(context.Background(), "m", "ns", metas, results, nil, at)
		all := []domain.ReplicaMetrics{
			{PodName: "glm-decode-abc12-0-0", VariantName: "v-decode"},
			{PodName: group1Row, VariantName: "v-decode"},
		}
		for _, r := range dropUnroutedRows(all, metas) {
			rows = append(rows, r.PodName)
		}
		m = metas[0]
		vc = domain.VariantCapacity{ReplicaCount: len(rows), ObservedReplicas: len(rows),
			PendingReplicas: max(0, m.CurrentReplicas-len(rows)), PerReplicaCapacity: 10}
		applyRouting(&vc, m)
		status = domain.VariantCapacity{ReplicaCount: m.CurrentReplicas, PerReplicaCapacity: 10}
		applyRouting(&status, m)
		return rows, vc, status, m
	}

	check := func(name string, at time.Time, readFails bool, group1Row string, wantRows []string, wantCount int) {
		t.Helper()
		rows, vc, status, m := cycle(at, readFails, group1Row)
		if m.HeldUnrouted {
			t.Fatalf("%s: the role is held; its template is selected", name)
		}
		if !slices.Equal(rows, wantRows) || vc.ReplicaCount != wantCount || vc.PendingReplicas != 0 {
			t.Fatalf("%s: rows %v ReplicaCount %d PendingReplicas %d, want rows %v, %d and 0",
				name, rows, vc.ReplicaCount, vc.PendingReplicas, wantRows, wantCount)
		}
		if status.ReplicaCount != wantCount || status.PendingReplicas != 0 {
			t.Fatalf("%s, counted from status: ReplicaCount %d PendingReplicas %d, want %d and 0",
				name, status.ReplicaCount, status.PendingReplicas, wantCount)
		}
	}
	leader, worker := "glm-decode-abc12-1-0", "glm-decode-abc12-1-1"
	check("good read", t0, false, leader, []string{"glm-decode-abc12-0-0"}, 1)
	check("good read, the leader did not report", t0, false, worker, []string{"glm-decode-abc12-0-0"}, 1)
	check("failed read within the TTL", t0.Add(time.Minute), true, leader, []string{"glm-decode-abc12-0-0"}, 1)
	check("good read again", t0.Add(2*time.Minute), false, leader, []string{"glm-decode-abc12-0-0"}, 1)
	check("failed read past the TTL", t0.Add(2*time.Minute+routingVerdictTTL+time.Second), true, leader,
		[]string{"glm-decode-abc12-0-0", leader}, 2)
}
