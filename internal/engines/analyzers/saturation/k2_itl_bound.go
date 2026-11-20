package saturation

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// SwitchK2BatchAtITLTarget is the replay switch for the TPOT objective's
// bound on the derived k2's batch (issue
// llm-d-incubation/llm-d-scaling-manager#258).
//
// The derived k2 (P3) holds a rank at the batch its token budget sustains in
// steady state, N_steady (steadyBatch). That is a throughput ceiling, with no
// latency in it. With a decode's budget charged only the prompt it computes
// (SwitchK2DecodeBudgetAtComputedPrompt), N_steady is large, and the role is
// sized to pack each decode rank to it: on a GPU run one decode replica
// carried the batch three had, and TPOT p90 rose from 12 to 15-16 ms.
//
// On, where the policy sets a TPOT objective (slo.tpot, the target the SLO
// analyzer prices; config.SLOPolicy), the batch is also held at N_itl, the
// batch at which the variant's own decode ITL line reaches it:
// min(N_steady, N_itl). The ITL line is in KV fill, ITL(k) = A x k + B, per
// token (SwitchITLPerToken, which reads the step time over the tokens a step
// emits at the measured speculative acceptance). The fill it reaches the
// target at, k_itl, holds a rank at k_itl x C / KVreq requests, C the rank's
// KV; so the bound on k2 is k_itl x the replica's KV, and the log carries
// N_itl a rank at the context, I + D x O/2. The line's k is the pod's mean
// fill, so the bound holds the mean ITL at the target; the busiest rank's
// requests run above it. With no TPOT objective there is no bound: the band's
// pricing point is a fill, not a latency, and nothing measured put the
// latency cost there.
//
// The bound applies only on a decode variant's line it learned itself
// (k2ITLDistrust): not one borrowed from a sibling, held from an earlier fit,
// priced one-sided at a prior, fitted above the band, a full fit resting
// below the spread that earns B, or pinned at the default intercept
// (itl.DefaultBaselineSec) rather than a B the key measured or read through
// its floor below the band. An aggregated line is report-only for mu
// (itlLineReportOnly) and does not price k2 either. A line in step time
// (SwitchITLPerToken off on a speculating engine) is not set against a
// per-token target. A target at or under the line's B is met at no fill, and
// one met outside the ITL band is read off the line where it was not fitted.
// In each of these cases the batch is N_steady, as before, and the
// k2-decision line says why.
//
// It lowers the derived k2 against the figure with the switch off. The lower
// bound (P2-borrow, P2-prior) is judged against the bounded figure, so a
// prior recorded above it can still price the replica; the borrowed line
// carries the bound's fields then. The capacity store keeps the bounded
// figure, so a parked variant's zero-replica estimate and a sibling's lookup
// inherit it. The prefill role has no derived k2 and does not move.
//
// On from the constructor: a policy that states a TPOT objective is never
// sized to pack decode past it. A replay turns it off (SetSwitchesForReplay).
const SwitchK2BatchAtITLTarget = "k2-batch-held-at-itl-target"

// Which bound held the derived k2's batch (k2BatchBound on the k2-decision
// line): the steady batch at the budget, or the ITL target. On a line a lower
// bound priced, k2BoundLower: the bound's fields are the derived figure's.
const (
	k2BoundSteady = "steady"
	k2BoundITL    = "itl"
	k2BoundLower  = "lower-bound"
)

// Where the ITL target the derived k2's batch is held at came from
// (itlBatchBound.source).
const (
	// itlTargetSLO is the policy's TPOT objective.
	itlTargetSLO = "slo-tpot"
)

// Why the derived k2's batch is not held at an ITL target
// (itlBatchBound.why), with the switch on.
const (
	itlBoundNoTarget       = "no-tpot-target"
	itlBoundNoLine         = "no-line"
	itlBoundBorrowed       = "borrowed-line"
	itlBoundHeld           = "held-line"
	itlBoundOneSided       = "one-sided-line"
	itlBoundAboveBand      = "above-band-line"
	itlBoundDefaultB       = "default-intercept"
	itlBoundPerStep        = "per-step-line"
	itlBoundUnderIntercept = "target-under-intercept"
	itlBoundBelowBand      = "target-below-band"
	itlBoundAboveBandFill  = "target-above-band"
	itlBoundResting        = "resting-fit"
	itlBoundNotDecode      = "not-decode"
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
	// source is itlTargetSLO.
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
// this cycle, with an intercept it measured or read through its floor. A B
// read through the floor may err high and the slope low; the line then reads
// ITL low past its readings and high before them, so a target above the
// readings' ITL gives a looser bound and one below it a tighter.
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
	case r.Resting():
		return itlBoundResting
	case r.Tier == itl.TierPinnedB && !r.BaselineLearned && !r.PinnedAtFloor:
		return itlBoundDefaultB
	}
	return ""
}

