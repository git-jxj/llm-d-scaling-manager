package fixtures

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	infextv1alpha2 "sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
)

const defaultInferenceObjectiveName = "e2e-default"

// The llm-d.ai InferenceObjective versions, in the order the fixture tries
// them. Router v1.0.0 reads llm-d.ai v1 and, as a deprecated secondary,
// v1alpha2; routers before it read llm-d.ai v1alpha2 and fall back to the
// gateway-api-inference-extension group (inference.networking.x-k8s.io), which
// v1.0.0 no longer reads at all. v1 targets pools with spec.poolRefs[],
// v1alpha2 with a single spec.poolRef.
var (
	llmdObjectiveGVK         = schema.GroupVersionKind{Group: "llm-d.ai", Version: "v1", Kind: "InferenceObjective"}
	llmdObjectiveV1Alpha2GVK = schema.GroupVersionKind{Group: "llm-d.ai", Version: "v1alpha2", Kind: "InferenceObjective"}
)

// EnsureInferenceObjective creates the default InferenceObjective (priority 0)
// for the router's flow-control queue when an InferenceObjective API exists.
// poolName must match the InferencePool metadata.name (typically the EPP
// Service name with an "-epp" suffix removed, e.g. optimized-baseline-epp →
// optimized-baseline).
//
// The object is optional. Priority 0 is also what a request that matches no
// objective gets, so requests queue the same without it. Callers must not
// treat applied=false as a reason to skip: it only reports that the cluster
// serves no InferenceObjective API. EnsureInferenceObjectiveOptional is the
// form for callers that only need it to be there when it can be.
func EnsureInferenceObjective(ctx context.Context, crClient client.Client, namespace, poolName string) (applied bool, err error) {
	return EnsureInferenceObjectiveNamed(ctx, crClient, namespace, defaultInferenceObjectiveName, poolName)
}

// EnsureInferenceObjectiveOptional creates the default InferenceObjective when
// the cluster serves an InferenceObjective API and reports through logf when it
// does not. Only a real error (not a missing API) is returned.
func EnsureInferenceObjectiveOptional(ctx context.Context, crClient client.Client, namespace, poolName string, logf func(format string, args ...any)) error {
	applied, err := EnsureInferenceObjective(ctx, crClient, namespace, poolName)
	if err != nil {
		return err
	}
	if !applied && logf != nil {
		logf("No InferenceObjective API on this cluster; requests queue at the default priority 0\n")
	}
	return nil
}

// EnsureInferenceObjectiveNamed creates or updates a named InferenceObjective,
// in the first form the cluster serves: llm-d.ai/v1, llm-d.ai/v1alpha2, then
// inference.networking.x-k8s.io/v1alpha2. With none of them it returns
// (false, nil).
func EnsureInferenceObjectiveNamed(ctx context.Context, crClient client.Client, namespace, objectiveName, poolName string) (applied bool, err error) {
	poolGroup, gErr := resolveInferencePoolGroup(ctx, crClient, namespace, poolName)
	if gErr != nil {
		return false, gErr
	}
	for _, gvk := range []schema.GroupVersionKind{llmdObjectiveGVK, llmdObjectiveV1Alpha2GVK} {
		applied, err = ensureLLMDInferenceObjective(ctx, crClient, gvk, namespace, objectiveName, poolName, poolGroup)
		if err != nil || applied {
			return applied, err
		}
	}
	return ensureLegacyInferenceObjective(ctx, crClient, namespace, objectiveName, poolName, poolGroup)
}

