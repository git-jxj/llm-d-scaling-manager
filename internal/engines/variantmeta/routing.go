package variantmeta

import (
	"context"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
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
	// Routed is true when a pool selects at least one variant of the model.
	Routed bool
	// Known is false when the pools, a scale target or a variant's Pods could
	// not be read, so whether the model is routed was not established.
	Known bool
}

// Route marks, in place, the replicas of each variant that no InferencePool
// selects (docs/proposals/pool-selected-supply.md), and reports per model what
// it found.
//
// A variant is selected when a pool selects its pod template, with the labels
// its controller adds at creation (an LWS leader's set name and worker index;
// a Deployment's pod-template-hash matches any value), or any of its live
// Pods. A model counts as routed when some variant is selected. For a routed
// model:
//
//   - a variant that is not selected is HeldUnrouted: the router sends it
//     nothing, whatever its size, so it is held rather than sized;
//   - in a selected variant, a live Pod whose own labels no pool selects (a
//     relabelled Pod) is counted in UnroutedReplicas and named in UnroutedPods.
//
// RoutingKnown is set on every variant whose verdict was established,
// including "nothing to exclude". It stays false when a read failed: the pools
// (counted, llmsm_metrics_collection_errors_total{query_type="pool_list"}), the
// variant's scale target, or its Pods where they decide the verdict. The
// caller decides what an unknown verdict means (the steady-state engine keeps
// the last known one for a bounded time); Route never turns a failed read into
// a hold or a release.
func Route(
	ctx context.Context,
	metas []domain.VariantMetadata,
	vas []llmdvariant.Variant,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	k8sClient client.Client,
	pools PoolLister,
) []ModelRouting {
	if pools == nil || len(metas) == 0 {
		return nil
	}
	logger := ctrl.LoggerFrom(ctx)

	vaByKey := make(map[string]*llmdvariant.Variant, len(vas))
	for i := range vas {
		vaByKey[vas[i].Namespace+"/"+vas[i].Name] = &vas[i]
	}

	// Group by (namespace, model): routing is decided per model.
	type group struct {
		namespace, model string
		idx              []int
	}
	var groups []*group
	byKey := map[string]*group{}
	for i := range metas {
		k := metas[i].Namespace + "/" + metas[i].ModelID
		g, ok := byKey[k]
		if !ok {
			g = &group{namespace: metas[i].Namespace, model: metas[i].ModelID}
			byKey[k] = g
			groups = append(groups, g)
		}
		g.idx = append(g.idx, i)
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
			reason := poolListErrorReason(err)
			metrics.IncMetricsCollectionErrors(constants.QueryTypePoolList, reason)
			logger.V(logging.DEFAULT).Info(
				"InferencePool listing failed; this cycle's routing verdict is unknown",
				"namespace", g.namespace, "model", g.model, "reason", reason, "error", err.Error())
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
			target   scaletarget.ScaleTargetAccessor
			pods     []corev1.Pod
			podsRead bool
			selected bool
			unknown  bool // whether it is selected could not be established
		}
		vs := make([]verdict, len(g.idx))
		for j, i := range g.idx {
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
		anyUnknown := false
		for _, v := range vs {
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
			case !v.podsRead:
				// Selected, but which of its Pods the pool reaches is unknown.
				res.Known = false
			default:
				markUnroutedPods(&metas[i], v.target, v.pods, sels)
				metas[i].RoutingKnown = true
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
		ctrl.LoggerFrom(ctx).V(logging.DEFAULT).Info(
			"Could not list a variant's pods to check which the router can reach",
			"namespace", namespace, "variant", variant, "error", err.Error())
		return nil, false
	}
	return items, true
}

// templateSelected reports whether a pool selects the target's pod template,
// with the labels its controller adds when it creates a Pod: an LWS leader
// carries the set name and worker index 0, and a Deployment's
// pod-template-hash is known only per ReplicaSet, so a selector on it is taken
// to match.
func templateSelected(st scaletarget.ScaleTargetAccessor, sels []map[string]string) bool {
	tpl := st.GetLeaderPodTemplateSpec()
	labels := map[string]string{}
	if tpl != nil {
		maps.Copy(labels, tpl.Labels)
	}
	lws := scaletarget.IsLeaderWorkerSet(st)
	if lws {
		labels[lwsv1.SetNameLabelKey] = st.GetName()
		labels[lwsv1.WorkerIndexLabelKey] = "0"
	}
	for _, sel := range sels {
		match := true
		for k, v := range sel {
			if !lws && k == appsv1.DefaultDeploymentUniqueLabelKey {
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
// selects.
func markUnroutedPods(m *domain.VariantMetadata, st scaletarget.ScaleTargetAccessor, pods []corev1.Pod, sels []map[string]string) {
	for p := range leaderPods(st, pods) {
		if poolutil.AnySelects(sels, p.Labels) {
			continue
		}
		countUnrouted(m, p)
	}
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

// poolListErrorReason names a failed InferencePool listing for the reason label
// of llmsm_metrics_collection_errors_total, with the Pod listing's vocabulary.
func poolListErrorReason(err error) string {
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
