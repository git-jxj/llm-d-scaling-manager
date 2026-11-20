package saturation

import (
	"math"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// SwitchK2BatchAtITLTarget is the replay switch for the TPOT objective's
// bound on a decode replica's k2 (issue
// llm-d-incubation/llm-d-scaling-manager#258).
//
// k2 is how many tokens a replica holds when it is busy, and nothing in it
// prices latency: the derived k2 (P3) holds a rank at the batch its token
// budget sustains in steady state, N_steady (steadyBatch), and a measured k2
// (P1, P2) at the fill a replica was seen to queue at. The more a decode
// replica holds, the slower each token. With a decode's budget charged only
// the prompt it computes (SwitchK2DecodeBudgetAtComputedPrompt), N_steady is
// large: on a GPU run one decode replica carried the batch three had, and
// TPOT p90 rose from 12 to 15-16 ms.
//
// On, where the policy sets a TPOT objective (slo.tpot, the target the SLO
// analyzer prices; config.SLOPolicy) and lists the SLO analyzer, a decode
// replica's k2, whichever priority priced it, is held at the KV its variant
// holds where its own decode ITL line reaches the target: a batch of
// min(N_steady, N_itl) on the derived k2. The ITL line is in KV fill,
// ITL(k) = A x k + B, per token (SwitchITLPerToken, which reads the step
// time over the tokens a step emits at the measured speculative
// acceptance). The fill it reaches the target at, k_itl, held to the ITL
// band, holds a rank at k_itl x C / KVreq requests, C the rank's KV; so the
// bound on k2 is k_itl x the replica's KV, and the k2-itl-bound line carries
// N_itl a rank at the context, I + D x O/2. The line's k is the pod's mean
// fill, so the bound holds the mean ITL at the target; the busiest rank's
// requests run above it. With no TPOT objective there is no bound: the
// band's pricing point is a fill, not a latency, and nothing measured put
// the latency cost there.
//
// The bound applies only on a decode variant's line it learned itself
// (k2ITLDistrust): not one borrowed from a sibling, held from an earlier fit,
// priced one-sided at a prior, fitted above the band, a full fit resting
// below the spread that earns B, or pinned at the default intercept
// (itl.DefaultBaselineSec) rather than a B the key measured or read through
// its floor below the band. A prefill or aggregated variant is not bound. A
// line in step time (SwitchITLPerToken off on a speculating engine) is not
// set against a per-token target. In each of these cases k2 is the chain's,
// as before, and the k2-itl-bound line says why.
//
// It only lowers k2, after the chain and its lower bound: the figure priced
// and the one the capacity store keeps are both held, so a parked variant's
// zero-replica estimate and a sibling's lookup inherit it. A measured window
// is not changed; it is held when read.
//
// On from the constructor: a policy that states a TPOT objective is not
// sized to pack decode past it. A replay turns it off (SetSwitchesForReplay).
const SwitchK2BatchAtITLTarget = "k2-batch-held-at-itl-target"

// Which bound held k2 (k2BatchBound on the k2-itl-bound line): the ITL
// target, or none, the chain's figure standing.
const (
	k2BoundNone = "none"
	k2BoundITL  = "itl"
)

// Where the ITL target k2 is held at came from (itlBatchBound.source).
const (
	// itlTargetSLO is the policy's TPOT objective.
	itlTargetSLO = "slo-tpot"
)

// Why k2 is not held at an ITL target (itlBatchBound.why), with the switch
// on.
const (
	itlBoundNoTarget  = "no-tpot-target"
	itlBoundNoLine    = "no-line"
	itlBoundBorrowed  = "borrowed-line"
	itlBoundHeld      = "held-line"
	itlBoundOneSided  = "one-sided-line"
	itlBoundAboveBand = "above-band-line"
	itlBoundDefaultB  = "default-intercept"
	itlBoundPerStep   = "per-step-line"
	itlBoundResting   = "resting-fit"
	itlBoundNotDecode = "not-decode"
)

// Where the fill the line reaches the target at was held to the ITL band
// (itlBatchBound.clamp): the line was not fitted past it.
const (
	// itlClampLow is a fill under the band's lower edge, or a target at or
	// under the line's intercept, which no fill meets: held at the edge.
	itlClampLow = "below-band"
	// itlClampHigh is a fill over the band's upper edge: held at the edge.
	itlClampHigh = "above-band"
)

// itlBatchBound is the KV fill at which a replica's ITL line reaches its
// target, the bound on its k2 (SwitchK2BatchAtITLTarget). The zero value is
// the switch off. With it on, fill is 0 where there is no bound, and why
// says why.
type itlBatchBound struct {
	// on is SwitchK2BatchAtITLTarget.
	on bool
	// fill is the KV fill the line reaches the target at, held to the ITL
	// band.
	fill float64
	// target is the latency target, seconds per token.
	target float64
	// source is itlTargetSLO.
	source string
	// clamp says the fill was held to the band's edge (itlClampLow,
	// itlClampHigh), and is empty where it was not.
	clamp string
	// why is empty with a bound, and otherwise why there is none.
	why string
}

// applies reports whether b bounds k2.
func (b itlBatchBound) applies() bool {
	return b.on && b.why == "" && b.fill > 0 && b.fill <= 1
}

// k2ITLDistrust says why the line r is not one k2 may be held at
// (SwitchK2BatchAtITLTarget), or "" when it is: a line the key fitted this
// cycle, with an intercept it measured or read through its floor. A B read
// through the floor may err high and the slope low; the line then reads ITL
// low past its readings and high before them, so a target above the
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

// k2ITLBound is the bound on k2 for variant this cycle
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
// with the switch on. No target (0) is no bound. The fill is held to the ITL
// band [lo, hi], where the line was fitted: a target the line reaches under
// lo, or at or under its intercept, at lo; one past hi at hi. Held at lo, a
// tighter target never asks for fewer replicas than a looser one, and the
// most it asks is 1/lo times the replicas at a full cache.
func itlBoundFor(m itl.Model, tpot, lo, hi float64) itlBatchBound {
	if !(tpot > 0) || math.IsInf(tpot, 0) {
		return itlBatchBound{on: true, why: itlBoundNoTarget}
	}
	if !itl.ValidModel(m.A, m.B) || !(lo > 0) || !(hi >= lo) || hi > 1 {
		return itlBatchBound{on: true, why: itlBoundNoLine}
	}
	b := itlBatchBound{on: true, fill: (tpot - m.B) / m.A, target: tpot, source: itlTargetSLO}
	switch {
	case !(b.fill >= lo):
		b.fill, b.clamp = lo, itlClampLow
	case b.fill > hi:
		b.fill, b.clamp = hi, itlClampHigh
	}
	return b
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

// logITLBound logs, with SwitchK2BatchAtITLTarget on, what the bound made
// of a replica's k2: the chain's figure (k2Unbounded, from priority) and the
// one priced, which bound held it (k2BatchBound), the target, the fill and
// any clamp to the band, why there was no bound where there was none, and
// the batch a rank under each: nSteady, the token budget's (steadyBatch),
// and nITL, the bound's fill of the rank's KV over I + D x O/2.
func logITLBound(logger logr.Logger, modelID, namespace, variant string, priority capacity.K2Source,
	params *capacity.EngineParams, batchCap pricedBatchCap, avgInput, avgOutput, dispersion float64,
	ranks int, kvTokens, unbounded, k2 int64, held bool) {
	b := batchCap.itl
	if !b.on {
		return
	}
	bound := k2BoundNone
	if held {
		bound = k2BoundITL
	}
	kv := []any{"modelID", modelID, "namespace", namespace, "variant", variant,
		"priority", priority.String(), "k2Unbounded", unbounded, "k2", k2, "k2BatchBound", bound,
		"itlBoundWhy", b.why, "nSteady", steadyBatchAt(params, batchCap.perRank, avgInput, avgOutput, batchCap.budget)}
	if b.applies() {
		if !(dispersion >= 1) || math.IsInf(dispersion, 0) {
			dispersion = 1
		}
		nITL := 0.0
		if kvReq := avgInput + dispersion*avgOutput/2; kvReq > 0 {
			nITL = b.fill * float64(kvTokens) / float64(max(ranks, 1)) / kvReq
		}
		kv = append(kv, "nITL", nITL, "itlTargetSource", b.source, "itlTargetFill", b.fill,
			"itlTargetSec", b.target, "itlTargetClamp", b.clamp)
	}
	logger.V(perReplicaLogLevel).Info("k2-itl-bound", kv...)
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
