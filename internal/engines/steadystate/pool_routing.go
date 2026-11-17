package steadystate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
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

// routingState is what the last cycle that could establish it found for one
// running variant.
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

// modelRoutingState is the last known model-level verdict: the variants a pool
// selects and the held ones, parked ones included, for decision.DefaultHeld.
type modelRoutingState struct {
	selected, held []string
	at             time.Time
}

// poolRead is one namespace's InferencePool listing for the current cycle.
type poolRead struct {
	selectors []map[string]string
	err       error
}

// heldEvent is a held Event to record once routingMu is released.
type heldEvent struct {
	va      *llmdvariant.Variant
	message string
}

// poolLister lists InferencePool selectors past the manager's cache, in the
// group the controller is configured for (POOL_GROUP), once per namespace per
// cycle: every model of a namespace reads the same result, and resetPoolReads
// starts the next cycle afresh. A failed listing is counted once per namespace
// per cycle (llmsm_metrics_collection_errors_total{query_type="pool_list"}) and
// logged when the namespace's state changes, naming the permission.
//
// Uncached for the same reason the collector's Pod listing is: a refused read
// must come back as an error that can be told from "no pool", and an informer
// on a kind the controller may not list would never sync.
func (e *Engine) poolLister() variantmeta.PoolLister {
	group := ""
	if e.Config != nil {
		group = e.Config.PoolGroup()
	}
	gknn, err := poolutil.GetPoolGKNN(group)
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
	group = gknn.Group
	return func(ctx context.Context, namespace string) ([]map[string]string, error) {
		e.routingMu.Lock()
		if r, ok := e.poolReads[namespace]; ok {
			e.routingMu.Unlock()
			return r.selectors, r.err
		}
		e.routingMu.Unlock()
		sels, err := poolutil.ListSelectors(ctx, reader, group, namespace)
		failed := ""
		if err != nil && !meta.IsNoMatchError(err) {
			failed = variantmeta.PoolListErrorReason(err)
			metrics.IncMetricsCollectionErrors(constants.QueryTypePoolList, failed)
		}
		e.routingMu.Lock()
		if e.poolReads == nil {
			e.poolReads = map[string]poolRead{}
		}
		e.poolReads[namespace] = poolRead{selectors: sels, err: err}
		if e.poolFailed == nil {
			e.poolFailed = map[string]string{}
		}
		was := e.poolFailed[namespace]
		if failed == "" {
			delete(e.poolFailed, namespace)
		} else {
			e.poolFailed[namespace] = failed
		}
		e.routingMu.Unlock()
		switch {
		case failed != "" && failed != was:
			ctrl.LoggerFrom(ctx).V(logging.DEFAULT).Info(
				"InferencePools could not be listed; held variants stay held for a while, then every replica is counted as before",
				"namespace", namespace, "group", group, "reason", failed, "error", err.Error(),
				"needs", "list on inferencepools."+group+" in this namespace")
		case failed == "" && was != "":
			ctrl.LoggerFrom(ctx).V(logging.DEFAULT).Info("InferencePools can be listed again", "namespace", namespace)
		}
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

// parkedVariants is this cycle's variants at zero replicas, by namespace/model,
// with their scale targets, and whether they could be listed.
type parkedVariants struct {
	byModel map[string][]llmdvariant.Variant
	targets map[string]scaletarget.ScaleTargetAccessor
	known   bool
}

// readParked lists the variants at zero replicas, for routing: a model whose
// routed variant is parked is still routed.
func (e *Engine) readParked(ctx context.Context) parkedVariants {
	if !e.poolSelectedSupply() || e.Variants == nil {
		return parkedVariants{}
	}
	vas, targets, err := utils.InactiveVariants(ctx, e.client, e.Variants)
	if err != nil {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Could not list parked variants; routing verdicts this cycle are unknown where they would decide", "error", err.Error())
		return parkedVariants{}
	}
	out := parkedVariants{byModel: map[string][]llmdvariant.Variant{}, targets: targets, known: true}
	for _, va := range vas {
		key := utils.GetNamespacedKey(va.Namespace, va.Spec.ModelID)
		out.byModel[key] = append(out.byModel[key], va)
	}
	return out
}

// routeModel establishes this cycle's routing verdict for one model's running
// variants and returns the rows the analyzers may read. Off
// (LLMSM_POOL_SELECTED_SUPPLY false), it clears whatever an earlier cycle
// published and changes nothing.
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
	modelKey := utils.GetNamespacedKey(namespace, modelID)
	if !e.poolSelectedSupply() {
		e.forgetRouting(modelKey, namespace, modelID, false)
		return rows
	}
	parked := e.parked.byModel[modelKey]
	targets := scaleTargets
	if len(parked) > 0 {
		targets = make(map[string]scaletarget.ScaleTargetAccessor, len(scaleTargets)+len(parked))
		for k, v := range e.parked.targets {
			targets[k] = v
		}
		for k, v := range scaleTargets {
			targets[k] = v
		}
	}
	results := variantmeta.Route(ctx, metas, modelVAs, parked, targets, k8sClient, e.poolLister())
	if !e.parked.known {
		// The parked variants could not be listed: a model that looks not
		// routed may be routed through one of them, so nothing is known.
		for i := range results {
			if !results[i].Routed {
				results[i].Known = false
				for j := range metas {
					metas[j].RoutingKnown = false
				}
			}
		}
	}
	e.resolveRouting(ctx, modelID, namespace, metas, results, variants, now)
	return dropUnroutedRows(rows, metas)
}

// routeParkedModels publishes the routing of the models with no running
// variant, so scale from zero does not wake a held variant of a model that is
// entirely at zero. active holds the namespace/model keys this cycle analyzes.
func (e *Engine) routeParkedModels(ctx context.Context, active map[string]bool, now time.Time) {
	if !e.poolSelectedSupply() || !e.parked.known {
		return
	}
	for key, vas := range e.parked.byModel {
		if active[key] {
			continue
		}
		for _, r := range variantmeta.Route(ctx, nil, nil, vas, e.parked.targets, e.client, e.poolLister()) {
			if r.Known {
				e.publishModelRouting(r.Namespace, r.ModelID, r.Selected, r.Held, now)
			}
		}
	}
}

// publishModelRouting records and publishes a model's selected and held
// variants for scale from zero.
func (e *Engine) publishModelRouting(namespace, modelID string, selected, held []string, now time.Time) {
	key := utils.GetNamespacedKey(namespace, modelID)
	e.routingMu.Lock()
	if e.modelRouting == nil {
		e.modelRouting = map[string]modelRoutingState{}
	}
	if len(selected) == 0 && len(held) == 0 {
		delete(e.modelRouting, key)
	} else {
		e.modelRouting[key] = modelRoutingState{selected: selected, held: held, at: now}
	}
	e.routingMu.Unlock()
	decision.DefaultHeld.Publish(namespace, modelID, held, selected, now)
}

// resolveRouting turns Route's findings into the verdict this cycle uses:
// a known verdict is recorded; an unknown one reuses the last known verdict
// for routingVerdictTTL and lapses to "counted as before" after it. It then
// logs each change, records the held Event, publishes llmsm_unrouted_replicas
// and the selected and held variants scale from zero reads.
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
	var events []heldEvent

	e.routingMu.Lock()
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
	var heldRunning []string
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
				if va := variants[key]; va != nil {
					events = append(events, heldEvent{va: va, message: heldMessage(*m, modelID, routed)})
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
			heldRunning = append(heldRunning, m.VariantName)
		}
	}

	// The model-level verdict for scale from zero: this cycle's when it was
	// established, else the last one while it is fresh, with the running
	// variants' kept verdicts folded in.
	var selected, held []string
	known := len(results) > 0 && results[0].Known
	switch prevModel, ok := e.modelRouting[modelKey]; {
	case known:
		selected, held = results[0].Selected, results[0].Held
	case ok && now.Sub(prevModel.at) <= routingVerdictTTL:
		selected, held = prevModel.selected, prevModel.held
		for _, v := range heldRunning {
			if !slices.Contains(held, v) {
				held = append(held, v)
			}
		}
	}
	e.routingMu.Unlock()

	// Straight to the recorder, not recordEvent: that allows one Event per
	// variant per cycle, and this one is recorded before the decisions, whose
	// own Event it must not displace. Outside the lock: it is I/O.
	if e.Recorder != nil {
		for _, ev := range events {
			e.Recorder.Event(llmdvariant.EventTarget(ev.va), corev1.EventTypeWarning, constants.K8SEventHeldNotInPool, ev.message)
		}
	}
	metrics.SetUnroutedReplicas(namespace, modelID, counts)
	if known || len(selected)+len(held) > 0 {
		e.publishModelRouting(namespace, modelID, selected, held, now)
	} else if len(results) > 0 {
		// Unknown for longer than the hold keeps it: no routing, as before.
		e.publishModelRouting(namespace, modelID, nil, nil, now)
	}
}

