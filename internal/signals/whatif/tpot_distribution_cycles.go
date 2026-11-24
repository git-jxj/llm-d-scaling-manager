package whatif

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The TPOT distribution's fit on cycles (cycle_fits.go): its one free
// parameter, the renewal coefficient C, fitted by interval-censored
// likelihood over every bucket of the router's TPOT histogram on the cycles'
// steps, checked on two self-check folds, and the window it is priced on.
// The capacity planner calls it on a recording's cycles, the slo analyzer on
// the controller's own.

// MinTPOTDistFitSteps is the fewest fit steps the distribution is fitted on:
// two hours of five-minute steps.
const MinTPOTDistFitSteps = 24

// tpotDistFolds are the self-check folds' cuts of the fit window's steps:
// each fits C on the steps before it and is scored on the steps after.
var tpotDistFolds = []float64{0.5, 0.75}

// TPOTDistReport is the experimental TPOT distribution's fit, as a
// backtest's answer carries it (whatifapi.BacktestResponse.TPOTDistribution)
// and the slo analyzer logs it.
type TPOTDistReport struct {
	// Model is TPOTRenewalLognormal; Experimental is always true.
	Model        string `json:"model"`
	Experimental bool   `json:"experimental"`
	// Abstained is why the distribution prices nothing, "" where it
	// prices: every TPOT percentile is then the planner's own, flagged
	// TPOTDistributionAbstained.
	Abstained string `json:"abstained,omitempty"`
	// Assumed is the fit in words, also in the answer's assumed lines.
	Assumed string `json:"assumed"`
	// C is the fitted renewal coefficient, Renewal its floor from the
	// speculative decoding (RenewalCoefficient; 0 without), and AtBound
	// says C ended at MaxRenewalC.
	C       float64 `json:"c"`
	Renewal float64 `json:"renewal"`
	AtBound bool    `json:"atBound,omitempty"`
	// FoldC is the C each self-check fold fits alone; FitSteps the fit's
	// steps.
	FoldC    []float64 `json:"foldC"`
	FitSteps int       `json:"fitSteps"`
	// SpecTokens is the decode engine's num_speculative_tokens (0 none or
	// not read), Accepted the drafts a step accepted over the fit window,
	// DecodeSlots its max-num-seqs, and SlotWait the Erlang C share of a
	// rank's sequences that would wait for a slot at the fit window's
	// highest mean running per rank (SlotWait).
	SpecTokens  int     `json:"specTokens"`
	Accepted    float64 `json:"accepted"`
	DecodeSlots int     `json:"decodeSlots"`
	SlotWait    float64 `json:"slotWait"`
	// Fold is the folds' score on the steps each did not see, and Test the
	// test's, at the histogram's resolution.
	Fold []TPOTDistScore `json:"fold"`
	Test *TPOTDistScore  `json:"test,omitempty"`
}

// TPOTDistScore is the distribution scored at the router histogram's
// resolution over Steps steps: the median and p90 over steps of KS, each
// step's largest gap between the predicted and the measured share under a
// bucket edge; and the steps whose measured p90 or p95 fell in a bucket
// holding the predicted one, beside the planner's own percentile's (Default).
// ShapeKSMedian and ShapeKSP90 are KS with the distribution placed at the
// measured mean: the shape's miss alone, the mean's taken out.
type TPOTDistScore struct {
	Steps            int     `json:"steps"`
	KSMedian         float64 `json:"ksMedian"`
	KSP90            float64 `json:"ksP90"`
	ShapeKSMedian    float64 `json:"shapeKsMedian"`
	ShapeKSP90       float64 `json:"shapeKsP90"`
	P90Inside        int     `json:"p90Inside"`
	P95Inside        int     `json:"p95Inside"`
	DefaultP90Inside *int    `json:"defaultP90Inside,omitempty"`
	DefaultP95Inside *int    `json:"defaultP95Inside,omitempty"`
}

