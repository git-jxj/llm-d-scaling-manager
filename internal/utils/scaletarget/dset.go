package scaletarget

import (
	"context"
	"fmt"
	"math"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			// No role scaler: a Static role, or an LWS without the
			// DisaggregatedSetRoleScaler kind at all (before v0.10.0).
			// Only the role writer registers such a target.
			acc, serr := fetchStaticRole(ctx, c, name, namespace)
			if serr != nil {
				return nil, serr
			}
			if acc != nil {
				return acc, nil
			}
		}
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

// fetchStaticRole resolves <set>-<role> to a Static role of a DisaggregatedSet
// in namespace. Both halves may hold dashes, so every split is tried, one
// read per candidate set. Nil and no error when no split names a Static role
// of an existing set: the target is not such a role. A read that fails other
// than NotFound or NoMatch (Forbidden, a timeout) is returned, never taken for
// absence. Two splits that both name a Static role -- sets "glm" with role
// "v2-decode" and "glm-v2" with role "decode" -- are an error: the name
// cannot say which, and a guess would scale the wrong set.
func fetchStaticRole(ctx context.Context, c client.Client, name, namespace string) (ScaleTargetAccessor, error) {
	var found ScaleTargetAccessor
	var foundSet string
	for i := strings.Index(name, "-"); i > 0 && i < len(name)-1; {
		setName, role := name[:i], name[i+1:]
		set := &unstructured.Unstructured{}
		set.SetGroupVersionKind(dsetGVK)
		err := c.Get(ctx, client.ObjectKey{Name: setName, Namespace: namespace}, set)
		switch {
		case err == nil:
			if RoleScalingMode(set, role) == "Static" {
				acc, aerr := NewDisaggregatedSetStaticRoleAccessor(set, role)
				if aerr != nil {
					return nil, aerr
				}
				if found != nil {
					return nil, fmt.Errorf("%s/%s is ambiguous: DisaggregatedSet %s and DisaggregatedSet %s both have a Static role it names; rename one",
						namespace, name, foundSet, setName)
				}
				found, foundSet = acc, setName
			}
		case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		default:
			return nil, fmt.Errorf("reading DisaggregatedSet %s/%s for target %s: %w", namespace, setName, name, err)
		}
		j := strings.Index(name[i+1:], "-")
		if j < 0 {
			break
		}
		i += j + 1
	}
	return found, nil
}

// DisaggregatedSetRoleGone reports whether the DisaggregatedSet role named
// name in namespace is gone: no role scaler of that name and no Static role
// a set names so, or the kind not served at all. One read per candidate, no
// backoff; any other error is not an absence.
func DisaggregatedSetRoleGone(ctx context.Context, c client.Client, name, namespace string) (bool, error) {
	scaler := &unstructured.Unstructured{}
	scaler.SetGroupVersionKind(dsetRoleScalerGVK)
	err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, scaler)
	switch {
	case err == nil:
		return scaler.GetDeletionTimestamp() != nil, nil
	case !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err):
		return false, err
	}
	acc, err := fetchStaticRole(ctx, c, name, namespace)
	if err != nil {
		return false, err
	}
	return acc == nil || acc.GetDeletionTimestamp() != nil, nil
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
	// The scaler's spec.replicas is what KEDA writes and the controller
	// applies; its status.replicas counts groups across every revision.
	desired, _, err := unstructured.NestedInt64(scaler.Object, "spec", "replicas")
	if err != nil {
		return nil, fmt.Errorf("%s %s/%s spec.replicas: %w",
			constants.DisaggregatedSetRoleScalerKind, scaler.GetNamespace(), scaler.GetName(), err)
	}
	current, _, _ := unstructured.NestedInt64(scaler.Object, "status", "replicas")
	deleted := set.GetDeletionTimestamp()
	if deleted == nil {
		deleted = scaler.GetDeletionTimestamp()
	}
	return buildRoleAccessor(set, role, metav1.ObjectMeta{
		Name: scaler.GetName(), Namespace: scaler.GetNamespace(), UID: scaler.GetUID(), DeletionTimestamp: deleted,
	}, clampInt32(desired), clampInt32(current))
}

