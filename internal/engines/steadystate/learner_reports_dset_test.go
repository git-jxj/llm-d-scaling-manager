package steadystate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// A DisaggregatedSet role's learner series goes when the role does: no role
// scaler and no set with a Static role of that name. A kind the check did not
// read would keep it forever.
func TestScaleTargetGoneDisaggregatedSetRole(t *testing.T) {
	scheme := runtime.NewScheme()
	setGVK := schema.FromAPIVersionAndKind(constants.DisaggregatedSetAPIVersion, constants.DisaggregatedSetKind)
	scheme.AddKnownTypeWithName(setGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.FromAPIVersionAndKind(constants.DisaggregatedSetAPIVersion, constants.DisaggregatedSetRoleScalerKind), &unstructured.Unstructured{})
	set := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.DisaggregatedSetAPIVersion,
		"kind":       constants.DisaggregatedSetKind,
		"metadata":   map[string]any{"name": "glm", "namespace": "ns"},
		"spec": map[string]any{"roles": []any{map[string]any{
			"name": "decode",
			"spec": map[string]any{"replicas": int64(2), "leaderWorkerTemplate": map[string]any{"size": int64(1), "workerTemplate": map[string]any{}}},
		}}},
	}}
	e := &Engine{client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(set).Build()}
	role := func(name string) metrics.LearnerVariant {
		return metrics.LearnerVariant{Namespace: "ns", Target: metrics.LearnerTarget{Kind: constants.DisaggregatedSetRoleScalerKind, Name: name}}
	}
	assert.False(t, e.scaleTargetGone(context.Background(), role("glm-decode")), "the set still has the Static role")
	assert.True(t, e.scaleTargetGone(context.Background(), role("glm-prefill")), "no scaler and no such role")
}
