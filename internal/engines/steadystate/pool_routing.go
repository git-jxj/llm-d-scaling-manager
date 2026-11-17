package steadystate

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	poolutil "github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/pool"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// routingState is what the last cycle reported for one variant, so a change is
// logged once rather than every cycle.
type routingState struct {
	held     bool
	unrouted int
}

// poolLister lists InferencePool selectors past the manager's cache, in the
// group the controller is configured for (POOL_GROUP, as cmd/main.go reads it).
//
// Uncached for the same reason the collector's Pod listing is: a refused read
// must come back as an error that can be told from "no pool", and an informer
// on a kind the controller may not list would never sync.
func (e *Engine) poolLister() variantmeta.PoolLister {
	gknn, err := poolutil.GetPoolGKNN(os.Getenv("POOL_GROUP"))
	if err != nil {
		return nil
	}
	var reader client.Reader = e.client
	if e.apiReader != nil {
		reader = e.apiReader
	}
	if reader == nil {
		return nil
	}
	group := gknn.Group
	return func(ctx context.Context, namespace string) ([]map[string]string, error) {
		return poolutil.ListSelectors(ctx, reader, group, namespace)
	}
}

// dropUnroutedRows removes the metric rows of replicas the router cannot
// reach: every row of a held variant, and the rows of Pods no pool selects.
// Their traffic, if any, does not come through the router, and their capacity
// is not supply for the traffic that does.
func dropUnroutedRows(rows []domain.ReplicaMetrics, metas []domain.VariantMetadata) []domain.ReplicaMetrics {
	held := map[string]bool{}
	pods := map[string]bool{}
	for _, m := range metas {
		if m.HeldUnrouted {
			held[m.VariantName] = true
		}
		for _, p := range m.UnroutedPods {
			pods[p] = true
		}
	}
	if len(held) == 0 && len(pods) == 0 {
		return rows
	}
	out := make([]domain.ReplicaMetrics, 0, len(rows))
	for _, r := range rows {
		if held[r.VariantName] || pods[r.PodName] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// applyRouting takes the replicas the router cannot reach out of one
// analyzer's per-variant capacity, after the scale-target clamp.
//
// A held variant contributes nothing and gets no per-replica capacity, which
// every optimizer reads as "do not size this variant": it stays at its current
// replica count. Sizing it would add replicas the router never reaches -- and
// if it is the cheapest variant, add them without end.
//
// For a selected variant, the Pods no pool selects are removed. Their rows were
// dropped before the analyzers ran, so a ReplicaCount taken from rows already
// excludes them; one taken from scale-target status (no rows: ObservedReplicas
// is 0) still counts the Ready ones, and they are subtracted. PendingReplicas
// is everything the target owns that did not report, which includes them, so
// it is capped at the routed replicas not already counted.
func applyRouting(vc *domain.VariantCapacity, m domain.VariantMetadata) {
	if m.HeldUnrouted {
		vc.ReplicaCount = 0
		vc.PendingReplicas = 0
		vc.StuckReplicas = 0
		vc.PendingAges = nil
		vc.PerReplicaCapacity = 0
		return
	}
	if m.UnroutedReplicas <= 0 {
		return
	}
	if vc.ObservedReplicas == 0 {
		vc.ReplicaCount = max(0, vc.ReplicaCount-m.UnroutedReady)
	}
	routed := max(0, m.CurrentReplicas-m.UnroutedReplicas)
	vc.ReplicaCount = min(vc.ReplicaCount, routed)
	vc.PendingReplicas = max(0, min(vc.PendingReplicas, routed-vc.ReplicaCount))
}

// reportRouting publishes llmsm_unrouted_replicas for the model, logs each
// change, and keeps a Warning Event on every held variant saying why it is held
// and how to release it.
func (e *Engine) reportRouting(
	ctx context.Context,
	modelID, namespace string,
	metas []domain.VariantMetadata,
	variants map[string]*llmdvariant.Variant,
) {
	logger := ctrl.LoggerFrom(ctx)
	counts := map[string]int{}
	var routedVariants []string
	for _, m := range metas {
		if !m.HeldUnrouted {
			routedVariants = append(routedVariants, m.VariantName)
		}
	}
	slices.Sort(routedVariants)

	e.routingMu.Lock()
	defer e.routingMu.Unlock()
	if e.routing == nil {
		e.routing = map[string]routingState{}
	}
	for _, m := range metas {
		key := utils.GetNamespacedKey(m.Namespace, m.VariantName)
		now := routingState{held: m.HeldUnrouted, unrouted: m.UnroutedReplicas}
		if now.held || now.unrouted > 0 {
			counts[m.VariantName] = m.UnroutedReplicas
		}
		prev, seen := e.routing[key]
		if now.held {
			msg := heldMessage(m, modelID, routedVariants)
			if !seen || !prev.held {
				logger.V(logging.DEFAULT).Info("variant held: no InferencePool selects its pod template",
					"variant", m.VariantName, "namespace", m.Namespace, "model", modelID,
					"replicas", m.CurrentReplicas, "routedVariants", routedVariants,
					"unhold", "add labels to its pod template that an InferencePool in the namespace selects, or create a pool that selects them")
			}
			if va := variants[key]; va != nil {
				e.recordEvent(va, corev1.EventTypeWarning, constants.K8SEventHeldNotInPool, msg)
			}
		} else if seen && prev.held {
			logger.V(logging.DEFAULT).Info("variant released: an InferencePool now selects its pod template",
				"variant", m.VariantName, "namespace", m.Namespace, "model", modelID)
		}
		if !now.held && now.unrouted != prev.unrouted {
			logger.V(logging.DEFAULT).Info("replicas no InferencePool selects are not counted as supply",
				"variant", m.VariantName, "namespace", m.Namespace, "model", modelID,
				"unrouted", m.UnroutedReplicas, "unroutedReady", m.UnroutedReady,
				"pods", m.UnroutedPods)
		}
		if now.held || now.unrouted > 0 || seen {
			e.routing[key] = now
		}
	}
	metrics.SetUnroutedReplicas(namespace, modelID, counts)
}

// heldMessage is the Event text on a held variant: that it is held, why, and
// how to release it.
func heldMessage(m domain.VariantMetadata, modelID string, routed []string) string {
	return fmt.Sprintf("%s is held at %d replica(s) and not counted as supply: no InferencePool in %s "+
		"selects its pod template, while other variants of model %s are routed by one (%s), so the router "+
		"sends it no traffic. To release it, add labels to its pod template that an InferencePool selects, "+
		"or create an InferencePool that selects it.",
		m.VariantName, m.CurrentReplicas, m.Namespace, modelID, strings.Join(routed, ", "))
}
