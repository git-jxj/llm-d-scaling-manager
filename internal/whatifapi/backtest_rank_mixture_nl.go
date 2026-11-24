package whatifapi

import (
	"fmt"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// This file holds what the backtest adds for the experimental rank-mixture
// variant whatif.QueueRankMixtureNLLine (BacktestRequest.TTFTModel): Z
// measured part by part on v2's queue and service line
// (whatif/rank_mixture_nl.go). It is fitted on the backtest's fit window as
// v2 is (fitRankMixtureStepsWith), prices each test step the same way, and
// abstains where its own checks fail (identifiedNL).

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

// identifiedNL is identified for the variant. Its Z is measured, not
// calibrated, so v2's checks on the spread and on A0 do not apply (A0 is a
// sum of non-negative intercepts); in their place the fit window must price
// its own router mean within [whatif.MinNLFitMean, whatif.MaxNLFitMean] of
// measured and its own p90 within [whatif.MinNLFitP90, whatif.MaxNLFitMean],
// since nothing was calibrated to either: a model that misses the window it
// was fitted on is not extrapolated from it.
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
	if r.SpreadDefaulted {
		why = append(why, fmt.Sprintf("fit window under %s: too short for its own checks", whatif.MinSpreadFitWindow))
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
