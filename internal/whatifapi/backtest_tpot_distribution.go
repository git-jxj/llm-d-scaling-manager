package whatifapi

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// This file prices a backtest's TPOT percentiles with the experimental
// per-request TPOT distribution (whatif.TPOTRenewalLognormal), where the
// request asks for it (BacktestRequest.TPOTModel). The mean TPOT stays the
// planner's: the distribution is placed around it. Its one free parameter,
// the renewal coefficient C, is fitted on the fit window's steps by
// interval-censored likelihood over every bucket of the router's TPOT
// histogram, and checked on the fit window's two self-check folds; no
// percentile is fitted, so p90 and p95 are both held out. Each step is also
// scored at the histogram's resolution (TPOTDistCheck): the predicted share
// under every bucket edge against the measured one. Every percentile it
// prices is flagged whatif.TPOTDistribution and reads OUTSIDE VALIDATED
// REGION.

// MinTPOTDistFitSteps is the fewest fit steps the distribution is fitted on:
// two hours of five-minute steps.
const MinTPOTDistFitSteps = 24

// tpotDistFolds are the self-check folds' cuts of the fit window's steps:
// each fits C on the steps before it and is scored on the steps after.
var tpotDistFolds = []float64{0.5, 0.75}

// TPOTDistReport is the experimental TPOT distribution's fit, as a
// backtest's answer carries it (BacktestResponse.TPOTDistribution).
type TPOTDistReport struct {
	// Model is whatif.TPOTRenewalLognormal; Experimental is always true.
	Model        string `json:"model"`
	Experimental bool   `json:"experimental"`
	// Abstained is why the distribution prices nothing, "" where it
	// prices: every TPOT percentile is then the planner's own, flagged
	// whatif.TPOTDistributionAbstained.
	Abstained string `json:"abstained,omitempty"`
	// Assumed is the fit in words, also in the answer's assumed lines.
	Assumed string `json:"assumed"`
	// C is the fitted renewal coefficient, Renewal its floor from the
	// speculative decoding (whatif.RenewalCoefficient; 0 without), and
	// AtBound says C ended at whatif.MaxRenewalC.
	C       float64 `json:"c"`
	Renewal float64 `json:"renewal"`
	AtBound bool    `json:"atBound,omitempty"`
	// FoldC is the C each self-check fold fits alone; FitSteps the fit's
	// steps.
	FoldC    []float64 `json:"foldC"`
	FitSteps int       `json:"fitSteps"`
	// MinFitSteps is the fewest fit steps the distribution was allowed to
	// fit on, where the request set one other than MinTPOTDistFitSteps
	// (BacktestRequest.TPOTDistMinFitSteps). ShortFit says it was fitted
	// on fewer than MinTPOTDistFitSteps all the same: every percentile it
	// prices is flagged whatif.TPOTDistributionShortFit.
	MinFitSteps int  `json:"minFitSteps,omitempty"`
	ShortFit    bool `json:"shortFit,omitempty"`
	// SpecTokens is the decode engine's num_speculative_tokens (0 none or
	// not read), Accepted the drafts a step accepted over the fit window,
	// DecodeSlots its max-num-seqs, and SlotWait the Erlang C share of a
	// rank's sequences that would wait for a slot at the fit window's
	// highest mean running per rank (whatif.SlotWait).
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

// TPOTDistCheck is the distribution at one test step: its log-variance and
// E[1/(L-1)], and its score at the histogram's resolution where measured.
type TPOTDistCheck struct {
	Sigma2 float64 `json:"sigma2"`
	Inv    float64 `json:"inv"`
	// KS is the largest gap under a bucket edge (TPOTDistScore), at the
	// predicted mean; P90Inside and P95Inside say the predicted percentile
	// lies in the measured one's bucket.
	KS        *float64 `json:"ks,omitempty"`
	P90Inside *bool    `json:"p90Inside,omitempty"`
	P95Inside *bool    `json:"p95Inside,omitempty"`
	// Refused is why the step was not priced, "" where it was; Abstained
	// says the model abstained on the whole backtest.
	Refused   string `json:"refused,omitempty"`
	Abstained bool   `json:"abstained,omitempty"`
	// planner is the planner's own p90 and p95 the distribution replaced,
	// for the score's comparison.
	planner []*float64
}

// tpotDistQ is a backtest's fitted distribution.
type tpotDistQ struct {
	c      float64
	report TPOTDistReport
	// stepRates is each fit step's mean arrival rate over the cycles it
	// holds that read one, beside the steps the fit read: the loads the
	// fit window served (tpot_model_main.go).
	stepRates []float64
}

// decodeWindow is the decode cycles of e at idx whose time is in (from, to].
func decodeWindow(e *inputsEntry, idx []int, from, to time.Time) []whatif.DecodeCycle {
	var out []whatif.DecodeCycle
	for _, i := range idx {
		if at := e.series[i].At; at.After(from) && !at.After(to) {
			out = append(out, e.decode[i])
		}
	}
	return out
}

// routerTPOTStep is the step anchored at the cycle a of e: its router TPOT
// histogram and mean over the router's window, and the decode cycles of that
// window. ok is false where the router's TPOT or the output lengths are not
// read.
func routerTPOTStep(e *inputsEntry, idx []int, a int) (whatif.TPOTDistStep, bool) {
	if a >= len(e.latency) || e.latency[a].Router == nil {
		return whatif.TPOTDistStep{}, false
	}
	r := e.latency[a].Router
	if !(r.TPOTCountRate > 0) || len(r.TPOTBuckets) == 0 {
		return whatif.TPOTDistStep{}, false
	}
	w := measureWindowOf(e.latency[a], MeasuredRouter)
	at := e.series[a].At
	edges, cum := make([]float64, len(r.TPOTBuckets)), make([]float64, len(r.TPOTBuckets))
	for i, b := range r.TPOTBuckets {
		edges[i], cum[i] = b.UpperBound, b.Rate
	}
	return whatif.TPOTDistStepOf(edges, cum, w, r.TPOTSumRate/r.TPOTCountRate,
		decodeWindow(e, idx, at.Add(-time.Duration(w*float64(time.Second))), at))
}

// fitTPOTDist fits the distribution on the fit entry's cycles at fitIdx, cut
// into steps of step, each anchored at its last cycle; slots is the decode
// engine's max-num-seqs and spec its num_speculative_tokens. minSteps is the
// fewest steps it fits on, 0 MinTPOTDistFitSteps.
func fitTPOTDist(fit *inputsEntry, fitIdx []int, step time.Duration, slots, spec, minSteps int) *tpotDistQ {
	q := &tpotDistQ{report: TPOTDistReport{Model: whatif.TPOTRenewalLognormal, Experimental: true, FoldC: []float64{},
		Fold: []TPOTDistScore{}, SpecTokens: spec, DecodeSlots: slots}}
	least := MinTPOTDistFitSteps
	if minSteps > 0 && minSteps != MinTPOTDistFitSteps {
		least, q.report.MinFitSteps = minSteps, minSteps
	}
	if len(fit.decode) != len(fit.series) {
		q.report.Abstained = "the fit source did not give the decode series"
		return q
	}
	idx := slices.Clone(fitIdx)
	slices.SortStableFunc(idx, func(a, b int) int { return fit.series[a].At.Compare(fit.series[b].At) })
	anchors := map[time.Time]int{}
	rates := map[time.Time][2]float64{}
	var order []time.Time
	for _, i := range idx {
		k := fit.series[i].At.UTC().Truncate(step)
		if _, ok := anchors[k]; !ok {
			order = append(order, k)
		}
		anchors[k] = i
		if s := fit.series[i]; s.RateKnown {
			r := rates[k]
			rates[k] = [2]float64{r[0] + s.Rate, r[1] + 1}
		}
	}
	var steps []whatif.TPOTDistStep
	var accepted []float64
	busiest := 0.0
	for _, k := range order {
		a := anchors[k]
		st, ok := routerTPOTStep(fit, idx, a)
		if !ok {
			continue
		}
		steps = append(steps, st)
		if r := rates[k]; r[1] > 0 {
			q.stepRates = append(q.stepRates, r[0]/r[1])
		}
		c := fit.decode[a]
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
	q.report.FitSteps = len(steps)
	if len(accepted) > 0 {
		slices.Sort(accepted)
		q.report.Accepted = accepted[len(accepted)/2]
	}
	q.report.Renewal = whatif.RenewalCoefficient(spec, q.report.Accepted)
	if slots > 0 {
		q.report.SlotWait = whatif.SlotWait(slots, busiest)
	}
	var why []string
	switch {
	case len(steps) < least:
		why = append(why, fmt.Sprintf("%d fit steps read the router's TPOT histogram and the decode pods' output lengths, under %d",
			len(steps), least))
	case slots <= 0:
		why = append(why, "the decode engine's max-num-seqs not read: whether sequences wait for a slot is not known")
	case q.report.SlotWait > whatif.MaxSlotWait:
		why = append(why, fmt.Sprintf("%.1f%% of a decode rank's sequences would wait for one of its %d slots (Erlang C at %.3g running): "+
			"a slot queue the distribution leaves out", 100*q.report.SlotWait, slots, busiest))
	}
	if len(why) > 0 {
		q.report.Abstained = strings.Join(why, "; ")
		return q
	}
	c, atBound, err := whatif.FitTPOTDist(steps, q.report.Renewal)
	if err != nil {
		q.report.Abstained = err.Error()
		return q
	}
	q.c, q.report.C, q.report.AtBound = c, c, atBound
	for _, cut := range tpotDistFolds {
		n := int(math.Round(cut * float64(len(steps))))
		fc, fb, ferr := whatif.FitTPOTDist(steps[:n], q.report.Renewal)
		if ferr != nil {
			q.report.Abstained = ferr.Error()
			return q
		}
		atBound = atBound || fb
		q.report.FoldC = append(q.report.FoldC, fc)
		var sc tpotDistScorer
		for _, st := range steps[n:] {
			sc.add(whatif.TPOTDist{GroupCV2: st.GroupCV2, C: fc}, st, st.Mean, nil)
		}
		q.report.Fold = append(q.report.Fold, sc.score())
	}
	if atBound {
		q.report.Abstained = fmt.Sprintf("renewal coefficient not identified (the fit, or a self-check fold's, ends at its bound %g)", whatif.MaxRenewalC)
		return q
	}
	q.report.Assumed = fmt.Sprintf("experimental TPOT distribution %s: a request's TPOT is lognormal around the planner's mean, "+
		"log-variance the decode groups' spread plus C E[1/(L-1)] over the step's output lengths (the renewal of %d-token speculative "+
		"steps), C %.3g fitted by interval-censored likelihood on every bucket of the fit window's router TPOT histogram "+
		"(floor %.3g; self-check folds %s); no slot queue (Erlang C %.2g%% at %d slots)",
		whatif.TPOTRenewalLognormal, spec, c, q.report.Renewal, joinFloats(q.report.FoldC), 100*q.report.SlotWait, slots)
	if len(steps) < MinTPOTDistFitSteps {
		q.report.ShortFit = true
		q.report.Assumed += fmt.Sprintf("; fitted on %d steps, under the default %d, down to the %d asked: every percentile it prices "+
			"is also flagged %s", len(steps), MinTPOTDistFitSteps, least, whatif.TPOTDistributionShortFit)
	}
	return q
}

// flags is the flags every percentile the distribution prices carries:
// TPOTDistribution, and TPOTDistributionShortFit where it was fitted on
// fewer steps than the default (TPOTDistReport.ShortFit).
func (r TPOTDistReport) flags() []whatif.Flag {
	if r.ShortFit {
		return []whatif.Flag{whatif.TPOTDistribution, whatif.TPOTDistributionShortFit}
	}
	return []whatif.Flag{whatif.TPOTDistribution}
}

// joinFloats is xs in three significant digits, comma separated.
func joinFloats(xs []float64) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%.3g", x)
	}
	return strings.Join(parts, ", ")
}

