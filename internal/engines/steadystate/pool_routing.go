package steadystate

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	poolutil "github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/pool"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// routingVerdictTTL is how long a variant's last routing verdict is kept when
// this cycle could not establish one (the pools, its scale target or its Pods
// could not be read). Within it a held variant stays held and its unselected
// Pods stay excluded, so one failed read does not release a hold and let the
// optimizer size a variant the router cannot reach. Past it the verdict lapses
// and the variant is counted as before the feature -- every replica supply,
// nothing held -- the same answer as for pools that cannot be read at all.
// Five minutes is ten cycles at the default 30 s interval.
const routingVerdictTTL = 5 * time.Minute

// heldEventEvery is how often the HeldNotInInferencePool Event is repeated
// while a variant stays held. It is also recorded on every change into the
// held state. The repeat keeps it visible past an Event's one-hour expiry.
const heldEventEvery = 30 * time.Minute

// routingState is what the last cycle that could read the pools established
// for one variant.
type routingState struct {
	// model is the variant's namespace/model key, for pruning.
	model    string
	held     bool
	unrouted int
	ready    int
	pods     []string
	// at is when the verdict was established; a verdict reused after a failed
	// read keeps its original time, so it lapses routingVerdictTTL after the
	// last good read.
	at time.Time
	// eventAt is when the held Event was last recorded.
	eventAt time.Time
	// lapsed is set once an unknown verdict has outlived routingVerdictTTL and
	// been logged, so the lapse is logged once.
	lapsed bool
}

// poolRead is one namespace's InferencePool listing for the current cycle.
type poolRead struct {
	selectors []map[string]string
	err       error
}

// poolLister lists InferencePool selectors past the manager's cache, in the
// group the controller is configured for (POOL_GROUP, as cmd/main.go reads it),
// once per namespace per cycle: every model of a namespace reads the same
// result, and resetPoolReads starts the next cycle afresh.
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
		e.routingMu.Lock()
		if r, ok := e.poolReads[namespace]; ok {
			e.routingMu.Unlock()
			return r.selectors, r.err
		}
		e.routingMu.Unlock()
		sels, err := poolutil.ListSelectors(ctx, reader, group, namespace)
		e.routingMu.Lock()
		if e.poolReads == nil {
			e.poolReads = map[string]poolRead{}
		}
		e.poolReads[namespace] = poolRead{selectors: sels, err: err}
		e.routingMu.Unlock()
		return sels, err
	}
}

// resetPoolReads forgets the cycle's InferencePool listings.
func (e *Engine) resetPoolReads() {
	e.routingMu.Lock()
	defer e.routingMu.Unlock()
	e.poolReads = nil
}

// poolSelectedSupply reports whether supply counts only what the pools select
// (LLMSM_POOL_SELECTED_SUPPLY). An engine built without a configuration -- a
// test's -- counts as before.
func (e *Engine) poolSelectedSupply() bool {
	return e.Config != nil && e.Config.PoolSelectedSupplyEnabled()
}

// routeModel establishes this cycle's routing verdict for one model's variants
// and returns the rows the analyzers may read. Off (LLMSM_POOL_SELECTED_SUPPLY
// false), it clears whatever an earlier cycle published and changes nothing.
func (e *Engine) routeModel(
	ctx context.Context,
	modelID, namespace string,
	metas []domain.VariantMetadata,
	modelVAs []llmdvariant.Variant,
	variants map[string]*llmdvariant.Variant,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	rows []domain.ReplicaMetrics,
	k8sClient client.Client,
	now time.Time,
) []domain.ReplicaMetrics {
	if !e.poolSelectedSupply() {
		e.forgetRouting(utils.GetNamespacedKey(namespace, modelID), namespace, modelID)
		return rows
	}
	results := variantmeta.Route(ctx, metas, modelVAs, scaleTargets, k8sClient, e.poolLister())
	e.resolveRouting(ctx, modelID, namespace, metas, results, variants, now)
	return dropUnroutedRows(rows, metas)
}

