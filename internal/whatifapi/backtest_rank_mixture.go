package whatifapi

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// This file prices a backtest's TTFT with the experimental rank-mixture model
// (whatif.QueueRankMixture), where the request asks for it
// (BacktestRequest.TTFTModel). The model is fitted on the fit window's steps,
// each step's inputs and its router-measured mean and p90, and every test
// step is priced from its own inputs: its rate and its prefill pods'
// computed-token and prompt-length histograms. Nothing else in the answer
// moves: TPOT, the exclusions and the scoring are the planner's. Every TTFT
// it prices is flagged whatif.RankMixtureTTFT, and reads OUTSIDE VALIDATED
// REGION.

// RankMixtureReport is the experimental model's fit, as a backtest's answer
// carries it (BacktestResponse.RankMixture).
type RankMixtureReport struct {
	// Model is whatif.QueueRankMixture; Experimental is always true.
	Model        string `json:"model"`
	Experimental bool   `json:"experimental"`
	// Abstained is why the model prices nothing, "" where it prices: an
	// identifiability check failed (a prompt slope not identified, the
	// spread not calibrated, the service line not identified, the ranks'
	// own rates or K not read) or the fit failed. Every TTFT is then the
	// planner's own, flagged whatif.RankMixtureAbstained. The fitted figures
	// below, where there are any, are diagnostics.
	Abstained string `json:"abstained,omitempty"`
	// Assumed is the fitted parameters in words, also in the answer's
	// assumed lines.
	Assumed string `json:"assumed"`
	// S0 and V are the service line, seconds and tokens a second.
	S0 float64 `json:"s0"`
	V  float64 `json:"v"`
	// ServiceProportional says the prompts did not spread enough to fit
	// S0 and V apart: S0 is 0 and V the computed tokens over the service
	// time (whatif.FitPSInversion).
	ServiceProportional bool `json:"serviceProportional,omitempty"`
	// Shares is each rank's share of the requests, busiest first;
	// ShareCV the busiest share's coefficient of variation over the fit's
	// steps (the shares' stability); EvenWithinRows the ranks whose own
	// rate the recording does not carry, split evenly from their row.
	Shares         []float64 `json:"shares"`
	ShareCV        float64   `json:"shareCv"`
	EvenWithinRows int       `json:"evenWithinRows"`
	// KV is the fitted KV-deferral wait (fitted, not derived); nil where
	// the queue-time histograms did not read it.
	KV *RankMixtureKV `json:"kv,omitempty"`
	// A0, Beta and Spread are the non-queue part Z = A0 + Spread Beta L,
	// seconds and seconds per prompt token; Beta is the sum of the three
	// slopes beside it. Spread is a calibration: 1 is what the model
	// expects, and its distance from 1 is what Beta misses.
	A0            float64 `json:"a0"`
	Beta          float64 `json:"beta"`
	BetaOverhead  float64 `json:"betaOverhead"`
	BetaDecode    float64 `json:"betaDecode"`
	BetaRemainder float64 `json:"betaRemainder"`
	// BetaUnidentified names the slopes the fit window's prompts did not
	// spread enough to fit: each is 0 (whatif.FitBeta).
	BetaUnidentified []string `json:"betaUnidentified,omitempty"`
	Spread           float64  `json:"spread"`
	// FoldSpread is the spread each self-check fold fits alone;
	// SpreadAtBound says the folds could not reach a p90 ratio of 1;
	// FitP90 is the fit window's median p90 ratio at the spread; FitSteps
	// the fit's steps.
	FoldSpread    []float64 `json:"foldSpread"`
	SpreadAtBound bool      `json:"spreadAtBound,omitempty"`
	// SpreadDefaulted says the fit window is shorter than
	// whatif.MinSpreadFitWindow: the spread is 1, not calibrated.
	// SpreadFloored says the folds fitted a spread under whatif.MinSpread,
	// and it is priced at that floor.
	SpreadDefaulted bool    `json:"spreadDefaulted,omitempty"`
	SpreadFloored   bool    `json:"spreadFloored,omitempty"`
	FitP90          float64 `json:"fitP90"`
	FitSteps        int     `json:"fitSteps"`
	// FitStepsBacklogged is the fit window's steps left out because they
	// were not stationary: their arrivals ran ahead of their completions
	// by more than MaxMixtureBacklog (backlogOf).
	FitStepsBacklogged int `json:"fitStepsBacklogged,omitempty"`
	// Queue is each rank's queue model (whatif.QueueLimitedProcessorSharing,
	// or processor sharing with Slots unknown); Slots its K, the prefill
	// engine's max-num-seqs (1 without chunked prefill), 0 not known.
	Queue string `json:"queue"`
	Slots int    `json:"slots"`
	// ZParts is Z measured part by part and FitMean the fit window's
	// median ratio of priced to measured mean: the variant's
	// (whatif.QueueRankMixtureNLLine) only, which calibrates nothing to the
	// router (backtest_rank_mixture_nl.go).
	ZParts  *RankMixtureZParts `json:"zParts,omitempty"`
	FitMean float64            `json:"fitMean,omitempty"`
	// Stratified says each rank's mean service and cs^2 were read from the
	// computed-token histogram itself, not from the rank's own draws
	// (BacktestRequest.RankMixtureStratified, on by default).
	Stratified bool `json:"stratified,omitempty"`
}

