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
// v2 is (fitRankMixtureSteps), prices each test step the same way, and
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
	// RemainderShapeMean is the router's measured request-processing mean
	// the remainder is drawn from, seconds, and RemainderShapeP95 that
	// law's 95th percentile over its mean
	// (BacktestRequest.RankMixtureRemainderSpread); both absent where the
	// remainder is its line.
	RemainderShapeMean float64 `json:"remainderShapeMean,omitempty"`
	RemainderShapeP95  float64 `json:"remainderShapeP95,omitempty"`
	// RemainderServer is the remainder drawn from the prefill pods' API
	// servers (BacktestRequest.RankMixtureRemainderServer), absent where it
	// was not asked or not applied; RemainderServerOff is why it was asked
	// and not applied.
	RemainderServer    *RankMixtureServer `json:"remainderServer,omitempty"`
	RemainderServerOff string             `json:"remainderServerOff,omitempty"`
}

// RankMixtureServer is the remainder server's fit (whatif.ZServer), seconds
// and seconds a prompt token: the render handler's line, the generation
// frontend's (absent where the pods did not record it), the router's work
// outside its render call, the unmeasured rest's line and its share of the
// fit window's remainder, the fit window's top rate past which a figure is
// flagged where that share is over whatif.MaxRemainderRestShare, and what it
// was read on.
type RankMixtureServer struct {
	RenderA      float64 `json:"renderA"`
	RenderB      float64 `json:"renderB"`
	HasRender    bool    `json:"hasRender"`
	FrontendA    float64 `json:"frontendA"`
	FrontendB    float64 `json:"frontendB"`
	HasFrontend  bool    `json:"hasFrontend"`
	RouterOther  float64 `json:"routerOther"`
	RouterOtherB float64 `json:"routerOtherB,omitempty"`
	RestA        float64 `json:"restA"`
	RestB        float64 `json:"restB"`
	RestShare    float64 `json:"restShare"`
	TopRate      float64 `json:"topRate"`
	FitSteps     int     `json:"fitSteps"`
	PodSteps     int     `json:"podSteps"`
}

// remainderShape sets the remainder's measured law, where it has one.
func (z *RankMixtureZParts) remainderShape(r whatif.ZPart) {
	if r.Shape == nil {
		return
	}
	z.RemainderShapeMean, z.RemainderShapeP95 = r.ShapeMean, r.ShapeQuantile(0.95)
}

// remainderServer sets the remainder server's fit, or why it was not
// applied, where m was asked for it.
func (z *RankMixtureZParts) remainderServer(m whatif.RankMixture) {
	if !m.RemainderServer || m.Parts == nil {
		return
	}
	s := m.Parts.Remainder.Server
	if s == nil {
		z.RemainderServerOff = m.RemainderServerOff
		return
	}
	z.RemainderServer = &RankMixtureServer{RenderA: s.R0, RenderB: s.R1, HasRender: s.HasRender, FrontendA: s.F0, FrontendB: s.F1,
		HasFrontend: s.HasFrontend, RouterOther: s.Other, RouterOtherB: s.Other1, RestA: s.N0, RestB: s.N1, RestShare: s.RestShare,
		TopRate: s.TopRate, FitSteps: s.Steps, PodSteps: s.PodSteps}
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
		why = append(why, fmt.Sprintf("fit window under %s: too short for its own checks", r.window()))
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
