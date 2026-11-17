package scaletarget

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// staticSet is dsetFixture named name, with role renamed to roleName and in
// Static mode.
func staticSet(t *testing.T, name, roleName string) *unstructured.Unstructured {
	t.Helper()
	set := dsetFixture()
	set.SetName(name)
	set.SetUID(types.UID("uid-" + name))
	roles, _, _ := unstructured.NestedSlice(set.Object, "spec", "roles")
	decode := roles[1].(map[string]any)
	delete(decode, "scaling")
	decode["name"] = roleName
	require.NoError(t, unstructured.SetNestedSlice(set.Object, roles, "spec", "roles"))
	return set
}

func dsetScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(dsetGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(dsetRoleScalerGVK, &unstructured.Unstructured{})
	return scheme
}

// forbidKind fails every Get of kind with Forbidden.
func forbidKind(kind string) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if obj.GetObjectKind().GroupVersionKind().Kind == kind {
				return apierrors.NewForbidden(schema.GroupResource{Group: dsetGVK.Group, Resource: "x"}, key.Name, assertErr("no grant"))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

// A failed read is never taken for an absent role: Forbidden on the set, with
// or without a role scaler in front of it, is an error the caller logs, not a
// NotFound it skips at debug.
func TestFetchScaleTarget_DisaggregatedSetReadErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("a scaler with no owner and no set-name label", func(t *testing.T) {
		orphan := roleScalerFixture("decode", 1, 1)
		orphan.SetOwnerReferences(nil)
		orphan.SetLabels(map[string]string{constants.DisaggregatedSetRoleLabelKey: "decode"})
		c := fake.NewClientBuilder().WithScheme(dsetScheme()).WithObjects(orphan).Build()
		_, err := FetchScaleTarget(ctx, c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-decode", dsetNS)
		require.Error(t, err)
		assert.False(t, apierrors.IsNotFound(err))
		assert.ErrorContains(t, err, "names no DisaggregatedSet")
	})

	t.Run("the set is forbidden after the scaler is found", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(dsetScheme()).WithObjects(dsetFixture(), roleScalerFixture("decode", 1, 1)).
			WithInterceptorFuncs(forbidKind(constants.DisaggregatedSetKind)).Build()
		_, err := FetchScaleTarget(ctx, c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-decode", dsetNS)
		// The backoff read retries Forbidden and gives up with its own error.
		require.Error(t, err)
		assert.False(t, apierrors.IsNotFound(err), "%v", err)
	})

	t.Run("no scaler, and the set is forbidden", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(dsetScheme()).WithObjects(staticSet(t, "glm", "decode")).
			WithInterceptorFuncs(forbidKind(constants.DisaggregatedSetKind)).Build()
		_, err := FetchScaleTarget(ctx, c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-decode", dsetNS)
		require.Error(t, err)
		assert.True(t, apierrors.IsForbidden(err), "%v", err)
	})

	t.Run("no scaler kind and no set kind: not found", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(dsetScheme()).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return &meta.NoKindMatchError{GroupKind: dsetGVK.GroupKind(), SearchedVersions: []string{"v1"}}
			},
		}).Build()
		_, err := FetchScaleTarget(ctx, c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-decode", dsetNS)
		assert.True(t, meta.IsNoMatchError(err), "%v", err)
	})
}

// Sets "glm" with role "v2-decode" and "glm-v2" with role "decode" both
// name glm-v2-decode. The name cannot say which, so neither is guessed.
func TestFetchScaleTarget_StaticRoleNameAmbiguity(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(dsetScheme()).
		WithObjects(staticSet(t, "glm", "v2-decode"), staticSet(t, "glm-v2", "decode")).Build()
	_, err := FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-v2-decode", dsetNS)
	require.Error(t, err)
	assert.ErrorContains(t, err, "ambiguous")

	// Either alone resolves.
	for _, set := range []*unstructured.Unstructured{staticSet(t, "glm", "v2-decode"), staticSet(t, "glm-v2", "decode")} {
		c := fake.NewClientBuilder().WithScheme(dsetScheme()).WithObjects(set).Build()
		acc, err := FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-v2-decode", dsetNS)
		require.NoError(t, err, set.GetName())
		got, _, _ := DisaggregatedSetRole(acc)
		assert.Equal(t, set.GetName(), got)
	}
}

// A Static role of a set of more than one slice is refused, as the role
// writer refuses the set: its count is per slice.
func TestStaticRoleAccessor_RefusesSlices(t *testing.T) {
	set := staticSet(t, "glm", "decode")
	require.NoError(t, unstructured.SetNestedField(set.Object, int64(2), "spec", "slices"))
	_, err := NewDisaggregatedSetStaticRoleAccessor(set, "decode")
	assert.ErrorContains(t, err, "slices")
}

func TestDisaggregatedSetRoleGone(t *testing.T) {
	ctx := context.Background()
	deleting := staticSet(t, "glm", "decode")
	now := metav1.Now()
	deleting.SetDeletionTimestamp(&now)
	deleting.SetFinalizers([]string{"x"})
	for _, tc := range []struct {
		name     string
		objs     []client.Object
		funcs    interceptor.Funcs
		target   string
		wantGone bool
		wantErr  bool
	}{
		{name: "a role scaler", objs: []client.Object{dsetFixture(), roleScalerFixture("decode", 1, 1)}, target: "glm-decode"},
		{name: "a Static role of a set", objs: []client.Object{staticSet(t, "glm", "decode")}, target: "glm-decode"},
		{name: "neither", objs: []client.Object{staticSet(t, "glm", "decode")}, target: "glm-prefill", wantGone: true},
		{name: "the set is being deleted", objs: []client.Object{deleting}, target: "glm-decode", wantGone: true},
		{name: "forbidden is not gone", objs: []client.Object{staticSet(t, "glm", "decode")},
			funcs: forbidKind(constants.DisaggregatedSetKind), target: "glm-decode", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(dsetScheme()).WithObjects(tc.objs...).WithInterceptorFuncs(tc.funcs).Build()
			gone, err := DisaggregatedSetRoleGone(ctx, c, tc.target, dsetNS)
			assert.Equal(t, tc.wantErr, err != nil, "%v", err)
			assert.Equal(t, tc.wantGone, gone)
		})
	}
}
