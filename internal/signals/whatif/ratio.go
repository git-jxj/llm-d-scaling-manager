package whatif

import (
	"errors"
	"fmt"
	"math"
	"slices"
)

// HistogramBucket is one cumulative bucket of a Prometheus histogram: Count
// observations at or below UpperEdge (the le label). The last bucket's edge
// may be +Inf.
type HistogramBucket struct {
	UpperEdge float64
	Count     float64
}

// TPOTWindow is one window of a per-request TPOT histogram
// (llm_d_epp_request_streaming_tpot_seconds): its cumulative buckets and its
// _sum and _count over the window.
type TPOTWindow struct {
	Buckets []HistogramBucket
	Sum     float64
	Count   float64
	// Fill is the decode replicas' mean KV fill over the window, when
	// FillKnown: the bucket R_q by fill files the window under (FillRatio).
	// A window with no fill counts toward the global R_q only.
	Fill      float64
	FillKnown bool
}

// coarseBucketWidth is the width of the q bucket, over the window's mean,
// above which an R_q is set by the bucket grid rather than the traffic.
const coarseBucketWidth = 0.25

// ratioPercentile is the percentile of the per-window ratios R_q is taken at.
const ratioPercentile = 0.90

// MaxEngineBucketWidth is the width of the q bucket, over the window's mean,
// past which the engine's histogram answers no percentile: four times
// coarseBucketWidth, a bucket wider than the mean itself. The quantile is then
// the bucket's edge whatever q was asked: on throughput-sweep-restart, whose
// vLLM buckets open at 10 ms around a mean of about 3 ms, p90, p99 and p99.9
// all read the same 21.79 ms. Below it a coarse R_q is flagged and stands. A
// stated line, not a measured one: read at the same rank as R_q, so one idle
// window does not decide it.
const MaxEngineBucketWidth = 4 * coarseBucketWidth

// TPOTRatioFromWindows learns R_q, the q-th percentile of per-request TPOT over
// its mean (docs/proposals/slo-capacity-planner.md, "Learning R_q").
//
// Per window, the bucket holding the q quantile is read at its UPPER edge and
// divided by the window's mean, _sum/_count: an upper edge never under-states
// the quantile, so the ratio is conservative. A bucket wider than a quarter of
// the mean is read inside, linearly (readWindow), so that each percentile keeps
// its own ratio; the answer is flagged. R_q is the 90th percentile of
// those per-window ratios (nearest rank), and never below 1. A window with no
// observations is skipped.
//
// Windows that carry their fill also give R_q by fill (TPOTRatio.ByFill): the
// same rule inside each FillRatioBucketWidth bucket of fill, the global R_q in
// a bucket thinner than MinFillRatioWindows, and never falling as the fill
// rises. With no such bucket ByFill is nil, and R_q is one ratio at every fill.
//
// The ratio comes back typed, ready for TPOTTarget.Ratio, with its flags:
// CoarseTPOTBucket and InterpolatedTPOTPercentile for a q bucket wider than a
// quarter of its window's mean. A
// refusal comes back as a non-nil *Unanswerable with a zero ratio, which a
// TPOTTarget reads as "no ratio" (NoTPOTHistogram), so ignoring it cannot
// price a percentile. The refusals: TPOTQuantileUnbounded when a window's
// quantile falls in the +Inf bucket, which has no edge to read;
// NoTPOTHistogram when no window holds an observation; MalformedHistogram for
// data that cannot be read; and InvalidInput only for a q outside (0, 1).
func TPOTRatioFromWindows(windows []TPOTWindow, q float64) (TPOTRatio, []Flag, *Unanswerable) {
	r, flags, _, refused := ratioAndWidth(windows, q)
	return r, flags, refused
}

// EngineTPOTRatio is TPOTRatioFromWindows on the engine's histogram, the
// fall-back for the router's (docs/proposals/slo-capacity-planner.md,
// "Sources"): every result carries EngineTPOTHistogram, and a q bucket past
// MaxEngineBucketWidth is refused, TPOTBucketTooWide, with CoarseTPOTBucket.
func EngineTPOTRatio(windows []TPOTWindow, q float64) (TPOTRatio, []Flag, *Unanswerable) {
	r, flags, width, refused := ratioAndWidth(windows, q)
	if refused != nil {
		u := *refused
		u.Flags = append(slices.Clone(u.Flags), EngineTPOTHistogram)
		return TPOTRatio{}, nil, &u
	}
	if width > MaxEngineBucketWidth {
		u := refuse(TPOTBucketTooWide, fmt.Sprintf("the engine's q bucket is %.3g times the mean, past %g: it reads the bucket's edge, not the percentile", width, float64(MaxEngineBucketWidth)))
		u.Flags = []Flag{CoarseTPOTBucket, EngineTPOTHistogram}
		return TPOTRatio{}, nil, &u
	}
	return r, append(flags, EngineTPOTHistogram), nil
}