// assumedLines is the distribution's assumed lines for the answer.
func (tq *tpotDistQ) assumedLines() []string {
	if tq.report.Abstained != "" {
		return []string{fmt.Sprintf("experimental TPOT distribution %s abstained (%s): every TPOT percentile is the planner's own, flagged %s",
			whatif.TPOTRenewalLognormal, tq.report.Abstained, whatif.TPOTDistributionAbstained)}
	}
	return []string{tq.report.Assumed + "; no percentile is fitted (p90 and p95 are held out); every TPOT percentile is flagged " +
		string(whatif.TPOTDistribution) + " and is OUTSIDE VALIDATED REGION"}
}

// priceTPOTDist sets a step's TPOT percentiles from the distribution around
// the step's predicted mean, the step's output lengths and groups read over
// its window. Where it cannot price, the planner's figures stand, flagged.
func (q *backtestQuestion) priceTPOTDist(pred *prediction, out *BacktestStep, st stepOf) {
	flag := func(f whatif.Flag) {
		for i, s := range backtestStats {
			if s != 0 && !slices.Contains(pred.tpot[i].Flags, string(f)) {
				pred.tpot[i].Flags = append(pred.tpot[i].Flags, string(f))
			}
		}
	}
	if q.tpotDist.report.Abstained != "" {
		out.TPOTDist = &TPOTDistCheck{Abstained: true}
		flag(whatif.TPOTDistributionAbstained)
		return
	}
	at := q.test.series[st.anchor].At
	inv, g2, ok := whatif.DecodeWindow(decodeWindow(q.test, st.window, st.windowStart, at))
	mean := pred.tpot[0].Value
	switch {
	case !ok:
		out.TPOTDist = &TPOTDistCheck{Refused: "the step's decode pods read no output-length histogram"}
		flag(whatif.TPOTDistribution)
		return
	case mean == nil || !(*mean > 0):
		out.TPOTDist = &TPOTDistCheck{Refused: "the planner gave no mean TPOT to place the distribution around"}
		flag(whatif.TPOTDistribution)
		return
	}
	d := whatif.TPOTDist{GroupCV2: g2, C: q.tpotDist.c}
	out.TPOTDist = &TPOTDistCheck{Sigma2: d.Sigma2(inv), Inv: inv}
	for i, s := range backtestStats {
		if s == 0 {
			continue
		}
		if s == 0.9 || s == 0.95 {
			out.TPOTDist.planner = append(out.TPOTDist.planner, pred.tpot[i].Value)
		}
		pred.tpot[i] = validate(figureOf(whatif.Answer{Value: d.Quantile(*mean, inv, s), Flags: q.tpotDist.report.flags()},
			unitSeconds, true), whatif.FigurePercentileTPOT, s, whatif.FigureClass{}, 0)
		pred.tpotBound[i] = nil
	}
}

