package whatifapi

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// This file prices a backtest's TTFT with the experimental rank-mixture
// model's non-linear variant (whatif.QueueRankMixtureNL), where the request
// asks for it (BacktestRequest.TTFTModel): the attention service law and Z
// measured part by part (whatif/rank_mixture_nl.go). It is fitted on the
// backtest's fit window as the rank mixture is, prices each test step the
// same way, and abstains where its own checks fail.

// RankMixtureZParts is Z measured part by part, seconds and seconds a
// prompt token, and the parts' spreads (whatif.ZParts).
type RankMixtureZParts struct {
	OverheadA  float64 `json:"overheadA"`
	OverheadB  float64 `json:"overheadB"`
	DecodeA    float64 `json:"decodeA"`
	DecodeB    float64 `json:"decodeB"`
	RemainderA float64 `json:"remainderA"`
	RemainderB float64 `json:"remainderB"`
	// OverheadSigma and DecodeSigma are the parts' lognormal spreads
	// (whatif.ZPart.Sigma); 0 is none.
	OverheadSigma float64 `json:"overheadSigma"`
	DecodeSigma   float64 `json:"decodeSigma"`
}

// fitRankMixtureNL fits whatif.QueueRankMixtureNL on the fit window's steps
// (mixtureFitSteps).
func fitRankMixtureNL(fit *inputsEntry, fitIdx []int, step time.Duration, slots int) (*rankMixtureQ, error) {
	steps, even, backlog, err := mixtureFitSteps(fit, fitIdx, step, whatif.QueueRankMixtureNL)
	if err != nil {
		return nil, err
	}
	m, diag, err := whatif.FitRankMixtureNL(steps, whatif.RankMixtureOptions{Slots: slots})
	if err != nil {
		return nil, InputErrorf("the experimental TTFT model %s did not fit: %v", whatif.QueueRankMixtureNL, err)
	}
	rq := reportOf(m, whatif.RankMixtureFit{Steps: diag.Steps, ShareCV: diag.ShareCV, FitP90: diag.FitP90}, slots, even, backlog)
	r := &rq.report
	r.Model, r.Law, r.B, r.CleanSteps, r.FitMean = string(whatif.QueueRankMixtureNL), string(m.Law), m.B, diag.CleanSteps, diag.FitMean
	if math.IsInf(m.V, 1) {
		// No term linear in the computed tokens: the attention term holds
		// them all. JSON has no infinity; 0 reads as none.
		r.V = 0
	}
	if p := m.Parts; p != nil {
		r.ZParts = &RankMixtureZParts{OverheadA: p.Overhead.A, OverheadB: p.Overhead.B, DecodeA: p.Decode.A, DecodeB: p.Decode.B,
			RemainderA: p.Remainder.A, RemainderB: p.Remainder.B, OverheadSigma: p.Overhead.Sigma, DecodeSigma: p.Decode.Sigma}
	}
	return rq, nil
}

// abstainedMixtureNL is the non-linear variant abstaining for why.
func abstainedMixtureNL(why string) *rankMixtureQ {
	rq := abstainedMixture(why)
	rq.report.Model = string(whatif.QueueRankMixtureNL)
	return rq
}

// identifiedNL is rq, or rq abstaining where a check of the non-linear
// variant fails. Its Z is measured, not calibrated, so v2's checks on the
// spread and on A0 do not apply (A0 is a sum of non-negative intercepts);
// in their place the fit window must price its own router mean within
// [whatif.MinNLFitMean, whatif.MaxNLFitMean] of measured and its own p90
// within [whatif.MinNLFitP90, whatif.MaxNLFitMean], since nothing was
// calibrated to either: a model that misses the window it was fitted on is
// not extrapolated from it.
func (rq *rankMixtureQ) identifiedNL() *rankMixtureQ {
	r := rq.report
	var why []string
	if r.ServiceProportional {
		why = append(why, "prefill service not identified (one prompt length)")
	}
	if r.EvenWithinRows > 0 {
		why = append(why, "the prefill ranks' own rates not recorded")
	}
	if r.Slots <= 0 {
		why = append(why, "the prefill engine's max-num-seqs not read")
	}
	if !(r.FitMean >= whatif.MinNLFitMean && r.FitMean <= whatif.MaxNLFitMean) {
		why = append(why, fmt.Sprintf("the fit window's own mean not priced (median ratio %.3g, outside %g-%g)",
			r.FitMean, whatif.MinNLFitMean, whatif.MaxNLFitMean))
	}
	if !(r.FitP90 >= whatif.MinNLFitP90 && r.FitP90 <= whatif.MaxNLFitMean) {
		why = append(why, fmt.Sprintf("the fit window's own p90 not priced (median ratio %.3g, outside %g-%g)",
			r.FitP90, whatif.MinNLFitP90, whatif.MaxNLFitMean))
	}
	if len(why) == 0 {
		return rq
	}
	rq.report.Abstained = strings.Join(why, "; ")
	return rq
}
