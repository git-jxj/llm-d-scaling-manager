package saturation

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/aggregation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/floor"
)

// SwitchDecodeOwesRouterBacklog is the replay switch for decode's share of
// the router queue on a P/D fleet (issue
// llm-d-incubation/llm-d-scaling-manager#51). Off, the floor charges the
// router's standing queue to prefill alone (applyThroughputFloor,
// queueOwner), so decode is sized for the arrivals only. That holds where
// prefill binds. Where decode binds, as under flow control with decode full,
// every queued request reaches decode as soon as prefill takes it, and decode
// sized for the arrivals drains the backlog only by its target-fill headroom:
// the property case known-truth/floor/flow-control-unlearned-mu-pd-decode
// ordered 0.45-0.85 of what the fleet needs.
//
// On, decode's floor is charged the queue at the rate prefill can hand it on
// (decodeRouterBacklogRate), over the floor's own drain horizon. Off from the
// constructor: only a replay turns it on, until a P/D run checks it.
const SwitchDecodeOwesRouterBacklog = "saturation-decode-owes-router-backlog"

// Why decode was charged the router's queue (decodeRouterBacklogRate).
const (
	// decodeOwedPrefillSpare: capped at what the prefill the analyzer
	// orders completes beyond the arrivals.
	decodeOwedPrefillSpare = "prefill-spare"
	// decodeOwedDecodeFull: prefill has no mu, and decode holds at least
	// its scale-up share of its ready supply, so decode is what binds.
	decodeOwedDecodeFull = "decode-full"
)

// decodeRouterBacklogRate is the rate, in requests a second, at which a P/D
// decode is owed the router's standing queue: what prefill can pass on to
// it beyond the arrivals, which the floor already charges every role. The
// floor then charges decode min(Q, rate x H), H its drain horizon, so the
// backlog rate is min(Q/H, rate). why names the case, and is empty where
// decode is owed nothing.
//
//   - Prefill has a floor term: rate is the prefill the analyzer orders, in
//     replicas, times prefill's mu, less lambda, and at least zero. The
//     prefill ordered is the larger of its ready replicas and its demand
//     (occupancy or floor, the larger) at the scale-up threshold of its
//     per-replica capacity, rounded up, as the engine orders whole replicas.
//     Where prefill binds that is about zero, and decode is sized for the
//     arrivals, as with the switch off. A prefill lower bound under-states
//     the rate, so decode is charged less, never more.
//   - Prefill has none: its spare is unknown. Decode is charged the whole
//     queue (an infinite rate) only while its resident KV is at least
//     scaleUp of its ready supply, the fill it scales out at. A full decode
//     is what holds the router's queue back; a decode below that fill takes
//     all prefill passes on, so prefill binds and decode is owed nothing.
//
// roleDemand is each role's occupancy demand and residency the queues'
// residency charge in it, so roleDemand - residency is decode's resident KV.
func decodeRouterBacklogRate(tf floor.Floor, variants []domain.VariantCapacity,
	roleDemand, residency map[string]float64, scaleUp float64) (rate float64, why string) {
	if !(tf.Lambda > 0) || math.IsInf(tf.Lambda, 1) || !(scaleUp > 0) {
		return 0, ""
	}
	if _, ok := tf.ByRole[domain.RoleDecode]; !ok {
		// Decode has no floor to charge.
		return 0, ""
	}
	if term, ok := tf.Terms[domain.RolePrefill]; ok && term.Mu > 0 && term.PerReplica > 0 {
		var ready int
		var perReplica float64
		for _, vc := range variants {
			if domain.CanonicalRole(vc.Role) != domain.RolePrefill {
				continue
			}
			ready += vc.ReplicaCount
			perReplica = max(perReplica, vc.PerReplicaCapacity)
		}
		ordered := float64(ready)
		if perReplica > 0 {
			tokens := max(roleDemand[domain.RolePrefill], tf.ByRole[domain.RolePrefill])
			// Rounded up as the engine orders whole replicas; the epsilon
			// keeps an exact fit from ordering one more.
			ordered = max(ordered, math.Ceil(tokens/(scaleUp*perReplica)-1e-9))
		}
		spare := ordered*term.Mu - tf.Lambda
		if !(spare > 0) || math.IsInf(spare, 1) {
			return 0, ""
		}
		return spare, decodeOwedPrefillSpare
	}
	supply := aggregation.AggregateByRole(variants)[domain.RoleDecode].TotalSupply
	resident := roleDemand[domain.RoleDecode] - residency[domain.RoleDecode]
	if supply > 0 && resident >= scaleUp*supply {
		return math.Inf(1), decodeOwedDecodeFull
	}
	return 0, ""
}
