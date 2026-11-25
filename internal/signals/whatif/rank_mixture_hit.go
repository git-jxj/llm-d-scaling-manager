package whatif

import (
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The rank mixture at another prefill count's prefix hit (issue
// llm-d-incubation/llm-d-scaling-manager#384): the model draws each
// request's computed tokens from the source's computed-token histogram, so
// where the hit priced at another prefill count moves the computed tokens a
// request (prefill.HitAtCountOf), the histogram moves with it.

// CumulativeMean is the mean of a cumulative histogram (upper bound to
// cumulative count, +Inf the total) as the rank mixture draws from it:
// log-uniform inside a bucket, uniform in one starting at or under 1. ok is
// false where it holds nothing.
func CumulativeMean(b map[float64]float64) (float64, bool) {
	h, ok := newCumHist(b)
	if !ok {
		return 0, false
	}
	var mean, prev float64
	for i := range h.cum {
		mean += (h.cum[i] - prev) * h.bucketMean(i)
		prev = h.cum[i]
	}
	return mean, true
}

// CumulativeQuantile is the length at share q in (0, 1) of a cumulative
// histogram as the rank mixture draws from it (CumulativeMean). ok is false
// where it holds nothing or q is not in (0, 1).
func CumulativeQuantile(b map[float64]float64, q float64) (float64, bool) {
	h, ok := newCumHist(b)
	if !ok || !(q > 0 && q < 1) {
		return 0, false
	}
	return h.ppf(q), true
}

// ComputedAtScale is computed, a cumulative computed-token histogram, with
// every bound scaled by scale, and capped at the prompt draw: at each bound
// its cumulative share is at least prompt's, so no quantile of it is above
// the whole prompt's at the same share, and no request computes more tokens
// than its prompt holds. Its total is computed's. computed comes back as it
// is where scale is 1, not a positive finite number, or either histogram
// holds nothing.
func ComputedAtScale(computed, prompt map[float64]float64, scale float64) map[float64]float64 {
	if scale == 1 || !(scale > 0) || math.IsInf(scale, 0) {
		return computed
	}
	hc, okc := newCumHist(computed)
	hp, okp := newCumHist(prompt)
	if !okc || !okp {
		return computed
	}
	var total float64
	edges := make([]float64, 0, len(computed)+len(prompt))
	for le, c := range computed {
		if Finite(c) {
			total = max(total, c)
		}
		if Finite(le) {
			edges = append(edges, le*scale)
		}
	}
	for le := range prompt {
		if Finite(le) {
			edges = append(edges, le)
		}
	}
	slices.Sort(edges)
	edges = slices.Compact(edges)
	out := make(map[float64]float64, len(edges)+1)
	for _, x := range edges {
		out[x] = total * max(hc.cdf(x/scale), hp.cdf(x))
	}
	out[math.Inf(1)] = total
	return out
}

// bucketMean is bucket i's mean as ppf draws it.
func (h cumHist) bucketMean(i int) float64 {
	lo, hi := h.lo[i], h.hi[i]
	switch {
	case hi <= lo:
		return lo
	case lo <= 1:
		return (lo + hi) / 2
	}
	return (hi - lo) / (math.Log(hi) - math.Log(lo))
}

// cdf is the cumulative share at or under x, ppf's inverse.
func (h cumHist) cdf(x float64) float64 {
	start := 0.0
	for i := range h.cum {
		lo, hi := h.lo[i], h.hi[i]
		switch {
		case x >= hi:
			start = h.cum[i]
			continue
		case x <= lo:
			return start
		case lo <= 1:
			return start + (h.cum[i]-start)*(x-lo)/(hi-lo)
		}
		return start + (h.cum[i]-start)*(math.Log(x)-math.Log(lo))/(math.Log(hi)-math.Log(lo))
	}
	return 1
}

// HitAtCountFlag is the flag a figure priced at r's prefill count carries:
// PrefixHitAtCount where the priced hit moved from the read one by more
// than MaterialPrefixHitMove/2, PrefixHitHeld where a hit above that is held
// at more pods, else none.
func HitAtCountFlag(r prefill.HitAtCount) Flag {
	material := MaterialPrefixHitMove / 2
	switch {
	case r.Moved() && math.Abs(r.FitHit-r.Hit) > material:
		return PrefixHitAtCount
	case r.Rule == prefill.HitHeld && r.FitHit > material:
		return PrefixHitHeld
	}
	return ""
}
