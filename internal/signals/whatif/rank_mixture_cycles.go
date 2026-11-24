package whatif

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The rank-mixture TTFT model's fit on cycles (cycle_fits.go): the steps cut
// from the cycles, the fit, its identifiability checks, and the window it is
// priced on. The capacity planner calls it on a recording's cycles, the slo
// analyzer on the controller's own.

// RankMixtureReport is the experimental model's fit, as a backtest's answer
// carries it (whatifapi.BacktestResponse.RankMixture) and the slo analyzer
// logs it.
type RankMixtureReport struct {
	// Model is QueueRankMixture; Experimental is always true.
	Model        string `json:"model"`
	Experimental bool   `json:"experimental"`
	// Abstained is why the model prices nothing, "" where it prices: an
	// identifiability check failed (a prompt slope not identified, the
	// spread not calibrated, the service line not identified, the ranks'
	// own rates or K not read) or the fit failed. Every TTFT is then the
	// planner's own, flagged RankMixtureAbstained. The fitted figures
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
	// time (FitPSInversion).
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
	// spread enough to fit: each is 0 (FitBeta).
	BetaUnidentified []string `json:"betaUnidentified,omitempty"`
	Spread           float64  `json:"spread"`
	// FoldSpread is the spread each self-check fold fits alone;
	// SpreadAtBound says the folds could not reach a p90 ratio of 1;
	// FitP90 is the fit window's median p90 ratio at the spread; FitSteps
	// the fit's steps.
	FoldSpread    []float64 `json:"foldSpread"`
	SpreadAtBound bool      `json:"spreadAtBound,omitempty"`
	// SpreadDefaulted says the fit window is shorter than
	// MinSpreadFitWindow: the spread is 1, not calibrated.
	// SpreadFloored says the folds fitted a spread under MinSpread, and it
	// is priced at that floor.
	SpreadDefaulted bool    `json:"spreadDefaulted,omitempty"`
	SpreadFloored   bool    `json:"spreadFloored,omitempty"`
	FitP90          float64 `json:"fitP90"`
	FitSteps        int     `json:"fitSteps"`
	// FitStepsBacklogged is the fit window's steps left out because they
	// were not stationary: their arrivals ran ahead of their completions
	// by more than MaxMixtureBacklog (MixtureBacklog).
	FitStepsBacklogged int `json:"fitStepsBacklogged,omitempty"`
	// Queue is each rank's queue model (QueueLimitedProcessorSharing, or
	// processor sharing with Slots unknown); Slots its K, the prefill
	// engine's max-num-seqs (1 without chunked prefill), 0 not known.
	Queue string `json:"queue"`
	Slots int    `json:"slots"`
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

// RankMixtureSwitches are the rank mixture's off-by-default fit switches,
// each independent of the other.
type RankMixtureSwitches struct {
	// FitSteps turns on the fit-step fixes (RankMixtureOptions
	// CleanFitSteps and SharesAtLoad).
	FitSteps bool
	// Stratified turns on the stratified service moments
	// (RankMixtureOptions.StratifiedMoments).
	Stratified bool
}

// RankMixtureFitted is the model fitted on cycles, and its report.
type RankMixtureFitted struct {
	M      RankMixture
	Report RankMixtureReport
}

// FitRankMixtureCycles fits the model on cycles, cut into steps of step:
// each step's inputs pooled over its anchor's measurement window (the
// router's, RouterWindowOf), its mean and p90 the router's at the anchor
// (RouterTTFTOf). A step that is not stationary (MixtureBacklogged) is left
// out: no queue model's steady state is what it measured, and its TTFT would
// pass into the fit as cost. slots is the prefill engine's max-num-seqs (0
// not known). It returns the steps it fitted on.
func FitRankMixtureCycles(cycles []FitCycle, step time.Duration, slots int, sw RankMixtureSwitches) (*RankMixtureFitted, []MixtureStep, error) {
	cycles = sortedByTime(cycles)
	order, anchors := stepAnchors(cycles, step)
	var steps []MixtureStep
	even, noRouter, noRanks, backlog := 0, 0, 0, 0
	for _, k := range order {
		a := anchors[k]
		if !cycles[a].Latency {
			continue
		}
		at := cycles[a].At
		from := at.Add(-time.Duration(RouterWindowOf(cycles[a].Router) * float64(time.Second)))
		window := MixtureCyclesIn(cycles, from, at)
		if len(window) == 0 {
			continue
		}
		if MixtureBacklogged(cycles, from, at) != "" {
			backlog++
			continue
		}
		st := MixtureStepOf(window, at)
		mean, p90, ok := RouterTTFTOf(cycles[a].Router)
		switch {
		case !ok:
			noRouter++
			continue
		case !(st.Rate > 0):
			noRanks++
			continue
		}
		st.RouterMean, st.RouterP90 = mean, p90
		steps = append(steps, st)
		even = max(even, window[len(window)-1].EvenWithinRows)
	}
	if len(steps) == 0 {
		return nil, nil, fmt.Errorf("the experimental TTFT model %s fits against the router's measured TTFT mean and p90 and the prefill "+
			"ranks' own rates on stationary steps: of the fit window's %d steps, %d read no router TTFT, %d no prefill rank's rate "+
			"and %d were not stationary", QueueRankMixture, len(order), noRouter, noRanks, backlog)
	}
	m, diag, err := FitRankMixture(steps, RankMixtureOptions{Slots: slots, MinSpreadSpan: MinSpreadFitWindow,
		SpreadFloor: MinSpread, CleanFitSteps: sw.FitSteps, SharesAtLoad: sw.FitSteps,
		StratifiedMoments: sw.Stratified})
	if err != nil {
		return nil, nil, fmt.Errorf("the experimental TTFT model %s did not fit: %w", QueueRankMixture, err)
	}
	r := RankMixtureReport{Model: string(QueueRankMixture), Experimental: true, Assumed: m.Assumed(), S0: m.S0, V: m.V,
		ServiceProportional: m.ServiceProportional, Shares: m.Shares, ShareCV: diag.ShareCV, EvenWithinRows: even, A0: m.A0,
		Beta: m.Beta, BetaOverhead: m.BetaOverhead, BetaDecode: m.BetaDecode, BetaRemainder: m.BetaRemainder,
		BetaUnidentified: m.BetaUnidentified, Spread: m.Spread, FoldSpread: diag.FoldSpread, SpreadAtBound: diag.SpreadAtBound,
		SpreadDefaulted: diag.SpreadDefaulted, SpreadFloored: diag.SpreadFloored, FitP90: diag.FitP90, FitSteps: diag.Steps,
		Slots: slots, FitStepsBacklogged: backlog, Queue: string(QueueLimitedProcessorSharing)}
	if slots <= 0 {
		r.Queue = string(QueueProcessorSharing)
	}
	if r.FoldSpread == nil {
		r.FoldSpread = []float64{}
	}
	if m.KV.Known {
		r.KV = &RankMixtureKV{P0: m.KV.P0, P1: m.KV.P1, M0: m.KV.M0, M1: m.KV.M1, RhoLo: m.KV.RhoLo, RhoHi: m.KV.RhoHi}
	}
	return &RankMixtureFitted{M: m, Report: r}, steps, nil
}

// AbstainedRankMixture is the model abstaining for why.
func AbstainedRankMixture(why string) *RankMixtureFitted {
	return &RankMixtureFitted{Report: RankMixtureReport{Model: string(QueueRankMixture), Experimental: true, Abstained: why,
		Shares: []float64{}, FoldSpread: []float64{}}}
}

// Identified is rq, abstaining where an identifiability check fails: the
// model prices only what its inputs identify.
//
// Two of the checks are on the calibration itself. The spread solves an
// equation, the folds' median p90 ratio equal to 1, by bisection over
// [0, MaxSpread]; a spread, pooled or one fold's, that ends at the upper
// bound has not solved it, and is the bound, not an estimate. A0 is refitted
// at each spread to hold the mean, so a spread that grows moves A0 down by as
// much as it moves the mean of Z up. A0 is Z at a prompt of no tokens, a
// time: a negative A0 is that counterweight, not a location.
func (rq *RankMixtureFitted) Identified() *RankMixtureFitted {
	r := rq.Report
	var why []string
	if len(r.BetaUnidentified) > 0 {
		why = append(why, "prompt slope not identified ("+strings.Join(r.BetaUnidentified, ", ")+")")
	}
	if r.SpreadDefaulted {
		why = append(why, fmt.Sprintf("spread not calibrated (fit window under %s)", MinSpreadFitWindow))
	}
	if r.Spread >= MaxSpread || slices.ContainsFunc(r.FoldSpread, func(s float64) bool { return s >= MaxSpread }) {
		why = append(why, fmt.Sprintf("spread not identified (a self-check fold's p90 ratio does not reach 1 at any spread up to %g)",
			MaxSpread))
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
	rq.Report.Abstained = strings.Join(why, "; ")
	return rq
}

// MixtureWindowFit is the model fitted on a window of cycles for pricing at
// any load, as the capacity planner's main view prices it and the slo
// analyzer prices its current load.
type MixtureWindowFit struct {
	// Fitted is the fit, Identified; its Report.Abstained says why it
	// prices nothing, "" where it prices.
	Fitted *RankMixtureFitted
	// Steps is the steps fitted on; Pooled the window's cycles pooled into
	// one step: the computed-token and prompt histograms every rate is
	// priced at; Top the highest step rate fitted, requests a second.
	Steps  []MixtureStep
	Pooled MixtureStep
	Top    float64
	// HeldOut is the model's mean TTFT scored on the window's self-check
	// folds (RankMixtureHeldOut); zero Steps where nothing was scored.
	HeldOut HeldOutScore
	// FitErr is why the fit itself failed, nil where it fitted (and may
	// still abstain on an identifiability check).
	FitErr error
}

// Abstained is why the fit prices nothing, "" where it prices.
func (w MixtureWindowFit) Abstained() string {
	if w.Fitted == nil {
		return "not fitted"
	}
	return w.Fitted.Report.Abstained
}

// FitMixtureWindow fits the model on cycles, cut into steps of step, with
// the prefill engine's max-num-seqs slots, and scores it on its self-check
// folds at tolerance. A fit that fails, or fails an identifiability check,
// abstains.
func FitMixtureWindow(cycles []FitCycle, step time.Duration, slots int, tolerance float64) MixtureWindowFit {
	rq, steps, err := FitRankMixtureCycles(cycles, step, slots, RankMixtureSwitches{})
	if err != nil {
		return MixtureWindowFit{Fitted: AbstainedRankMixture(err.Error()), FitErr: err}
	}
	w := MixtureWindowFit{Fitted: rq.Identified(), Steps: steps}
	if w.Fitted.Report.Abstained != "" {
		return w
	}
	for _, st := range steps {
		w.Top = max(w.Top, st.Rate)
	}
	sorted := sortedByTime(cycles)
	first, last := sorted[0].At, sorted[len(sorted)-1].At
	w.Pooled = MixtureStepOf(MixtureCyclesIn(cycles, first.Add(-time.Nanosecond), last), last)
	o := RankMixtureOptions{Slots: rq.M.Slots, MinSpreadSpan: MinSpreadFitWindow, SpreadFloor: MinSpread}
	w.HeldOut = RankMixtureHeldOut(steps, o, rq.M.Spread, tolerance)
	return w
}

// Price is the model's TTFT at rate requests a second over the fleet it was
// fitted at scaled by scale (the fitted prefill replicas over the priced
// fleet's), at the pooled window: the mean and each quantile of qs, from
// one set of draws. ok is false where the model abstained or refused the
// rate.
func (w MixtureWindowFit) Price(rate, scale float64, qs ...float64) (mean float64, quantiles []float64, ok bool) {
	if w.Abstained() != "" {
		return 0, nil, false
	}
	m := w.Fitted.M
	d, u := m.Draws(rate*scale, w.Pooled.Computed, w.Pooled.Prompt, DefaultMixtureDraws)
	if u != nil {
		return 0, nil, false
	}
	mean, quantiles = d.Stats(m.A0, m.Spread*m.Beta, qs...)
	if !Finite(mean) {
		return 0, nil, false
	}
	for _, v := range quantiles {
		if !Finite(v) {
			return 0, nil, false
		}
	}
	return mean, quantiles, true
}
