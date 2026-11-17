package scaletarget

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

const dsetNS = "serving"

// dsetFixture is a DisaggregatedSet with a prefill and a decode role at
// different group counts and sizes, as LWS v1.0.0 writes it. It carries fields
// the repository's LWS module does not know (groupIdentity, scaling), which the
// conversion must ignore.
func dsetFixture() *unstructured.Unstructured {
	role := func(name string, size, gpus int64, extra map[string]any) map[string]any {
		r := map[string]any{
			"name": name,
			"spec": map[string]any{
				"replicas": int64(7),
				"leaderWorkerTemplate": map[string]any{
					"size":          size,
					"groupIdentity": "Ordinal",
					"workerTemplate": map[string]any{
						"metadata": map[string]any{"labels": map[string]any{"llm-d.ai/role": name}},
						"spec": map[string]any{
							"containers": []any{map[string]any{
								"name":  "vllm",
								"image": "registry.example/vllm:v1",
								"resources": map[string]any{
									"limits": map[string]any{"nvidia.com/gpu": gpus},
								},
							}},
						},
					},
				},
			},
		}
		for k, v := range extra {
			r[k] = v
		}
		return r
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.DisaggregatedSetAPIVersion,
		"kind":       constants.DisaggregatedSetKind,
		"metadata": map[string]any{
			"name": "glm", "namespace": dsetNS, "uid": "uid-ds", "generation": int64(3),
		},
		"spec": map[string]any{
			"roles": []any{
				role("prefill", 2, 8, map[string]any{"scaling": map[string]any{"mode": "External"}}),
				role("decode", 4, 8, map[string]any{"scaling": map[string]any{"mode": "External"}}),
			},
		},
		"status": map[string]any{
			"observedGeneration": int64(3),
			"roleStatuses": []any{
				map[string]any{"name": "prefill", "replicas": int64(3), "readyReplicas": int64(2), "updatedReplicas": int64(3)},
				map[string]any{"name": "decode", "replicas": int64(7), "readyReplicas": int64(7), "updatedReplicas": int64(5)},
			},
		},
	}}
	return u
}

// roleScalerFixture is the DisaggregatedSetRoleScaler the controller creates
// for role of dsetFixture.
func roleScalerFixture(role string, desired, current int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.DisaggregatedSetAPIVersion,
		"kind":       constants.DisaggregatedSetRoleScalerKind,
		"metadata": map[string]any{
			"name": "glm-" + role, "namespace": dsetNS, "uid": "uid-scaler-" + role,
			"labels": map[string]any{
				constants.DisaggregatedSetNameLabelKey: "glm",
				constants.DisaggregatedSetRoleLabelKey: role,
			},
			"ownerReferences": []any{map[string]any{
				"apiVersion": constants.DisaggregatedSetAPIVersion, "kind": constants.DisaggregatedSetKind,
				"name": "glm", "uid": "uid-ds", "controller": true,
			}},
		},
		"spec":   map[string]any{"replicas": desired},
		"status": map[string]any{"replicas": current},
	}}
}

func TestDSetRoleAccessor_RolesAreIndependent(t *testing.T) {
	prefill, err := NewDisaggregatedSetRoleAccessor(roleScalerFixture("prefill", 3, 3), dsetFixture())
	require.NoError(t, err)
	decode, err := NewDisaggregatedSetRoleAccessor(roleScalerFixture("decode", 7, 7), dsetFixture())
	require.NoError(t, err)

	// Each role is its own scale target with its own count and group shape:
	// 3 prefill groups of 2 pods, 7 decode groups of 4.
	assert.Equal(t, "glm-prefill", prefill.GetName())
	assert.Equal(t, int32(3), *prefill.GetReplicas())
	assert.Equal(t, int32(2), prefill.GetGroupSize())
	assert.Equal(t, 16, prefill.GetTotalGPUsPerReplica())
	assert.Equal(t, int32(2), prefill.GetStatusReadyReplicas())

	assert.Equal(t, "glm-decode", decode.GetName())
	assert.Equal(t, int32(7), *decode.GetReplicas())
	assert.Equal(t, int32(4), decode.GetGroupSize())
	assert.Equal(t, 32, decode.GetTotalGPUsPerReplica())
	assert.Equal(t, int32(7), decode.GetStatusReplicas())
	assert.Equal(t, "uid-scaler-decode", string(decode.GetUID()))

	// Its replicas are LeaderWorkerSet groups; the leader is the worker
	// template when the role sets no leader template.
	assert.True(t, IsLeaderWorkerSet(decode))
	require.NotNil(t, decode.GetLeaderPodTemplateSpec())
	assert.Equal(t, "decode", decode.GetLeaderPodTemplateSpec().Labels["llm-d.ai/role"])
	assert.Equal(t, []int{8, 8, 8, 8}, PodGPUs(decode))

	set, role, ok := DisaggregatedSetRole(decode)
	assert.True(t, ok)
	assert.Equal(t, "glm", set)
	assert.Equal(t, "decode", role)
}

