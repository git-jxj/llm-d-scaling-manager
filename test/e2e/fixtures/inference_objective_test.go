package fixtures

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	infextv1alpha2 "sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
)

var legacyObjectiveGVK = infextv1alpha2.SchemeGroupVersion.WithKind("InferenceObjective")

// objectiveClient is a fake client that answers "no matches for kind" for any
// InferenceObjective kind not in served, the way a cluster without that CRD
// version does, and returns failWith (when set) from every InferenceObjective
// call. The fake client alone accepts every kind.
func objectiveClient(t *testing.T, failWith error, served ...schema.GroupVersionKind) client.Client {
	t.Helper()
	fails := map[schema.GroupVersionKind]error{}
	if failWith != nil {
		for _, gvk := range served {
			fails[gvk] = failWith
		}
	}
	return objectiveClientFailing(t, fails, served...)
}

// objectiveClientFailing is objectiveClient with a separate error per served kind.
func objectiveClientFailing(t *testing.T, fails map[schema.GroupVersionKind]error, served ...schema.GroupVersionKind) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := infextv1alpha2.Install(scheme); err != nil {
		t.Fatal(err)
	}
	gate := func(obj client.Object) error {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			return err
		}
		if gvk.Kind != "InferenceObjective" {
			return nil
		}
		for _, s := range served {
			if s == gvk {
				return fails[gvk]
			}
		}
		return &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := gate(obj); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := gate(obj); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := gate(obj); err != nil {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
}

func getObjective(c client.Client, gvk schema.GroupVersionKind) (*unstructured.Unstructured, bool) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: defaultInferenceObjectiveName}, u)
	return u, err == nil
}

func TestEnsureInferenceObjectivePrefersLLMDV1(t *testing.T) {
	t.Setenv("POOL_GROUP", "inference.networking.k8s.io")
	ctx := context.Background()

	c := objectiveClient(t, nil, llmdObjectiveGVK, llmdObjectiveV1Alpha2GVK, legacyObjectiveGVK)
	applied, err := EnsureInferenceObjective(ctx, c, "ns", "pool")
	if err != nil || !applied {
		t.Fatalf("all APIs served: applied=%v err=%v, want true, nil", applied, err)
	}
	u, ok := getObjective(c, llmdObjectiveGVK)
	if !ok {
		t.Fatal("all APIs served: no llm-d.ai/v1 objective, the version router v1.0.0 reads first")
	}
	if _, ok := getObjective(c, legacyObjectiveGVK); ok {
		t.Fatal("all APIs served: an x-k8s objective was created as well")
	}
	refs, _, _ := unstructured.NestedSlice(u.Object, "spec", "poolRefs")
	if len(refs) != 1 || refs[0].(map[string]any)["name"] != "pool" || refs[0].(map[string]any)["group"] != "inference.networking.k8s.io" {
		t.Fatalf("poolRefs = %v, want one ref to inference.networking.k8s.io pool", refs)
	}
	// A second call is a no-op, not an error.
	if applied, err := EnsureInferenceObjective(ctx, c, "ns", "pool"); err != nil || !applied {
		t.Fatalf("re-run: applied=%v err=%v", applied, err)
	}
	if err := DeleteInferenceObjective(ctx, c, "ns"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := getObjective(c, llmdObjectiveGVK); ok {
		t.Fatal("delete left the llm-d.ai/v1 objective")
	}
}

// A cluster with the llm-d.ai CRD from a router before v1.0.0 serves only
// v1alpha2, which takes a single poolRef.
func TestEnsureInferenceObjectiveUsesLLMDV1Alpha2(t *testing.T) {
	t.Setenv("POOL_GROUP", "inference.networking.k8s.io")
	c := objectiveClient(t, nil, llmdObjectiveV1Alpha2GVK, legacyObjectiveGVK)
	applied, err := EnsureInferenceObjective(context.Background(), c, "ns", "pool")
	if err != nil || !applied {
		t.Fatalf("llm-d.ai v1alpha2 served: applied=%v err=%v, want true, nil", applied, err)
	}
	u, ok := getObjective(c, llmdObjectiveV1Alpha2GVK)
	if !ok {
		t.Fatal("llm-d.ai v1alpha2 served: no llm-d.ai/v1alpha2 objective")
	}
	if name, _, _ := unstructured.NestedString(u.Object, "spec", "poolRef", "name"); name != "pool" {
		t.Fatalf("spec.poolRef.name = %q, want pool", name)
	}
	if _, ok := getObjective(c, legacyObjectiveGVK); ok {
		t.Fatal("llm-d.ai v1alpha2 served: an x-k8s objective was created as well")
	}
}

func TestEnsureInferenceObjectiveFallsBackToLegacyGroup(t *testing.T) {
	t.Setenv("POOL_GROUP", "inference.networking.k8s.io")
	ctx := context.Background()
	c := objectiveClient(t, nil, legacyObjectiveGVK)
	applied, err := EnsureInferenceObjective(ctx, c, "ns", "pool")
	if err != nil || !applied {
		t.Fatalf("only x-k8s served: applied=%v err=%v, want true, nil", applied, err)
	}
	if _, ok := getObjective(c, legacyObjectiveGVK); !ok {
		t.Fatal("only x-k8s served: no x-k8s objective")
	}
	if err := DeleteInferenceObjective(ctx, c, "ns"); err != nil {
		t.Fatalf("delete of a legacy-only objective: %v", err)
	}
	if _, ok := getObjective(c, legacyObjectiveGVK); ok {
		t.Fatal("delete left the x-k8s objective")
	}
}

func TestEnsureInferenceObjectiveNoAPIIsNotAnError(t *testing.T) {
	t.Setenv("POOL_GROUP", "inference.networking.k8s.io")
	c := objectiveClient(t, nil)
	applied, err := EnsureInferenceObjective(context.Background(), c, "ns", "pool")
	if err != nil || applied {
		t.Fatalf("no API served: applied=%v err=%v, want false, nil", applied, err)
	}
	var logged int
	if err := EnsureInferenceObjectiveOptional(context.Background(), c, "ns", "pool", func(string, ...any) { logged++ }); err != nil || logged != 1 {
		t.Fatalf("optional, no API served: err=%v, logged %d times, want nil and once", err, logged)
	}
	if err := DeleteInferenceObjective(context.Background(), c, "ns"); err != nil {
		t.Fatalf("delete with no API served: %v", err)
	}
}

func TestInferenceObjectiveForbiddenPropagates(t *testing.T) {
	t.Setenv("POOL_GROUP", "inference.networking.k8s.io")
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "llm-d.ai", Resource: "inferenceobjectives"}, "", errors.New("rbac"))
	c := objectiveClient(t, forbidden, llmdObjectiveGVK, llmdObjectiveV1Alpha2GVK, legacyObjectiveGVK)
	if applied, err := EnsureInferenceObjective(context.Background(), c, "ns", "pool"); !apierrors.IsForbidden(err) || applied {
		t.Fatalf("Ensure under Forbidden: applied=%v err=%v, want false and the Forbidden error", applied, err)
	}
	if err := EnsureInferenceObjectiveOptional(context.Background(), c, "ns", "pool", nil); !apierrors.IsForbidden(err) {
		t.Fatalf("EnsureOptional under Forbidden: err=%v, want the Forbidden error", err)
	}
	err := DeleteInferenceObjective(context.Background(), c, "ns")
	if !apierrors.IsForbidden(err) {
		t.Fatalf("Delete under Forbidden: err=%v, want the Forbidden error", err)
	}
}

