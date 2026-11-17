package main

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The report on one fleet (capacity-planner at-replicas, POST /v1/report):
// one replay of the recording, then every figure, for every shape and every
// load, priced from the replayed state on the paths replicas-to-slo and
// slo-to-rate take (servedState, servedFleet). A stated shape is priced as
// --assume-input-tokens and --assume-output-tokens price one. The report adds
// no model of its own: the verdict compares those figures.

// reportQuestion is the command's name, as the JSON names its question.
const reportQuestion = "at-replicas"

// defaultLoads are the tables' rates when none is asked.
var defaultLoads = []string{"50%", "80%", "100%", "seen"}

// tailPastHistogram is a TTFT percentile above maxPromptTailQ on a prompt
// spread no histogram measured: the tail rests on the assumed spread.
const tailPastHistogram whatif.Flag = "TailPastPromptHistogram"

// extrapolatedShape is a stated shape whose IL or OL lies outside the range
// of per-cycle means the recording saw.
const extrapolatedShape whatif.Flag = "ExtrapolatedShape"

// decodeOnly is a report whose prefill side does not price: its verdict and
// its figures judge decode alone, and it gives no fleet rate at the SLO.
const decodeOnly whatif.Flag = "DecodeOnly"

// assumeInputTokensWords names the flag a stated prompt length came from in
// the assumed words (ttft.go); the report says --shape in its place.
const assumeInputTokensWords = "(--assume-input-tokens)"

// maxPromptTailQ is the highest TTFT percentile priced without the flag on an
// assumed or clamped prompt spread: p95, the highest the SLO analyzer's
// targets are judged at on the goldens.
const maxPromptTailQ = 0.95

// cellTTFTQ is the TTFT percentile a summary cell prints.
const cellTTFTQ = 0.9

// decodeKneeNotValidated is the decode knee's standing on the goldens
// (docs/proposals/slo-capacity-planner.md, "Measured on the goldens").
const decodeKneeNotValidated = "decode knee: not validated; it read 44% to 103% high on the goldens " +
	"(docs/proposals/slo-capacity-planner.md, Measured on the goldens)"

// e2eNote says what the e2e column is.
const e2eNote = "e2e mean is a sum, TTFT mean + (OL - 1) x TPOT mean, not a percentile"

// noSLONote is the verdict's basis with no target at all.
const noSLONote = "no SLO target (the policy sets none; pass --ttft or --tpot): judged on headroom alone"

// reporter prices one report from one replayed state.
type reporter struct {
	st *learnreplay.State
	s  *servedState
	q  whatifapi.ReportQuery
	// headroom is the operating point's fraction of the fleet knee, and
	// headroomSource where it came from.
	headroom       float64
	headroomSource string
	// ttft and tpot are the targets judged by; nil with none.
	ttft, tpot             *whatifapi.ReportTarget
	ttftSource, tpotSource string
	// load is the rate judged at, 0 with none, and loadWords it in words
	// ("the p95 load of 41.7 r/s").
	load      float64
	loadWords string
	// loadStat is the statistic the load was read at ("p95", "stated").
	loadStat string
	fleets   map[fleetKey]*servedFleet
	// shape is the index of the shape priced, sh the shape.
	shape int
	sh    whatifapi.ReportShapeAsk
	// results is every figure the shape prints, for its flags and the exit
	// code; assumed is every assumed input, once each.
	results []whatif.Result
	assumed []string
	pv      pricedVariant
}

// fleetKey names a fleet priced: the shape, the counts and the statistics.
type fleetKey struct {
	shape        int
	np, nd       int
	ttftQ, tpotQ float64
}

// Report is POST /v1/report on the replayed state.
func (s *servedState) Report(ctx context.Context, q whatifapi.ReportQuery) (*whatifapi.ReportResponse, error) {
	return buildReport(ctx, s.st, s, q)
}

// buildReport prices the report q asks of st: every shape and every load
// from the one replayed state.
func buildReport(ctx context.Context, st *learnreplay.State, s *servedState, q whatifapi.ReportQuery) (*whatifapi.ReportResponse, error) {
	r := &reporter{st: st, s: s, q: q, fleets: map[fleetKey]*servedFleet{}}
	r.resolveTargets()
	if len(r.q.Shapes) == 0 {
		r.q.Shapes = []whatifapi.ReportShapeAsk{{Measured: true, Label: "measured"}}
	}
	if len(r.q.Loads) == 0 {
		for _, l := range defaultLoads {
			a, _ := whatifapi.ParseReportLoad(l)
			r.q.Loads = append(r.q.Loads, a)
		}
	}
	// LoadSeenX is an alias for a growth load: a column and a verdict of its
	// own, never the load the fleet is judged at.
	if x := r.q.LoadSeenX; x > 0 && x != 1 && !slices.ContainsFunc(r.q.Loads, func(l whatifapi.ReportLoadAsk) bool {
		return l.Grown() && l.X == x
	}) {
		r.q.Loads = append(slices.Clone(r.q.Loads), whatifapi.GrowthLoad(x))
	}
	r.sh = r.q.Shapes[0]
	base, err := r.fleet(q.Prefill, q.Decode, r.sloTTFTQ(), r.sloTPOTQ())
	if err != nil {
		return nil, err
	}
	np, nd := r.counts(base)
	out := &whatifapi.ReportResponse{Question: reportQuestion, Shapes: []whatifapi.ReportShape{}, NotValidated: []string{}}
	out.Source = whatifapi.ReportSource{Namespace: st.Namespace, Model: st.ModelID, Cycle: st.Cycle, TotalCycles: st.Cycles,
		FirstAt: st.FirstAt, LastAt: st.At}
	out.Fleet = whatifapi.ReportFleet{Prefill: np, Decode: nd, PrefillVariant: base.pf.variant, DecodeVariant: base.pv.state.Variant,
		PrefillFrom: from(q.Prefill), DecodeFrom: from(q.Decode)}
	r.loadOf(&out.Load)
	out.SLO = r.sloOf()
	for _, l := range r.q.Loads {
		out.Loads = append(out.Loads, l.Label)
	}
	code := whatif.ExitAnswered
	ttftPriced := false
	for i, sh := range r.q.Shapes {
		r.shape, r.sh, r.results, r.assumed = i, sh, nil, nil
		rs, c, priced, err := r.shapeReport(ctx, np, nd)
		if err != nil {
			return nil, err
		}
		out.Shapes = append(out.Shapes, rs)
		code = max(code, c)
		ttftPriced = ttftPriced || priced
	}
	out.NotValidated = append(out.NotValidated, decodeKneeNotValidated)
	if ttftPriced {
		out.NotValidated = append(out.NotValidated, footerNote([]*pricedFleet{base.pf}))
	}
	out.NotValidated = append(out.NotValidated, e2eNote)
	out.ExitCode = code
	return out, nil
}