// A custom resource's template is stored undefaulted: limits alone are what
// the pods will request, and an explicit request is kept as written.
func TestRequestsFromLimits(t *testing.T) {
	gpu := corev1.ResourceName("nvidia.com/gpu")
	spec := corev1.PodSpec{Containers: []corev1.Container{
		{Name: "limits-only", Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{gpu: resource.MustParse("8")}}},
		{Name: "both", Resources: corev1.ResourceRequirements{
			Limits:   corev1.ResourceList{gpu: resource.MustParse("8")},
			Requests: corev1.ResourceList{gpu: resource.MustParse("4")}}},
	}}
	requestsFromLimits(&spec)
	assert.Equal(t, int64(8), spec.Containers[0].Resources.Requests.Name(gpu, resource.DecimalSI).Value())
	assert.Equal(t, int64(4), spec.Containers[1].Resources.Requests.Name(gpu, resource.DecimalSI).Value())
}

func TestDSetRoleAccessor_RollingOut(t *testing.T) {
	// prefill: every group updated, generation observed.
	prefill, err := NewDisaggregatedSetRoleAccessor(roleScalerFixture("prefill", 3, 3), dsetFixture())
	require.NoError(t, err)
	assert.False(t, RollingOut(prefill))

	// decode: 5 of 7 groups on the latest revision.
	decode, err := NewDisaggregatedSetRoleAccessor(roleScalerFixture("decode", 7, 7), dsetFixture())
	require.NoError(t, err)
	assert.True(t, RollingOut(decode))

	// A spec the controller has not observed yet rolls every role.
	set := dsetFixture()
	set.SetGeneration(4)
	prefill, err = NewDisaggregatedSetRoleAccessor(roleScalerFixture("prefill", 3, 3), set)
	require.NoError(t, err)
	assert.True(t, RollingOut(prefill))

	// So does a role with no status yet.
	set = dsetFixture()
	require.NoError(t, unstructured.SetNestedSlice(set.Object, []any{}, "status", "roleStatuses"))
	prefill, err = NewDisaggregatedSetRoleAccessor(roleScalerFixture("prefill", 3, 3), set)
	require.NoError(t, err)
	assert.True(t, RollingOut(prefill))
}

func TestDSetRoleAccessor_Refusals(t *testing.T) {
	sub := roleScalerFixture("decode", 1, 1)
	sub.SetLabels(map[string]string{
		constants.DisaggregatedSetRoleLabelKey:    "decode",
		constants.DisaggregatedSetSubRoleLabelKey: "batch",
	})
	_, err := NewDisaggregatedSetRoleAccessor(sub, dsetFixture())
	assert.ErrorContains(t, err, "sub-role")

	noRole := roleScalerFixture("decode", 1, 1)
	noRole.SetLabels(nil)
	_, err = NewDisaggregatedSetRoleAccessor(noRole, dsetFixture())
	assert.ErrorContains(t, err, constants.DisaggregatedSetRoleLabelKey)

	_, err = NewDisaggregatedSetRoleAccessor(roleScalerFixture("encode", 1, 1), dsetFixture())
	assert.ErrorContains(t, err, `no role "encode"`)
}

func TestGroupPodLabels(t *testing.T) {
	decode, err := NewDisaggregatedSetRoleAccessor(roleScalerFixture("decode", 7, 7), dsetFixture())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		constants.DisaggregatedSetNameLabelKey: "glm",
		constants.DisaggregatedSetRoleLabelKey: "decode",
	}, GroupPodLabels(decode))

	lws := NewLWSAccessor(&lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "lws"}})
	assert.Equal(t, map[string]string{lwsv1.SetNameLabelKey: "lws"}, GroupPodLabels(lws))
	assert.Nil(t, GroupPodLabels(NewDeploymentAccessor(nil)))

	pod := func(labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: labels}}
	}
	// A pod of either revision of the decode role is decode's; the prefill
	// pod of the same set and the decode pod of another set are not.
	assert.True(t, OwnsGroupPod(decode, pod(map[string]string{
		constants.DisaggregatedSetNameLabelKey: "glm", constants.DisaggregatedSetRoleLabelKey: "decode",
		lwsv1.SetNameLabelKey: "glm-0-aaaa1111-decode"})))
	assert.True(t, OwnsGroupPod(decode, pod(map[string]string{
		constants.DisaggregatedSetNameLabelKey: "glm", constants.DisaggregatedSetRoleLabelKey: "decode",
		lwsv1.SetNameLabelKey: "glm-0-bbbb2222-decode"})))
	assert.False(t, OwnsGroupPod(decode, pod(map[string]string{
		constants.DisaggregatedSetNameLabelKey: "glm", constants.DisaggregatedSetRoleLabelKey: "prefill"})))
	assert.False(t, OwnsGroupPod(decode, pod(map[string]string{
		constants.DisaggregatedSetNameLabelKey: "other", constants.DisaggregatedSetRoleLabelKey: "decode"})))
	assert.True(t, OwnsGroupPod(lws, pod(map[string]string{lwsv1.SetNameLabelKey: "lws"})))
}

