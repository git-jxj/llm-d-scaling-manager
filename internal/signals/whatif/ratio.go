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
	// SumMeasured says Sum is the histogram's own _sum over the same
	// requests the buckets count, which a coarse bucket's bound needs
	// (quantileRead.meanBound). A sum built another way, the engine's
	// count times its step-time ITL, is not: its window is read at the q
	// bucket's upper edge.
	SumMeasured bool
}

// coarseBucketWidth is the width of the q bucket, over the window's mean,
// above which an R_q is set by the bucket grid rather than the traffic.
const coarseBucketWidth = 0.25

// medianConfidence is the confidence that R_q is at or above the median of the
// per-window ratios (ratioRank).
const medianConfidence = 0.90

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
// Per window, the bucket holding the q quantile is read at its upper edge, or,
// for a bucket wider than a quarter of the mean, at the lower of that edge and
// the bound the window's sum sets (readWindow), and divided by the window's
// mean, _sum/_count. Either reading is at or above the window's true quantile,
// and the second keeps percentiles that share a coarse bucket apart where the
// sum allows it. R_q is the median of the per-window ratios plus a margin
// (ratioRank: the order statistic at or above the median with 90%
// confidence), and never below 1. A window with no observations is skipped.
//
// Windows that carry their fill also give R_q by fill (TPOTRatio.ByFill): the
// same rule inside each FillRatioBucketWidth bucket of fill, the global R_q in
// a bucket thinner than MinFillRatioWindows, and raised, never falling, as the
// fill rises. With no such bucket ByFill is nil, and R_q is one ratio at every
// fill.
//
// The ratio comes back typed, ready for TPOTTarget.Ratio, with its flags:
// CoarseTPOTBucket for a q bucket wider than a quarter of its window's mean,
// and MeanBoundedTPOTPercentile where the sum bound read it below the bucket's
// edge. A
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
	bounded := false
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
		coarse, bounded = coarse || rd.coarse, bounded || rd.bounded
	}
	if len(ratios) == 0 {
		return fail(NoTPOTHistogram, "no window holds a TPOT observation")
	}
	slices.Sort(ratios)
	slices.Sort(widths)
	rank := ratioRank(len(ratios))
	var flags []Flag
	if coarse {
		flags = []Flag{CoarseTPOTBucket}
	}
	if bounded {
		flags = append(flags, MeanBoundedTPOTPercentile)
	}
	global := max(1, ratios[rank])
	return TPOTRatio{Value: global, ByFill: fillRatios(windows, q, global)}, flags, widths[rank], nil
}

// ratioRank is the index, in n sorted per-window ratios, R_q is read at: the
// lowest order statistic at or above the median with probability
// medianConfidence, P(Binomial(n, 1/2) <= k) >= medianConfidence. It is the
// median plus a margin that shrinks as windows accrue: with 6 windows or fewer
// it is the largest (the worst window), with 30 the 19th, with 576 about the
// 304th. Each window's reading is already at or above its own quantile
// (readWindow), so R_q is at or above the median window's true ratio with that
// confidence.
func ratioRank(n int) int {
	return medianRank(n)
}

// WorstWindow reports whether R_q from n windows is the worst of them
// (ratioRank picks the largest): true for 6 windows or fewer, so the human
// output can say so.
func WorstWindow(n int) bool { return n > 0 && ratioRank(n) == n-1 }

// medianRank is ratioRank's arithmetic.
func medianRank(n int) int {
	cdf := 0.0
	for k := range n {
		lg, _ := math.Lgamma(float64(n + 1))
		lk, _ := math.Lgamma(float64(k + 1))
		lnk, _ := math.Lgamma(float64(n - k + 1))
		cdf += math.Exp(lg - lk - lnk - float64(n)*math.Ln2)
		if cdf >= medianConfidence {
			return k
		}
	}
	return n - 1
}

// windowReading is one window's reading of the q quantile: its ratio to the
// window's mean, the width of the q bucket over the mean, whether that bucket
// is coarse, and whether the window's mean bounded the reading below the
// bucket's upper edge.
type windowReading struct {
	ratio, width    float64
	coarse, bounded bool
}