// shapeReport prices the fleet at the shape r.sh: its exit code, and whether
// its TTFT priced.
func (r *reporter) shapeReport(ctx context.Context, np, nd int) (whatifapi.ReportShape, int, bool, error) {
	base, err := r.fleet(r.q.Prefill, r.q.Decode, r.sloTTFTQ(), r.sloTPOTQ())
	if err != nil {
		return whatifapi.ReportShape{}, 0, false, err
	}
	r.pv = base.pv
	rs := whatifapi.ReportShape{Label: r.sh.Label, InputTokens: r.pv.shape.AvgInputTokens, OutputTokens: r.pv.shape.AvgOutputTokens,
		PrefixHitRate: r.pv.shape.PrefixHitRate, Source: measuredText(r.pv), Flags: []string{}, Assumed: []string{}}
	if !r.sh.Measured {
		rs.Source = sourceAsked
		if why := r.extrapolated(); why != "" {
			rs.Extrapolated, rs.ExtrapolatedWhy = true, why
			r.results = append(r.results, whatif.Answer{Flags: []whatif.Flag{extrapolatedShape}})
		}
	}
	r.addAssumed(base.Assumed()...)
	ev, err := r.evaluate(np, nd)
	if err != nil {
		return rs, 0, false, err
	}
	if err := ctx.Err(); err != nil {
		return rs, 0, false, err
	}
	if ev.kneeOK && !ev.prefillPriced {
		r.results = append(r.results, whatif.Answer{Flags: []whatif.Flag{decodeOnly}})
	}
	r.capacity(&rs.Capacity, base, ev)
	r.use(&rs.Utilization, base, ev)
	if rs.Table, err = r.table(ctx, ev); err != nil {
		return rs, 0, false, err
	}
	if rs.Verdict, err = r.verdict(np, nd, ev); err != nil {
		return rs, 0, false, err
	}
	if rs.Growth, err = r.growth(np, nd); err != nil {
		return rs, 0, false, err
	}
	rs.Flags = flagNamesOf(r.results)
	for _, w := range r.assumed {
		// A stated shape is priced as --assume-input-tokens prices a prompt.
		rs.Assumed = append(rs.Assumed, strings.ReplaceAll(w, assumeInputTokensWords, "(--shape)"))
	}
	if rs.Assumed == nil {
		rs.Assumed = []string{}
	}
	code := whatif.ExitCode(ev.prefillKnee, ev.decodeKnee, ev.ttftAt, ev.tpotAt)
	if code == whatif.ExitInternal {
		code = whatif.ExitAnswered
	}
	return rs, code, base.pf.refused == nil, nil
}

// extrapolated says how a stated shape lies outside the per-cycle means the
// recording saw; "" inside them.
func (r *reporter) extrapolated() string {
	rg := r.pv.state.ShapeRange
	if rg.Cycles == 0 {
		return "the recording saw no traffic to compare it with"
	}
	var out []string
	if il := r.sh.IL; il < rg.ILMin || il > rg.ILMax {
		out = append(out, fmt.Sprintf("IL %s outside the %s-%s seen", num(round(il)), num(round(rg.ILMin)), num(round(rg.ILMax))))
	}
	if ol := r.sh.OL; ol < rg.OLMin || ol > rg.OLMax {
		out = append(out, fmt.Sprintf("OL %s outside the %s-%s seen", num(round(ol)), num(round(rg.OLMin)), num(round(rg.OLMax))))
	}
	// A stated hit rate is extrapolated past the one seen as a length is; the
	// tolerance only absorbs the per-cycle means' rounding.
	if h := r.sh.Hit; h != nil && (*h < rg.HitMin-hitTolerance || *h > rg.HitMax+hitTolerance) {
		out = append(out, fmt.Sprintf("prefix hit %.2f outside the %.2f-%.2f seen", *h, rg.HitMin, rg.HitMax))
	}
	return strings.Join(out, "; ")
}

// growth judges the fleet at each growth load asked, the load seen times X:
// a what-if beside the verdict at the load seen, which it never changes.
func (r *reporter) growth(np, nd int) ([]whatifapi.ReportGrowth, error) {
	out := []whatifapi.ReportGrowth{}
	if !(r.load > 0) {
		return out, nil
	}
	seen, words := r.load, r.loadWords
	defer func() { r.load, r.loadWords = seen, words }()
	for _, l := range r.q.Loads {
		if !l.Grown() {
			continue
		}
		r.load = l.X * seen
		r.loadWords = fmt.Sprintf("%s x the %s load (%s)", num(l.X), r.loadStat, rateWords(r.load))
		ev, err := r.evaluate(np, nd)
		if err != nil {
			return nil, err
		}
		v, err := r.verdict(np, nd, ev)
		if err != nil {
			return nil, err
		}
		out = append(out, whatifapi.ReportGrowth{Load: l.Label, X: l.X, Rate: r.load, Verdict: v})
	}
	return out, nil
}

// hitTolerance is how far a stated prefix-hit rate may lie outside the range
// seen and not be extrapolated.
const hitTolerance = 0.005

// from names where a count came from: 0 is the recording's.
func from(asked int) string {
	if asked > 0 {
		return sourceAsked
	}
	return "recording"
}

// resolveTargets reads the headroom and the targets: the query's, else the
// policy's.
func (r *reporter) resolveTargets() {
	p := r.st.Policy
	switch {
	case r.q.Headroom > 0:
		r.headroom, r.headroomSource = r.q.Headroom, sourceAsked
	case p != nil && p.ScaleUpThreshold > 0:
		r.headroom, r.headroomSource = p.ScaleUpThreshold, "scaleUpThreshold"
	default:
		r.headroom, r.headroomSource = config.DefaultScaleUpThreshold, "default"
	}
	r.ttft, r.ttftSource = r.q.TTFT, sourceAsked
	if r.ttft == nil && p != nil && p.SLO != nil && p.SLO.TTFT != nil {
		r.ttft = &whatifapi.ReportTarget{Seconds: p.SLO.TTFT.Target.Seconds(), Percentile: float64(p.SLO.TTFTPercentile()) / 100}
		r.ttftSource = "policy"
	}
	r.tpot, r.tpotSource = r.q.TPOT, sourceAsked
	if r.tpot == nil && p != nil && p.SLO != nil && p.SLO.TPOT != nil {
		r.tpot = &whatifapi.ReportTarget{Seconds: p.SLO.TPOT.Target.Seconds()}
		r.tpotSource = "policy"
	}
	if len(r.q.TTFTPercentiles) == 0 {
		r.q.TTFTPercentiles = []float64{0.5, 0.9}
	}
}

