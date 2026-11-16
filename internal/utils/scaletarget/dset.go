package scaletarget

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/resources"
)

// dsetRoleAccessor is one role of a DisaggregatedSet, scaled through its
// DisaggregatedSetRoleScaler. Its replicas are LeaderWorkerSet groups of the
// role's template, so it answers every group question (size, GPUs per group,
// leader and worker templates) as the LeaderWorkerSet it embeds. That
// LeaderWorkerSet is synthetic: the role's template, with the scaler's name,
// UID and replica counts. The child LeaderWorkerSets themselves are not read;
// their names change with every rollout.
type dsetRoleAccessor struct {
	lwsAccessor
	set, role  string
	rollingOut bool
}

// DisaggregatedSetRole returns the DisaggregatedSet and role acc scales, and
// false when acc is not a DisaggregatedSet role.
func DisaggregatedSetRole(acc ScaleTargetAccessor) (set, role string, ok bool) {
	a, ok := acc.(*dsetRoleAccessor)
	if !ok {
		return "", "", false
	}
	return a.set, a.role, true
}

var (
	dsetRoleScalerGVK = schema.FromAPIVersionAndKind(constants.DisaggregatedSetAPIVersion, constants.DisaggregatedSetRoleScalerKind)
	dsetGVK           = schema.FromAPIVersionAndKind(constants.DisaggregatedSetAPIVersion, constants.DisaggregatedSetKind)
)

// fetchDisaggregatedSetRole reads DisaggregatedSetRoleScaler name in namespace
// and the DisaggregatedSet that controls it. Both are read unstructured, so a
// cluster without the CRD only fails the targets that name it.
func fetchDisaggregatedSetRole(ctx context.Context, c client.Client, name, namespace string) (ScaleTargetAccessor, error) {
	scaler := &unstructured.Unstructured{}
	scaler.SetGroupVersionKind(dsetRoleScalerGVK)
	if err := resources.GetResourceWithBackoff(ctx, c, client.ObjectKey{Name: name, Namespace: namespace}, scaler,
		constants.StandardBackoff, constants.DisaggregatedSetRoleScalerKind); err != nil {
		return nil, err
	}
	setName := disaggregatedSetOf(scaler)
	if setName == "" {
		return nil, fmt.Errorf("%s %s/%s names no DisaggregatedSet: no controller reference and no %s label",
			constants.DisaggregatedSetRoleScalerKind, namespace, name, constants.DisaggregatedSetNameLabelKey)
	}
	set := &unstructured.Unstructured{}
	set.SetGroupVersionKind(dsetGVK)
	if err := resources.GetResourceWithBackoff(ctx, c, client.ObjectKey{Name: setName, Namespace: namespace}, set,
		constants.StandardBackoff, constants.DisaggregatedSetKind); err != nil {
		return nil, err
	}
	return NewDisaggregatedSetRoleAccessor(scaler, set)
}

// disaggregatedSetOf is the DisaggregatedSet a role scaler belongs to: its
// controller, which is how the DisaggregatedSet controller binds it, else its
// set-name label.
func disaggregatedSetOf(scaler *unstructured.Unstructured) string {
	if ref := metav1.GetControllerOf(scaler); ref != nil && ref.Kind == constants.DisaggregatedSetKind {
		return ref.Name
	}
	return scaler.GetLabels()[constants.DisaggregatedSetNameLabelKey]
}

// NewDisaggregatedSetRoleAccessor builds the scale target of role scaler from the
// DisaggregatedSet set. It refuses a sub-role scaler: a sub-role shares its
// parent's groups, and a group's sub-role can change after it is assigned.
func NewDisaggregatedSetRoleAccessor(scaler, set *unstructured.Unstructured) (ScaleTargetAccessor, error) {
	labels := scaler.GetLabels()
	if sub := labels[constants.DisaggregatedSetSubRoleLabelKey]; sub != "" {
		return nil, fmt.Errorf("%s %s/%s scales sub-role %q, which is not supported",
			constants.DisaggregatedSetRoleScalerKind, scaler.GetNamespace(), scaler.GetName(), sub)
	}
	role := labels[constants.DisaggregatedSetRoleLabelKey]
	if role == "" {
		return nil, fmt.Errorf("%s %s/%s has no %s label",
			constants.DisaggregatedSetRoleScalerKind, scaler.GetNamespace(), scaler.GetName(), constants.DisaggregatedSetRoleLabelKey)
	}

	roleSpec, err := roleOf(set, role)
	if err != nil {
		return nil, err
	}
	var spec lwsv1.LeaderWorkerSetSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(roleSpec, &spec); err != nil {
		return nil, fmt.Errorf("DisaggregatedSet %s/%s role %q: %w", set.GetNamespace(), set.GetName(), role, err)
	}

	// The scaler's spec.replicas is what KEDA writes and the controller
	// applies; its status.replicas counts groups across every revision.
	desired, _, err := unstructured.NestedInt64(scaler.Object, "spec", "replicas")
	if err != nil {
		return nil, fmt.Errorf("%s %s/%s spec.replicas: %w",
			constants.DisaggregatedSetRoleScalerKind, scaler.GetNamespace(), scaler.GetName(), err)
	}
	current, _, _ := unstructured.NestedInt64(scaler.Object, "status", "replicas")
	replicas := int32(desired)
	spec.Replicas = &replicas
	if t := spec.LeaderWorkerTemplate.LeaderTemplate; t != nil {
		requestsFromLimits(&t.Spec)
	}
	requestsFromLimits(&spec.LeaderWorkerTemplate.WorkerTemplate.Spec)

	status, rolling := roleStatusOf(set, role)
	status.Replicas = int32(current)

	deleted := set.GetDeletionTimestamp()
	if deleted == nil {
		deleted = scaler.GetDeletionTimestamp()
	}
	return &dsetRoleAccessor{
		lwsAccessor: lwsAccessor{lws: &lwsv1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:              scaler.GetName(),
				Namespace:         scaler.GetNamespace(),
				UID:               scaler.GetUID(),
				DeletionTimestamp: deleted,
			},
			Spec:   spec,
			Status: status,
		}},
		set:        set.GetName(),
		role:       role,
		rollingOut: rolling,
	}, nil
}