// NewDisaggregatedSetStaticRoleAccessor builds the scale target of a Static
// role of DisaggregatedSet set, which has no role scaler: its desired count is
// the role's spec.replicas (1 when unset, the API default), which only the
// role writer (internal/dsetwriter) changes. Its name is <set>-<role>, the
// name a role scaler would have, so attribution is the same on both paths.
// It refuses an External role: that one is the role scaler's. It refuses a
// set of more than one slice, as the role writer does: a role's count is then
// per slice, while the engines would size its total.
func NewDisaggregatedSetStaticRoleAccessor(set *unstructured.Unstructured, role string) (ScaleTargetAccessor, error) {
	if slices, found, _ := unstructured.NestedInt64(set.Object, "spec", "slices"); found && slices > 1 {
		return nil, fmt.Errorf("DisaggregatedSet %s/%s has spec.slices %d: a Static role is scaled only in a set of one slice",
			set.GetNamespace(), set.GetName(), slices)
	}
	if RoleScalingMode(set, role) == "External" {
		return nil, fmt.Errorf("DisaggregatedSet %s/%s role %q is External: it is scaled through its %s",
			set.GetNamespace(), set.GetName(), role, constants.DisaggregatedSetRoleScalerKind)
	}
	roleSpec, err := roleOf(set, role)
	if err != nil {
		return nil, err
	}
	desired, found, _ := unstructured.NestedInt64(roleSpec, "replicas")
	if !found {
		desired = 1
	}
	current := int64(0)
	statuses, _, _ := unstructured.NestedSlice(set.Object, "status", "roleStatuses")
	for _, s := range statuses {
		if m, ok := s.(map[string]any); ok && m["name"] == role {
			current, _, _ = unstructured.NestedInt64(m, "replicas")
		}
	}
	return buildRoleAccessor(set, role, metav1.ObjectMeta{
		Name: set.GetName() + "-" + role, Namespace: set.GetNamespace(), UID: set.GetUID(),
		DeletionTimestamp: set.GetDeletionTimestamp(),
	}, clampInt32(desired), clampInt32(current))
}

// clampInt32 is n bounded to [0, MaxInt32], so a count stored as an int64
// never wraps into a negative one.
func clampInt32(n int64) int32 {
	return int32(min(max(n, 0), math.MaxInt32))
}

// RoleScalingMode is role's spec.roles[].scaling.mode in set: "Static" when
// unset, the API default, and "" when set has no such role.
func RoleScalingMode(set *unstructured.Unstructured, role string) string {
	roles, _, _ := unstructured.NestedSlice(set.Object, "spec", "roles")
	for _, r := range roles {
		m, ok := r.(map[string]any)
		if !ok || m["name"] != role {
			continue
		}
		mode, _, _ := unstructured.NestedString(m, "scaling", "mode")
		if mode == "" {
			mode = "Static"
		}
		return mode
	}
	return ""
}

// buildRoleAccessor is the scale target of role in set, named and dated by
// om, at desired and current groups.
func buildRoleAccessor(set *unstructured.Unstructured, role string, om metav1.ObjectMeta,
	desired, current int32) (ScaleTargetAccessor, error) {
	roleSpec, err := roleOf(set, role)
	if err != nil {
		return nil, err
	}
	var spec lwsv1.LeaderWorkerSetSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(roleSpec, &spec); err != nil {
		return nil, fmt.Errorf("DisaggregatedSet %s/%s role %q: %w", set.GetNamespace(), set.GetName(), role, err)
	}
	spec.Replicas = &desired
	if t := spec.LeaderWorkerTemplate.LeaderTemplate; t != nil {
		requestsFromLimits(&t.Spec)
	}
	requestsFromLimits(&spec.LeaderWorkerTemplate.WorkerTemplate.Spec)

	status, rolling := roleStatusOf(set, role)
	status.Replicas = current
	return &dsetRoleAccessor{
		lwsAccessor: lwsAccessor{lws: &lwsv1.LeaderWorkerSet{
			ObjectMeta: om,
			Spec:       spec,
			Status:     status,
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
		out.ReadyReplicas = clampInt32(ready)
		out.UpdatedReplicas = clampInt32(updated)
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
