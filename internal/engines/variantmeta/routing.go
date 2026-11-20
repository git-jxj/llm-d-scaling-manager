package variantmeta

import (
	"context"
	"maps"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	poolutil "github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/pool"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// PoolLister returns the Pod selectors of the InferencePools in one namespace.
// Its error is the API server's, so a kind that is not served can be told from
// a read that failed.
type PoolLister func(ctx context.Context, namespace string) ([]map[string]string, error)

// ModelRouting is what Route found for one model in one namespace.
type ModelRouting struct {
	Namespace, ModelID string
	// Pools is how many InferencePools the namespace has; 0 also when the
	// kind is not served.
	Pools int
	// PoolReadFailed is the reason the pool listing failed (forbidden,
	// unavailable or other), "" when it was read or the kind is not served.
	PoolReadFailed string
	// Routed is true when a pool selects at least one variant of the model,
	// at any replica count.
	Routed bool
	// Known is false when the pools, a scale target or a variant's Pods could
	// not be read, so the verdict for some variant was not established.
	Known bool
	// Selected and Held name the variants established as selected and as
	// held, the parked ones (at zero replicas) included. Both are empty for a
	// model that is not routed.
	Selected, Held []string
}

// Route marks, in place, the replicas of each variant that no InferencePool
// selects (docs/proposals/pool-selected-supply.md), and reports per model what
// it found.
//
// A variant is selected when a pool selects its pod template, with the labels
// its controller adds at creation (an LWS leader's set name and worker index;
// a Deployment's pod-template-hash matches any value), or any of its live
// Pods. A model counts as routed when some variant is selected -- the parked
// ones included, by template: a model whose routed variant scale-to-zero has
// parked is still routed, so its canary stays held and the parked variant is
// what wakes. For a routed model:
//
//   - a variant that is not selected is HeldUnrouted: the router sends it
//     nothing, whatever its size, so it is held rather than sized;
//   - in a selected variant, a live Pod whose own labels no pool selects (a
//     relabelled Pod) is counted in UnroutedReplicas and named in UnroutedPods.
//
// metas are the running variants; parked are the model's variants at zero
// replicas, which get no metadata but count towards Routed and are named in
// Selected or Held. RoutingKnown is set on every running variant whose verdict
// was established, including "nothing to exclude". It stays false when a read
// failed: the pools, the variant's scale target, or its Pods where they decide
// the verdict. The caller counts and logs a failed pool listing and decides
// what an unknown verdict means (the steady-state engine keeps the last known
// one for a bounded time); Route never turns a failed read into a hold or a
// release.
func Route(
	ctx context.Context,
	metas []domain.VariantMetadata,
	vas []llmdvariant.Variant,
	parked []llmdvariant.Variant,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	k8sClient client.Client,
	pools PoolLister,
) []ModelRouting {
	if pools == nil || len(metas)+len(parked) == 0 {
		return nil
	}

	vaByKey := make(map[string]*llmdvariant.Variant, len(vas))
	for i := range vas {
		vaByKey[vas[i].Namespace+"/"+vas[i].Name] = &vas[i]
	}

	// Group by (namespace, model): routing is decided per model.
	type group struct {
		namespace, model string
		idx              []int
		parked           []*llmdvariant.Variant
	}
	var groups []*group
	byKey := map[string]*group{}
	groupFor := func(namespace, model string) *group {
		k := namespace + "/" + model
		g, ok := byKey[k]
		if !ok {
			g = &group{namespace: namespace, model: model}
			byKey[k] = g
			groups = append(groups, g)
		}
		return g
	}
	for i := range metas {
		g := groupFor(metas[i].Namespace, metas[i].ModelID)
		g.idx = append(g.idx, i)
	}
	for i := range parked {
		g := groupFor(parked[i].Namespace, parked[i].Spec.ModelID)
		g.parked = append(g.parked, &parked[i])
	}

	out := make([]ModelRouting, 0, len(groups))
	for _, g := range groups {
		res := ModelRouting{Namespace: g.namespace, ModelID: g.model}
		sels, err := pools(ctx, g.namespace)
		switch {
		case err == nil:
		case meta.IsNoMatchError(err):
			// The kind is not served, so no pool can exist: no router,
			// nothing to check, and nothing failed.
			sels = nil
		default:
			res.PoolReadFailed = PoolListErrorReason(err)
			out = append(out, res)
			continue
		}
		res.Pools = len(sels)
		if len(sels) == 0 {
			res.Known = true
			for _, i := range g.idx {
				metas[i].RoutingKnown = true
			}
			out = append(out, res)
			continue
		}

		type verdict struct {
			name     string
			target   scaletarget.ScaleTargetAccessor
			pods     []corev1.Pod
			podsRead bool
			selected bool
			unknown  bool // whether it is selected could not be established
		}
		vs := make([]verdict, len(g.idx))
		for j, i := range g.idx {
			vs[j].name = metas[i].VariantName
			va := vaByKey[metas[i].Namespace+"/"+metas[i].VariantName]
			if va == nil {
				vs[j].unknown = true
				continue
			}
			st, ok := resolveScaleTarget(ctx, va, scaleTargets, k8sClient)
			if !ok {
				vs[j].unknown = true
				continue
			}
			vs[j].target = st
			vs[j].pods, vs[j].podsRead = targetPods(ctx, k8sClient, metas[i].Namespace, metas[i].VariantName, st)
			switch {
			case templateSelected(st, sels):
				vs[j].selected = true
			case !vs[j].podsRead:
				vs[j].unknown = true
			default:
				for p := range leaderPods(st, vs[j].pods) {
					if poolutil.AnySelects(sels, p.Labels) {
						vs[j].selected = true
						break
					}
				}
			}
		}
		// A parked variant has no Pods: its template alone says.
		ps := make([]verdict, len(g.parked))
		for j, va := range g.parked {
			ps[j].name = va.Name
			st, ok := resolveScaleTarget(ctx, va, scaleTargets, k8sClient)
			if !ok {
				ps[j].unknown = true
				continue
			}
			ps[j].selected = templateSelected(st, sels)
		}
		anyUnknown := false
		for _, v := range slices.Concat(vs, ps) {
			res.Routed = res.Routed || v.selected
			anyUnknown = anyUnknown || v.unknown
		}
		if !res.Routed {
			// Not routed -- unless a variant that could not be read is the
			// one a pool selects, in which case nothing is known.
			res.Known = !anyUnknown
			for _, i := range g.idx {
				metas[i].RoutingKnown = res.Known
			}
			out = append(out, res)
			continue
		}
		res.Known = true
		for j, i := range g.idx {
			v := vs[j]
			switch {
			case v.unknown:
				res.Known = false
			case !v.selected:
				markHeld(&metas[i], v.target, v.pods)
				metas[i].RoutingKnown = true
				res.Held = append(res.Held, v.name)
			case !v.podsRead:
				// Selected, but which of its Pods the pool reaches is unknown.
				res.Known = false
				res.Selected = append(res.Selected, v.name)
			default:
				markUnroutedPods(&metas[i], v.target, v.pods, sels)
				metas[i].RoutingKnown = true
				res.Selected = append(res.Selected, v.name)
			}
		}
		for _, v := range ps {
			switch {
			case v.unknown:
				res.Known = false
			case v.selected:
				res.Selected = append(res.Selected, v.name)
			default:
				res.Held = append(res.Held, v.name)
			}
		}
		out = append(out, res)
	}
	return out
}

// targetPods lists the scale target's own Pods, by ownership and not by its
// template's labels: a Pod relabelled out of the pool no longer carries them
// all, and two variants of one model routinely share the rest. The cached
// client serves it, as it serves Discover's lists.
func targetPods(ctx context.Context, k8sClient client.Client, namespace, variant string,
	st scaletarget.ScaleTargetAccessor) ([]corev1.Pod, bool) {
	if k8sClient == nil {
		return nil, false
	}
	var items []corev1.Pod
	var err error
	if sel := scaletarget.LabelSelector(st); sel != nil {
		items, err = ListDeploymentPods(ctx, k8sClient, namespace, st.GetName(), sel)
	} else {
		items, err = ListVariantPods(ctx, k8sClient, namespace, st)
	}
	if err != nil {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info(
			"Could not list a variant's pods to check which the router can reach",
			"namespace", namespace, "variant", variant, "error", err.Error())
		return nil, false
	}
	return items, true
}

// templateSelected reports whether a pool selects the target's pod template,
// with the labels its controller adds when it creates a Pod: a group's leader
// carries worker index 0 and the labels that select its group's Pods
// (scaletarget.GroupPodLabels: an LWS's set name; a DisaggregatedSet role's
// set and role). Two labels are known only once the controller creates the
// Pod, so a selector on either is taken to match: a Deployment's
// pod-template-hash, set per ReplicaSet, and a DisaggregatedSet role's child
// LeaderWorkerSet name, which changes with every rollout. Otherwise a parked
// role a pool selects that way would read as held, and never wake.
func templateSelected(st scaletarget.ScaleTargetAccessor, sels []map[string]string) bool {
	tpl := st.GetLeaderPodTemplateSpec()
	labels := map[string]string{}
	if tpl != nil {
		maps.Copy(labels, tpl.Labels)
	}
	lws := scaletarget.IsLeaderWorkerSet(st)
	_, _, dset := scaletarget.DisaggregatedSetRole(st)
	if lws {
		maps.Copy(labels, scaletarget.GroupPodLabels(st))
		labels[lwsv1.WorkerIndexLabelKey] = "0"
	}
	for _, sel := range sels {
		match := true
		for k, v := range sel {
			if !lws && k == appsv1.DefaultDeploymentUniqueLabelKey {
				continue
			}
			if dset && k == lwsv1.SetNameLabelKey {
				continue
			}
			if labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// leaderPods yields the live Pods that stand for a replica: every Pod of a
// Deployment, and the leader (worker index 0) of each LWS group, through which
// the router reaches the group.
func leaderPods(st scaletarget.ScaleTargetAccessor, pods []corev1.Pod) func(func(*corev1.Pod) bool) {
	multiPod := st.GetGroupSize() > 1
	return func(yield func(*corev1.Pod) bool) {
		for i := range pods {
			p := &pods[i]
			if p.DeletionTimestamp != nil {
				continue
			}
			if multiPod && p.Labels[lwsv1.WorkerIndexLabelKey] != "0" {
				continue
			}
			if !yield(p) {
				return
			}
		}
	}
}

// markHeld marks a variant no pool selects, counting its replicas the way a
// selected variant's unselected ones are counted: live leaders that are Ready
// or starting, stuck ones left to StuckReplicas.
func markHeld(m *domain.VariantMetadata, st scaletarget.ScaleTargetAccessor, pods []corev1.Pod) {
	m.HeldUnrouted = true
	for p := range leaderPods(st, pods) {
		countUnrouted(m, p)
	}
}

// markUnroutedPods counts the live Pods of a selected variant that no pool
// selects. On a LeaderWorkerSet a group is judged and counted by its leader,
// and its live workers are named too, uncounted: the collector names a
// group's row for its leader, but for its lowest worker when the leader did
// not report, and that row must go as well.
func markUnroutedPods(m *domain.VariantMetadata, st scaletarget.ScaleTargetAccessor, pods []corev1.Pod, sels []map[string]string) {
	var groups map[string]bool
	for p := range leaderPods(st, pods) {
		if poolutil.AnySelects(sels, p.Labels) {
			continue
		}
		countUnrouted(m, p)
		if st.GetGroupSize() > 1 {
			if groups == nil {
				groups = map[string]bool{}
			}
			groups[routingGroupKey(p)] = true
		}
	}
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || p.Labels[lwsv1.WorkerIndexLabelKey] == "0" || !groups[routingGroupKey(p)] {
			continue
		}
		m.UnroutedPods = append(m.UnroutedPods, p.Name)
	}
}

// routingGroupKey is the LeaderWorkerSet group of p: its set name and group
// index. The set name tells apart a DisaggregatedSet role's revisions, each a
// child LeaderWorkerSet with its own group 0.
func routingGroupKey(p *corev1.Pod) string {
	return p.Labels[lwsv1.SetNameLabelKey] + "/" + p.Labels[lwsv1.GroupIndexLabelKey]
}

func countUnrouted(m *domain.VariantMetadata, p *corev1.Pod) {
	m.UnroutedPods = append(m.UnroutedPods, p.Name)
	switch {
	case PodReady(p):
		m.UnroutedReady++
		m.UnroutedReplicas++
	case podStarting(p):
		m.UnroutedReplicas++
	}
}

// PoolListErrorReason names a failed InferencePool listing for the reason label
// of llmsm_metrics_collection_errors_total, with the Pod listing's vocabulary.
func PoolListErrorReason(err error) string {
	switch {
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return "forbidden"
	case apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsTooManyRequests(err):
		return "unavailable"
	default:
		return "other"
	}
}
