package prefill

import (
	"math"
	"slices"
)

// The prefix hit at another prefill count (issue
// llm-d-incubation/llm-d-scaling-manager#384).
//
// A cache-aware router keeps each shared prefix on one prefill pod, so N pods
// split the prefixes N ways and each pod's cache holds a larger share of its
// own. Where the prefixes do not fit one pod's cache, the hit then rises with
// N: on a lab run of 64 shared documents against one pod's GPU cache, the
// hit was 0.345 on 1 pod, 0.526 on 2 and 0.792 on 4. A hit read at one count
// and held at another under-prices the prompt tokens a pod computes at fewer
// pods (7,580 a request priced at 1 pod, 10,470 measured) and over-prices
// them at more.
//
// HitAtCount gives the hit at the priced count N' from the hit h read at the
// fit's count N, and the computed tokens a request with it:
//
//   - N' = N: h, nothing moves.
//   - N' < N, with the engine's KV residency histograms (ReuseHistograms):
//     the reuse-gap rule. The histograms close Che's occupancy equation at
//     the fit; at N' a pod takes N/N' times the block accesses with the same
//     reuse gaps (affinity keeps a prefix on one pod), so the cache horizon
//     T' solves the occupancy at N', and the hits priced are those whose gap
//     is under T'. Never above h, but not a floor: in the lab simulation it
//     read over the truth from 4 pods to 2, where duplication changes
//     between the counts. Untested on real data, and no caller passes the
//     histograms yet.
//   - N' < N without them: the floor h x N'/N. The hit is concave in each
//     pod's cache over its working set, so it is at least that; on every
//     measured and simulated pair of the lab run it read at or under the
//     truth, at the cost of 13% to 28% more computed tokens than measured.
//   - N' > N: h, held. Under affinity the hit rises with N, so the computed
//     tokens read high; where the working set fits one pod it stays near
//     flat, less the cold copies a prefix sent to a second pod makes. The
//     bracket's upper edge, min(1, h x N'/N), is how high the hit may rise.
//
// The computed tokens a request scale by (1 - h')/(1 - h): the whole prompt
// does not move with the count, only the share of it the cache serves.

// HitRule names how HitAtCount priced the hit.
type HitRule string

// The rules.
const (
	// HitSameCount is the fit's own count: the hit as read.
	HitSameCount HitRule = "same"
	// HitFloor is the concavity floor h x N'/N, at fewer pods.
	HitFloor HitRule = "floor"
	// HitReuseGap is the reuse-gap rule, at fewer pods.
	HitReuseGap HitRule = "gap"
	// HitHeld is the fit's hit held, at more pods.
	HitHeld HitRule = "held"
)

// ReuseBucket is one cumulative bucket of a Prometheus histogram: Count
// observations at or under UpperEdge, seconds. The last edge may be +Inf.
type ReuseBucket struct {
	UpperEdge, Count float64
}

// ReuseHistograms are a prefill variant's KV residency histograms over the
// fit's window (vLLM with --kv-cache-metrics), summed over its pods: Gap is
// vllm:kv_block_reuse_gap_seconds, one observation per block hit, the time
// since the block's last use, and GapSum its _sum; Idle is
// vllm:kv_block_idle_before_evict_seconds, one observation per block
// evicted, the time it sat unused. Both cumulative, edges ascending.
type ReuseHistograms struct {
	Gap    []ReuseBucket
	GapSum float64
	Idle   []ReuseBucket
}

// HitAtCount is the hit priced at a prefill count other than the fit's.
type HitAtCount struct {
	// FitHit is h, the fit's hit (1 - computed / whole prompt, every tier);
	// Hit is h', the hit priced at Count.
	FitHit, Hit float64
	// FitCount is N, the prefill pods the hit was read on; Count is N'.
	FitCount, Count int
	// Rule is how Hit was priced.
	Rule HitRule
	// Lo and Hi bracket the hit at Count: the floor and the fit's hit at
	// fewer pods, the fit's hit and min(1, h x N'/N) at more.
	Lo, Hi float64
	// Computed is the computed tokens a request at Count, ComputedScale
	// that over the fit's: (1 - h')/(1 - h).
	Computed, ComputedScale float64
}

// Moved says the hit priced differs from the fit's.
func (a HitAtCount) Moved() bool { return a.Hit != a.FitHit }

// HitAtCountOf prices the hit and the computed tokens a request at count,
// from the fit's hit h and computed tokens a request on fitCount pods, with
// the reuse-gap rule where reuse is non-nil and usable. ok is false where
// nothing can be priced: a count or the fit's count under 1, h outside [0, 1)
// or not a number, or computed not a positive finite number; the caller then
// keeps the fit's figures. At h = 0 the hit stays 0 at every count.
func HitAtCountOf(h, computed float64, fitCount, count int, reuse *ReuseHistograms) (HitAtCount, bool) {
	if fitCount < 1 || count < 1 || !(h >= 0 && h < 1) || !(computed > 0) || math.IsInf(computed, 0) {
		return HitAtCount{}, false
	}
	x := float64(count) / float64(fitCount)
	out := HitAtCount{FitHit: h, Hit: h, FitCount: fitCount, Count: count, Lo: h, Hi: h, Computed: computed, ComputedScale: 1}
	switch {
	case count == fitCount:
		out.Rule = HitSameCount
		return out, true
	case count > fitCount:
		out.Rule, out.Hi = HitHeld, min(1, h*x)
		return out, true
	}
	out.Rule, out.Hit, out.Lo = HitFloor, h*x, h*x
	if g, ok := reuseGapHit(h, fitCount, count, reuse); ok {
		out.Rule, out.Hit = HitReuseGap, max(min(g, h), 0)
	}
	out.ComputedScale = (1 - out.Hit) / (1 - h)
	out.Computed = computed * out.ComputedScale
	return out, true
}