// RankMixtureKV is the fitted KV-deferral wait.
type RankMixtureKV struct {
	P0    float64 `json:"p0"`
	P1    float64 `json:"p1"`
	M0    float64 `json:"m0"`
	M1    float64 `json:"m1"`
	RhoLo float64 `json:"rhoLo"`
	RhoHi float64 `json:"rhoHi"`
}

// RankMixtureCheck is what one step's data says of the model's assumptions
// (whatif.RankMixtureChecks): the busiest rank's utilization, its limited-PS
// admission, its relaxation time against the step, and whether the KV lines
// were read outside their fit.
type RankMixtureCheck struct {
	BusiestRho        float64 `json:"busiestRho"`
	RelaxationSeconds float64 `json:"relaxationSeconds"`
	KVOutside         bool    `json:"kvOutside,omitempty"`
	// AdmissionShare is the share of requests that wait for one of a
	// rank's K places (sum p_i rho_i^K); BusiestRhoK the busiest rank's
	// rho^K and AdmissionWait its mean wait, seconds; TailIndex its sojourn
	// tail index (Nair, Wierman and Zwart) from ServiceTailIndex, the
	// service's; HeavyTraffic a rank above whatif.MaxLPSRho
	// (whatif.RankMixtureChecks).
	AdmissionShare   float64 `json:"admissionShare"`
	BusiestRhoK      float64 `json:"busiestRhoK"`
	AdmissionWait    float64 `json:"admissionWait"`
	TailIndex        float64 `json:"tailIndex,omitempty"`
	ServiceTailIndex float64 `json:"serviceTailIndex,omitempty"`
	HeavyTraffic     bool    `json:"heavyTraffic,omitempty"`
	// Refused is why the model did not price the step, "" where it did.
	// Abstained says the model abstained on the whole backtest
	// (RankMixtureReport.Abstained).
	Refused   string `json:"refused,omitempty"`
	Abstained bool   `json:"abstained,omitempty"`
}

// rankMixtureQ is a backtest's fitted rank mixture.
type rankMixtureQ struct {
	m      whatif.RankMixture
	report RankMixtureReport
	// fitPrefill is the prefill pods the fit window served (the median of
	// its cycles), the count the ranks' shares were read at; 0 unknown.
	fitPrefill int
}

// rateScale is what a test step's fleet rate is multiplied by before the
// fitted shares split it: the fit's prefill pods over the step's, so each
// fitted rank takes the rate one of the step's ranks takes, as the answer
// path prices another count (mixtureFleet.scale). The shares hold one entry
// per fitted rank, so a step on twice the pods split over them priced each
// rank at twice its rate. 1 where either count is unknown.
func (rq *rankMixtureQ) rateScale(testPrefill int) float64 {
	if rq.fitPrefill <= 0 || testPrefill <= 0 {
		return 1
	}
	return float64(rq.fitPrefill) / float64(testPrefill)
}

// mixtureWindow is the cycles of e at idx whose time is in (from, to].
func mixtureWindow(e *inputsEntry, idx []int, from, to time.Time) []whatif.MixtureCycle {
	var out []whatif.MixtureCycle
	for _, i := range idx {
		if at := e.series[i].At; at.After(from) && !at.After(to) {
			out = append(out, e.mixture[i])
		}
	}
	return out
}

// MaxMixtureBacklog is the largest backlog (backlogOf) at which the rank
// mixture reads a step as stationary. A step of length w whose arrivals A
// run ahead of its completions D leaves (A - D) w requests more waiting at
// its end, which at D adds (A - D)/D of the step to the last request's wait:
// a transient that share of the step. The pointwise-stationary approximation
// takes a transient under whatif.MaxRelaxationShare of the step as settled.
const MaxMixtureBacklog = whatif.MaxRelaxationShare

