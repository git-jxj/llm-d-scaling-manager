package fixtures

import (
	"context"
	"testing"

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
// does. The fake client alone accepts every kind.
func objectiveClient(t *testing.T, served ...schema.GroupVersionKind) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := infextv1alpha2.Install(scheme); err != nil {
		t.Fatal(err)
	}
	unserved := func(obj client.Object) error {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			return err
		}
		if gvk.Kind != "InferenceObjective" {
			return nil
		}
		for _, s := range served {
			if s == gvk {
				return nil
			}
		}
		return &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := unserved(obj); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := unserved(obj); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := unserved(obj); err != nil {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
}

func objectiveExists(t *testing.T, c client.Client, gvk schema.GroupVersionKind) bool {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: defaultInferenceObjectiveName}, u)
	return err == nil
}

func TestEnsureInferenceObjectivePrefersLLMDGroup(t *testing.T) {
	t.Setenv("POOL_GROUP", "inference.networking.k8s.io")
	ctx := context.Background()

	c := objectiveClient(t, llmdObjectiveGVK, legacyObjectiveGVK)
	applied, err := EnsureInferenceObjective(ctx, c, "ns", "pool")
	if err != nil || !applied {
		t.Fatalf("both APIs served: applied=%v err=%v, want true, nil", applied, err)
	}
	if !objectiveExists(t, c, llmdObjectiveGVK) {
		t.Fatal("both APIs served: no llm-d.ai/v1 objective, which is the only one router v1.0.0 reads")
	}
	if objectiveExists(t, c, legacyObjectiveGVK) {
		t.Fatal("both APIs served: an x-k8s objective was created as well")
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(llmdObjectiveGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: defaultInferenceObjectiveName}, u); err != nil {
		t.Fatal(err)
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
	if objectiveExists(t, c, llmdObjectiveGVK) {
		t.Fatal("delete left the llm-d.ai/v1 objective")
	}
}

func TestEnsureInferenceObjectiveFallsBackToLegacyGroup(t *testing.T) {
	t.Setenv("POOL_GROUP", "inference.networking.k8s.io")
	c := objectiveClient(t, legacyObjectiveGVK)
	applied, err := EnsureInferenceObjective(context.Background(), c, "ns", "pool")
	if err != nil || !applied {
		t.Fatalf("only x-k8s served: applied=%v err=%v, want true, nil", applied, err)
	}
	if !objectiveExists(t, c, legacyObjectiveGVK) {
		t.Fatal("only x-k8s served: no x-k8s objective")
	}
}

func TestEnsureInferenceObjectiveNoAPIIsNotAnError(t *testing.T) {
	t.Setenv("POOL_GROUP", "inference.networking.k8s.io")
	c := objectiveClient(t)
	applied, err := EnsureInferenceObjective(context.Background(), c, "ns", "pool")
	if err != nil || applied {
		t.Fatalf("no API served: applied=%v err=%v, want false, nil", applied, err)
	}
	if err := DeleteInferenceObjective(context.Background(), c, "ns"); err != nil {
		t.Fatalf("delete with no API served: %v", err)
	}
}