// sloTTFTQ is the TTFT statistic the target is judged at: the target's, else
// the first the table asks for.
func (r *reporter) sloTTFTQ() float64 {
	if r.ttft != nil {
		return r.ttft.Percentile
	}
	return r.q.TTFTPercentiles[0]
}

// sloTPOTQ is the TPOT statistic the target is judged at: 0 for the mean.
func (r *reporter) sloTPOTQ() float64 {
	if r.tpot != nil {
		return r.tpot.Percentile
	}
	return r.q.TPOTPercentile
}

// fleet is the fleet at the shape priced, np prefill and nd decode replicas
// (0 the recorded count), at the statistics asked; one per key.
func (r *reporter) fleet(np, nd int, ttftQ, tpotQ float64) (*servedFleet, error) {
	k := fleetKey{r.shape, np, nd, ttftQ, tpotQ}
	if sf, ok := r.fleets[k]; ok {
		return sf, nil
	}
	assume := r.q.Assume
	if tpotQ == 0 {
		// R_q prices a percentile only: on the mean it is not read.
		assume.TPOTRatio = nil
	}
	kn := whatifapi.Knobs{Prefill: np, Decode: nd, TTFTPercentile: ttftQ, TPOTPercentile: tpotQ,
		Assume: assume, Headroom: r.headroom, ArrivalCV2: r.q.ArrivalCV2}
	if !r.sh.Measured {
		kn.PromptTokens, kn.OutputTokens, kn.PrefixHit = r.sh.IL, r.sh.OL, r.sh.Hit
	}
	sf, err := r.s.fleetAt(kn)
	if err != nil {
		return nil, err
	}
	r.fleets[k] = sf
	return sf, nil
}

// counts is the base fleet's replica counts as priced.
func (r *reporter) counts(base *servedFleet) (np, nd int) {
	nd = base.pv.decode.Replicas
	switch {
	case r.q.Prefill > 0:
		np = r.q.Prefill
	case base.pf.refused == nil:
		np = base.pf.fleet.Prefill.Replicas
	default:
		for _, ps := range r.st.Prefill {
			if base.pf.variant == "" || ps.Variant == base.pf.variant {
				np = ps.Pods
				break
			}
		}
	}
	return np, nd
}

// addAssumed adds words to the assumed list, once each.
func (r *reporter) addAssumed(words ...string) {
	for _, w := range words {
		if !slices.Contains(r.assumed, w) {
			r.assumed = append(r.assumed, w)
		}
	}
}

// loadOf is the load the fleet is judged at: the query's, else the statistic
// asked (p95 by default) of the per-cycle arrival rate over the window's
// cycles that read one above 0. A growth load is a what-if of its own
// (shapeReport), never the load judged at. The mean, p95 and
// peak are reported whichever is judged, and the busiest RollingWindow.
func (r *reporter) loadOf(l *whatifapi.ReportLoad) {
	l.Cycles = len(r.st.ArrivalRates)
	l.Statistic = r.q.LoadStatistic
	if l.Statistic == "" {
		l.Statistic = whatifapi.DefaultLoadStatistic
	}
	var rates []float64
	for _, x := range r.st.ArrivalRates {
		if whatif.Finite(x) && x > 0 {
			rates = append(rates, x)
		}
	}
	l.CyclesWithTraffic = len(rates)
	stats := map[string]float64{}
	if len(rates) > 0 {
		stats = loadStats(rates)
		mean, p95, peak := stats[whatifapi.LoadMean], stats[whatifapi.LoadP95], stats[whatifapi.LoadPeak]
		l.Mean, l.P95, l.Peak = &mean, &p95, &peak
		l.Rolling = rollingPeak(r.st.ArrivalRates, cycleSeconds(r.st), whatifapi.RollingWindow.Seconds())
	}
	var seen float64
	switch {
	case r.q.LoadSeen > 0:
		seen, l.Source, l.Statistic = r.q.LoadSeen, sourceAsked, loadStated
	case len(rates) > 0:
		seen, l.Source = stats[l.Statistic], "observed"
	default:
		l.Source = "none"
		l.Rate = whatifapi.FigureOf(whatif.Unanswerable{Reason: whatif.NoTraffic,
			Detail: fmt.Sprintf("no arrival rate above 0 in the %d cycles read; pass --load-seen", l.Cycles)}, unitRate, false)
		return
	}
	l.Seen = &seen
	r.load, r.loadStat = seen, l.Statistic
	l.Rate = whatifapi.FigureOf(whatif.Answer{Value: r.load}, unitRate, false)
	r.loadWords = fmt.Sprintf("the %s load of %s", l.Statistic, rateWords(r.load))
}

// loadStated is the load's statistic when the question stated the load.
const loadStated = "stated"

// loadStats is the mean, the p95 (nearest rank) and the highest of rates,
// by statistic name.
func loadStats(rates []float64) map[string]float64 {
	s := slices.Clone(rates)
	slices.Sort(s)
	var sum float64
	for _, x := range s {
		sum += x
	}
	rank := int(math.Ceil(0.95 * float64(len(s))))
	return map[string]float64{whatifapi.LoadMean: sum / float64(len(s)), whatifapi.LoadP95: s[max(rank, 1)-1],
		whatifapi.LoadPeak: s[len(s)-1]}
}

// cycleSeconds is the mean time between the state's cycles, 0 with fewer
// than two.
func cycleSeconds(st *learnreplay.State) float64 {
	n := len(st.ArrivalRates)
	if n < 2 || !st.At.After(st.FirstAt) {
		return 0
	}
	return st.At.Sub(st.FirstAt).Seconds() / float64(n-1)
}

// rollingPeak is the highest mean of rates over consecutive cycles spanning
// window seconds, a cycle every cycle seconds; idle cycles count as 0. Nil
// when the cycle time is unknown or the window holds fewer cycles than that.
func rollingPeak(rates []float64, cycle, window float64) *whatifapi.ReportRolling {
	if !(cycle > 0) {
		return nil
	}
	k := max(int(math.Round(window/cycle)), 1)
	if len(rates) < k {
		return nil
	}
	var sum, peak float64
	for i, x := range rates {
		if whatif.Finite(x) && x > 0 {
			sum += x
		}
		if i >= k {
			if y := rates[i-k]; whatif.Finite(y) && y > 0 {
				sum -= y
			}
		}
		if i >= k-1 {
			peak = max(peak, sum/float64(k))
		}
	}
	return &whatifapi.ReportRolling{WindowSeconds: float64(k) * cycle, Cycles: k, Peak: peak}
}

// rateWords is a rate in words: "41.7 r/s".
func rateWords(x float64) string { return fmt.Sprintf("%.1f r/s", x) }

// secondsWords is a latency in words: "3.9 s", or "640 ms" below a second.
func secondsWords(x float64) string {
	if x >= 1 {
		return fmt.Sprintf("%.1f s", x)
	}
	return fmt.Sprintf("%.0f ms", 1000*x)
}