// resolveRouting turns Route's findings into the verdict this cycle uses:
// a known verdict is recorded; an unknown one reuses the last known verdict
// for routingVerdictTTL and lapses to "counted as before" after it. It then
// logs each change, keeps the held Event, publishes llmsm_unrouted_replicas
// and the held set scale from zero reads.
func (e *Engine) resolveRouting(
	ctx context.Context,
	modelID, namespace string,
	metas []domain.VariantMetadata,
	results []variantmeta.ModelRouting,
	variants map[string]*llmdvariant.Variant,
	now time.Time,
) {
	logger := ctrl.LoggerFrom(ctx)
	modelKey := utils.GetNamespacedKey(namespace, modelID)

	e.routingMu.Lock()
	defer e.routingMu.Unlock()
	if e.routing == nil {
		e.routing = map[string]routingState{}
	}
	if e.modelUnselected == nil {
		e.modelUnselected = map[string]bool{}
	}

	// A namespace with pools in which none selects this model: safe (counted as
	// before) but the router may have no endpoint for it, so say so once.
	for _, r := range results {
		unselected := r.Known && r.Pools > 0 && !r.Routed
		key := utils.GetNamespacedKey(r.Namespace, r.ModelID)
		if unselected && !e.modelUnselected[key] {
			logger.V(logging.DEFAULT).Info("InferencePools exist in the namespace but none selects any variant of this model; its supply is counted as before",
				"namespace", r.Namespace, "model", r.ModelID, "pools", r.Pools,
				"hint", "check the pools' selectors against the variants' pod labels")
		}
		if unselected {
			e.modelUnselected[key] = true
		} else if r.Known {
			delete(e.modelUnselected, key)
		}
	}

	var routed []string
	for _, m := range metas {
		if !m.HeldUnrouted {
			routed = append(routed, m.VariantName)
		}
	}
	slices.Sort(routed)

	counts := map[string]int{}
	var held []string
	for i := range metas {
		m := &metas[i]
		key := utils.GetNamespacedKey(m.Namespace, m.VariantName)
		prev, seen := e.routing[key]
		if !m.RoutingKnown {
			switch {
			case seen && now.Sub(prev.at) <= routingVerdictTTL:
				m.HeldUnrouted, m.UnroutedReplicas, m.UnroutedReady = prev.held, prev.unrouted, prev.ready
				m.UnroutedPods = slices.Clone(prev.pods)
				logger.V(logging.DEBUG).Info("routing verdict unknown this cycle; keeping the last one",
					"variant", m.VariantName, "namespace", m.Namespace, "held", prev.held,
					"unrouted", prev.unrouted, "age", now.Sub(prev.at).String())
			case seen && !prev.lapsed && (prev.held || prev.unrouted > 0):
				logger.V(logging.DEFAULT).Info("routing verdict unknown for longer than the hold keeps it; every replica is counted as before",
					"variant", m.VariantName, "namespace", m.Namespace, "model", modelID,
					"lastKnown", prev.at, "keptFor", routingVerdictTTL.String())
				prev.lapsed = true
				e.routing[key] = prev
			}
		} else {
			st := routingState{model: modelKey, held: m.HeldUnrouted, unrouted: m.UnroutedReplicas,
				ready: m.UnroutedReady, pods: slices.Clone(m.UnroutedPods), at: now, eventAt: prev.eventAt}
			switch {
			case st.held && (!seen || !prev.held):
				logger.V(logging.DEFAULT).Info("variant held: no InferencePool selects it while other variants of its model are routed",
					"variant", m.VariantName, "namespace", m.Namespace, "model", modelID,
					"replicas", m.CurrentReplicas, "routedVariants", routed,
					"release", "add labels to its pod template that an InferencePool in the namespace selects, or create a pool that selects it")
			case !st.held && seen && prev.held:
				logger.V(logging.DEFAULT).Info("variant released: an InferencePool now selects it, or its model is no longer routed",
					"variant", m.VariantName, "namespace", m.Namespace, "model", modelID)
			}
			if !st.held && st.unrouted != prev.unrouted {
				logger.V(logging.DEFAULT).Info("replicas no InferencePool selects are not counted as supply",
					"variant", m.VariantName, "namespace", m.Namespace, "model", modelID,
					"unrouted", m.UnroutedReplicas, "unroutedReady", m.UnroutedReady, "pods", m.UnroutedPods)
			}
			if st.held && (!prev.held || st.eventAt.IsZero() || st.at.Sub(prev.eventAt) >= heldEventEvery) {
				// Straight to the recorder, not recordEvent: that allows one
				// Event per variant per cycle, and this one is recorded before
				// the decisions, whose own Event it must not displace.
				if va := variants[key]; va != nil && e.Recorder != nil {
					e.Recorder.Event(llmdvariant.EventTarget(va), corev1.EventTypeWarning,
						constants.K8SEventHeldNotInPool, heldMessage(*m, modelID, routed))
				}
				st.eventAt = st.at
			}
			if !st.held {
				st.eventAt = time.Time{}
			}
			e.routing[key] = st
		}
		if m.HeldUnrouted || m.UnroutedReplicas > 0 {
			counts[m.VariantName] = m.UnroutedReplicas
		}
		if m.HeldUnrouted {
			held = append(held, m.VariantName)
		}
	}
	metrics.SetUnroutedReplicas(namespace, modelID, counts)
	decision.DefaultHeld.Publish(namespace, modelID, held, now)
}

