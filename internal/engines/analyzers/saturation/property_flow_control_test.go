package saturation

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Property cases for a fleet under flow control (issue #51): the requests
// wait at the router, the engines' own queues read empty, and so neither k2
// nor the mu written beside it is ever learned.
//
// k2 not being learned is not what matters there. Capacity is min(k1, k2),
// and a replica that fills past k1 (0.8 of its KV) before it queues has a k2
// above k1, which cannot move capacity: known-truth/k2/above-k1-leaves-
// capacity holds that. What a P1-obs reading also writes is the replica's
// measured mu, and mu prices the throughput floor. With none, the floor
// prices from the mu the ITL line derives at pricingK (whatif.DeriveMuAtCap,
// SaturatedThroughputDerived). The floor cases ask whether that fallback
// orders what the learned mu orders, within the margin the two prices differ
// by on purpose, and never fewer replicas than the fleet needs.

func flowControlCases() []propCase {
	return []propCase{
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-decode", family: familyDirect, tuning: tunedDefault, check: checkFlowControlFloor(fcDecode, nil)},
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-pd-decode", family: familyDirect, tuning: tunedDefault, check: checkFlowControlFloor(fcPD, nil)},
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-aggregated", family: familyDirect, tuning: tunedDefault, check: checkFlowControlFloor(fcAggregated, nil)},
		{class: classKnownTruth, learner: "floor", name: "flow-control-unlearned-mu-dp8", family: familyDirect, tuning: tunedDefault, check: checkFlowControlFloor(fcDP8, nil)},
		{class: classKnownTruth, learner: "k2", name: "above-k1-leaves-capacity", family: familyDirect, tuning: tunedDefault, check: checkK2AboveK1Capacity},
	}
}

// fcShape is the fleet a flow-control case runs: two pods of variant v,
// with a prefill variant beside them on P/D.
type fcShape struct {
	name string
	// role is v's role; "" is aggregated.
	role string
	// pd adds a prefill variant p, so the router's queue is charged to it.
	pd bool
	// ranks is the engine ranks a pod runs (data parallel).
	ranks int
}

var (
	fcDecode     = fcShape{name: "decode", role: domain.RoleDecode, ranks: 1}
	fcPD         = fcShape{name: "pd-decode", role: domain.RoleDecode, pd: true, ranks: 1}
	fcAggregated = fcShape{name: "aggregated", role: "", ranks: 1}
	fcDP8        = fcShape{name: "dp8", role: domain.RoleDecode, ranks: 8}
)

// The generator. Every engine rank follows the true ITL line (propITL) and
// holds requests of 1000 in / 500 out, 1250 tokens of KV each; its batch cap
// (512) never binds. At a KV fraction k a rank runs k x KV / 1250 sequences
// and completes them at that over ITL(k) x 500 a second (fcRate). The fleet
// saturates at kTrue, 0.85-0.95 of its KV: above k1 (0.8), so a k2 learned
// there cannot move capacity.
const (
	fcPods      = 2
	fcKVReq     = 1250.0
	fcOutput    = 500.0
	fcMaxSeqs   = 512
	fcRouterQ   = 40 // the router's queue on the first flow-controlled cycle
	fcWarmup    = 16 // cycles of the ramp the line is fitted on
	fcLearnFor  = 20 // cycles the learned run's engines queue
	fcSaturated = 16 // cycles under flow control, in both runs
	// fcKPrice is the fill the derived mu is priced at, and the fill the
	// autoscaler targets: kvCacheThreshold x scaleUpThreshold (pricingK).
	fcKPrice = 0.8 * floorTarget
	// fcRateNoise is the relative noise on what a replica completes: a
	// reading repeated exactly is not a new sample (exactRepeatLocked).
	fcRateNoise = 0.01
)

// fcRate is one pod's completions a second at KV fraction k.
func fcRate(k float64, ranks int) float64 {
	return float64(ranks) * k * float64(propKv) / fcKVReq / propITL(k) / fcOutput
}

// fcRow is one pod of shape s at KV fraction k with queue waiting on its
// engines, completing rate.
func fcRow(s fcShape, pod string, k float64, queue int, rate float64, ready time.Time) domain.ReplicaMetrics {
	kv := propKv * int64(s.ranks)
	rm := makeReplicaMetrics(pod, "v", int64(k*float64(kv)), kv, queue, 1000, fcOutput)
	rm.KvUsageInstant, rm.KvUsageInstantKnown = rm.KvCacheUsage, true
	rm.KvCacheUsageKnown = true
	rm.RequestRate, rm.RequestRateKnown = rate, true
	rm.GenerationTokenRate, rm.GenerationTokenRateKnown = rate*fcOutput, true
	rm.PrefillComputedTokenRate = rate * 1000
	rm.AvgITL, rm.AvgITLKnown = propITL(k), true
	rm.ReadySince = ready
	if s.ranks > 1 {
		rm.EngineRanks = s.ranks
		rm.MaxRankQueueLength = queue / s.ranks
		rm.MaxRankKvCacheUsage = k
	}
	if s.pd {
		rm.CapacityQueueKnown, rm.CapacityQueueLength = true, queue
	}
	return rm
}

// fcStates is shape s's variant states.
func fcStates(s fcShape) []domain.VariantReplicaState {
	v := domain.VariantReplicaState{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: fcPods, GPUsPerReplica: s.ranks, Role: s.role}
	if !s.pd {
		return []domain.VariantReplicaState{v}
	}
	return []domain.VariantReplicaState{v,
		{VariantName: "p", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill}}
}

// fcOutcome is what one run priced on its last cycle.
type fcOutcome struct {
	// replicas is v's role's demand in replicas at the scale-up target, and
	// perReplica the per-replica capacity.
	replicas, perReplica float64
	// measuredMu is the measured mu on record, 0 when none.
	measuredMu float64
	// queue is the router's queue on the last cycle.
	queue float64
	// mu is the service rate v was priced on (domain.ServiceRateSignal) and
	// source where it came from.
	mu     float64
	source string
}

// runFlowControl runs shape s at kTrue: a ramp the ITL line is fitted on,
// then fcSaturated cycles of flow control with arrivals at rho times what
// the fleet serves: the excess waits at the router, from fcRouterQ up, and
// the engines' queues read empty. learned runs fcLearnFor cycles first at
// capacity with the queue on the engines instead (flow control off), so k2
// and mu are learned there. The served rates' noise is drawn from seed, the
// same in both runs. wrap, when set, edits each of v's rows before the
// analyzer reads it (a mutant).
func runFlowControl(s fcShape, kTrue, rho float64, seed uint64, learned bool, wrap func(*domain.ReplicaMetrics)) (fcOutcome, error) {
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "v", capacity.Record{AcceleratorName: "H200", GpuCount: s.ranks,
		EngineParams: keyParams(fcMaxSeqs)}, propStart)
	rig := newPropRig()
	rig.a = NewSaturationAnalyzer(store)
	rig.a.now = func() time.Time { return rig.clock }
	ready := rig.clock.Add(-time.Hour)
	states := fcStates(s)
	fleet := func(k [fcPods]float64, queue int, rate [fcPods]float64) []domain.ReplicaMetrics {
		rows := make([]domain.ReplicaMetrics, 0, fcPods+1)
		for p := range fcPods {
			rows = append(rows, fcRow(s, fmt.Sprintf("r%d", p), k[p], queue, rate[p], ready))
			if wrap != nil {
				wrap(&rows[p])
			}
		}
		if s.pd {
			pre := satRow("p0", 20_000, 0, 6)
			pre.VariantName, pre.ReadySince = "p", ready
			rows = append(rows, pre)
		}
		return rows
	}
	// The ramp: unsaturated, no queue anywhere, k from 0.20 to 0.625.
	for i := range fcWarmup {
		var k, rate [fcPods]float64
		lambda := 0.0
		for p := range fcPods {
			k[p] = 0.20 + 0.025*float64(i) + 0.05*float64(p)
			rate[p] = fcRate(k[p], s.ranks)
			lambda += rate[p]
		}
		if _, err := rig.analyze(fleet(k, 0, rate), states, lambda, propCycle); err != nil {
			return fcOutcome{}, err
		}
	}
	mu := fcRate(kTrue, s.ranks)
	full := [fcPods]float64{kTrue, kTrue}
	served := func(r *rand.Rand) [fcPods]float64 {
		var rate [fcPods]float64
		for p := range rate {
			rate[p] = mu * (1 + fcRateNoise*r.NormFloat64())
		}
		return rate
	}
	if learned {
		// Flow control off, the fleet at capacity: the queue stands on the
		// engines, and k2 and mu are learned.
		r := propRand(seed + 1<<32)
		for range fcLearnFor {
			if _, err := rig.analyze(fleet(full, 10*s.ranks, served(r)), states, fcPods*mu, propCycle); err != nil {
				return fcOutcome{}, err
			}
		}
	}
	// Flow control on.
	r := propRand(seed + 2<<32)
	lambda := rho * fcPods * mu
	queue := float64(fcRouterQ)
	var res *domain.AnalyzerResult
	for i := range fcSaturated {
		if i > 0 {
			queue += (lambda - fcPods*mu) * propCycle.Seconds()
		}
		q := int64(queue)
		rig.input = func(in *domain.AnalyzerInput) {
			in.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: q}
		}
		var err error
		if res, err = rig.analyze(fleet(full, 0, served(r)), states, lambda, propCycle); err != nil {
			return fcOutcome{}, err
		}
	}
	out := fcOutcome{measuredMu: rig.measuredMu(), queue: queue, replicas: math.NaN()}
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == "v" {
			out.perReplica = vc.PerReplicaCapacity
			if vc.ServiceRate != nil {
				out.mu, out.source = vc.ServiceRate.Rate, vc.ServiceRate.RateSource
			}
		}
	}
	demand := res.TotalDemand
	if s.pd {
		demand = res.RoleDemand[domain.RoleDecode]
	}
	if out.perReplica > 0 {
		out.replicas = demand / (floorTarget * out.perReplica)
	}
	return out, nil
}