// fleetEval is one fleet checked at the load: its knees, the SLO figures at
// the load, and whether it holds them with headroom.
type fleetEval struct {
	np, nd                  int
	prefillKnee, decodeKnee whatif.Result
	// knee is the fleet knee, the lower of the roles' that priced; kneeOK
	// says one priced, and prefillPriced that the prefill knee did. binds
	// names the role that sets it.
	knee          float64
	kneeOK        bool
	prefillPriced bool
	binds         string
	// ttftAt and tpotAt are the targets' statistics at the load, nil with no
	// target or no load.
	ttftAt, tpotAt whatif.Result
	// holds and known are the check: with headroom, every target met; known
	// false when a figure it needs was refused for its data.
	holds, known bool
	// under is a known miss: the load past the headroom (pastOperating, the
	// operating point op), or a target missed (slo says which).
	under         bool
	pastOperating bool
	op            float64
	slo           sloAt
	reason        string
}

// sloAt is the targets judged at rate on sf: each target's figure, whether
// every one is met and whether that is known, and the words.
type sloAt struct {
	ttft, tpot  whatif.Result
	met, known  bool
	saturated   bool
	missed      bool
	parts       []string
	targetCount int
	// ttftMissed and tpotMissed say which target was missed; onset that the
	// TTFT miss is the flow-control onset's, its figure within the target.
	ttftMissed, tpotMissed, onset bool
	// unknown is the first target not judged, and its figure.
	unknown    string
	unknownFig whatif.Result
}

// sloAtRate judges the targets at rate on sf.
func (r *reporter) sloAtRate(sf *servedFleet, rate float64, words bool) sloAt {
	s := sloAt{met: true, known: true}
	fold := func(label string, fig whatif.Result, target float64, met, known, onset bool) bool {
		s.targetCount++
		if u, ok := fig.(whatif.Unanswerable); ok && (u.Reason == whatif.Saturated || u.Reason == whatif.AboveDecodeOnset) {
			s.saturated = true
		}
		missed := false
		switch {
		case !known:
			s.met, s.known = false, false
			if s.unknown == "" {
				s.unknown, s.unknownFig = label, fig
			}
		case !met:
			s.met, s.missed, missed = false, true, true
		}
		s.parts = append(s.parts, r.judged(label, fig, target, met, known, onset))
		return missed
	}
	if r.ttft != nil {
		t, w := sf.TTFTPoint(rate)
		if words {
			r.addAssumed(w...)
		}
		s.ttft = t
		met, known := whatif.TTFTMeets(t, whatif.TTFTTarget{Seconds: r.ttft.Seconds, Percentile: r.ttft.Percentile})
		onset := false
		if v, ok := t.(whatif.TTFT); ok && !met && v.Seconds <= r.ttft.Seconds && v.HasFlag(whatif.AboveFlowControlOnset) {
			onset = true
		}
		s.ttftMissed = fold("TTFT "+whatifapi.ReportStatisticName(r.ttft.Percentile), t, r.ttft.Seconds, met, known, onset)
		s.onset = onset
	}
	if r.tpot != nil {
		mean, pct := sf.TPOTAt(rate)
		t := mean
		if r.tpot.Percentile > 0 {
			t = pct
		}
		s.tpot = t
		met, known := tpotMeets(t, r.tpot.Seconds)
		s.tpotMissed = fold("TPOT "+whatifapi.ReportStatisticName(r.tpot.Percentile), t, r.tpot.Seconds, met, known, false)
	}
	// A known miss decides whatever else was not judged.
	if s.missed {
		s.known = true
	}
	return s
}

// evaluate prices the fleet at np prefill and nd decode replicas at the load.
// An error is a fleet that could not be priced at all, not a refused figure.
func (r *reporter) evaluate(np, nd int) (fleetEval, error) {
	e := fleetEval{np: np, nd: nd}
	sf, err := r.fleet(np, nd, r.sloTTFTQ(), r.sloTPOTQ())
	if err != nil {
		return e, fmt.Errorf("pricing %s: %w", fleetText(np, nd), err)
	}
	e.prefillKnee, e.decodeKnee = sf.PrefillKnee(), sf.DecodeKnee()
	pk, pok := e.prefillKnee.(whatif.Answer)
	dk, dok := e.decodeKnee.(whatif.Answer)
	e.prefillPriced = pok
	switch {
	case dok && pok && pk.Value < dk.Value:
		e.knee, e.kneeOK, e.binds = pk.Value, true, rolePrefill
	case dok:
		e.knee, e.kneeOK, e.binds = dk.Value, true, roleDecodeWord
	}
	if !e.kneeOK {
		e.reason = "the decode knee is not priced: " + refusedText(e.decodeKnee.(whatif.Unanswerable))
		return e, nil
	}
	if !(r.load > 0) {
		e.reason = "no load to judge at"
		return e, nil
	}
	var parts []string
	e.known, e.holds = true, true
	e.op = r.headroom * e.knee
	if r.load > e.op {
		e.holds, e.under, e.pastOperating = false, true, true
		if r.load >= e.knee {
			parts = append(parts, fmt.Sprintf("%.2f r/s is %.1fx the fleet knee of %.2f r/s", r.load, r.load/e.knee, e.knee))
		} else {
			parts = append(parts, fmt.Sprintf("%.2f r/s is above %s x the fleet knee %.2f r/s = %.2f r/s", r.load, num(r.headroom), e.knee, e.op))
		}
	} else {
		parts = append(parts, fmt.Sprintf("%.2f r/s within %s x the fleet knee %.2f r/s = %.2f r/s", r.load, num(r.headroom), e.knee, e.op))
	}
	if !e.prefillPriced {
		parts = append(parts, "prefill not priced ("+reasonOf(e.prefillKnee)+")")
	}
	s := r.sloAtRate(sf, r.load, true)
	e.ttftAt, e.tpotAt, e.slo = s.ttft, s.tpot, s
	parts = append(parts, s.parts...)
	switch {
	case s.targetCount == 0:
		parts = append(parts, "no SLO target")
	case !s.met && s.known:
		e.holds, e.under = false, true
	case !s.met:
		e.holds, e.known = false, false
	}
	// A known miss decides the check whatever else was not judged.
	e.known = e.known || e.under
	e.reason = strings.Join(parts, "; ")
	return e, nil
}

// roleDecodeWord is the decode role as the report names it.
const roleDecodeWord = "decode"

// tpotMeets reads a TPOT figure against a target: a refusal for saturation
// is a known miss, any other an unknown.
func tpotMeets(r whatif.Result, target float64) (met, known bool) {
	switch v := r.(type) {
	case whatif.Answer:
		return v.Value <= target, true
	case whatif.Unanswerable:
		if v.Reason == whatif.Saturated {
			return false, true
		}
	}
	return false, false
}