// TPOTDistFitted is the distribution fitted on cycles.
type TPOTDistFitted struct {
	C      float64
	Report TPOTDistReport
	// StepRates is each fit step's mean arrival rate over the cycles it
	// holds that read one, beside the steps the fit read: the loads the fit
	// window served.
	StepRates []float64
}

// TPOTWindowOf is the router's per-request TPOT histogram of one reading,
// as R_q is learned from it (TPOTRatioFromWindows): bucket rates for counts,
// which give the same quantile and mean. ok is false with no bucket.
func TPOTWindowOf(rl *domain.RouterLatency) (TPOTWindow, bool) {
	if rl == nil || len(rl.TPOTBuckets) == 0 {
		return TPOTWindow{}, false
	}
	w := TPOTWindow{Sum: rl.TPOTSumRate, Count: rl.TPOTCountRate, SumMeasured: true,
		Buckets: make([]HistogramBucket, len(rl.TPOTBuckets))}
	for i, b := range rl.TPOTBuckets {
		w.Buckets[i] = HistogramBucket{UpperEdge: b.UpperBound, Count: b.Rate}
	}
	return w, true
}

// RouterTPOTStepAt is the step anchored at the cycle anchor: its router TPOT
// histogram and mean over the router's window, and the decode cycles of
// cycles in that window. ok is false where the router's TPOT or the output
// lengths are not read.
func RouterTPOTStepAt(cycles []FitCycle, anchor *FitCycle) (TPOTDistStep, bool) {
	if !anchor.Latency || anchor.Router == nil {
		return TPOTDistStep{}, false
	}
	r := anchor.Router
	if !(r.TPOTCountRate > 0) || len(r.TPOTBuckets) == 0 {
		return TPOTDistStep{}, false
	}
	w := RouterWindowOf(r)
	at := anchor.At
	edges, cum := make([]float64, len(r.TPOTBuckets)), make([]float64, len(r.TPOTBuckets))
	for i, b := range r.TPOTBuckets {
		edges[i], cum[i] = b.UpperBound, b.Rate
	}
	return TPOTDistStepOf(edges, cum, w, r.TPOTSumRate/r.TPOTCountRate,
		DecodeCyclesIn(cycles, at.Add(-time.Duration(w*float64(time.Second))), at))
}