// fcTruth is what shape s needs at kTrue, in replicas: the arrivals and the
// router's queue drained over the floor's drain time, with every replica at
// the fill the autoscaler targets (fcKPrice), where the true line has it
// complete fcRate(fcKPrice). On P/D the router's queue is prefill's to drain
// first, and decode is charged the arrivals alone.
func fcTruth(s fcShape, rho, kTrue, queue float64) float64 {
	if s.pd {
		queue = 0
	}
	return (rho*fcPods*fcRate(kTrue, s.ranks) + queue/floorDrainT) / fcRate(fcKPrice, s.ranks)
}

// fcLearnedOver is how much more the learned mu orders than the true line's
// mu at the target fill, by design. The learned mu is read at saturation
// (kTrue) and the engine divides it by the scale-up threshold; the derived
// one is priced at the target fill and not divided again (replicaCost). The
// two meet only where throughput is linear in fill. It is concave, so the
// learned mu orders mu(fcKPrice) / (target x mu(kTrue)) times as much: 1.10
// to 1.12 on the generator's line. That is the margin the never-learned order
// may sit under the learned one by.
func fcLearnedOver(kTrue float64) float64 {
	return fcRate(fcKPrice, 1) / (floorTarget * fcRate(kTrue, 1))
}

// checkFlowControlFloor: shape s overloaded under flow control at kTrue
// (0.85-0.95 of KV) and rho (1.1-1.5 times what it serves), priced on its
// last cycle with the mu it learned with flow control off first (learned)
// and with none (never learned). Never learned:
//
//   - orders at least the truth (fcTruth), less propTruthTol: the side a
//     fallback errs to is more replicas;
//   - orders at least the learned order over fcLearnedOver, less
//     propTruthTol: under it by no more than the learned mu over-orders by
//     design;
//   - orders at most the larger of the learned order and the truth, plus
//     propTruthTol: more is a cost the learned mu would not have paid.
//
// wrap, when set, edits each of v's rows in the never-learned run: the
// fallback under test, put back wrong (a mutant).
func checkFlowControlFloor(s fcShape, wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		kTrue := 0.85 + 0.1*r.Float64()
		rho := 1.1 + 0.4*r.Float64()
		lr, err := runFlowControl(s, kTrue, rho, seed, true, nil)
		if err != nil {
			return err
		}
		if !(lr.measuredMu > 0) || lr.source != domain.ServiceRateSourceMeasured {
			return violation(kindNotLearned, "%s: the learned run priced on %q mu %.2f (measured %.2f), not its measured mu", s.name, lr.source, lr.mu, lr.measuredMu)
		}
		nl, err := runFlowControl(s, kTrue, rho, seed, false, wrap)
		if err != nil {
			return err
		}
		truth := fcTruth(s, rho, kTrue, nl.queue)
		over := fcLearnedOver(kTrue)
		msg := fmt.Sprintf("%s at %.3f of KV, rho %.2f, router queue %.0f: never learned ordered %.3f replicas (%s mu %.2f), learned %.3f (mu %.2f), truth %.3f (mu %.2f at the target fill)",
			s.name, kTrue, rho, nl.queue, nl.replicas, nl.source, nl.mu, lr.replicas, lr.mu, truth, fcRate(fcKPrice, s.ranks))
		if math.IsNaN(nl.replicas) || math.IsNaN(lr.replicas) {
			return violation(kindNotLearned, "%s: no order", msg)
		}
		if nl.replicas < truth*(1-propTruthTol) {
			return violation(kindWrongSide, "%s: under the truth", msg)
		}
		if nl.replicas < lr.replicas/over*(1-propTruthTol) {
			return violation(kindWrongSide, "%s: under the learned order by more than its %.3fx", msg, over)
		}
		return holds(nl.replicas <= max(lr.replicas, truth)*(1+propTruthTol), kindOverPriced, "%s: over both", msg)
	}
}