// backlogOf is (A - D)/D over the cycles of e at idx in (from, to]: A the
// router's request rate (the arrivals), D the engine rows' (every request
// completes on a decode or an unsplit row). ok is false where the cycles do
// not read both.
func backlogOf(e *inputsEntry, idx []int, from, to time.Time) (backlog, arrivals, completions float64, ok bool) {
	n := 0
	for _, i := range idx {
		s := e.series[i]
		if s.At.After(from) && !s.At.After(to) && s.RateKnown && s.Rate > 0 && s.EngineRate > 0 {
			arrivals += s.Rate
			completions += s.EngineRate
			n++
		}
	}
	if n == 0 {
		return 0, 0, 0, false
	}
	arrivals, completions = arrivals/float64(n), completions/float64(n)
	return (arrivals - completions) / completions, arrivals, completions, true
}

// backlogged is why a step whose backlog is over MaxMixtureBacklog is not
// stationary, or "" where it is (or the rates are not read).
func backlogged(e *inputsEntry, idx []int, from, to time.Time) string {
	b, a, d, ok := backlogOf(e, idx, from, to)
	if !ok || b <= MaxMixtureBacklog {
		return ""
	}
	return fmt.Sprintf("not stationary: arrivals %.3g r/s ran ahead of completions %.3g r/s, a backlog adding %.0f%% of the step "+
		"to the wait (over %.0f%%)", a, d, 100*b, 100*MaxMixtureBacklog)
}

// fitRankMixture fits the model on the fit entry's cycles at fitIdx, cut into
// steps of step: each step's inputs pooled over its anchor's measurement
// window, its mean and p90 the router's at the anchor. A step that is not
// stationary (backlogged) is left out: no queue model's steady state is what
// it measured, and its TTFT would pass into the fit as cost.
func fitRankMixture(fit *inputsEntry, fitIdx []int, step time.Duration, slots int, sw rankMixtureSwitches) (*rankMixtureQ, error) {
	rq, _, err := fitRankMixtureSteps(fit, fitIdx, step, slots, sw)
	return rq, err
}

// rankMixtureSwitches are the rank mixture's switches, each independent of
// the other.
type rankMixtureSwitches struct {
	// fitSteps turns on the fit-step fixes (whatif.RankMixtureOptions
	// CleanFitSteps and SharesAtLoad): BacktestRequest.RankMixtureFitSteps,
	// off by default.
	fitSteps bool
	// stratified prices with the stratified service moments
	// (whatif.RankMixtureOptions.StratifiedMoments):
	// BacktestRequest.RankMixtureStratified, on by default.
	stratified bool
	// nlLine fits the variant whatif.QueueRankMixtureNLLine in place of v2
	// (BacktestRequest.TTFTModel); it fits with both switches above on.
	nlLine bool
}

// rankMixtureSwitchesOf is the switches req turns on. The server has set
// an absent rankMixtureStratified to its own default; the CLI's backtest
// always sets it.
func rankMixtureSwitchesOf(req BacktestRequest) rankMixtureSwitches {
	return rankMixtureSwitches{fitSteps: req.RankMixtureFitSteps,
		stratified: stratifiedOf(req.RankMixtureStratified, whatif.DefaultRankMixtureStratified),
		nlLine:     req.TTFTModel == string(whatif.QueueRankMixtureNLLine)}
}

// model is the model sw fits.
func (sw rankMixtureSwitches) model() whatif.QueueModel {
	if sw.nlLine {
		return whatif.QueueRankMixtureNLLine
	}
	return whatif.QueueRankMixture
}

// stratifiedOf is a request's rankMixtureStratified: the value where it
// names one, def where it is absent.
func stratifiedOf(asked *bool, def bool) bool {
	if asked == nil {
		return def
	}
	return *asked
}

