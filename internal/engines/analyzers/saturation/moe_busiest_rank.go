package saturation

import "github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"

// SwitchMoEBusiestRank is the replay switch that fits the ITL line of a
// data-parallel MoE engine against its busiest rank's KV fill, not the mean
// over its ranks (itl.Admission.BusiestRankFill).
//
// vLLM runs the data-parallel ranks of an expert-parallel MoE engine in
// lockstep: each step is padded to the busiest rank's batch, idle ranks run
// dummy batches, and the expert all-to-all syncs every MoE layer, so the
// pod's step time follows its busiest rank. Its data-parallel balancer evens
// out request counts, not KV, so the busiest rank fills first: at a mean
// fill at the band's top it can be full, preempt, and stall every rank. A
// line and a band read on the mean put the knee too high (issue
// llm-d-incubation/llm-d-scaling-manager#312).
//
// On, for a variant whose engine runs expert parallelism over more than one
// data-parallel rank (BusiestRankFillFor), the line's readings and its band
// are the busiest rank's, and the capacity planner prices the mean fill at
// the band's top over the busiest/mean ratio its replay read
// (learnreplay.VariantState.BusiestRankRatio). Dense engines' ranks step
// independently and are unchanged. Planner-only and off from the
// constructor; the planner's replay does not turn it on
// (learnreplay.PlannerSwitches): no recording we hold runs data parallelism,
// so the evidence for it is synthetic.
const SwitchMoEBusiestRank = "moe-dp-busiest-rank"

// BusiestRankFillFor reports whether a variant run under params has its ITL
// line read at its busiest rank: SwitchMoEBusiestRank on, and an engine
// with expert parallelism over more than one data-parallel rank. A row of
// one rank reads the same either way (domain.ReplicaMetrics.BusiestRankKvUsageForITL).
func (a *SaturationAnalyzer) BusiestRankFillFor(params *capacity.EngineParams) bool {
	return a.moeBusiestRank && params != nil && params.ExpertParallel && params.DataParallelSize > 1
}
