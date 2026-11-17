package whatif

import (
	"fmt"
	"math"
	"slices"
)

// FillRatioBucketWidth is the width, in KV fill, of the buckets R_q is learned
// in: [0, 0.2), [0.2, 0.4) and so on up to a full cache, the last bucket
// closed at 1.
const FillRatioBucketWidth = 0.2

// fillRatioBuckets is how many buckets cover fills 0 to 1.
const fillRatioBuckets = 5

// MinFillRatioWindows is how many windows a fill bucket needs before its own
// R_q stands. A thinner bucket is priced at the global R_q, and an answer that
// reads it carries GlobalTPOTRatio.
const MinFillRatioWindows = 5

// FillRatio is R_q learned over one bucket of KV fill
// (docs/proposals/slo-capacity-planner.md, "R_q by fill").
type FillRatio struct {
	// Lo and Hi are the bucket's fill edges, Lo inclusive.
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
	// Value is the R_q priced in the bucket: its own, read by the rule of
	// TPOTRatioFromWindows on its windows alone, or the global R_q when the
	// bucket is thin; then raised to the bucket below it, so that R_q never
	// falls as the fill rises.
	Value float64 `json:"value"`
	// Learned is the bucket's own R_q before the raise, 0 when thin.
	Learned float64 `json:"learned,omitempty"`
	// Windows is how many windows fell in the bucket: its confidence count.
	Windows int `json:"windows"`
	// Thin marks a bucket with fewer than MinFillRatioWindows windows.
	Thin bool `json:"thin,omitempty"`
}

// center is the fill R_q is read at exactly, between two buckets' centers
// interpolated.
func (b FillRatio) center() float64 { return (b.Lo + b.Hi) / 2 }

// At is R_q at fill k, and whether the global R_q stood in for a thin bucket
// there. A ratio with no ByFill is Value at every fill. With ByFill, R_q is
// read between the two bucket centers around k, linearly, and flat past the
// first and the last center. Since the buckets never fall, neither does At.
func (r TPOTRatio) At(k float64) (float64, bool) {
	b := r.ByFill
	if len(b) == 0 {
		return r.Value, false
	}
	if k <= b[0].center() {
		return b[0].Value, b[0].Thin
	}
	last := len(b) - 1
	if k >= b[last].center() {
		return b[last].Value, b[last].Thin
	}
	i := 0
	for k > b[i+1].center() {
		i++
	}
	lo, hi := b[i], b[i+1]
	w := (k - lo.center()) / (hi.center() - lo.center())
	return lo.Value + w*(hi.Value-lo.Value), lo.Thin && w < 1 || hi.Thin && w > 0
}

// check refuses a by-fill curve that is not one TPOTRatioByFill learns: a
// value below 1 or not finite, buckets out of order, or a value that falls as
// the fill rises. A curve that breaks it would let slo-to-rate's inverse fold
// over.
func (r TPOTRatio) check() *Unanswerable {
	prev := FillRatio{Lo: math.Inf(-1), Hi: math.Inf(-1), Value: 1}
	for i, b := range r.ByFill {
		if !Finite(b.Value) || !(b.Value >= prev.Value) || !(b.Lo >= prev.Hi) || !(b.Hi > b.Lo) {
			u := refuse(InvalidInput, fmt.Sprintf("R_q by fill, bucket %d [%g, %g) at %g after %g: want rising buckets and a ratio of at least 1 that never falls", i, b.Lo, b.Hi, b.Value, prev.Value))
			return &u
		}
		prev = b
	}
	return nil
}

// fillRatios is R_q by fill learned from the windows whose fill is known, with
// global standing in for a thin bucket. It is nil when no window carries a
// fill or no bucket holds MinFillRatioWindows of them: every fill would then
// read the global R_q, and the answer is the one-ratio answer, unflagged.
func fillRatios(windows []TPOTWindow, q, global float64) []FillRatio {
	ratios := make([][]float64, fillRatioBuckets)
	for _, w := range windows {
		if !w.FillKnown || !Finite(w.Fill) || w.Count == 0 || !(w.Sum > 0) {
			continue
		}
		rd, empty, err := readWindow(w, q)
		if err != nil || empty {
			continue
		}
		i := min(fillRatioBuckets-1, max(0, int(math.Floor(w.Fill/FillRatioBucketWidth))))
		ratios[i] = append(ratios[i], rd.ratio)
	}
	out := make([]FillRatio, fillRatioBuckets)
	learned := false
	for i, rs := range ratios {
		b := FillRatio{Lo: float64(i) * FillRatioBucketWidth, Hi: float64(i+1) * FillRatioBucketWidth, Windows: len(rs)}
		if len(rs) < MinFillRatioWindows {
			b.Thin, b.Value = true, global
		} else {
			slices.Sort(rs)
			b.Learned = max(1, rs[int(math.Ceil(ratioPercentile*float64(len(rs))))-1])
			b.Value, learned = b.Learned, true
		}
		if i > 0 {
			b.Value = max(b.Value, out[i-1].Value)
		}
		out[i] = b
	}
	if !learned {
		return nil
	}
	return out
}
