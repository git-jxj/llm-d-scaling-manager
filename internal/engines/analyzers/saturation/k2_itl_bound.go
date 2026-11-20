package saturation

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// SwitchK2BatchAtITLTarget is the replay switch for a latency bound on the
// derived k2's batch (issue llm-d-incubation/llm-d-scaling-manager#258).
//
// The derived k2 (P3) holds a rank at the batch its token budget sustains in
// steady state, N_steady (steadyBatch). That is a throughput ceiling, with no
// latency in it. On a day-long P/D recording it read 16-19 requests a rank
// while the ranks ran 2-6, and the ITL line through the ranks' own readings
// put a rank at 16-19 at about twice the ITL they served at. Priced at its
// true KV per request (SwitchK2DerivedAtResidentKV), k2 then sized the decode
// role to that ceiling and ordered fewer replicas than served the traffic.
//
// On, the batch is also held at N_itl, the batch at which the replica's ITL
// line reaches the latency target: min(N_steady, N_itl). The ITL line is in
// KV fill, ITL(k) = A x k + B, per token (SwitchITLPerToken, which reads the
// step time over the tokens a step emits at the measured speculative
// acceptance). The target is the model's TPOT objective where the policy
// sets one, and otherwise the line at the ITL band's pricing point (pricingK),
// the fill the derived mu is priced at. The fill it gives, k_itl, holds a
// rank at k_itl x C / KVreq requests, C the rank's KV; so the bound on k2 is
// k_itl x the replica's KV, whatever a request is priced at, and the log
// carries N_itl a rank at the KV per request the derived k2 prices.
//
// The bound applies only on a line the variant learned itself
// (k2ITLDistrust): not one borrowed from a sibling, held from an earlier fit,
// priced one-sided at a prior, fitted above the band, or pinned at the default
// intercept (itl.DefaultBaselineSec) rather than a B the key measured, or read
// through its floor below the band. A line in step time (SwitchITLPerToken
// off on a speculating engine) is not set against a per-token target. A TPOT
// target at or under the line's B is met at no fill. In each of these cases
// the batch is N_steady, as before, and the k2-decision line says why.
//
// It only lowers k2. The lower bound is judged against the bounded figure,
// and the capacity store keeps it. The prefill role has no derived k2, and the
// zero-replica estimate has no line; neither moves.
//
// Off from the constructor: on, a decode is priced at less capacity where its
// line is learned, and the default changes only after review.
const SwitchK2BatchAtITLTarget = "k2-batch-held-at-itl-target"

// Where the ITL target the derived k2's batch is held at came from
// (itlBatchBound.source).
const (
	// itlTargetSLO is the policy's TPOT objective.
	itlTargetSLO = "slo-tpot"
	// itlTargetPricingPoint is the line at the ITL band's pricing point.
	itlTargetPricingPoint = "band-pricing-point"
)

// Why the derived k2's batch is not held at an ITL target
// (itlBatchBound.why), with the switch on.
const (
	itlBoundNoLine         = "no-line"
	itlBoundBorrowed       = "borrowed-line"
	itlBoundHeld           = "held-line"
	itlBoundOneSided       = "one-sided-line"
	itlBoundAboveBand      = "above-band-line"
	itlBoundDefaultB       = "default-intercept"
	itlBoundPerStep        = "per-step-line"
	itlBoundUnderIntercept = "target-under-intercept"
)

// itlBatchBound is the KV fill at which a replica's ITL line reaches its
// target, the bound on the derived k2's batch (SwitchK2BatchAtITLTarget).
// The zero value is the switch off. With it on, fill is 0 where there is no
// bound, and why says why.
type itlBatchBound struct {
	// on is SwitchK2BatchAtITLTarget.
	on bool
	// fill is the KV fill the line reaches the target at, in (0, 1].
	fill float64
	// target is the latency target, seconds per token.
	target float64
	// source is itlTargetSLO or itlTargetPricingPoint.
	source string
	// why is empty with a bound, and otherwise why there is none.
	why string
}

// applies reports whether b bounds the batch.
func (b itlBatchBound) applies() bool {
	return b.on && b.why == "" && b.fill > 0 && b.fill <= 1
}

// k2ITLDistrust says why the line r is not one the derived k2's batch may be
// held at (SwitchK2BatchAtITLTarget), or "" when it is: a line the key fitted
// this cycle, with an intercept it measured.
func k2ITLDistrust(r itl.Reading) string {
	switch {
	case r.Model.IsZero() || r.Tier == "":
		return itlBoundNoLine
	case r.SlopeHeld || r.TierHeld:
		return itlBoundHeld
	case r.PinnedAtBPrior || r.PinnedAtLeastITL:
		return itlBoundOneSided
	case r.AboveBandFit:
		return itlBoundAboveBand
	case r.Tier == itl.TierPinnedB && !r.BaselineLearned && !r.PinnedAtFloor:
		return itlBoundDefaultB
	}
	return ""
}