// FitTPOTDistCycles fits the distribution on cycles, cut into steps of step,
// each anchored at its last cycle; slots is the decode engine's max-num-seqs
// and spec its num_speculative_tokens.
func FitTPOTDistCycles(cycles []FitCycle, step time.Duration, slots, spec int) *TPOTDistFitted {
	q := &TPOTDistFitted{Report: TPOTDistReport{Model: TPOTRenewalLognormal, Experimental: true, FoldC: []float64{},
		Fold: []TPOTDistScore{}, SpecTokens: spec, DecodeSlots: slots}}
	cycles = sortedByTime(cycles)
	order, anchors := stepAnchors(cycles, step)
	rates := map[time.Time][2]float64{}
	for i := range cycles {
		if s := &cycles[i]; s.RateKnown {
			k := s.At.UTC().Truncate(step)
			r := rates[k]
			rates[k] = [2]float64{r[0] + s.Rate, r[1] + 1}
		}
	}
	var steps []TPOTDistStep
	var accepted []float64
	busiest := 0.0
	for _, k := range order {
		a := anchors[k]
		st, ok := RouterTPOTStepAt(cycles, &cycles[a])
		if !ok {
			continue
		}
		steps = append(steps, st)
		if r := rates[k]; r[1] > 0 {
			q.StepRates = append(q.StepRates, r[0]/r[1])
		}
		c := cycles[a].Decode
		busiest = max(busiest, c.MeanRunning)
		var w, s float64
		for _, g := range c.Groups {
			if g.AcceptedKnown {
				w += g.Rate
				s += g.Rate * g.Accepted
			}
		}
		if w > 0 {
			accepted = append(accepted, s/w)
		}
	}
	q.Report.FitSteps = len(steps)
	if len(accepted) > 0 {
		slices.Sort(accepted)
		q.Report.Accepted = accepted[len(accepted)/2]
	}
	q.Report.Renewal = RenewalCoefficient(spec, q.Report.Accepted)
	if slots > 0 {
		q.Report.SlotWait = SlotWait(slots, busiest)
	}
	var why []string
	switch {
	case len(steps) < MinTPOTDistFitSteps:
		why = append(why, fmt.Sprintf("%d fit steps read the router's TPOT histogram and the decode pods' output lengths, under %d",
			len(steps), MinTPOTDistFitSteps))
	case slots <= 0:
		why = append(why, "the decode engine's max-num-seqs not read: whether sequences wait for a slot is not known")
	case q.Report.SlotWait > MaxSlotWait:
		why = append(why, fmt.Sprintf("%.1f%% of a decode rank's sequences would wait for one of its %d slots (Erlang C at %.3g running): "+
			"a slot queue the distribution leaves out", 100*q.Report.SlotWait, slots, busiest))
	}
	if len(why) > 0 {
		q.Report.Abstained = strings.Join(why, "; ")
		return q
	}
	c, atBound, err := FitTPOTDist(steps, q.Report.Renewal)
	if err != nil {
		q.Report.Abstained = err.Error()
		return q
	}
	q.C, q.Report.C, q.Report.AtBound = c, c, atBound
	for _, cut := range tpotDistFolds {
		n := int(math.Round(cut * float64(len(steps))))
		fc, fb, ferr := FitTPOTDist(steps[:n], q.Report.Renewal)
		if ferr != nil {
			q.Report.Abstained = ferr.Error()
			return q
		}
		atBound = atBound || fb
		q.Report.FoldC = append(q.Report.FoldC, fc)
		var sc TPOTDistScorer
		for _, st := range steps[n:] {
			sc.Add(TPOTDist{GroupCV2: st.GroupCV2, C: fc}, st, st.Mean, nil)
		}
		q.Report.Fold = append(q.Report.Fold, sc.Score())
	}
	if atBound {
		q.Report.Abstained = fmt.Sprintf("renewal coefficient not identified (the fit, or a self-check fold's, ends at its bound %g)", MaxRenewalC)
		return q
	}
	q.Report.Assumed = fmt.Sprintf("experimental TPOT distribution %s: a request's TPOT is lognormal around the planner's mean, "+
		"log-variance the decode groups' spread plus C E[1/(L-1)] over the step's output lengths (the renewal of %d-token speculative "+
		"steps), C %.3g fitted by interval-censored likelihood on every bucket of the fit window's router TPOT histogram "+
		"(floor %.3g; self-check folds %s); no slot queue (Erlang C %.2g%% at %d slots)",
		TPOTRenewalLognormal, spec, c, q.Report.Renewal, joinFloats(q.Report.FoldC), 100*q.Report.SlotWait, slots)
	return q
}

// joinFloats is xs in three significant digits, comma separated.
func joinFloats(xs []float64) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%.3g", x)
	}
	return strings.Join(parts, ", ")
}

// TPOTDistScorer pools steps' scores at the histogram's resolution.
type TPOTDistScorer struct {
	ks, shape            []float64
	p90, p95, dp90, dp95 int
	// compared says the planner's own percentiles were scored beside.
	compared bool
	n        int
}