// fitRankMixtureSteps is fitRankMixture, and the steps it fitted on.
func fitRankMixtureSteps(fit *inputsEntry, fitIdx []int, step time.Duration, slots int, sw rankMixtureSwitches) (
	*rankMixtureQ, []whatif.MixtureStep, error) {
	if len(fit.mixture) != len(fit.series) {
		return nil, nil, InputErrorf("the experimental TTFT model %s needs the recording's per-rank series, which this source did not give",
			sw.model())
	}
	idx := slices.Clone(fitIdx)
	slices.SortStableFunc(idx, func(a, b int) int { return fit.series[a].At.Compare(fit.series[b].At) })
	anchors := map[time.Time]int{}
	var order []time.Time
	for _, i := range idx {
		k := fit.series[i].At.UTC().Truncate(step)
		if _, ok := anchors[k]; !ok {
			order = append(order, k)
		}
		anchors[k] = i
	}
	var steps []whatif.MixtureStep
	even, noRouter, noRanks, backlog := 0, 0, 0, 0
	for _, k := range order {
		a := anchors[k]
		if a >= len(fit.latency) {
			continue
		}
		w := measureWindowOf(fit.latency[a], MeasuredRouter)
		at := fit.series[a].At
		from := at.Add(-time.Duration(w * float64(time.Second)))
		cycles := mixtureWindow(fit, idx, from, at)
		if len(cycles) == 0 {
			continue
		}
		if backlogged(fit, idx, from, at) != "" {
			backlog++
			continue
		}
		st := whatif.MixtureStepOf(cycles, at)
		m := measuredOf(fit.latency[a], MeasuredRouter, w, true)
		switch {
		case m.ttft[0] == nil || m.ttft[2] == nil || math.IsInf(*m.ttft[0], 0) || math.IsInf(*m.ttft[2], 0):
			noRouter++
			continue
		case !(st.Rate > 0):
			noRanks++
			continue
		}
		st.RouterMean, st.RouterP90 = *m.ttft[0], *m.ttft[2]
		steps = append(steps, st)
		even = max(even, cycles[len(cycles)-1].EvenWithinRows)
	}
	if len(steps) == 0 {
		return nil, nil, InputErrorf("the experimental TTFT model %s fits against the router's measured TTFT mean and p90 and the prefill "+
			"ranks' own rates on stationary steps: of the fit window's %d steps, %d read no router TTFT, %d no prefill rank's rate "+
			"and %d were not stationary", sw.model(), len(order), noRouter, noRanks, backlog)
	}
	o := whatif.RankMixtureOptions{Slots: slots, MinSpreadSpan: whatif.MinSpreadFitWindow,
		SpreadFloor: whatif.MinSpread, CleanFitSteps: sw.fitSteps, SharesAtLoad: sw.fitSteps,
		StratifiedMoments: sw.stratified}
	fitter := whatif.FitRankMixture
	if sw.nlLine {
		fitter = whatif.FitRankMixtureNL
	}
	m, diag, err := fitter(steps, o)
	if err != nil {
		return nil, nil, InputErrorf("the experimental TTFT model %s did not fit: %v", sw.model(), err)
	}
	r := RankMixtureReport{Model: string(sw.model()), Experimental: true, Assumed: m.Assumed(), S0: m.S0, V: m.V, ServiceProportional: m.ServiceProportional,
		Shares: m.Shares, ShareCV: diag.ShareCV, EvenWithinRows: even, A0: m.A0, Beta: m.Beta, BetaOverhead: m.BetaOverhead,
		BetaDecode: m.BetaDecode, BetaRemainder: m.BetaRemainder, BetaUnidentified: m.BetaUnidentified, Spread: m.Spread, FoldSpread: diag.FoldSpread,
		SpreadAtBound: diag.SpreadAtBound, SpreadDefaulted: diag.SpreadDefaulted, SpreadFloored: diag.SpreadFloored, FitP90: diag.FitP90, FitSteps: diag.Steps, Slots: slots,
		FitStepsBacklogged: backlog, Queue: string(whatif.QueueLimitedProcessorSharing), Stratified: sw.stratified}
	if slots <= 0 {
		r.Queue = string(whatif.QueueProcessorSharing)
	}
	if r.FoldSpread == nil {
		r.FoldSpread = []float64{}
	}
	if m.KV.Known {
		r.KV = &RankMixtureKV{P0: m.KV.P0, P1: m.KV.P1, M0: m.KV.M0, M1: m.KV.M1, RhoLo: m.KV.RhoLo, RhoHi: m.KV.RhoHi}
	}
	if p := m.Parts; p != nil {
		r.FitMean = diag.FitMean
		r.ZParts = &RankMixtureZParts{OverheadA: p.Overhead.A, OverheadB: p.Overhead.B, DecodeA: p.Decode.A, DecodeB: p.Decode.B,
			RemainderA: p.Remainder.A, RemainderB: p.Remainder.B, OverheadSigma: p.Overhead.Sigma, DecodeSigma: p.Decode.Sigma}
	}
	return &rankMixtureQ{m: m, report: r, fitPrefill: actualOf(seriesAt(fit.series, idx)).Prefill}, steps, nil
}