// A delete that fails in one group must not stop the delete in another.
func TestDeleteInferenceObjectiveRunsEveryDelete(t *testing.T) {
	t.Setenv("POOL_GROUP", "inference.networking.k8s.io")
	ctx := context.Background()
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "llm-d.ai", Resource: "inferenceobjectives"}, "", errors.New("rbac"))

	// Create the legacy object through a client that serves only that group.
	seed := objectiveClient(t, nil, legacyObjectiveGVK)
	if applied, err := EnsureInferenceObjective(ctx, seed, "ns", "pool"); err != nil || !applied {
		t.Fatalf("seed: applied=%v err=%v", applied, err)
	}
	legacy := &infextv1alpha2.InferenceObjective{}
	if err := seed.Get(ctx, client.ObjectKey{Namespace: "ns", Name: defaultInferenceObjectiveName}, legacy); err != nil {
		t.Fatal(err)
	}

	c := objectiveClientFailing(t, map[schema.GroupVersionKind]error{llmdObjectiveGVK: forbidden},
		llmdObjectiveGVK, llmdObjectiveV1Alpha2GVK, legacyObjectiveGVK)
	legacy.ResourceVersion = ""
	if err := c.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	err := DeleteInferenceObjective(ctx, c, "ns")
	if !apierrors.IsForbidden(err) {
		t.Fatalf("Delete with llm-d.ai v1 forbidden: err=%v, want the Forbidden error", err)
	}
	if _, ok := getObjective(c, legacyObjectiveGVK); ok {
		t.Fatal("the llm-d.ai v1 delete failing stopped the x-k8s delete")
	}
}

func TestInferenceObjectiveAPIMissing(t *testing.T) {
	gr := schema.GroupResource{Group: "llm-d.ai", Resource: "inferenceobjectives"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"no kind match", &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "llm-d.ai", Kind: "InferenceObjective"}}, true},
		{"resource missing, this group", apierrors.NewNotFound(gr, ""), true},
		{"resource missing, another group", apierrors.NewNotFound(schema.GroupResource{Group: "other.io", Resource: "inferenceobjectives"}, ""), false},
		{"object missing in a served resource", apierrors.NewNotFound(gr, "e2e-default"), false},
		{"forbidden", apierrors.NewForbidden(gr, "", errors.New("rbac")), false},
	}
	for _, tc := range cases {
		if got := inferenceObjectiveAPIMissing(tc.err, "llm-d.ai"); got != tc.want {
			t.Errorf("%s: inferenceObjectiveAPIMissing = %v, want %v", tc.name, got, tc.want)
		}
	}
}