// judged is one target's check in words, with its numbers.
func (r *reporter) judged(label string, fig whatif.Result, target float64, met, known, onset bool) string {
	switch {
	case !known:
		return fmt.Sprintf("%s not judged (%s)", label, reasonOf(fig))
	case onset:
		return fmt.Sprintf("%s %s is within %s but misses: the rate is above the prefill flow-control onset, where the router queues",
			label, secondsText(fig), msText(target))
	case met:
		return fmt.Sprintf("%s %s meets %s", label, secondsText(fig), msText(target))
	}
	return fmt.Sprintf("%s %s misses %s", label, secondsText(fig), msText(target))
}

// secondsText is a latency figure in ms, or its refusal's reason.
func secondsText(r whatif.Result) string {
	if v, ok := valueOf(r); ok {
		return msText(v)
	}
	return reasonOf(r)
}

// valueOf is an answered figure's value.
func valueOf(r whatif.Result) (float64, bool) {
	switch v := r.(type) {
	case whatif.Answer:
		return v.Value, true
	case whatif.TTFT:
		return v.Seconds, true
	}
	return 0, false
}

// reasonOf is a refusal's reason, "saturated" for saturation, "" on an
// answer.
func reasonOf(r whatif.Result) string {
	u, ok := r.(whatif.Unanswerable)
	switch {
	case !ok:
		return ""
	case u.Reason == whatif.Saturated:
		return "saturated"
	}
	return string(u.Reason)
}

// capacity fills the capacity rows.
func (r *reporter) capacity(c *whatifapi.ReportLimits, base *servedFleet, ev fleetEval) {
	c.Headroom, c.HeadroomSource = r.headroom, r.headroomSource
	ol := r.pv.shape.AvgOutputTokens
	r.results = append(r.results, ev.prefillKnee, ev.decodeKnee)
	c.PrefillKnee = r.rate(ev.prefillKnee, ol, prefillBinds(base))
	c.DecodeKnee = r.rate(ev.decodeKnee, ol, decodeBinds(ev.decodeKnee))
	if !ev.kneeOK {
		c.FleetKnee = r.rate(ev.decodeKnee, ol, "")
		c.Operating = c.FleetKnee
		c.MaxAtSLO = c.FleetKnee
		return
	}
	binds := ev.binds
	if !ev.prefillPriced {
		binds = "decode (prefill not priced)"
	}
	c.FleetKnee = r.rate(whatif.Answer{Value: ev.knee}, ol, binds)
	c.Operating = r.rate(whatif.Answer{Value: r.headroom * ev.knee}, ol, fmt.Sprintf("%s x fleet knee (%s)", num(r.headroom), r.headroomSource))
	maxRate, binds := r.maxAtSLO(base, ev)
	r.results = append(r.results, maxRate)
	c.MaxAtSLO = r.rate(maxRate, ol, binds)
}

// rate is a rate row: the figure, the output tokens it carries, what binds.
func (r *reporter) rate(res whatif.Result, ol float64, binds string) whatifapi.ReportRate {
	out := whatifapi.ReportRate{Figure: whatifapi.FigureOf(res, unitRate, false), Binds: binds}
	if v, ok := valueOf(res); ok {
		tok := v * ol
		out.OutputTokensPerSecond = &tok
	}
	return out
}

// prefillBinds says what sets the prefill knee.
func prefillBinds(sf *servedFleet) string {
	if sf.pf.refused != nil {
		return ""
	}
	p := sf.pf.fleet.Prefill
	return fmt.Sprintf("prefill compute: N_p %d x V_P / (f %.3g x %s tokens)", p.Replicas, sf.pf.in.Imbalance, num(round(p.PromptTokens)))
}

// decodeBinds says what sets the decode knee.
func decodeBinds(r whatif.Result) string {
	a, ok := r.(whatif.Answer)
	if !ok {
		return ""
	}
	return fmt.Sprintf("decode %s at fill %.3f", boundText(a.Basis.BoundBy), a.Basis.Fill)
}

// maxAtSLO is slo-to-rate's fleet rate at the targets with no headroom: the
// highest rate that holds them. With no TTFT target, decode's rate at the
// TPOT target bounded by the prefill knee. With a prefill side that does not
// price there is none, as slo-to-rate gives no fleet row: decode alone does
// not bound the fleet.
func (r *reporter) maxAtSLO(base *servedFleet, ev fleetEval) (whatif.Result, string) {
	tpotSeconds := 0.0
	if r.tpot != nil {
		tpotSeconds = r.tpot.Seconds
	}
	if u, ok := ev.prefillKnee.(whatif.Unanswerable); ok {
		u.Detail = "no fleet rate: the prefill side is not priced, and decode alone does not bound the fleet (slo-to-rate gives none either): " + u.Detail
		return u, ""
	}
	if r.ttft != nil && base.pf.refused == nil {
		lim := base.Limits(r.ttft.Seconds, tpotSeconds, r.headroom)
		r.addAssumed(lim.Notes...)
		if a, ok := lim.WithoutHeadroom.(whatif.Answer); ok {
			return a, boundText(a.Basis.BoundBy)
		}
		return lim.WithoutHeadroom, ""
	}
	switch {
	case r.tpot == nil && r.ttft != nil:
		// The TTFT target alone, on a prefill side that does not price.
		return *base.pf.refused, ""
	case r.tpot == nil:
		return whatif.Unanswerable{Reason: whatif.InvalidInput, Detail: "no SLO target to bound a rate"}, ""
	}
	d := base.decodeRate(tpotSeconds, 1)
	a, ok := d.(whatif.Answer)
	if !ok {
		return d, ""
	}
	binds := "decode " + boundText(a.Basis.BoundBy)
	if pk, ok := ev.prefillKnee.(whatif.Answer); ok && pk.Value < a.Value {
		a.Value, binds = pk.Value, "prefill knee"
	}
	return a, binds
}