// readWindow reads the q quantile of one window with observations (Count and
// Sum above 0), never below the true quantile.
//
// A q bucket no wider than coarseBucketWidth times the mean is read at its
// upper edge. A coarser one is read at the lower of its upper edge and the
// bound the window's sum sets (quantileRead.meanBound): every request is at
// least its bucket's lower edge, and the requests at or above the rank in the
// q bucket are at least the quantile, so the sum caps how far above the lower
// edge the quantile can sit. The bound depends on q, so p50, p90 and p99 that
// share a coarse bucket can read apart where the sum allows it, and read the
// bucket's edge together where it does not.
func readWindow(w TPOTWindow, q float64) (windowReading, bool, error) {
	qr, empty, err := quantileBucket(w.Buckets, q, w.Count)
	if err != nil || empty {
		return windowReading{}, empty, err
	}
	mean := w.Sum / w.Count
	rd := windowReading{ratio: qr.upper / mean, width: (qr.upper - qr.lower) / mean}
	if rd.width > coarseBucketWidth {
		rd.coarse = true
		if b := qr.meanBound(w); b < qr.upper {
			rd.ratio, rd.bounded = b/mean, true
		}
	}
	return rd, false, nil
}

// quantileRead is the bucket holding the q quantile: its edges, the rank, the
// cumulative count at its top, the histogram's total, and the sum of every
// observation's bucket lower edge.
type quantileRead struct {
	lower, upper     float64
	rank, cum, total float64
	floorSum         float64
}

// meanBound is the most the q quantile can be, given the window's sum: the
// observations at or above the rank inside the bucket, cum - rank of them, are
// each at least the quantile x, and every observation is at least its bucket's
// lower edge, so sum >= floorSum + (cum - rank)*(x - lower).
//
// The sum must cover every observation the buckets count. It is the upper
// edge, which caps nothing, where that is not shown: a sum not measured
// (TPOTWindow.SumMeasured), buckets holding more observations than _count
// (scraped a moment apart: the extra ones' share of the sum is unknown), no
// observation above the rank in the bucket, or a sum below the floor. With
// _count at or above the buckets' total, _sum covers them and more, so the
// bound only loosens.
func (qr quantileRead) meanBound(w TPOTWindow) float64 {
	above, slack := qr.cum-qr.rank, w.Sum-qr.floorSum
	if !w.SumMeasured || qr.total > w.Count || !(above > 0) || !(slack >= 0) {
		return qr.upper
	}
	return min(qr.upper, qr.lower+slack/above)
}

// quantileBucket finds the bucket holding the q quantile, read the way
// histogram_quantile reads it:
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
func quantileBucket(buckets []HistogramBucket, q, count float64) (quantileRead, bool, error) {
	if len(buckets) == 0 {
		return quantileRead{}, false, refuse(MalformedHistogram, fmt.Sprintf("%g observations and no buckets", count))
	}
	cum := make([]float64, len(buckets))
	prevEdge, running := 0.0, 0.0
	for j, b := range buckets {
		// NaN fails the comparison and is refused with the rest.
		if !(b.UpperEdge > prevEdge) || !Finite(b.Count) || b.Count < 0 {
			return quantileRead{}, false, refuse(MalformedHistogram, fmt.Sprintf("bucket %d (le %g, count %g) out of order", j, b.UpperEdge, b.Count))
		}
		running = max(running, b.Count)
		cum[j], prevEdge = running, b.UpperEdge
	}
	n := len(buckets)
	total := count
	if math.IsInf(buckets[n-1].UpperEdge, 1) {
		total = cum[n-1]
	}
	if !(total > 0) {
		return quantileRead{}, true, nil
	}
	// Every observation is at least its bucket's lower edge; those past the
	// last finite edge, at least that edge.
	floorSum, lo, prevCum := 0.0, 0.0, 0.0
	for j, b := range buckets {
		floorSum += lo * (cum[j] - prevCum)
		lo, prevCum = b.UpperEdge, cum[j]
	}
	if !math.IsInf(lo, 1) && total > prevCum {
		floorSum += lo * (total - prevCum)
	}
	rank := q * total
	prevEdge = 0
	for j, b := range buckets {
		if cum[j] >= rank {
			if math.IsInf(b.UpperEdge, 1) {
				return quantileRead{}, false, refuse(TPOTQuantileUnbounded, fmt.Sprintf("the quantile is above the last finite edge %g", prevEdge))
			}
			return quantileRead{lower: prevEdge, upper: b.UpperEdge, rank: rank, cum: cum[j], total: total, floorSum: floorSum}, false, nil
		}
		prevEdge = b.UpperEdge
	}
	return quantileRead{}, false, refuse(TPOTQuantileUnbounded, fmt.Sprintf("the quantile is above the last edge %g", prevEdge))
}