// forgetRouting drops everything published for one model: its verdicts and
// its gauge series, and, unless keepPublished, its routing for scale from
// zero. For a model gone from the cycle, and for every model while the feature
// is off.
func (e *Engine) forgetRouting(modelKey, namespace, modelID string, keepPublished bool) {
	e.routingMu.Lock()
	for key, st := range e.routing {
		if st.model == modelKey {
			delete(e.routing, key)
		}
	}
	delete(e.modelUnselected, modelKey)
	if !keepPublished {
		delete(e.modelRouting, modelKey)
	}
	e.routingMu.Unlock()
	metrics.SetUnroutedReplicas(namespace, modelID, nil)
	if !keepPublished {
		decision.DefaultHeld.Publish(namespace, modelID, nil, nil, time.Time{})
	}
}

// retainRouting forgets the routing of every model not in activeKeys
// (namespace/model keys), alongside the other per-model series. A model whose
// variants are all parked keeps its published routing: routeParkedModels
// renews it, so scale from zero does not wake its held variant.
func (e *Engine) retainRouting(activeKeys map[string]bool) {
	e.routingMu.Lock()
	gone := map[string]bool{}
	for _, st := range e.routing {
		if !activeKeys[st.model] {
			gone[st.model] = true
		}
	}
	for key := range e.modelRouting {
		if !activeKeys[key] {
			gone[key] = true
		}
	}
	e.routingMu.Unlock()
	for key := range gone {
		namespace, modelID, _ := strings.Cut(key, "/")
		_, parked := e.parked.byModel[key]
		e.forgetRouting(key, namespace, modelID, parked)
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