// forgetRouting drops everything published for one model: its verdicts, its
// gauge series and its held set. For a model gone from the cycle, and for every
// model while the feature is off.
func (e *Engine) forgetRouting(modelKey, namespace, modelID string) {
	e.routingMu.Lock()
	for key, st := range e.routing {
		if st.model == modelKey {
			delete(e.routing, key)
		}
	}
	delete(e.modelUnselected, modelKey)
	e.routingMu.Unlock()
	metrics.SetUnroutedReplicas(namespace, modelID, nil)
	decision.DefaultHeld.Publish(namespace, modelID, nil, time.Time{})
}

// retainRouting forgets the routing of every model not in activeKeys
// (namespace/model keys), alongside the other per-model series.
func (e *Engine) retainRouting(activeKeys map[string]bool) {
	e.routingMu.Lock()
	gone := map[string]bool{}
	for _, st := range e.routing {
		if !activeKeys[st.model] {
			gone[st.model] = true
		}
	}
	e.routingMu.Unlock()
	for key := range gone {
		namespace, modelID, _ := strings.Cut(key, "/")
		e.forgetRouting(key, namespace, modelID)
	}
}

// dropUnroutedRows removes the metric rows of replicas the router cannot
// reach: every row of a held variant, and the rows of Pods no pool selects.
// Their traffic, if any, does not come through the router, and their capacity
// is not supply for the traffic that does.
//
// A warm-pool bridge lent to a held variant goes with the variant's rows. It
// does not arise in practice: the pool lends to a variant short of replicas,
// and a held variant is never sized, so it is never short.
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

// heldMessage is the Event text on a held variant: that it is held, why, and
// how to release it.
func heldMessage(m domain.VariantMetadata, modelID string, routed []string) string {
	return fmt.Sprintf("%s is held at %d replica(s) and not counted as supply: no InferencePool in %s "+
		"selects it, while other variants of model %s are routed by one (%s), so the router "+
		"sends it no traffic and the scaling manager neither scales it nor counts it. To release it, add "+
		"labels to its pod template that an InferencePool selects, or create an InferencePool that selects it. "+
		"LLMSM_POOL_SELECTED_SUPPLY=false turns this off for every model.",
		m.VariantName, m.CurrentReplicas, m.Namespace, modelID, strings.Join(routed, ", "))
}