// k2ITLBound is the bound on the derived k2's batch for variant this cycle
// (SwitchK2BatchAtITLTarget): the fill at which its line reaches the TPOT
// objective, and none with no objective set.
func (c *cycle) k2ITLBound(variant string) itlBatchBound {
	if !c.a.k2AtITLTarget {
		return itlBatchBound{}
	}
	if domain.CanonicalRole(c.rolesByVariant[variant]) != domain.RoleDecode {
		return itlBatchBound{on: true, why: itlBoundNotDecode}
	}
	why, ok := c.itlDistrust[variant]
	if !ok {
		why = itlBoundNoLine
	}
	if why != "" {
		return itlBatchBound{on: true, why: why}
	}
	tpot := sloTPOTTarget(c.cfg)
	if !(tpot > 0) || math.IsInf(tpot, 0) {
		return itlBatchBound{on: true, why: itlBoundNoTarget}
	}
	params := engineParamsFor(c.a, c.input.Namespace, c.input.ModelID, variant)
	if params.SpeculatesPerStep() && !c.a.itlPerToken {
		return itlBatchBound{on: true, why: itlBoundPerStep}
	}
	lo, hi := c.cfg.ITLBand()
	return itlBoundFor(c.itlModels[variant], tpot, lo, hi)
}

// sloTPOTTarget is the policy's TPOT objective in seconds, or 0 with none.
// Both halves are needed, as for the TTFT pricing (config.PricesTTFT): the
// target, and the SLO analyzer that reads it listed. A target with the
// analyzer unlisted is read by nothing.
func sloTPOTTarget(cfg *config.ScalingPolicy) float64 {
	if cfg == nil || cfg.SLO == nil || cfg.SLO.TPOT == nil || !cfg.SLOAnalyzerListed() {
		return 0
	}
	return cfg.SLO.TPOT.Target.Seconds()
}

// itlBoundFor is the bound line m gives at a TPOT target of tpot seconds,
// with the switch on. No target (0) is no bound, and nor is a target the line
// reaches outside the ITL band [lo, hi]: the line was not fitted there.
func itlBoundFor(m itl.Model, tpot, lo, hi float64) itlBatchBound {
	if !(tpot > 0) || math.IsInf(tpot, 0) {
		return itlBatchBound{on: true, why: itlBoundNoTarget}
	}
	if !itl.ValidModel(m.A, m.B) {
		return itlBatchBound{on: true, why: itlBoundNoLine}
	}
	fill := (tpot - m.B) / m.A
	if !(fill > 0) {
		return itlBatchBound{on: true, target: tpot, source: itlTargetSLO, why: itlBoundUnderIntercept}
	}
	if fill < lo {
		return itlBatchBound{on: true, fill: fill, target: tpot, source: itlTargetSLO, why: itlBoundBelowBand}
	}
	if hi > 0 && fill > hi {
		return itlBatchBound{on: true, fill: fill, target: tpot, source: itlTargetSLO, why: itlBoundAboveBandFill}
	}
	return itlBatchBound{on: true, fill: min(fill, 1), target: tpot, source: itlTargetSLO}
}

// k2BatchBoundFields is what the P3 k2-decision line adds with
// SwitchK2BatchAtITLTarget on: the steady batch and the ITL bound's batch a
// rank, which of the two held k2 (k2BatchBound "steady" or "itl"), the
// target and where it came from, and why there was no bound where there was
// none. N_itl is the bound's fill of the rank's KV over the KV a request is
// priced at, I + D x O/2.
func k2BatchBoundFields(params *capacity.EngineParams, batchCap pricedBatchCap, avgInput, avgOutput, dispersion float64,
	ranks int, kvTokens, atSteady int64, itlHeld, borrowed bool) []any {
	b := batchCap.itl
	bound := k2BoundSteady
	if itlHeld {
		bound = k2BoundITL
	}
	if borrowed {
		bound = k2BoundLower
	}
	out := []any{"nSteady", steadyBatchAt(params, batchCap.perRank, avgInput, avgOutput, batchCap.budget),
		"k2AtSteady", atSteady, "k2BatchBound", bound, "itlTargetSource", b.source, "itlBoundWhy", b.why}
	if !b.applies() {
		return out
	}
	if !(dispersion >= 1) || math.IsInf(dispersion, 0) {
		dispersion = 1
	}
	kvReq := avgInput + dispersion*avgOutput/2
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

// steadyBatchAt is the requests one engine rank holds in steady state at
// batch cap batchCap under params, its token budget spent as budget says
// (steadyBatch): the cap where params state no budget, 0 where the derived
// k2 has nothing to price (estimateCapacityAtCapBudget).
func steadyBatchAt(params *capacity.EngineParams, batchCap int64, avgInput, avgOutput float64, budget stepBudget) float64 {
	if batchCap <= 0 || avgOutput <= 0 || (params != nil && params.EffectiveMaxBatchedTokens <= 0) {
		return 0
	}
	if params == nil {
		return float64(batchCap)
	}
	return steadyBatch(float64(params.EffectiveMaxBatchedTokens), float64(batchCap), avgInput, avgOutput, budget)
}