// use fills the utilization at the load.
func (r *reporter) use(u *whatifapi.ReportUse, base *servedFleet, ev fleetEval) {
	d := base.pv.decode
	u.Decode.BandTop = d.Band.Hi
	slots := slotsPerReplica(d)
	u.Decode.Slots = slots * int64(d.Replicas)
	if !(r.load > 0) {
		f := whatifapi.FigureOf(whatif.Unanswerable{Reason: whatif.NoTraffic, Detail: "no load to judge at"}, "", false)
		u.Decode.KVFill, u.Decode.SlotsOccupied, u.Decode.WaitProbability = f, f, f
		u.Prefill.Pooled, u.Prefill.WorstPod = f, f
		return
	}
	fill := whatif.FillAtRate(d, r.load)
	u.Decode.KVFill = whatifapi.FigureOf(fill, "", false)
	if a, ok := fill.(whatif.Answer); ok {
		seqs := math.Min(a.Value*d.CapacityTokens/d.KVPerRequest, float64(slots)) * float64(d.Replicas)
		u.Decode.SlotsOccupied = whatifapi.FigureOf(whatif.Answer{Value: seqs}, "", false)
	} else {
		u.Decode.SlotsOccupied = u.Decode.KVFill
	}
	switch a := whatif.DecodeAdmission(d, r.load).(type) {
	case whatif.Admission:
		u.Decode.WaitProbability = whatifapi.FigureOf(whatif.Answer{Value: a.WaitProbability, Flags: a.Flags}, "", false)
		r.results = append(r.results, whatif.Answer{Value: a.WaitProbability, Flags: a.Flags})
	case whatif.Unanswerable:
		u.Decode.WaitProbability = whatifapi.FigureOf(a, "", false)
	}
	pk, ok := ev.prefillKnee.(whatif.Answer)
	if !ok {
		f := whatifapi.FigureOf(ev.prefillKnee, "", false)
		u.Prefill.Pooled, u.Prefill.WorstPod = f, f
		return
	}
	// The busiest pod's utilization is the load over the prefill knee, which
	// is N_p x V_P (less the margin) / (f x the prompt); pooled, f is 1.
	f := max(base.pf.in.Imbalance, 1)
	worst := r.load / pk.Value
	u.Prefill.Imbalance = f
	u.Prefill.WorstPod = whatifapi.FigureOf(whatif.Answer{Value: worst}, "", false)
	u.Prefill.Pooled = whatifapi.FigureOf(whatif.Answer{Value: worst / f}, "", false)
}

// slotsPerReplica is the batch slots of one decode replica: the cap, or the
// whole requests the band's top holds where that is fewer, as the knee and
// the admission chain read it.
func slotsPerReplica(d whatif.Decode) int64 {
	if !(d.KVPerRequest > 0) {
		return d.ReplicaCap
	}
	kv := int64(math.Floor(d.Band.Hi * d.CapacityTokens / d.KVPerRequest))
	if d.ReplicaCap > 0 && d.ReplicaCap < kv {
		return d.ReplicaCap
	}
	return kv
}

// sloOf is the targets judged by.
func (r *reporter) sloOf() whatifapi.ReportSLO {
	var s whatifapi.ReportSLO
	if r.ttft != nil {
		s.TTFT = &whatifapi.ReportObjective{Statistic: whatifapi.ReportStatisticName(r.ttft.Percentile), Target: r.ttft.Seconds, Source: r.ttftSource}
	}
	if r.tpot != nil {
		s.TPOT = &whatifapi.ReportObjective{Statistic: whatifapi.ReportStatisticName(r.tpot.Percentile), Target: r.tpot.Seconds, Source: r.tpotSource}
	}
	return s
}

// table is the shape's SLO against load at the base fleet, one row per load
// asked, in the order asked: every row from the same replayed state.
func (r *reporter) table(ctx context.Context, ev fleetEval) ([]whatifapi.ReportRow, error) {
	np0, nd0, tq := r.q.Prefill, r.q.Decode, r.q.TPOTPercentile
	mean, err := r.fleet(np0, nd0, 0, tq)
	if err != nil {
		return nil, err
	}
	slo, err := r.fleet(np0, nd0, r.sloTTFTQ(), r.sloTPOTQ())
	if err != nil {
		return nil, err
	}
	cellTTFT, err := r.fleet(np0, nd0, cellTTFTQ, tq)
	if err != nil {
		return nil, err
	}
	rows := []whatifapi.ReportRow{}
	for _, l := range r.q.Loads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row := whatifapi.ReportRow{Load: l.Label, TTFT: []whatifapi.ReportStat{}}
		switch {
		case l.Grown() && r.load > 0:
			row.Rate, row.Label = l.X*r.load, "load "+row.Load
		case l.Seen && r.load > 0:
			row.Rate, row.Label = r.load, "load "+row.Load
		case l.Seen:
			continue
		case l.Share > 0 && ev.kneeOK:
			row.Rate, row.Label = l.Share*ev.knee, l.Label+" of knee"
			if l.Share == 1 {
				// The 100% row is the knee itself, not a product that rounds off it.
				row.Rate = ev.knee
			}
		case l.Share > 0:
			continue
		default:
			row.Rate, row.Label = l.Rate, l.Label
		}
		if ev.kneeOK {
			share := row.Rate / ev.knee
			if l.Share > 0 {
				share = l.Share
			}
			row.ShareOfKnee = &share
		}
		for _, q := range r.q.TTFTPercentiles {
			sf, err := r.fleet(np0, nd0, q, tq)
			if err != nil {
				return nil, err
			}
			row.TTFT = append(row.TTFT, r.ttftStat(sf, q, row.Rate))
		}
		tm, tp := mean.TPOTAt(row.Rate)
		r.results = append(r.results, tm)
		row.TPOTMean = whatifapi.FigureOf(tm, unitSeconds, true)
		if tq > 0 {
			r.results = append(r.results, tp)
			row.TPOTPercentile = &whatifapi.ReportStat{Statistic: whatifapi.ReportStatisticName(tq), Figure: whatifapi.FigureOf(tp, unitSeconds, true)}
		}
		// What an assumed term does to one point is said once, at the load
		// (evaluate), not at every row.
		ttftMean, _ := mean.TTFTPoint(row.Rate)
		row.E2EMeanSum = whatifapi.FigureOf(e2eSum(ttftMean, tm, r.pv.shape.AvgOutputTokens), unitSeconds, true)
		row.Cell = r.cell(slo, cellTTFT, row)
		rows = append(rows, row)
	}
	return rows, nil
}

// ttftStat is the TTFT at q and rate on sf, flagged where the tail rests on
// an assumed prompt spread.
func (r *reporter) ttftStat(sf *servedFleet, q, rate float64) whatifapi.ReportStat {
	t, _ := sf.TTFTPoint(rate)
	if q > maxPromptTailQ && sf.pf.refused == nil && sf.pf.in.PromptCVSource != shapeMeasured {
		t = withFlags(t, []whatif.Flag{tailPastHistogram})
	}
	r.results = append(r.results, t)
	return whatifapi.ReportStat{Statistic: whatifapi.ReportStatisticName(q), Figure: whatifapi.FigureOf(t, unitSeconds, true)}
}

