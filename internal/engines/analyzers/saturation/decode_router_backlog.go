package saturation

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
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
	// decodeOwedDecodeAtMu: prefill has no mu, and decode completes what
	// its ready replicas complete at their mu, so decode is what binds.
	decodeOwedDecodeAtMu = "decode-at-mu"
)

// The band over decode's served share (completions over ready replicas x
// mu) in which a P/D decode with no prefill mu is owed the router's queue:
// none at or below decodeAtMuFrom, all of it from decodeAtMuFull, and in
// proportion between, so the charge does not appear and vanish across one
// reading. Decode that completes its mu is at its pricing point, and under
// flow control the router holds what it cannot take. In the flow-control
// property cases (seeds 1-100) a decode-bound decode reads 1.04-1.15, and
// one behind a prefill bottleneck 0.54-0.92: it takes all prefill passes on
// and is not what holds the router back.
const (
	decodeAtMuFrom = 0.95
	decodeAtMuFull = 1.0
)

// decodeOwedRead is how much of the router's standing queue a P/D decode is
// owed (decodeRouterBacklogRate): min(Q, rate x H) x share, H decode's drain
// horizon. why names the case, and is empty where decode is owed nothing.
// served is decode's served share where it was read (0 otherwise), for the
// log.
type decodeOwedRead struct {
	rate, share, served float64
	why                 string
}

// owed is the requests of a router queue of queued decode is charged, over
// drain horizon horizon: 0 where it is owed none or the product is not a
// finite positive number.
func (d decodeOwedRead) owed(queued, horizon float64) float64 {
	if !(d.rate > 0) || !(d.share > 0) {
		return 0
	}
	q := min(queued, d.rate*horizon)
	if !(q > 0) || math.IsInf(q, 1) {
		return 0
	}
	return d.share * q
}

// decodeRouterBacklogRate is how a P/D decode is owed the router's standing
// queue: at what prefill can pass on to it beyond the arrivals, which the
// floor already charges every role. The floor then charges decode
// min(Q, rate x H) x share, H its drain horizon.
//
//   - Prefill has a floor term: rate is the prefill the analyzer orders, in
//     replicas, times prefill's mu, less lambda, and at least zero; share is
//     1. The prefill ordered is the larger of its ready replicas and its
//     demand (occupancy or floor, the larger) at the scale-up threshold of
//     its per-replica capacity, rounded up, as the engine orders whole
//     replicas. Where prefill binds that is about zero, and decode is sized
//     for the arrivals, as with the switch off. A prefill lower bound
//     under-states the rate, so decode is charged less, never more.
//   - Prefill has none: its spare is unknown. Decode is charged the whole
//     queue (an infinite rate) in proportion to how far its served share,
//     decodeServed over its ready replicas x its own mu, is into the band
//     decodeAtMuFrom-decodeAtMuFull. Decode that completes its mu is what
//     holds the router's queue back; one that completes less takes all
//     prefill passes on, so prefill binds and decode is owed nothing. A full
//     KV alone is not the evidence: decode can be full of long requests and
//     still wait on prefill. Nor is decode's own queue: under flow control
//     the router holds the requests decode cannot take, so decode's engine
//     queue reads empty exactly where decode binds.
//
// decodeServed is decode's completions a second (completedRateByRole), and
// roleDemand each role's occupancy demand.
func decodeRouterBacklogRate(tf floor.Floor, variants []domain.VariantCapacity,
	roleDemand map[string]float64, decodeServed, scaleUp float64) decodeOwedRead {
	if !(tf.Lambda > 0) || math.IsInf(tf.Lambda, 1) || !(scaleUp > 0) {
		return decodeOwedRead{}
	}
	if _, ok := tf.ByRole[domain.RoleDecode]; !ok {
		// Decode has no floor to charge.
		return decodeOwedRead{}
	}
	ready := map[string]int{}
	var prefillPerReplica float64
	for _, vc := range variants {
		role := domain.CanonicalRole(vc.Role)
		ready[role] += vc.ReplicaCount
		if role == domain.RolePrefill {
			prefillPerReplica = max(prefillPerReplica, vc.PerReplicaCapacity)
		}
	}
	if term, ok := tf.Terms[domain.RolePrefill]; ok && term.Mu > 0 && term.PerReplica > 0 {
		ordered := float64(ready[domain.RolePrefill])
		if prefillPerReplica > 0 {
			tokens := max(roleDemand[domain.RolePrefill], tf.ByRole[domain.RolePrefill])
			// Rounded up as the engine orders whole replicas; the epsilon
			// keeps an exact fit from ordering one more.
			ordered = max(ordered, math.Ceil(tokens/(scaleUp*prefillPerReplica)-1e-9))
		}
		spare := ordered*term.Mu - tf.Lambda
		if !(spare > 0) || math.IsInf(spare, 1) {
			return decodeOwedRead{}
		}
		return decodeOwedRead{rate: spare, share: 1, why: decodeOwedPrefillSpare}
	}
	mu := tf.Terms[domain.RoleDecode].Mu
	n := float64(ready[domain.RoleDecode])
	if !(mu > 0) || math.IsInf(mu, 1) || !(n > 0) || !(decodeServed > 0) || math.IsInf(decodeServed, 1) {
		return decodeOwedRead{}
	}
	served := decodeServed / (n * mu)
	share := min(1, (served-decodeAtMuFrom)/(decodeAtMuFull-decodeAtMuFrom))
	if !(share > 0) {
		return decodeOwedRead{served: served}
	}
	return decodeOwedRead{rate: math.Inf(1), share: share, served: served, why: decodeOwedDecodeAtMu}
}