// abstainedMixture is the model abstaining for why.
func abstainedMixture(why string) *rankMixtureQ {
	return &rankMixtureQ{report: RankMixtureReport{Model: string(whatif.QueueRankMixture), Experimental: true, Abstained: why,
		Shares: []float64{}, FoldSpread: []float64{}}}
}

// identified is rq, or rq abstaining where an identifiability check fails:
// the model prices only what its inputs identify.
//
// Two of the checks are on the calibration itself. The spread solves an
// equation, the folds' median p90 ratio equal to 1, by bisection over
// [0, whatif.MaxSpread]; a spread, pooled or one fold's, that ends at the
// upper bound has not solved it, and is the bound, not an estimate. A0 is
// refitted at each spread to hold the mean, so a spread that grows moves A0
// down by as much as it moves the mean of Z up. A0 is Z at a prompt of no
// tokens, a time: a negative A0 is that counterweight, not a location.
func (rq *rankMixtureQ) identified() *rankMixtureQ {
	if rq.report.Model == string(whatif.QueueRankMixtureNLLine) {
		return rq.identifiedNL()
	}
	r := rq.report
	var why []string
	if len(r.BetaUnidentified) > 0 {
		why = append(why, "prompt slope not identified ("+strings.Join(r.BetaUnidentified, ", ")+")")
	}
	if r.SpreadDefaulted {
		why = append(why, fmt.Sprintf("spread not calibrated (fit window under %s)", whatif.MinSpreadFitWindow))
	}
	if r.Spread >= whatif.MaxSpread || slices.ContainsFunc(r.FoldSpread, func(s float64) bool { return s >= whatif.MaxSpread }) {
		why = append(why, fmt.Sprintf("spread not identified (a self-check fold's p90 ratio does not reach 1 at any spread up to %g)",
			whatif.MaxSpread))
	}
	if r.A0 < 0 {
		why = append(why, fmt.Sprintf("non-queue constant negative (a0 %.3g s)", r.A0))
	}
	if r.ServiceProportional {
		why = append(why, "prefill service line not identified (one prompt length)")
	}
	if r.EvenWithinRows > 0 {
		why = append(why, "the prefill ranks' own rates not recorded")
	}
	if r.Slots <= 0 {
		why = append(why, "the prefill engine's max-num-seqs not read")
	}
	if len(why) == 0 {
		return rq
	}
	rq.report.Abstained = strings.Join(why, "; ")
	return rq
}

// mixturePriced says the rank mixture priced the step's TTFT.
func (q *backtestQuestion) mixturePriced(out BacktestStep) bool {
	return q.mixture != nil && out.RankMixture != nil && out.RankMixture.Refused == "" && !out.RankMixture.Abstained
}

// assumedLines is the model's assumed lines for the answer.
func (rq *rankMixtureQ) assumedLines() []string {
	if rq.report.Abstained != "" {
		return []string{fmt.Sprintf("experimental TTFT model %s abstained (%s): every TTFT is the planner's own, flagged %s",
			rq.report.Model, rq.report.Abstained, whatif.RankMixtureAbstained)}
	}
	out := []string{rq.report.Assumed + "; spread fitted on the fit window's self-check folds so that their median p90 ratio is 1 " +
		"(p95 is not fitted); every TTFT is flagged " + string(whatif.RankMixtureTTFT) + " and is OUTSIDE VALIDATED REGION"}
	if rq.report.ZParts != nil {
		out[0] = rq.report.Assumed + fmt.Sprintf("; nothing calibrated to the router (the fit window prices its own mean at %.3g of "+
			"measured); every TTFT is flagged %s and is OUTSIDE VALIDATED REGION", rq.report.FitMean, whatif.RankMixtureTTFT)
	}
	if len(rq.report.BetaUnidentified) > 0 {
		out = append(out, fmt.Sprintf("the fit window's prompts did not spread: the prompt term's %s slope is 0, its cost held in a0 at the "+
			"prompts read", strings.Join(rq.report.BetaUnidentified, ", ")))
	}
	if rq.report.ServiceProportional {
		out = append(out, "the fit window's prompts did not spread: the prefill service is taken proportional to the computed prompt, "+
			"read at the prompts it was measured at")
	}
	if rq.report.SpreadDefaulted {
		out = append(out, fmt.Sprintf("the fit window is shorter than %s: the prompt term's spread is 1, not calibrated", whatif.MinSpreadFitWindow))
	}
	if rq.report.SpreadFloored {
		out = append(out, fmt.Sprintf("the folds fitted the prompt term's spread under %g: it is priced at %g", whatif.MinSpread, whatif.MinSpread))
	}
	if rq.report.EvenWithinRows > 0 {
		out = append(out, fmt.Sprintf("%d prefill ranks' own request rates are not in the recording: each row's rate is split evenly "+
			"between its ranks, so the share profile is flatter than the fleet's", rq.report.EvenWithinRows))
	}
	return out
}