// cell is a row's summary cell: TTFT p90, the TPOT percentile (the mean with
// none asked), and the targets judged at the row's rate.
func (r *reporter) cell(slo, ttft *servedFleet, row whatifapi.ReportRow) whatifapi.ReportCell {
	c := whatifapi.ReportCell{}
	i := slices.IndexFunc(row.TTFT, func(s whatifapi.ReportStat) bool { return s.Statistic == "p90" })
	if i >= 0 {
		c.TTFT = row.TTFT[i]
	} else {
		c.TTFT = r.ttftStat(ttft, cellTTFTQ, row.Rate)
	}
	c.TPOT = whatifapi.ReportStat{Statistic: "mean", Figure: row.TPOTMean}
	// The percentile where it priced; else the mean, which a refused R_q
	// leaves answered.
	if row.TPOTPercentile != nil && row.TPOTPercentile.Value != nil {
		c.TPOT = *row.TPOTPercentile
	}
	s := r.sloAtRate(slo, row.Rate, false)
	switch {
	case s.saturated || (row.ShareOfKnee != nil && *row.ShareOfKnee >= 1):
		c.Mark = whatifapi.CellSaturated
	case s.targetCount == 0:
		c.Mark = whatifapi.CellNoTarget
	case s.met:
		c.Mark = whatifapi.CellMet
	case s.onset && !s.tpotMissed:
		c.Mark = whatifapi.CellOnset
	case s.known:
		c.Mark = whatifapi.CellMissed
	default:
		c.Mark = whatifapi.CellUnknown
	}
	return c
}

// e2eSum is the mean e2e as a sum of means: TTFT mean + (OL - 1) x TPOT mean.
// A refused part refuses the sum with its reason.
func e2eSum(ttft, tpot whatif.Result, ol float64) whatif.Result {
	t, tok := valueOf(ttft)
	p, pok := valueOf(tpot)
	switch {
	case !tok:
		return ttft
	case !pok:
		return tpot
	}
	return whatif.Answer{Value: t + max(ol-1, 0)*p}
}

// verdict judges the fleet at the load: under-provisioned on a known miss;
// not judged when its own check is not, or when the prefill side does not
// price and so decode alone was judged; over-provisioned when one replica
// fewer of a role still holds every target with headroom at the same load;
// right-sized otherwise. Summary says it in one plain sentence.
func (r *reporter) verdict(np, nd int, base fleetEval) (whatifapi.ReportVerdict, error) {
	v := whatifapi.ReportVerdict{Scope: whatifapi.ScopeFleet, Shrink: []string{}, Grow: []string{},
		Checks: []whatifapi.ReportCheck{check(base)}}
	if base.kneeOK && !base.prefillPriced {
		v.Scope = whatifapi.ScopeDecodeOnly
	}
	r.results = append(r.results, base.ttftAt, base.tpotAt)
	v.Reason = fleetText(np, nd) + ": " + base.reason
	switch {
	case !base.kneeOK:
		v.Verdict = whatifapi.VerdictNotJudged
		v.Summary = fmt.Sprintf("%s: the decode knee is not priced (%s).", v.Verdict, refusalWords(base.decodeKnee))
		return v, nil
	case !(r.load > 0):
		v.Verdict = whatifapi.VerdictNotJudged
		v.Summary = v.Verdict + ": the window saw no traffic to judge at; state a load (--load-seen)."
		return v, nil
	case base.under:
		v.Verdict = whatifapi.VerdictUnder
		grow, err := r.grow(np, nd, base, &v)
		if err != nil {
			return v, err
		}
		cause, _, _ := r.underWords(base)
		v.Summary = fmt.Sprintf("%s: %s; %s.", v.Verdict, cause, grow)
		return v, nil
	case !base.known:
		v.Verdict = whatifapi.VerdictNotJudged
		v.Summary = fmt.Sprintf("%s: at %s, %s cannot be priced (%s).", v.Verdict, r.loadWords, base.slo.unknown,
			refusalWords(base.slo.unknownFig))
		return v, nil
	case !base.prefillPriced:
		v.Verdict = whatifapi.VerdictNotJudged
		v.Reason += ". The prefill side is not priced: decode alone cannot show the fleet right-sized or over-provisioned"
		v.Summary = fmt.Sprintf("%s: the prefill side is not priced (%s); decode alone holds %s at %s.", v.Verdict,
			refusalWords(base.prefillKnee), r.targetWords(), r.loadWords)
		return v, nil
	}
	var fewer []fleetEval
	if np > 1 {
		e, err := r.evaluate(np-1, nd)
		if err != nil {
			return v, err
		}
		fewer = append(fewer, e)
	}
	if nd > 1 {
		e, err := r.evaluate(np, nd-1)
		if err != nil {
			return v, err
		}
		fewer = append(fewer, e)
	}
	var held, failed []string
	restsOnDecode := false
	for _, e := range fewer {
		v.Checks = append(v.Checks, check(e))
		role := roleDecodeWord
		if e.np < np {
			role = rolePrefill
		}
		if e.holds {
			v.Shrink = append(v.Shrink, role)
			held = append(held, fleetText(e.np, e.nd)+" holds: "+e.reason)
			restsOnDecode = restsOnDecode || role == roleDecodeWord || e.binds == roleDecodeWord
			continue
		}
		failed = append(failed, fleetText(e.np, e.nd)+" does not: "+e.reason)
	}
	if len(v.Shrink) > 0 {
		v.Verdict = whatifapi.VerdictOver
		v.Reason = fmt.Sprintf("one %s replica fewer still holds every target at %.2f r/s with headroom. %s",
			strings.Join(v.Shrink, " or one "), r.load, strings.Join(held, ". "))
		v.Summary = fmt.Sprintf("%s: at %s, one %s replica fewer still holds %s with headroom", v.Verdict, r.loadWords,
			strings.Join(v.Shrink, " or one "), r.targetWords())
		if restsOnDecode {
			// The decode knee reads high on the goldens: a decode replica
			// removed on it is the step most exposed to that.
			v.Reason += ". " + decodeKneeNotValidated
			v.Summary += ", but the decode knee this rests on is not validated and has read 44% to 103% high"
		}
		v.Summary += "."
		return v, nil
	}
	v.Verdict = whatifapi.VerdictRight
	if len(failed) > 0 {
		v.Reason += ". " + strings.Join(failed, ". ")
		v.Summary = fmt.Sprintf("%s: at %s the fleet holds %s with headroom, and one replica fewer of any role would not.",
			v.Verdict, r.loadWords, r.targetWords())
	} else {
		v.Reason += ". No role has a replica to spare"
		v.Summary = fmt.Sprintf("%s: at %s the fleet holds %s with headroom, and no role has a replica to spare.",
			v.Verdict, r.loadWords, r.targetWords())
	}
	if r.ttft == nil && r.tpot == nil {
		v.Reason += ". " + noSLONote
	}
	return v, nil
}

// targetWords names what a holding fleet holds: every target, or its
// operating point with none set.
func (r *reporter) targetWords() string {
	if r.ttft == nil && r.tpot == nil {
		return "its operating point (no SLO target is set)"
	}
	return "every target"
}