// k2ITLBound is the bound on the derived k2's batch for variant this cycle
// (SwitchK2BatchAtITLTarget): the fill at which its line reaches the TPOT
// objective, or the line's own at the band's pricing point with none set.
func (c *cycle) k2ITLBound(variant string) itlBatchBound {
	if !c.a.k2AtITLTarget {
		return itlBatchBound{}
	}
	why, ok := c.itlDistrust[variant]
	if !ok {
		why = itlBoundNoLine
	}
	if why != "" {
		return itlBatchBound{on: true, why: why}
	}
	params := engineParamsFor(c.a, c.input.Namespace, c.input.ModelID, variant)
	if params.SpeculatesPerStep() && !c.a.itlPerToken {
		return itlBatchBound{on: true, why: itlBoundPerStep}
	}
	return itlBoundFor(c.itlModels[variant], sloTPOTTarget(c.cfg), pricingK(c.cfg))
}

// sloTPOTTarget is the policy's TPOT objective in seconds, or 0 with none.
func sloTPOTTarget(cfg *config.ScalingPolicy) float64 {
	if cfg == nil || cfg.SLO == nil || cfg.SLO.TPOT == nil {
		return 0
	}
	return cfg.SLO.TPOT.Target.Seconds()
}

// itlBoundFor is the bound line m gives at a TPOT target of tpot seconds, or,
// with none (0), at the fill kPrice, with the switch on.
func itlBoundFor(m itl.Model, tpot, kPrice float64) itlBatchBound {
	if !itl.ValidModel(m.A, m.B) {
		return itlBatchBound{on: true, why: itlBoundNoLine}
	}
	if !(tpot > 0) || math.IsInf(tpot, 0) {
		if !(kPrice > 0) || kPrice > 1 {
			return itlBatchBound{on: true, why: itlBoundNoLine}
		}
		return itlBatchBound{on: true, fill: kPrice, target: m.ITLAt(kPrice), source: itlTargetPricingPoint}
	}
	fill := (tpot - m.B) / m.A
	if !(fill > 0) {
		return itlBatchBound{on: true, target: tpot, source: itlTargetSLO, why: itlBoundUnderIntercept}
	}
	return itlBatchBound{on: true, fill: min(fill, 1), target: tpot, source: itlTargetSLO}
}

// k2BatchBoundFields is what the P3 k2-decision line adds with
// SwitchK2BatchAtITLTarget on: the steady batch and the ITL bound's batch a
// rank, which of the two held k2 (k2BatchBound "steady" or "itl"), the
// target and where it came from, and why there was no bound where there was
// none. N_itl is the bound's fill of the rank's KV over the KV a request is
// priced at: the resident reading where it prices k2, else I + D x O/2.
func k2BatchBoundFields(params *capacity.EngineParams, batchCap pricedBatchCap, avgInput, avgOutput, dispersion float64,
	ranks int, kvTokens, atSteady int64, itlHeld bool) []any {
	b := batchCap.itl
	bound := "steady"
	if itlHeld {
		bound = "itl"
	}
	out := []any{"nSteady", derivedBatch(params, batchCap.perRank, avgInput, avgOutput, batchCap.budget),
		"k2AtSteady", atSteady, "k2BatchBound", bound, "itlTargetSource", b.source, "itlBoundWhy", b.why}
	if !b.applies() {
		return out
	}
	if !(dispersion >= 1) || math.IsInf(dispersion, 0) {
		dispersion = 1
	}
	kvReq := avgInput + dispersion*avgOutput/2
	if batchCap.resident.known() {
		kvReq = batchCap.resident.perRequest
	}
	nITL := 0.0
	if kvReq > 0 {
		nITL = b.fill * float64(kvTokens) / float64(max(ranks, 1)) / kvReq
	}
	return append(out, "nITL", nITL, "itlTargetFill", b.fill, "itlTargetSec", b.target)
}

// itlBoundedK2 is k2 held at the KV the replica holds at the bound's fill,
// kvTokens of it in all, and whether the bound held it there.
func itlBoundedK2(k2 int64, b itlBatchBound, kvTokens int64) (int64, bool) {
	if !b.applies() || kvTokens <= 0 {
		return k2, false
	}
	if at := int64(b.fill * float64(kvTokens)); at > 0 && at < k2 {
		return at, true
	}
	return k2, false
}