// requestsFromLimits gives every container a request for each resource it
// limits without requesting, as the API server defaults a pod. A template in
// a custom resource is stored as written, undefaulted, and a role that sets
// only `limits: {nvidia.com/gpu: 8}` -- the usual way to write GPUs -- would
// otherwise read as requesting none; the pods its groups run do request 8.
func requestsFromLimits(spec *corev1.PodSpec) {
	for _, cs := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range cs {
			r := &cs[i].Resources
			for name, q := range r.Limits {
				if _, ok := r.Requests[name]; ok {
					continue
				}
				if r.Requests == nil {
					r.Requests = corev1.ResourceList{}
				}
				r.Requests[name] = q.DeepCopy()
			}
		}
	}
}

// roleOf returns spec.roles[name=role].spec of DisaggregatedSet set: a
// LeaderWorkerSetSpec.
func roleOf(set *unstructured.Unstructured, role string) (map[string]any, error) {
	roles, _, err := unstructured.NestedSlice(set.Object, "spec", "roles")
	if err != nil {
		return nil, fmt.Errorf("DisaggregatedSet %s/%s spec.roles: %w", set.GetNamespace(), set.GetName(), err)
	}
	for _, r := range roles {
		m, ok := r.(map[string]any)
		if !ok || m["name"] != role {
			continue
		}
		spec, ok := m["spec"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("DisaggregatedSet %s/%s role %q has no spec", set.GetNamespace(), set.GetName(), role)
		}
		return spec, nil
	}
	return nil, fmt.Errorf("DisaggregatedSet %s/%s has no role %q", set.GetNamespace(), set.GetName(), role)
}

// roleStatusOf returns the ready count of role from set's status, and whether
// the role is rolling out: the controller has not observed the set's latest
// spec, or some of the role's groups are not of its latest revision. An
// unobserved role (no status yet) is reported as rolling out, so readings are
// held rather than trusted.
func roleStatusOf(set *unstructured.Unstructured, role string) (lwsv1.LeaderWorkerSetStatus, bool) {
	var out lwsv1.LeaderWorkerSetStatus
	observed, found, _ := unstructured.NestedInt64(set.Object, "status", "observedGeneration")
	rolling := !found || observed < set.GetGeneration()
	statuses, _, _ := unstructured.NestedSlice(set.Object, "status", "roleStatuses")
	for _, s := range statuses {
		m, ok := s.(map[string]any)
		if !ok || m["name"] != role {
			continue
		}
		replicas, _, _ := unstructured.NestedInt64(m, "replicas")
		ready, _, _ := unstructured.NestedInt64(m, "readyReplicas")
		updated, _, _ := unstructured.NestedInt64(m, "updatedReplicas")
		out.ReadyReplicas = int32(ready)
		out.UpdatedReplicas = int32(updated)
		return out, rolling || updated < replicas
	}
	return out, true
}

// GroupPodLabels returns the labels that select the pods of acc's groups: a
// LeaderWorkerSet's by its set-name label, a DisaggregatedSet role's by the
// set and role labels, which span every child LeaderWorkerSet and revision.
// Nil for a target that is not grouped.
func GroupPodLabels(acc ScaleTargetAccessor) map[string]string {
	switch a := acc.(type) {
	case *dsetRoleAccessor:
		return map[string]string{
			constants.DisaggregatedSetNameLabelKey: a.set,
			constants.DisaggregatedSetRoleLabelKey: a.role,
		}
	case *lwsAccessor:
		return map[string]string{lwsv1.SetNameLabelKey: a.GetName()}
	}
	return nil
}

// OwnsGroupPod reports whether pod p is one of grouped target acc's pods, by
// the labels GroupPodLabels selects on. False for a target that is not
// grouped.
func OwnsGroupPod(acc ScaleTargetAccessor, p *corev1.Pod) bool {
	sel := GroupPodLabels(acc)
	if len(sel) == 0 || p == nil {
		return false
	}
	for k, v := range sel {
		if p.Labels[k] != v {
			return false
		}
	}
	return true
}
