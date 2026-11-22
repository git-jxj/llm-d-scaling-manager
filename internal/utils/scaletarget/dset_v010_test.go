package scaletarget

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// childLWS is a child LeaderWorkerSet of set glm for role at revision rev, as
// the DisaggregatedSet controller names and labels it.
func childLWS(role, rev string, replicas, ready, updated int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name": "glm-0-" + rev + "-" + role, "namespace": dsetNS,
			"labels": map[string]any{
				constants.DisaggregatedSetNameLabelKey: "glm",
				constants.DisaggregatedSetRoleLabelKey: role,
				dsetRevisionLabelKey:                   rev,
			},
		},
		"spec":   map[string]any{"replicas": replicas},
		"status": map[string]any{"replicas": replicas, "readyReplicas": ready, "updatedReplicas": updated},
	}}
	u.SetGroupVersionKind(lwsListGVK.GroupVersion().WithKind("LeaderWorkerSet"))
	return u
}

// LeaderWorkerSet v0.10.0's DisaggregatedSet controller writes no status on
// the set: no roleStatuses, no observedGeneration. Read as it is, every role
// rolls out forever and a Static role holds zero groups. The role status is
// then read from the child LeaderWorkerSets.
func TestFetchScaleTarget_SetWithoutStatusReadsChildren(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(dsetGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(dsetRoleScalerGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(lwsListGVK.GroupVersion().WithKind("LeaderWorkerSet"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(lwsListGVK, &unstructured.UnstructuredList{})

	v010Set := func() *unstructured.Unstructured {
		set := dsetFixture()
		unstructured.RemoveNestedField(set.Object, "status")
		roles, _, _ := unstructured.NestedSlice(set.Object, "spec", "roles")
		delete(roles[1].(map[string]any), "scaling") // decode: Static
		require.NoError(t, unstructured.SetNestedSlice(set.Object, roles, "spec", "roles"))
		return set
	}

	t.Run("one revision: the children's counts, not rolling out", func(t *testing.T) {
		objs := []client.Object{v010Set(), roleScalerFixture("prefill", 3, 3),
			childLWS("prefill", "aaa", 3, 3, 3), childLWS("decode", "aaa", 7, 6, 7)}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

		decode, err := FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-decode", dsetNS)
		require.NoError(t, err)
		assert.Equal(t, int32(7), decode.GetStatusReplicas(), "the Static role's groups, from its child")
		assert.Equal(t, int32(6), decode.GetStatusReadyReplicas())
		assert.False(t, RollingOut(decode), "one revision runs: not rolling out")

		prefill, err := FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-prefill", dsetNS)
		require.NoError(t, err)
		assert.Equal(t, int32(3), prefill.GetStatusReplicas(), "the scaler's status, as before")
		assert.False(t, RollingOut(prefill))
	})

	t.Run("two revisions with groups: rolling out", func(t *testing.T) {
		objs := []client.Object{v010Set(), childLWS("prefill", "aaa", 3, 3, 3),
			childLWS("decode", "aaa", 4, 4, 4), childLWS("decode", "bbb", 3, 1, 3)}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		decode, err := FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-decode", dsetNS)
		require.NoError(t, err)
		assert.Equal(t, int32(7), decode.GetStatusReplicas(), "groups across both revisions")
		assert.Equal(t, int32(5), decode.GetStatusReadyReplicas())
		assert.True(t, RollingOut(decode))
	})

	t.Run("a set whose controller writes status is read as it is", func(t *testing.T) {
		set := v010Set()
		require.NoError(t, unstructured.SetNestedField(set.Object, int64(3), "status", "observedGeneration"))
		require.NoError(t, unstructured.SetNestedSlice(set.Object, []any{
			map[string]any{"name": "decode", "replicas": int64(2), "readyReplicas": int64(2), "updatedReplicas": int64(2)},
		}, "status", "roleStatuses"))
		objs := []client.Object{set, childLWS("decode", "aaa", 7, 7, 7)}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		decode, err := FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-decode", dsetNS)
		require.NoError(t, err)
		assert.Equal(t, int32(2), decode.GetStatusReplicas(), "the set's own status wins")
	})

	t.Run("no children yet: unobserved, rolling out, as before", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(v010Set()).Build()
		decode, err := FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-decode", dsetNS)
		require.NoError(t, err)
		assert.Equal(t, int32(0), decode.GetStatusReplicas())
		assert.True(t, RollingOut(decode))
	})
}