// underWords is why an under-provisioned fleet misses, in words, the role
// to add replicas of, and why that role: the knee's role when the miss is
// saturation, decode for TPOT, and for TTFT the role whose wait is the larger
// part of it at the load.
func (r *reporter) underWords(e fleetEval) (cause, role, why string) {
	s := e.slo
	knee := e.binds + " sets the knee"
	switch {
	case e.pastOperating && r.load >= e.knee:
		return fmt.Sprintf("at %s, %.1fx the fleet knee of %s, the fleet saturates", r.loadWords, r.load/e.knee, rateWords(e.knee)),
			e.binds, knee
	case e.pastOperating:
		return fmt.Sprintf("at %s the fleet runs past its operating point of %s (%s of its %s knee)", r.loadWords,
			rateWords(e.op), num(r.headroom), rateWords(e.knee)), e.binds, knee
	case s.ttftMissed && s.onset:
		return fmt.Sprintf("at %s TTFT %s misses", r.loadWords, whatifapi.ReportStatisticName(r.ttft.Percentile)),
			rolePrefill, "the rate is past the prefill flow-control onset, where the router queues"
	case s.ttftMissed:
		role, why := ttftRole(s.ttft, e)
		return r.missWords("TTFT", r.ttft, s.ttft), role, why
	case s.tpotMissed:
		return r.missWords("TPOT", r.tpot, s.tpot), roleDecodeWord, "decode sets TPOT"
	}
	return "at " + r.loadWords + " a target is missed", e.binds, knee
}

// ttftRole is the role that sets a missed TTFT at the load, and why: the
// prefill queue or decode admission, whichever mean wait is the larger; the
// knee's role when the TTFT is refused for saturation.
func ttftRole(t whatif.Result, e fleetEval) (role, why string) {
	v, ok := t.(whatif.TTFT)
	if !ok {
		return e.binds, e.binds + " sets the knee, and TTFT saturates there"
	}
	p := v.Parts
	prefillWait, decodeWait := p.EngineWait+p.RouterWait+p.BurstWait, p.DecodeWait+p.KVPullWait
	if decodeWait > prefillWait {
		return roleDecodeWord, fmt.Sprintf("decode admission, a %s mean wait, is the larger wait in TTFT", secondsWords(decodeWait))
	}
	return rolePrefill, fmt.Sprintf("the prefill queue, a %s mean wait, is the larger wait in TTFT", secondsWords(prefillWait))
}

// missWords is one missed target in words.
func (r *reporter) missWords(name string, t *whatifapi.ReportTarget, fig whatif.Result) string {
	label := name + " " + whatifapi.ReportStatisticName(t.Percentile)
	if v, ok := valueOf(fig); ok {
		return fmt.Sprintf("at %s %s is %s against a %s target", r.loadWords, label, secondsWords(v), secondsWords(t.Seconds))
	}
	return fmt.Sprintf("at %s %s is saturated", r.loadWords, label)
}

// grow checks one more replica of each role that prices at the load, adds
// the fleets tried to v's checks and the roles whose one more replica holds
// to v.Grow, and says what to add.
func (r *reporter) grow(np, nd int, base fleetEval, v *whatifapi.ReportVerdict) (string, error) {
	type try struct {
		role   string
		np, nd int
	}
	tries := []try{{roleDecodeWord, np, nd + 1}}
	if base.prefillPriced {
		tries = []try{{rolePrefill, np + 1, nd}, {roleDecodeWord, np, nd + 1}}
	}
	for _, t := range tries {
		e, err := r.evaluate(t.np, t.nd)
		if err != nil {
			return "", err
		}
		v.Checks = append(v.Checks, check(e))
		if e.holds {
			v.Grow = append(v.Grow, t.role)
		}
	}
	_, role, why := r.underWords(base)
	if slices.Contains(v.Grow, role) {
		return fmt.Sprintf("add %s replicas (%s): one more makes it hold", role, why), nil
	}
	return fmt.Sprintf("add %s replicas (%s): one more is not enough", role, why), nil
}

// refusalWords is why a figure is refused, in words a reader acts on and
// with no reason code; "" on an answer.
func refusalWords(res whatif.Result) string {
	u, ok := res.(whatif.Unanswerable)
	if !ok {
		return ""
	}
	switch u.Reason {
	case whatif.NoPrefillCapacity:
		return "the recording learned no prefill speed; --assume-vp states one"
	case whatif.NoFixedLatency:
		return "the recording learned no fixed prefill latency; --assume-a states one"
	case whatif.NoImbalance:
		return "the recording learned no prefill imbalance between pods"
	case whatif.NoPrefillReplicas:
		return "the fleet has no prefill replica"
	case whatif.FlowControlUnknown:
		return "the router's flow-control thresholds are not known"
	case whatif.Saturated, whatif.AboveDecodeOnset:
		return "the fleet is saturated there"
	case whatif.NoTPOTHistogram, whatif.TPOTBucketTooWide, whatif.TPOTQuantileUnbounded:
		return "the TPOT histogram cannot give that percentile; judge TPOT at the mean"
	case whatif.NoTraffic:
		return "the recording saw no traffic of this shape"
	case whatif.NoLine, whatif.PinnedDefaultB:
		return "the recording learned no usable decode latency line"
	case whatif.NoCapacity, whatif.NoBatchCap:
		return "the decode replicas' capacity is not known"
	}
	return "the figure is refused; the checks give the reason"
}

// check is one fleet's check on the wire.
func check(e fleetEval) whatifapi.ReportCheck {
	return whatifapi.ReportCheck{Prefill: e.np, Decode: e.nd, Holds: e.holds, Known: e.known, Reason: e.reason}
}

// fleetText names a fleet.
func fleetText(np, nd int) string {
	return fmt.Sprintf("%d prefill + %d decode", np, nd)
}

// flagNamesOf is every flag the results carry, once each, in the order they
// first appear.
func flagNamesOf(results []whatif.Result) []string {
	out := []string{}
	for _, r := range results {
		var from []whatif.Flag
		switch v := r.(type) {
		case whatif.Answer:
			from = v.Flags
		case whatif.TTFT:
			from = v.Flags
		case whatif.Unanswerable:
			from = v.Flags
		}
		for _, f := range from {
			if !slices.Contains(out, string(f)) {
				out = append(out, string(f))
			}
		}
	}
	return out
}

// measuredText says where a measured shape came from.
func measuredText(pv pricedVariant) string {
	m := pv.state.Shape
	if m.Cycles == 0 {
		return fmt.Sprintf("no traffic by cycle %d", pv.cycle)
	}
	return fmt.Sprintf("measured, cycles %d-%d", m.FromCycle, m.ToCycle)
}

// sourceAsked is a figure the question stated rather than the source read.
const sourceAsked = "asked"