// reuseGapHit is the reuse-gap rule's hit at count < fitCount, false where
// the histograms do not give it: no hit or no eviction counted, edges not
// ascending, counts not cumulative, or a horizon not read.
//
// Occupancy, Che's approximation read as an identity: a pod's cache holds,
// over a span, the sum over its block accesses of min(the gap to the next
// access, T), T the cache horizon. A hit block holds its gap; every miss,
// evicted after T idle, holds T. At the fit, with H hits, M misses (in steady
// state every miss is evicted once, so M is the eviction count) and T the
// median idle before eviction,
//
//	B ~ S(inf) + M T                        (per span, per pod)
//
// S(t) the sum of the hit gaps under t and H(t) their count. At N' < N one
// pod takes N/N' times the accesses with the same gaps, so T' solves
//
//	(N/N') (S(T') + (M + H - H(T')) T') = S(inf) + M T
//
// and the hit is h H(T')/H: the hits whose gap is under the new horizon.
// Gaps longer than T are evictions, never seen as gaps, so the rule works
// downward only.
func reuseGapHit(h float64, fitCount, count int, r *ReuseHistograms) (float64, bool) {
	if r == nil {
		return 0, false
	}
	gap, okG := newReuseHist(r.Gap)
	idle, okI := newReuseHist(r.Idle)
	if !okG || !okI {
		return 0, false
	}
	t := idle.quantile(0.5)
	if !(t > 0) || math.IsInf(t, 0) {
		return 0, false
	}
	hits, misses := gap.total(), idle.total()
	all := gap.sumUnder(math.Inf(1))
	if r.GapSum > 0 && !math.IsInf(r.GapSum, 0) {
		all = r.GapSum
	}
	target := all + misses*t
	ratio := float64(fitCount) / float64(count)
	occupancy := func(tp float64) float64 {
		return ratio * (gap.sumUnder(tp) + (misses+hits-gap.countUnder(tp))*tp)
	}
	lo, hi := 0.0, t
	if !(occupancy(hi) >= target) {
		// The horizon at N' is at least the fit's: no fewer pods do that.
		return 0, false
	}
	for range 100 {
		mid := (lo + hi) / 2
		if occupancy(mid) < target {
			lo = mid
		} else {
			hi = mid
		}
	}
	return h * gap.countUnder(lo) / hits, true
}

// reuseHist is a cumulative histogram read with every observation spread
// evenly across its bucket, the first from 0; an observation past the last
// finite edge sits on that edge.
type reuseHist struct {
	edges, counts []float64
}

// newReuseHist reads b, false with no observation, edges not ascending or
// not above 0, or counts falling or not finite.
func newReuseHist(b []ReuseBucket) (reuseHist, bool) {
	var h reuseHist
	prevEdge, prevCount := 0.0, 0.0
	for _, x := range b {
		if !(x.UpperEdge > prevEdge) || !(x.Count >= prevCount) || math.IsInf(x.Count, 0) {
			return reuseHist{}, false
		}
		h.edges = append(h.edges, x.UpperEdge)
		h.counts = append(h.counts, x.Count)
		prevEdge, prevCount = x.UpperEdge, x.Count
	}
	if len(h.edges) == 0 || !(prevCount > 0) {
		return reuseHist{}, false
	}
	if math.IsInf(h.edges[len(h.edges)-1], 1) && len(h.edges) == 1 {
		return reuseHist{}, false
	}
	return h, true
}

// total is the observations counted.
func (h reuseHist) total() float64 { return h.counts[len(h.counts)-1] }

// bucket is bucket i's lower and upper edge, an infinite upper edge read as
// its lower one.
func (h reuseHist) bucket(i int) (lo, hi float64) {
	if i > 0 {
		lo = h.edges[i-1]
	}
	hi = h.edges[i]
	if math.IsInf(hi, 1) {
		hi = lo
	}
	return lo, hi
}

// countUnder is the observations under t.
func (h reuseHist) countUnder(t float64) float64 {
	n, _ := h.under(t)
	return n
}

// sumUnder is the sum of the observations under t.
func (h reuseHist) sumUnder(t float64) float64 {
	_, s := h.under(t)
	return s
}

// under is the count and the sum of the observations under t. A bucket
// past the last finite edge holds its observations on that edge.
func (h reuseHist) under(t float64) (n, s float64) {
	prev := 0.0
	for i := range h.edges {
		c := h.counts[i] - prev
		prev = h.counts[i]
		lo, hi := h.bucket(i)
		switch {
		case hi == lo:
			if t > lo {
				n, s = n+c, s+c*lo
			}
		case t >= hi:
			n, s = n+c, s+c*(lo+hi)/2
		case t > lo:
			part := c * (t - lo) / (hi - lo)
			n, s = n+part, s+part*(lo+t)/2
		}
	}
	return n, s
}

// quantile is the q quantile, interpolated in its bucket; the last finite
// edge where it falls past it.
func (h reuseHist) quantile(q float64) float64 {
	target := q * h.total()
	i := max(slices.IndexFunc(h.counts, func(c float64) bool { return c >= target }), 0)
	lo, hi := h.bucket(i)
	below := 0.0
	if i > 0 {
		below = h.counts[i-1]
	}
	c := h.counts[i] - below
	if !(c > 0) || hi == lo {
		return hi
	}
	return lo + (hi-lo)*(target-below)/c
}