// Add scores d at st around mean; def is the planner's own p90 and p95,
// nil where not compared. It returns the step's KS and whether its p90 and
// p95 fell in the measured one's bucket.
func (s *TPOTDistScorer) Add(d TPOTDist, st TPOTDistStep, mean float64, def []*float64) (float64, bool, bool) {
	ks, in := d.BucketScore(st, mean, 0.9, 0.95)
	s.ks = append(s.ks, ks)
	shape, _ := d.BucketScore(st, st.Mean)
	s.shape = append(s.shape, shape)
	s.n++
	if in[0] {
		s.p90++
	}
	if in[1] {
		s.p95++
	}
	if len(def) == 2 {
		s.compared = true
		for j, q := range []float64{0.9, 0.95} {
			if def[j] != nil && InsideMeasuredBucket(st, q, *def[j]) {
				if j == 0 {
					s.dp90++
				} else {
					s.dp95++
				}
			}
		}
	}
	return ks, in[0], in[1]
}

// InsideMeasuredBucket says v lies in the bucket of st holding its measured
// q quantile.
func InsideMeasuredBucket(st TPOTDistStep, q, v float64) bool {
	total := 0.0
	for _, c := range st.Counts {
		total += c
	}
	cum, lo := 0.0, 0.0
	for i, e := range st.Edges {
		prev := cum / total
		cum += st.Counts[i]
		if prev < q && cum/total >= q {
			return v > lo && v <= e
		}
		lo = e
	}
	return false
}

// Score is the pooled score.
func (s *TPOTDistScorer) Score() TPOTDistScore {
	out := TPOTDistScore{Steps: s.n, P90Inside: s.p90, P95Inside: s.p95}
	if s.compared {
		dp90, dp95 := s.dp90, s.dp95
		out.DefaultP90Inside, out.DefaultP95Inside = &dp90, &dp95
	}
	out.KSMedian, out.KSP90 = medianAndP90(s.ks)
	out.ShapeKSMedian, out.ShapeKSP90 = medianAndP90(s.shape)
	return out
}

// medianAndP90 is the median and the nearest-rank p90 of xs, 0 with none.
func medianAndP90(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[len(s)/2], s[min(len(s)-1, int(math.Ceil(0.9*float64(len(s))))-1)]
}

// TPOTWindowFit is the distribution fitted on a window of cycles for pricing
// at any load, as the capacity planner's main view prices it and the slo
// analyzer prices its current load: a percentile is the mean times a ratio
// that does not move with the rate (Ratio).
type TPOTWindowFit struct {
	// Fitted is the fit; Abstained why it prices nothing, "" where it
	// prices.
	Fitted    *TPOTDistFitted
	Abstained string
	// Dist is the fitted distribution and Inv the window's E[1/(L-1)],
	// pooled over its cycles: what every rate is priced at. Top is the
	// highest step rate fitted, requests a second.
	Dist TPOTDist
	Inv  float64
	Top  float64
}

// FitTPOTWindow fits the distribution on cycles, cut into steps of step,
// with the decode engine's max-num-seqs slots and num_speculative_tokens
// spec.
func FitTPOTWindow(cycles []FitCycle, step time.Duration, slots, spec int) TPOTWindowFit {
	tq := FitTPOTDistCycles(cycles, step, slots, spec)
	w := TPOTWindowFit{Fitted: tq}
	if tq.Report.Abstained != "" {
		w.Abstained = tq.Report.Abstained
		return w
	}
	sorted := sortedByTime(cycles)
	first, last := sorted[0].At, sorted[len(sorted)-1].At
	inv, g2, ok := DecodeWindow(DecodeCyclesIn(cycles, first.Add(-time.Nanosecond), last))
	if !ok {
		w.Abstained = "the source's decode pods read no output-length histogram"
		return w
	}
	w.Inv, w.Dist = inv, TPOTDist{GroupCV2: g2, C: tq.C, Renewal: tq.Report.Renewal}
	for _, rate := range tq.StepRates {
		w.Top = max(w.Top, rate)
	}
	return w
}

// Ratio is the q percentile over the mean, the same at every rate; 0 where
// the fit abstained.
func (w TPOTWindowFit) Ratio(q float64) float64 {
	if w.Abstained != "" {
		return 0
	}
	return w.Dist.Quantile(1, w.Inv, q)
}