// checkK2AboveK1Capacity: each flow-control shape saturated at 0.85-0.95 of
// its KV, above k1, priced with k2 learned there (flow control off) and with
// none (flow control on): the per-replica capacity is the same, k1, within
// 1e-9. A k2 above k1 cannot move capacity, so its absence costs none.
func checkK2AboveK1Capacity(seed uint64) error {
	r := propRand(seed)
	kTrue := 0.85 + 0.1*r.Float64()
	for _, s := range []fcShape{fcDecode, fcPD, fcAggregated, fcDP8} {
		lr, err := runFlowControl(s, kTrue, 1.2, seed, true, nil)
		if err != nil {
			return err
		}
		nl, err := runFlowControl(s, kTrue, 1.2, seed, false, nil)
		if err != nil {
			return err
		}
		k1 := float64(propKv*int64(s.ranks)) * 0.8
		if !(lr.perReplica > 0 && nl.perReplica > 0) {
			return violation(kindNotLearned, "%s: no per-replica capacity (%.0f learned, %.0f not)", s.name, lr.perReplica, nl.perReplica)
		}
		if relErr(nl.perReplica, lr.perReplica) > 1e-9 || relErr(lr.perReplica, k1) > 1e-3 {
			return violation(kindMoved, "%s saturated at %.3f of KV: per-replica capacity %.0f with k2 learned, %.0f without (k1 %.0f)",
				s.name, kTrue, lr.perReplica, nl.perReplica, k1)
		}
	}
	return nil
}