// ratioAndWidth is R_q and its flags, and the width of the q bucket over the
// window's mean at the rank R_q is read at.
func ratioAndWidth(windows []TPOTWindow, q float64) (TPOTRatio, []Flag, float64, *Unanswerable) {
	fail := func(r Reason, detail string) (TPOTRatio, []Flag, float64, *Unanswerable) {
		u := refuse(r, detail)
		return TPOTRatio{}, nil, 0, &u
	}
	if !(q > 0 && q < 1) {
		return fail(InvalidInput, fmt.Sprintf("percentile %g, want 0 < q < 1", q))
	}
	var ratios, widths []float64
	coarse := false
	for i, w := range windows {
		if !Finite(w.Count) || w.Count < 0 || !Finite(w.Sum) {
			return fail(MalformedHistogram, fmt.Sprintf("window %d: count %g, sum %g", i, w.Count, w.Sum))
		}
		if w.Count == 0 {
			continue
		}
		if !(w.Sum > 0) {
			return fail(MalformedHistogram, fmt.Sprintf("window %d: %g observations summing to %g", i, w.Count, w.Sum))
		}
		rd, empty, err := readWindow(w, q)
		if err != nil {
			var u Unanswerable
			if !errors.As(err, &u) {
				u = refuse(MalformedHistogram, err.Error())
			}
			return fail(u.Reason, fmt.Sprintf("window %d: %s", i, u.Detail))
		}
		if empty {
			continue
		}
		ratios = append(ratios, rd.ratio)
		widths = append(widths, rd.width)
		coarse = coarse || rd.coarse
	}
	if len(ratios) == 0 {
		return fail(NoTPOTHistogram, "no window holds a TPOT observation")
	}
	slices.Sort(ratios)
	slices.Sort(widths)
	rank := int(math.Ceil(ratioPercentile*float64(len(ratios)))) - 1
	var flags []Flag
	if coarse {
		flags = []Flag{CoarseTPOTBucket, InterpolatedTPOTPercentile}
	}
	global := max(1, ratios[rank])
	return TPOTRatio{Value: global, ByFill: fillRatios(windows, q, global)}, flags, widths[rank], nil
}

// windowReading is one window's reading of the q quantile: its ratio to the
// window's mean, the width of the q bucket over the mean, and whether that
// bucket is coarse.
type windowReading struct {
	ratio, width float64
	coarse       bool
}

// readWindow reads the q quantile of one window with observations (Count and
// Sum above 0). A q bucket no wider than coarseBucketWidth times the mean is
// read at its upper edge, which never under-states the quantile. A coarser one
// is read inside the bucket, linearly in its count, as histogram_quantile
// reads it: its upper edge would give every percentile that lands in the
// bucket the same ratio, so p50, p90 and p99 would price one TPOT.
func readWindow(w TPOTWindow, q float64) (windowReading, bool, error) {
	lower, upper, frac, empty, err := quantileBucket(w.Buckets, q, w.Count)
	if err != nil || empty {
		return windowReading{}, empty, err
	}
	mean := w.Sum / w.Count
	rd := windowReading{ratio: upper / mean, width: (upper - lower) / mean}
	if rd.width > coarseBucketWidth {
		rd.coarse = true
		rd.ratio = (lower + frac*(upper-lower)) / mean
	}
	return rd, false, nil
}

// quantileBucket finds the bucket holding the q quantile and returns its lower
// and upper edges, and frac, how far into the bucket's own count the rank
// falls (0 to 1), read the way histogram_quantile reads them:
//
//   - the cumulative counts are made monotone (a running maximum), since
//     rate() over buckets scraped a moment apart leaves small dips that are
//     not an error;
//   - the rank is q times the +Inf bucket's count, the histogram's own total,
//     rather than _count, which is scraped separately and may exceed it; a
//     histogram with no +Inf bucket ranks against count.
//
// empty is true when the buckets hold no observation. The error is an
// Unanswerable: MalformedHistogram for no buckets under a positive count, an
// edge that does not rise from above 0 (a TPOT is positive), or a count that
// is not finite and non-negative; TPOTQuantileUnbounded for a quantile past
// the last finite edge.
func quantileBucket(buckets []HistogramBucket, q, count float64) (lower, upper, frac float64, empty bool, err error) {
	if len(buckets) == 0 {
		return 0, 0, 0, false, refuse(MalformedHistogram, fmt.Sprintf("%g observations and no buckets", count))
	}
	cum := make([]float64, len(buckets))
	prevEdge, running := 0.0, 0.0
	for j, b := range buckets {
		// NaN fails the comparison and is refused with the rest.
		if !(b.UpperEdge > prevEdge) || !Finite(b.Count) || b.Count < 0 {
			return 0, 0, 0, false, refuse(MalformedHistogram, fmt.Sprintf("bucket %d (le %g, count %g) out of order", j, b.UpperEdge, b.Count))
		}
		running = max(running, b.Count)
		cum[j], prevEdge = running, b.UpperEdge
	}
	total := count
	if n := len(buckets); math.IsInf(buckets[n-1].UpperEdge, 1) {
		total = cum[n-1]
	}
	if !(total > 0) {
		return 0, 0, 0, true, nil
	}
	rank := q * total
	prevEdge = 0
	prevCum := 0.0
	for j, b := range buckets {
		if cum[j] >= rank {
			if math.IsInf(b.UpperEdge, 1) {
				return 0, 0, 0, false, refuse(TPOTQuantileUnbounded, fmt.Sprintf("the quantile is above the last finite edge %g", prevEdge))
			}
			frac = 1.0
			if in := cum[j] - prevCum; in > 0 {
				frac = (rank - prevCum) / in
			}
			return prevEdge, b.UpperEdge, frac, false, nil
		}
		prevEdge, prevCum = b.UpperEdge, cum[j]
	}
	return 0, 0, 0, false, refuse(TPOTQuantileUnbounded, fmt.Sprintf("the quantile is above the last edge %g", prevEdge))
}