// ensureLLMDInferenceObjective writes the llm-d.ai form of the given version.
// It returns (false, nil) when the cluster does not serve that version.
func ensureLLMDInferenceObjective(ctx context.Context, crClient client.Client, gvk schema.GroupVersionKind, namespace, objectiveName, poolName, poolGroup string) (bool, error) {
	desired := buildLLMDInferenceObjective(gvk, namespace, objectiveName, poolName, poolGroup)
	cErr := crClient.Create(ctx, desired)
	if cErr == nil {
		return true, nil
	}
	if inferenceObjectiveAPIMissing(cErr, gvk.Group) {
		return false, nil
	}
	if !apierrors.IsAlreadyExists(cErr) {
		return false, fmt.Errorf("create %s InferenceObjective %s: %w", gvk.GroupVersion(), objectiveName, cErr)
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(gvk)
	if getErr := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: objectiveName}, current); getErr != nil {
		return false, fmt.Errorf("get existing %s InferenceObjective %s: %w", gvk.GroupVersion(), objectiveName, getErr)
	}
	if apiequality.Semantic.DeepEqual(current.Object["spec"], desired.Object["spec"]) {
		return true, nil
	}
	current.Object["spec"] = desired.Object["spec"]
	if upErr := crClient.Update(ctx, current); upErr != nil {
		return false, fmt.Errorf("update %s InferenceObjective %s: %w", gvk.GroupVersion(), objectiveName, upErr)
	}
	return true, nil
}

func buildLLMDInferenceObjective(gvk schema.GroupVersionKind, namespace, objectiveName, poolName, poolGroup string) *unstructured.Unstructured {
	ref := map[string]any{"group": poolGroup, "kind": "InferencePool", "name": poolName}
	spec := map[string]any{"priority": int64(0)}
	if gvk.Version == llmdObjectiveV1Alpha2GVK.Version {
		spec["poolRef"] = ref
	} else {
		spec["poolRefs"] = []any{ref}
	}
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(objectiveName)
	return obj
}

// ensureLegacyInferenceObjective writes the inference.networking.x-k8s.io form,
// which only routers before v1.0.0 read. It returns (false, nil) when the
// cluster does not serve that API either.
func ensureLegacyInferenceObjective(ctx context.Context, crClient client.Client, namespace, objectiveName, poolName, poolGroup string) (bool, error) {
	desired := buildInferenceObjective(namespace, objectiveName, poolName, poolGroup)

	if cErr := crClient.Create(ctx, desired); cErr != nil {
		if apierrors.IsAlreadyExists(cErr) {
			key := client.ObjectKey{Namespace: namespace, Name: objectiveName}
			current := &infextv1alpha2.InferenceObjective{}
			getErr := crClient.Get(ctx, key, current)
			if getErr != nil {
				return false, fmt.Errorf("get existing InferenceObjective %s: %w", objectiveName, getErr)
			}
			if inferenceObjectiveSpecEqual(current.Spec, desired.Spec) {
				return true, nil
			}
			current.Spec = desired.Spec
			if upErr := crClient.Update(ctx, current); upErr != nil {
				return false, fmt.Errorf("update InferenceObjective %s: %w", objectiveName, upErr)
			}
			return true, nil
		}
		if inferenceObjectiveAPIMissing(cErr, infextv1alpha2.GroupName) {
			return false, nil
		}
		return false, fmt.Errorf("create InferenceObjective %s: %w", objectiveName, cErr)
	}
	return true, nil
}

// DeleteInferenceObjective removes the default InferenceObjective if present.
func DeleteInferenceObjective(ctx context.Context, crClient client.Client, namespace string) error {
	return DeleteInferenceObjectiveNamed(ctx, crClient, namespace, defaultInferenceObjectiveName)
}

// DeleteInferenceObjectiveNamed removes a named InferenceObjective from every
// form EnsureInferenceObjectiveNamed may have written. Every delete runs; a
// missing object or an API the cluster does not serve is not an error, and
// the other errors are returned joined.
func DeleteInferenceObjectiveNamed(ctx context.Context, crClient client.Client, namespace, objectiveName string) error {
	ignorable := func(err error, group string) bool {
		return err == nil || apierrors.IsNotFound(err) || inferenceObjectiveAPIMissing(err, group)
	}
	var errs []error
	for _, gvk := range []schema.GroupVersionKind{llmdObjectiveGVK, llmdObjectiveV1Alpha2GVK} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		obj.SetNamespace(namespace)
		obj.SetName(objectiveName)
		if err := crClient.Delete(ctx, obj); !ignorable(err, gvk.Group) {
			errs = append(errs, fmt.Errorf("delete %s InferenceObjective %s: %w", gvk.GroupVersion(), objectiveName, err))
		}
	}
	err := crClient.Delete(ctx, &infextv1alpha2.InferenceObjective{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objectiveName,
			Namespace: namespace,
		},
	})
	if !ignorable(err, infextv1alpha2.GroupName) {
		errs = append(errs, fmt.Errorf("delete %s InferenceObjective %s: %w", infextv1alpha2.SchemeGroupVersion, objectiveName, err))
	}
	return errors.Join(errs...)
}

