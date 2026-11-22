package fixtures

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// DisaggregatedSetGVK and DisaggregatedSetRoleScalerGVK are the LeaderWorkerSet
// project's P/D kinds (LWS v0.10.0 and later), read unstructured: the module
// this repository pins predates them.
var (
	DisaggregatedSetGVK           = schema.FromAPIVersionAndKind(constants.DisaggregatedSetAPIVersion, constants.DisaggregatedSetKind)
	DisaggregatedSetRoleScalerGVK = schema.FromAPIVersionAndKind(constants.DisaggregatedSetAPIVersion, constants.DisaggregatedSetRoleScalerKind)
)

// DSetRole is one role of a DisaggregatedSet a spec stands up.
type DSetRole struct {
	// Name is the role's name in the set, and its llm-d.ai/role label
	// (prefill or decode).
	Name string
	// External is scaling.mode External: KEDA scales the role through its
	// DisaggregatedSetRoleScaler. False is Static, the API's default.
	External bool
	// Size is the pods of one group (leaderWorkerTemplate.size).
	Size int32
	// Replicas is the groups the role starts with.
	Replicas int32
}

// DSetOption adjusts the pod templates of every role before the set is created.
type DSetOption = LWSOption

// CreateModelServiceDSet creates a DisaggregatedSet serving modelID with the
// given roles, each role's template the model-service LeaderWorkerSet's
// (buildModelServiceLWS) with its role label and opts applied. The set's
// controller replaces the pods' app label with <set>-<slice>-<role>: select
// them by the disaggregatedset.x-k8s.io labels.
// annotations go on the set. It is created, not applied: a create owns
// every field of the stored set, replicas included, so a role writer that
// should write it needs "force" (docs/reference/disaggregatedset.md,
// "Ownership of replicas").
func CreateModelServiceDSet(ctx context.Context, crClient client.Client, namespace, name, modelID string,
	useSimulator bool, maxNumSeqs int, annotations map[string]string, roles []DSetRole, opts ...DSetOption) error {
	var specs []any
	for _, r := range roles {
		l := buildModelServiceLWS(namespace, name+"-"+r.Name, name+"-pool", modelID, useSimulator, maxNumSeqs, r.Size)
		WithLWSRole(r.Name)(l)
		for _, opt := range opts {
			opt(l)
		}
		l.Spec.Replicas = ptr.To(r.Replicas)
		spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&l.Spec)
		if err != nil {
			return fmt.Errorf("role %s of DisaggregatedSet %s: %w", r.Name, name, err)
		}
		// Only what the role sets: the converter writes every zero-valued
		// field of the LeaderWorkerSet spec, and the set's schema refuses an
		// empty rolloutStrategy.type.
		roleSpec := map[string]any{}
		for _, k := range []string{"replicas", "leaderWorkerTemplate", "startupPolicy"} {
			if v, ok := spec[k]; ok {
				roleSpec[k] = dropNulls(v)
			}
		}
		role := map[string]any{"name": r.Name, "spec": roleSpec}
		if r.External {
			// The role scaler owns the count; the set's replicas seed it.
			role["scaling"] = map[string]any{"mode": "External"}
		}
		specs = append(specs, role)
	}
	set := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec":     map[string]any{"roles": specs},
	}}
	set.SetGroupVersionKind(DisaggregatedSetGVK)
	set.SetLabels(map[string]string{"test-resource": defaultTestResourceLabelValue})
	set.SetAnnotations(annotations)
	if err := crClient.Create(ctx, set); err != nil {
		return fmt.Errorf("create DisaggregatedSet %s/%s: %w", namespace, name, err)
	}
	return nil
}

// dropNulls removes the nil values (metadata.creationTimestamp and the like)
// from an unstructured tree, which a CRD's validation reads as null.
func dropNulls(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range t {
			if e != nil {
				out[k] = dropNulls(e)
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, dropNulls(e))
		}
		return out
	default:
		return v
	}
}

// DeleteDisaggregatedSet deletes a DisaggregatedSet. Idempotent.
func DeleteDisaggregatedSet(ctx context.Context, crClient client.Client, namespace, name string) error {
	set := &unstructured.Unstructured{}
	set.SetGroupVersionKind(DisaggregatedSetGVK)
	set.SetNamespace(namespace)
	set.SetName(name)
	if err := crClient.Delete(ctx, set); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete DisaggregatedSet %s/%s: %w", namespace, name, err)
	}
	return nil
}

// DSetReplicas is one read of a DisaggregatedSet: each role's
// spec.replicas and status readyReplicas, by role name.
type DSetReplicas struct {
	Spec, Ready map[string]int64
}

// ReadDisaggregatedSet reads the set's per-role spec.replicas and
// status.roleStatuses[].readyReplicas.
func ReadDisaggregatedSet(ctx context.Context, crClient client.Client, namespace, name string) (DSetReplicas, error) {
	set := &unstructured.Unstructured{}
	set.SetGroupVersionKind(DisaggregatedSetGVK)
	if err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, set); err != nil {
		return DSetReplicas{}, err
	}
	out := DSetReplicas{Spec: map[string]int64{}, Ready: map[string]int64{}}
	roles, _, _ := unstructured.NestedSlice(set.Object, "spec", "roles")
	for _, r := range roles {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		n, _, _ := unstructured.NestedString(m, "name")
		v, _, _ := unstructured.NestedInt64(m, "spec", "replicas")
		out.Spec[n] = v
	}
	statuses, _, _ := unstructured.NestedSlice(set.Object, "status", "roleStatuses")
	for _, r := range statuses {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		n, _, _ := unstructured.NestedString(m, "name")
		v, _, _ := unstructured.NestedInt64(m, "readyReplicas")
		out.Ready[n] = v
	}
	return out, nil
}

// ChildReadyGroups sums status.readyReplicas of the set's child
// LeaderWorkerSets by role. LeaderWorkerSet v0.10.0 writes no status on the
// set itself, so this, not ReadDisaggregatedSet's Ready, is what says the
// groups run there.
func ChildReadyGroups(ctx context.Context, crClient client.Client, namespace, set string) (map[string]int64, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "leaderworkerset.x-k8s.io", Version: "v1", Kind: "LeaderWorkerSetList"})
	if err := crClient.List(ctx, list, client.InNamespace(namespace),
		client.MatchingLabels{constants.DisaggregatedSetNameLabelKey: set}); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, l := range list.Items {
		ready, _, _ := unstructured.NestedInt64(l.Object, "status", "readyReplicas")
		out[l.GetLabels()[constants.DisaggregatedSetRoleLabelKey]] += ready
	}
	return out, nil
}

// RoleScalerReplicas reads the DisaggregatedSetRoleScaler name: spec.replicas
// (what KEDA's HPA wrote) and status.replicas (the groups running).
func RoleScalerReplicas(ctx context.Context, crClient client.Client, namespace, name string) (spec, status int64, err error) {
	rs := &unstructured.Unstructured{}
	rs.SetGroupVersionKind(DisaggregatedSetRoleScalerGVK)
	if err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, rs); err != nil {
		return 0, 0, err
	}
	spec, _, _ = unstructured.NestedInt64(rs.Object, "spec", "replicas")
	status, _, _ = unstructured.NestedInt64(rs.Object, "status", "replicas")
	return spec, status, nil
}
