package variantmeta

import (
	"context"

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

// Route marks, in place, the replicas of each variant that no InferencePool
// selects (docs/proposals/pool-selected-supply.md).
//
// A model counts as routed in a namespace when some pool there selects the pod
// template of at least one of its variants. For a routed model:
//
//   - a variant whose template no pool selects is HeldUnrouted: the router
//     sends it nothing, whatever its size, so it is held rather than sized;
//   - in a variant whose template is selected, a live Pod whose own labels are
//     not (a relabelled Pod) is counted in UnroutedReplicas and named in
//     UnroutedPods.
//
// Everything else is left as Discover set it -- which is supply counted as
// before -- in three cases: the namespace has no pool (or the kind is not
// served), no pool selects any variant of the model (it is served some other
// way), or the pools could not be read. The last is counted
// (llmsm_metrics_collection_errors_total, query_type=pool_list) and logged:
// a refused read is not an absent pool, and treating it as one would hold
// every variant, while treating it as "nothing selected" would zero supply.
func Route(
	ctx context.Context,
	metas []domain.VariantMetadata,
	vas []llmdvariant.Variant,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	k8sClient client.Client,
	pools PoolLister,
) {
	if pools == nil || len(metas) == 0 {
		return
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

	selectorsByNS := map[string][]map[string]string{}
	readByNS := map[string]bool{}
	for _, g := range groups {
		if _, done := readByNS[g.namespace]; !done {
			sels, err := pools(ctx, g.namespace)
			switch {
			case err == nil:
				readByNS[g.namespace] = true
				selectorsByNS[g.namespace] = sels
			case meta.IsNoMatchError(err):
				// The kind is not served, so no pool can exist: no router,
				// nothing to check, and nothing failed.
				readByNS[g.namespace] = true
			default:
				readByNS[g.namespace] = false
				reason := poolListErrorReason(err)
				metrics.IncMetricsCollectionErrors(constants.QueryTypePoolList, reason)
				logger.V(logging.DEFAULT).Info(
					"InferencePool listing failed; supply is counted without checking which replicas the router can reach",
					"namespace", g.namespace, "reason", reason, "error", err.Error())
			}
		}
		sels := selectorsByNS[g.namespace]
		if !readByNS[g.namespace] || len(sels) == 0 {
			continue
		}

		selected := make([]bool, len(g.idx))
		templates := make([]scaletarget.ScaleTargetAccessor, len(g.idx))
		routed := false
		for j, i := range g.idx {
			va := vaByKey[metas[i].Namespace+"/"+metas[i].VariantName]
			if va == nil {
				continue
			}
			st, ok := resolveScaleTarget(ctx, va, scaleTargets, k8sClient)
			if !ok {
				continue
			}
			templates[j] = st
			if tpl := st.GetLeaderPodTemplateSpec(); tpl != nil && poolutil.AnySelects(sels, tpl.Labels) {
				selected[j] = true
				routed = true
			}
		}
		if !routed {
			logger.V(logging.DEBUG).Info(
				"no InferencePool selects any variant of this model; supply is counted as before",
				"namespace", g.namespace, "model", g.model)
			continue
		}
		for j, i := range g.idx {
			st := templates[j]
			if st == nil {
				continue
			}
			if !selected[j] {
				metas[i].HeldUnrouted = true
				metas[i].UnroutedReplicas = metas[i].CurrentReplicas
				metas[i].UnroutedReady = metas[i].ReadyReplicas
				continue
			}
			markUnroutedPods(ctx, &metas[i], st, k8sClient, sels)
		}
	}
}

// markUnroutedPods counts the live Pods of a selected variant that no pool
// selects. A listing that fails leaves the variant as it was.
func markUnroutedPods(
	ctx context.Context,
	m *domain.VariantMetadata,
	st scaletarget.ScaleTargetAccessor,
	k8sClient client.Client,
	sels []map[string]string,
) {
	if k8sClient == nil {
		return
	}
	// The scale target's own Pods, by ownership and not by the template's
	// labels: a Pod relabelled out of the pool no longer carries them all, and
	// two variants of one model routinely share the rest.
	var items []corev1.Pod
	var err error
	if sel := scaletarget.LabelSelector(st); sel != nil {
		items, err = ListDeploymentPods(ctx, k8sClient, m.Namespace, st.GetName(), sel)
	} else {
		items, err = ListVariantPods(ctx, k8sClient, m.Namespace, st)
	}
	if err != nil {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info(
			"Could not list a variant's pods to check which the router can reach",
			"namespace", m.Namespace, "variant", m.VariantName, "error", err.Error())
		return
	}

	multiPod := st.GetGroupSize() > 1
	for i := range items {
		p := &items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		// One replica of a LeaderWorkerSet is one group, counted by its
		// leader; the router reaches the group through it.
		if multiPod && p.Labels[lwsv1.WorkerIndexLabelKey] != "0" {
			continue
		}
		if poolutil.AnySelects(sels, p.Labels) {
			continue
		}
		m.UnroutedPods = append(m.UnroutedPods, p.Name)
		switch {
		case PodReady(p):
			m.UnroutedReady++
			m.UnroutedReplicas++
		case podStarting(p):
			m.UnroutedReplicas++
		}
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
