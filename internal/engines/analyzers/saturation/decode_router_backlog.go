package saturation

import (
	"math"
	"slices"
	"time"

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

// Why decode was charged the router's queue (decodeRouterBacklogRate), or
// why it was not where it read as binding.
const (
	// decodeOwedPrefillSpare: capped at what the prefill the analyzer
	// orders completes beyond the arrivals.
	decodeOwedPrefillSpare = "prefill-spare"
	// decodeOwedDecodeAtMu: prefill has no mu, and decode is full and
	// completes what its ready replicas complete at their mu, so decode is
	// what binds.
	decodeOwedDecodeAtMu = "decode-at-mu"
	// decodeOwedMuSuspect: decode reads as completing well over its own mu,
	// which a right mu does not allow: the mu is low, and nothing is owed.
	decodeOwedMuSuspect = "mu-suspect"
)

// How a P/D decode with no prefill mu is judged to be what holds the router's
// queue back (decodeAtMuWeight). Two readings, each smoothed over the last
// decodeOwingSamples cycles (their median):
//
//   - its KV fill, the mean of its ready replicas' busiest-rank usage, against
//     the policy's kvCacheThreshold. Flow control holds requests at the router
//     once the engines read saturated, so a decode that holds them back is at
//     the threshold. The charge is in proportion from decodeFullFrom of the
//     threshold to the threshold. This is what separates the two cases: in
//     the flow-control property cases (seeds 1-100) a decode-bound decode
//     reads 0.85-0.95 of its KV and one behind a prefill bottleneck 0.09-0.43.
//   - its served share, completions over its ready replicas times its own mu,
//     in proportion from decodeAtMuFrom to decodeAtMuFull. Decode that is
//     full but completes well under its mu is full of long requests, not
//     binding (the review's probe: prefill-bound, decode raised to 0.70-0.80
//     of KV). The share alone cannot separate the cases: the classes read
//     0.54-0.92 and 1.04-1.15 in the same cases, but a completion rate read
//     20% high lifts the first to 1.10. So the band is wide, 0.80-1.00, for
//     that error and mu's, and the fill decides.
//
// A smoothed share above decodeAtMuImplausible is a mu error, not a binding
// decode (decodeOwedMuSuspect): decode cannot complete that far over a right
// mu, and the charge, sized on that mu, would be several times too large. A
// decode-bound decode reads at most 1.15 in the property cases.
//
// The charge then falls by at most decodeOwingStep a cycle, so it does not
// appear and vanish with each reading; a mu error drops it at once.
const (
	decodeAtMuFrom        = 0.80
	decodeAtMuFull        = 1.0
	decodeAtMuImplausible = 1.25
	decodeFullFrom        = 0.9
	decodeOwingSamples    = 4
	decodeOwingStep       = 0.25
	// decodeOwingMaxAge is how old a sample, or the charge last held, may be
	// and still count: two minutes, eight cycles.
	decodeOwingMaxAge = 2 * time.Minute
)

// decodeOwedRead is how much of the router's standing queue a P/D decode is
// owed (decodeRouterBacklogRate): min(Q, rate x H) x share, H decode's drain
// horizon. why names the case, and is empty where decode is owed nothing.
// served and fill are decode's smoothed served share and KV fill where they
// were read (0 otherwise), for the log.
type decodeOwedRead struct {
	rate, share, served, fill float64
	why                       string
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

// decodeEvidence is what a P/D decode reads this cycle for decodeAtMuWeight:
// its served share (completions over ready replicas x mu) and its KV fill.
// ok is false where either is not read.
type decodeEvidence struct {
	share, fill float64
	ok          bool
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
//   - Prefill has none: its spare is unknown. ok is true, and the caller
//     weighs decode's own evidence (decodeAtMuWeight) for how much of the
//     whole queue (an infinite rate) it is owed. Decode's own queue is not
//     that evidence: under flow control the router holds the requests decode
//     cannot take, so decode's engine queue reads empty exactly where decode
//     binds.
//
// roleDemand is each role's occupancy demand.
func decodeRouterBacklogRate(tf floor.Floor, variants []domain.VariantCapacity,
	roleDemand map[string]float64, scaleUp float64) (read decodeOwedRead, weigh bool) {
	if !(tf.Lambda > 0) || math.IsInf(tf.Lambda, 1) || !(scaleUp > 0) {
		return decodeOwedRead{}, false
	}
	if _, ok := tf.ByRole[domain.RoleDecode]; !ok {
		// Decode has no floor to charge.
		return decodeOwedRead{}, false
	}
	ready := 0
	var prefillPerReplica float64
	for _, vc := range variants {
		if domain.CanonicalRole(vc.Role) == domain.RolePrefill {
			ready += vc.ReplicaCount
			prefillPerReplica = max(prefillPerReplica, vc.PerReplicaCapacity)
		}
	}
	if term, ok := tf.Terms[domain.RolePrefill]; ok && term.Mu > 0 && term.PerReplica > 0 {
		ordered := float64(ready)
		if prefillPerReplica > 0 {
			tokens := max(roleDemand[domain.RolePrefill], tf.ByRole[domain.RolePrefill])
			// Rounded up as the engine orders whole replicas; the epsilon
			// keeps an exact fit from ordering one more.
			ordered = max(ordered, math.Ceil(tokens/(scaleUp*prefillPerReplica)-1e-9))
		}
		spare := ordered*term.Mu - tf.Lambda
		if !(spare > 0) || math.IsInf(spare, 1) {
			return decodeOwedRead{}, false
		}
		return decodeOwedRead{rate: spare, share: 1, why: decodeOwedPrefillSpare}, false
	}
	return decodeOwedRead{rate: math.Inf(1)}, true
}

// readDecodeEvidence is a P/D decode's evidence this cycle: its completions a
// second (served, completedRateByRole) over its ready replicas in variants
// times mu, decode's floor mu; and the mean busiest-rank KV usage of its
// Ready replicas in metrics that are not lent by the warm pool.
func readDecodeEvidence(metrics []domain.ReplicaMetrics, variants []domain.VariantCapacity,
	roleOf map[string]string, served, mu float64) decodeEvidence {
	n := 0
	for _, vc := range variants {
		if domain.CanonicalRole(vc.Role) == domain.RoleDecode {
			n += vc.ReplicaCount
		}
	}
	var sum float64
	read := 0
	for _, rm := range metrics {
		if roleOf[rm.VariantName] != domain.RoleDecode || !rm.Ready || rm.FromWarmPool {
			continue
		}
		k := rm.BusiestRankKvUsage()
		if !(k >= 0) || k > 1 {
			continue
		}
		sum += k
		read++
	}
	if !(mu > 0) || math.IsInf(mu, 1) || n <= 0 || !(served > 0) || math.IsInf(served, 1) || read == 0 {
		return decodeEvidence{}
	}
	return decodeEvidence{share: served / (float64(n) * mu), fill: sum / float64(read), ok: true}
}

// decodeOwingMemo is, per namespace|model, a P/D decode's recent evidence
// (decodeAtMuWeight): the last decodeOwingSamples readings, and the share of
// the router queue it was last charged and when.
type decodeOwingMemo struct {
	samples []decodeOwingSample
	weight  float64
	seen    time.Time
}

type decodeOwingSample struct {
	at          time.Time
	share, fill float64
}

// rampUp is 0 at or below lo, 1 at or above hi, and in proportion between.
func rampUp(x, lo, hi float64) float64 {
	return min(1, max(0, (x-lo)/(hi-lo)))
}

// medianOf is the median of xs, which is not empty.
func medianOf(xs []float64) float64 {
	s := slices.Sorted(slices.Values(xs))
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

// decodeAtMuWeight is the share of the router's queue a P/D decode with no
// prefill mu is owed, from this cycle's evidence ev and the cycles before it
// for key (the consts above), and the smoothed share and fill it read. why is
// decodeOwedDecodeAtMu where it is owed some, decodeOwedMuSuspect where the
// share says the mu is wrong, and empty otherwise. kvThreshold is the
// policy's kvCacheThreshold; with none (0) decode is not judged full.
func (a *SaturationAnalyzer) decodeAtMuWeight(key string, ev decodeEvidence, kvThreshold float64) (weight, share, fill float64, why string) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.decodeOwing == nil {
		a.decodeOwing = make(map[string]*decodeOwingMemo)
	}
	m := a.decodeOwing[key]
	if m == nil || now.Sub(m.seen) > decodeOwingMaxAge {
		m = &decodeOwingMemo{}
		a.decodeOwing[key] = m
	}
	m.seen = now
	if ev.ok {
		m.samples = append(m.samples, decodeOwingSample{at: now, share: ev.share, fill: ev.fill})
	}
	m.samples = slices.DeleteFunc(m.samples, func(s decodeOwingSample) bool { return now.Sub(s.at) > decodeOwingMaxAge })
	if n := len(m.samples); n > decodeOwingSamples {
		m.samples = slices.Delete(m.samples, 0, n-decodeOwingSamples)
	}
	if len(m.samples) == 0 {
		m.weight = 0
		return 0, 0, 0, ""
	}
	shares := make([]float64, len(m.samples))
	fills := make([]float64, len(m.samples))
	for i, s := range m.samples {
		shares[i], fills[i] = s.share, s.fill
	}
	share, fill = medianOf(shares), medianOf(fills)
	if share > decodeAtMuImplausible {
		m.weight = 0
		return 0, share, fill, decodeOwedMuSuspect
	}
	w := rampUp(share, decodeAtMuFrom, decodeAtMuFull)
	if kvThreshold > 0 {
		w *= rampUp(fill, decodeFullFrom*kvThreshold, kvThreshold)
	} else {
		w = 0
	}
	weight = max(w, m.weight-decodeOwingStep)
	m.weight = weight
	if weight > 0 {
		why = decodeOwedDecodeAtMu
	}
	return weight, share, fill, why
}