func TestFetchScaleTarget_DisaggregatedSetRoleScaler(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(dsetGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(dsetGVK.GroupVersion().WithKind(constants.DisaggregatedSetKind+"List"), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(dsetRoleScalerGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(dsetRoleScalerGVK.GroupVersion().WithKind(constants.DisaggregatedSetRoleScalerKind+"List"), &unstructured.UnstructuredList{})

	objs := []client.Object{dsetFixture(), roleScalerFixture("decode", 5, 7)}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	acc, err := FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-decode", dsetNS)
	require.NoError(t, err)
	assert.Equal(t, int32(5), *acc.GetReplicas(), "spec.replicas of the scaler, the count KEDA wrote")
	assert.Equal(t, int32(7), acc.GetStatusReplicas())
	assert.Equal(t, int32(4), acc.GetGroupSize())

	_, err = FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-prefill", dsetNS)
	assert.True(t, apierrors.IsNotFound(err), "a role still in Static mode has no scaler: %v", err)

	// A scaler whose set is gone is not found either.
	orphan := roleScalerFixture("prefill", 1, 1)
	orphan.SetOwnerReferences(nil)
	orphan.SetLabels(map[string]string{
		constants.DisaggregatedSetNameLabelKey: "gone",
		constants.DisaggregatedSetRoleLabelKey: "prefill",
	})
	require.NoError(t, c.Create(context.Background(), orphan))
	_, err = FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-prefill", dsetNS)
	assert.True(t, apierrors.IsNotFound(err), "set named by label is read: %v", err)
}

// A Static role has no role scaler; the role writer registers it under the
// name a scaler would have, <set>-<role>, and the target is read from the set.
// Set and role names may both hold dashes.
func TestFetchScaleTarget_StaticRoleFromTheSet(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(dsetGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(dsetRoleScalerGVK, &unstructured.Unstructured{})

	set := dsetFixture()
	set.SetName("glm-v2")
	roles, _, _ := unstructured.NestedSlice(set.Object, "spec", "roles")
	delete(roles[1].(map[string]any), "scaling") // decode: Static
	require.NoError(t, unstructured.SetNestedSlice(set.Object, roles, "spec", "roles"))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(set).Build()

	acc, err := FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-v2-decode", dsetNS)
	require.NoError(t, err)
	setName, role, ok := DisaggregatedSetRole(acc)
	require.True(t, ok)
	assert.Equal(t, []string{"glm-v2", "decode"}, []string{setName, role})
	assert.Nil(t, Annotations(acc), "a role has no object of its own to annotate")
	assert.Equal(t, "glm-v2-decode", acc.GetName())
	assert.Equal(t, int32(7), *acc.GetReplicas(), "the role's spec.replicas")
	assert.Equal(t, int32(7), acc.GetStatusReplicas())
	assert.Equal(t, map[string]string{
		constants.DisaggregatedSetNameLabelKey: "glm-v2",
		constants.DisaggregatedSetRoleLabelKey: "decode",
	}, GroupPodLabels(acc))

	// prefill is External with no scaler yet: not found, not read from the set.
	_, err = FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-v2-prefill", dsetNS)
	assert.True(t, apierrors.IsNotFound(err), "%v", err)
}

// LWS v0.9.0 has no DisaggregatedSetRoleScaler kind: reading one fails with no
// REST mapping, not NotFound. That is the version the role writer exists for,
// so its Static roles must still be read from the set, and promptly.
func TestFetchScaleTarget_StaticRoleWithoutTheScalerKind(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(dsetGVK, &unstructured.Unstructured{})
	set := dsetFixture()
	roles, _, _ := unstructured.NestedSlice(set.Object, "spec", "roles")
	delete(roles[1].(map[string]any), "scaling") // decode: Static
	require.NoError(t, unstructured.SetNestedSlice(set.Object, roles, "spec", "roles"))
	scalerGets := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(set).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if obj.GetObjectKind().GroupVersionKind().Kind == constants.DisaggregatedSetRoleScalerKind {
				scalerGets++
				return &meta.NoKindMatchError{GroupKind: dsetRoleScalerGVK.GroupKind(), SearchedVersions: []string{"v1"}}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()

	acc, err := FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-decode", dsetNS)
	require.NoError(t, err)
	_, _, ok := DisaggregatedSetRole(acc)
	assert.True(t, ok)
	assert.Equal(t, int32(7), *acc.GetReplicas())
	assert.Equal(t, 1, scalerGets, "a missing kind is not retried: no backoff can install a CRD")
}