// price prices one test step's TTFT at each of backtestStats from the
// cycles of its window, over a step of stepSeconds, on testPrefill prefill
// pods (rateScale). ok is false where the model refused.
func (rq *rankMixtureQ) price(cycles []whatif.MixtureCycle, at time.Time, stepSeconds float64, testPrefill int) ([]Figure, RankMixtureCheck, bool) {
	st := whatif.MixtureStepOf(cycles, at)
	d, u := rq.m.Draws(st.Rate*rq.rateScale(testPrefill), st.Computed, st.Prompt, whatif.DefaultMixtureDraws)
	if u != nil {
		return nil, RankMixtureCheck{Refused: u.Error()}, false
	}
	c := d.Checks
	chk := RankMixtureCheck{RelaxationSeconds: c.Relaxation, KVOutside: c.KVOutside, AdmissionShare: c.AdmissionShare,
		BusiestRhoK: c.BusiestRhoK, AdmissionWait: c.AdmissionWait, TailIndex: c.TailIndex, ServiceTailIndex: c.ServiceTailIndex,
		HeavyTraffic: c.HeavyTraffic}
	if len(c.Rho) > 0 {
		chk.BusiestRho = slices.Max(c.Rho)
	}
	flags := append([]whatif.Flag{whatif.RankMixtureTTFT}, c.Flags(stepSeconds)...)
	qs := []float64{}
	for _, q := range backtestStats {
		if q != 0 {
			qs = append(qs, q)
		}
	}
	mean, quant := d.Stats(rq.m.A0, rq.m.Spread*rq.m.Beta, qs...)
	out := make([]Figure, len(backtestStats))
	j := 0
	for i, q := range backtestStats {
		v := mean
		if q != 0 {
			v = quant[j]
			j++
		}
		out[i] = figureOf(whatif.Answer{Value: v, Flags: flags}, unitSeconds, true)
	}
	return out, chk, true
}

// priceMixture sets a step's TTFT from the rank mixture: its figures in
// pred's place, no bound, and the step's relaxation and settling from the
// model's busiest rank. Where the model refused, the planner's figures
// stand, flagged with the experimental model. It refuses a step that is not
// stationary (backlogged): the model prices a steady state, and a step whose
// backlog grows has none.
func (q *backtestQuestion) priceMixture(pred *prediction, out *BacktestStep, st stepOf) {
	if q.mixture.report.Abstained != "" {
		out.RankMixture = &RankMixtureCheck{Abstained: true}
		for i := range pred.ttft {
			if !slices.Contains(pred.ttft[i].Flags, string(whatif.RankMixtureAbstained)) {
				pred.ttft[i].Flags = append(pred.ttft[i].Flags, string(whatif.RankMixtureAbstained))
			}
		}
		return
	}
	at := q.test.series[st.anchor].At
	var figs []Figure
	var chk RankMixtureCheck
	ok := false
	if why := backlogged(q.test, st.window, st.windowStart, at); why != "" {
		chk.Refused = why
	} else {
		testPrefill := actualOf(seriesAt(q.test.series, st.window)).Prefill
		figs, chk, ok = q.mixture.price(mixtureWindow(q.test, st.window, st.windowStart, at), at, st.windowSeconds, testPrefill)
	}
	out.RankMixture = &chk
	if !ok {
		for i := range pred.ttft {
			if !slices.Contains(pred.ttft[i].Flags, string(whatif.RankMixtureTTFT)) {
				pred.ttft[i].Flags = append(pred.ttft[i].Flags, string(whatif.RankMixtureTTFT))
			}
		}
		return
	}
	for i := range pred.ttft {
		pred.ttft[i] = validate(figs[i], whatif.FigureTTFT, backtestStats[i], whatif.FigureClass{}, 0)
		pred.ttftBound[i] = nil
	}
	out.RelaxationSeconds = ptr(chk.RelaxationSeconds)
	out.Unsettled = !settles(chk.RelaxationSeconds, st.windowSeconds)
}