// tpotDistScorer pools steps' scores at the histogram's resolution.
type tpotDistScorer struct {
	ks, shape            []float64
	p90, p95, dp90, dp95 int
	// compared says the planner's own percentiles were scored beside.
	compared bool
	n        int
}

// add scores d at st around mean; def is the planner's own p90 and p95,
// nil where not compared.
func (s *tpotDistScorer) add(d whatif.TPOTDist, st whatif.TPOTDistStep, mean float64, def []*float64) (float64, bool, bool) {
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
			if def[j] != nil && insideMeasuredBucket(st, q, *def[j]) {
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

// insideMeasuredBucket says v lies in the bucket of st holding its measured
// q quantile.
func insideMeasuredBucket(st whatif.TPOTDistStep, q, v float64) bool {
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

// score is the pooled score.
func (s *tpotDistScorer) score() TPOTDistScore {
	out := TPOTDistScore{Steps: s.n, P90Inside: s.p90, P95Inside: s.p95}
	if s.compared {
		out.DefaultP90Inside, out.DefaultP95Inside = ptrInt(s.dp90), ptrInt(s.dp95)
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

// scoreTPOTDist scores every judged, priced step of out at the router
// histogram's resolution, steps[k] the layout of out.Steps[k].
func (q *backtestQuestion) scoreTPOTDist(steps []stepOf, out *BacktestResponse) {
	if q.tpotDist.report.Abstained != "" {
		return
	}
	var sc tpotDistScorer
	for k := range out.Steps {
		s := &out.Steps[k]
		chk := s.TPOTDist
		if chk == nil || chk.Refused != "" || s.Excluded != "" || steps[k].anchor < 0 {
			continue
		}
		st, ok := routerTPOTStep(q.test, steps[k].window, steps[k].anchor)
		mean := s.TPOT[0].Predicted.Value
		if !ok || mean == nil {
			continue
		}
		d := whatif.TPOTDist{GroupCV2: chk.Sigma2 - q.tpotDist.c*chk.Inv, C: q.tpotDist.c}
		ks, p90, p95 := sc.add(d, whatif.TPOTDistStep{Mean: st.Mean, Inv: chk.Inv, Edges: st.Edges, Counts: st.Counts}, *mean, chk.planner)
		chk.KS, chk.P90Inside, chk.P95Inside = ptr(ks), &p90, &p95
	}
	t := sc.score()
	q.tpotDist.report.Test = &t
}

// note is the distribution's score at the histogram's resolution in words.
func (tq *tpotDistQ) note() string {
	t := tq.report.Test
	if tq.report.Abstained != "" || t == nil || t.Steps == 0 {
		return fmt.Sprintf("Experimental TPOT distribution %s: not scored at the histogram's resolution (it priced no judged step).",
			whatif.TPOTRenewalLognormal)
	}
	return fmt.Sprintf("Experimental TPOT distribution %s at the router histogram's resolution, over %d judged steps: the largest gap "+
		"between the predicted and the measured share of requests under a bucket edge has median %.3f and p90 %.3f (%.3f and %.3f "+
		"placed at the measured mean: the shape alone); the predicted p90 lies in the bucket the measured p90 fell in on %d steps "+
		"(the planner's own ratio: %d), the p95 on %d (own: %d).",
		whatif.TPOTRenewalLognormal, t.Steps, t.KSMedian, t.KSP90, t.ShapeKSMedian, t.ShapeKSP90, t.P90Inside, deref(t.DefaultP90Inside),
		t.P95Inside, deref(t.DefaultP95Inside))
}

// deref is *p, 0 for nil.
func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
