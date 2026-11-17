package main

import (
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// Every figure the planner prints says how far off it read where it was
// measured (whatif.Envelope; docs/guides/capacity-planner/accuracy.md):
// "validated: error ..." inside a measured region, "OUTSIDE VALIDATED
// REGION (...)" anywhere else. This file builds what the envelope judges a
// figure by: its kind, its class, its rate over its knee, and its flags.

// judge is the shipped envelope's verdict on one figure.
func judge(kind whatif.FigureKind, q float64, class whatif.FigureClass, share float64, r whatif.Result) whatif.Validation {
	return whatif.DefaultEnvelope().Judge(whatif.Figure{Kind: kind, Percentile: q, Class: class, KneeShare: share, Flags: resultFlags(r)})
}

// resultFlags is an answer's flags, or a refusal's.
func resultFlags(r whatif.Result) []whatif.Flag {
	switch v := r.(type) {
	case whatif.Answer:
		return v.Flags
	case whatif.TTFT:
		return v.Flags
	case whatif.Unanswerable:
		return v.Flags
	}
	return nil
}

// class is the class of a decode variant's own figures: its accelerator,
// replicas and shape. Flow control and the prefill pods are not read.
func (pv pricedVariant) class() whatif.FigureClass {
	c := whatif.FigureClass{Topology: whatif.TopologyPD, Accelerator: pv.state.Accelerator, DecodeReplicas: pv.decode.Replicas,
		PromptTokens: pv.shape.AvgInputTokens, OutputTokens: pv.shape.AvgOutputTokens}
	if domain.CanonicalRole(pv.state.Role) == domain.RoleBoth {
		c.Topology = whatif.TopologyAggregated
	}
	return c
}

// class is the class of a fleet's figures: a spread or arrivals stated by a
// flag are known, as a measured one is; the defaults are not.
func (pf *pricedFleet) class() whatif.FigureClass {
	c := whatif.FleetClass(pf.fleet)
	c.PromptCVUnknown = pf.fleet.Prefill.PromptCVAssumed && !pf.statedCV
	c.ArrivalsUnknown = slices.Contains(pf.flags, assumedPoisson) && !slices.Contains(pf.flags, assumedArrivalCV2)
	if pf.pv != nil {
		c.Accelerator = pf.pv.state.Accelerator
		if pf.fleet.Decode == nil {
			c.DecodeReplicas, c.OutputTokens = pf.pv.decode.Replicas, pf.pv.shape.AvgOutputTokens
		}
		if domain.CanonicalRole(pf.pv.state.Role) == domain.RoleBoth {
			c.Topology = whatif.TopologyAggregated
		}
	}
	return c
}

// share is rate over knee; 0 where either is not a figure.
func share(rate, knee float64) float64 {
	if !(rate > 0 && knee > 0) {
		return 0
	}
	return rate / knee
}

// lowerKnee is the lower of a TTFT's two knees that is a figure.
func lowerKnee(t whatif.TTFT) float64 {
	k := t.PrefillKnee
	if t.DecodeKnee > 0 && (k <= 0 || t.DecodeKnee < k) {
		k = t.DecodeKnee
	}
	return k
}

// ttftValidation judges a TTFT priced at rate.
func (pf *pricedFleet) ttftValidation(q float64, rate float64, r whatif.Result) whatif.Validation {
	s := 0.0
	if t, ok := r.(whatif.TTFT); ok {
		s = share(rate, lowerKnee(t))
	}
	return judge(whatif.FigureTTFT, q, pf.class(), s, r)
}

// kneeValidation judges the prefill knee.
func (pf *pricedFleet) kneeValidation(r whatif.Result) whatif.Validation {
	return judge(whatif.FigurePrefillKnee, 0, pf.class(), 0, r)
}

// rateValidation judges a rate asked at a TTFT target, the prefill role's or
// the fleet's, as whatif.RateFigure reads it: by what bound it, at its share
// of the lower knee, which the prefill role's answer carries (role). The API
// judges its limits the same way.
func (pf *pricedFleet) rateValidation(q float64, r, role whatif.Result) whatif.Validation {
	a, ok := r.(whatif.Answer)
	if !ok {
		return judge(whatif.FigureRateAtTTFT, q, pf.class(), 0, r)
	}
	knee := 0.0
	if p, ok := role.(whatif.Answer); ok && p.Basis.TTFT != nil {
		knee = p.Basis.TTFT.KneeRate
	}
	by := make([]string, len(a.Basis.BoundBy))
	for i, b := range a.Basis.BoundBy {
		by[i] = string(b)
	}
	return whatif.DefaultEnvelope().Judge(whatif.RateFigure(a.Value, by, q, pf.class(), knee, a.Flags))
}

// kneeValidation judges decode's knee.
func (pv pricedVariant) kneeValidation(r whatif.Result) whatif.Validation {
	return judge(whatif.FigureDecodeKnee, 0, pv.class(), 0, r)
}

// tpotValidation judges a TPOT at rate: the mean (q 0) or a percentile.
func (pv pricedVariant) tpotValidation(q float64, rate float64, knee, r whatif.Result) whatif.Validation {
	kind := whatif.FigureMeanTPOT
	if q != 0 {
		kind = whatif.FigurePercentileTPOT
	}
	s := 0.0
	if k, ok := knee.(whatif.Answer); ok {
		s = share(rate, k.Value)
	}
	return judge(kind, q, pv.class(), s, r)
}

// rateValidation judges decode's rate at a TPOT target.
func (pv pricedVariant) rateValidation(r whatif.Result) whatif.Validation {
	return judge(whatif.FigureRateAtTPOT, 0, pv.class(), 0, r)
}

// validated is a result on the wire with its validation; a refusal has no
// figure to judge.
func validated(w *wireResult, v whatif.Validation) *wireResult {
	if w != nil && w.Answer != nil {
		w.Answer.Validation = whatifapi.ValidationOf(v)
	}
	return w
}

// validatedTTFT is validated for a TTFT on the wire.
func validatedTTFT(w *wireTTFTResult, v whatif.Validation) *wireTTFTResult {
	if w != nil && w.Answer != nil {
		w.Answer.Validation = whatifapi.ValidationOf(v)
	}
	return w
}