func buildInferenceObjective(namespace, objectiveName, poolName, poolGroup string) *infextv1alpha2.InferenceObjective {
	return &infextv1alpha2.InferenceObjective{
		TypeMeta: metav1.TypeMeta{
			APIVersion: infextv1alpha2.SchemeGroupVersion.String(),
			Kind:       "InferenceObjective",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      objectiveName,
			Namespace: namespace,
		},
		Spec: infextv1alpha2.InferenceObjectiveSpec{
			Priority: ptr.To(0),
			PoolRef: infextv1alpha2.PoolObjectReference{
				Group: infextv1alpha2.Group(poolGroup),
				Kind:  infextv1alpha2.Kind("InferencePool"),
				Name:  infextv1alpha2.ObjectName(poolName),
			},
		},
	}
}

// inferenceObjectiveSpecEqual compares spec semantically (priority unset vs 0 matches).
func inferenceObjectiveSpecEqual(a, b infextv1alpha2.InferenceObjectiveSpec) bool {
	return apiequality.Semantic.DeepEqual(
		normalizeInferenceObjectiveSpec(a),
		normalizeInferenceObjectiveSpec(b),
	)
}

func normalizeInferenceObjectiveSpec(spec infextv1alpha2.InferenceObjectiveSpec) infextv1alpha2.InferenceObjectiveSpec {
	if spec.Priority == nil {
		spec.Priority = ptr.To(0)
	}
	return spec
}

// inferenceObjectiveAPIMissing reports whether err says the cluster does not
// serve the InferenceObjective API of the given group.
func inferenceObjectiveAPIMissing(err error, group string) bool {
	if err == nil {
		return false
	}
	if meta.IsNoMatchError(err) {
		return true
	}
	se, ok := err.(*apierrors.StatusError)
	if !ok {
		return false
	}
	if se.ErrStatus.Reason != metav1.StatusReasonNotFound {
		return false
	}
	details := se.ErrStatus.Details
	if details == nil {
		// No details to tell which API is missing: only the server's own
		// "no such resource" wording counts.
		return strings.Contains(strings.ToLower(se.ErrStatus.Message), "the server could not find the requested resource")
	}
	// A missing resource names no object; a missing object of a served
	// resource does, and is not this.
	return details.Group == group && details.Kind == "inferenceobjectives" && details.Name == ""
}

func resolveInferencePoolGroup(ctx context.Context, crClient client.Client, namespace, poolName string) (string, error) {
	if envPoolGroup := os.Getenv("POOL_GROUP"); envPoolGroup != "" {
		return envPoolGroup, nil
	}

	inferencePoolCandidates := []schema.GroupVersionResource{
		{Group: "inference.networking.k8s.io", Version: "v1", Resource: "inferencepools"},
		{Group: "inference.networking.x-k8s.io", Version: "v1alpha2", Resource: "inferencepools"},
	}

	for _, gvr := range inferencePoolCandidates {
		pool := &unstructured.Unstructured{}
		pool.SetAPIVersion(gvr.Group + "/" + gvr.Version)
		pool.SetKind("InferencePool")
		err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: poolName}, pool)
		if err == nil {
			return gvr.Group, nil
		}
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue
		}
		return "", fmt.Errorf("detect InferencePool API group for %q: %w", poolName, err)
	}

	return "", fmt.Errorf("detect InferencePool API group for %q: no supported InferencePool resource found", poolName)
}
