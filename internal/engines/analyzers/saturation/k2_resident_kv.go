package saturation

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// SwitchK2DerivedAtResidentKV is the replay switch for the KV a resident
// request holds in the derived k2 (P3; issue
// llm-d-incubation/llm-d-scaling-manager#201, the follow-up to PR #205).
//
// The derived k2 is the requests one engine rank holds in steady state times
// the KV each one holds, and it prices each at the context, I + D x O/2
// (estimateCapacityAtCapBudget). Demand is the KV the replica's requests
// actually hold. An engine that holds more KV per request than its context
// (a sparse-attention indexer's keys, a speculative draft model's KV; issue
// #188) fills its cache at the same request count with that many more
// tokens, so its k2 is that many times too low against demand, and the
// fleet is ordered that many times the replicas. On a day-long P/D
// recording the resident KV per running request was 2.7-3.8x the context
// (p5-p95).
//
// On, the derived k2 prices each request at the fleet's resident reading
// (fleetResidentKVPerRequest), as SwitchKVPerRequestAboveContext prices it
// for mu, where all of these hold (k2ResidentKV):
//
//   - SwitchKVPerRequestHitsResident and SwitchKVPerRequestAboveContext are
//     on. The reading is trusted above the context only behind the gates
//     that switch adds: the count gate taken at the reading, at least
//     minAboveContextSequences requests, and the requests that hold KV
//     without running counted among them. Without it the reading is held at
//     the context, which is what the derived k2 prices already.
//   - The reading was taken and priced as read: residentBoundNone or
//     residentBoundAboveContext, not held at either bound and not refused.
//   - It is above the fleet's whole context. A reading below it is prefix
//     sharing, which the derived k2 has never priced; this switch leaves it.
//   - No shape change is outstanding: the requests resident then belong to
//     two shapes, and the derived k2 prices no spread either.
//
// It only raises k2, and never above k1: the figure is the larger of the k2
// priced at the context and the k2 priced at the reading, the latter held
// at k1 (k2AtResidentKV). Anywhere else it is priced at the context, as
// before. The prefill role has no derived k2, and the zero-replica estimate
// has no reading; neither moves.
//
// Off from the constructor: on, a decode is priced at more capacity and
// fewer replicas are ordered, and the default changes only after a GPU
// check, with SwitchKVPerRequestAboveContext's.
const SwitchK2DerivedAtResidentKV = "k2-derived-at-resident-kv"

// k2ResidentKV is the KV a resident request holds that the derived k2 prices
// a replica at under SwitchK2DerivedAtResidentKV: fleetShape's KVreq as
// withResidentKV priced it, with bound the bound it reported. It is 0, the
// context, unless every condition the switch's comment lists holds.
func (a *SaturationAnalyzer) k2ResidentKV(fleetShape shape.Shape, bound string, shapeChanging bool) float64 {
	if !a.k2AtResidentKV || !a.kvHitsResident || !a.kvAboveContext || shapeChanging {
		return 0
	}
	if bound != residentBoundNone && bound != residentBoundAboveContext {
		return 0
	}
	kv := fleetShape.KVreq
	if math.IsInf(kv, 0) || !(kv > fleetShape.Context()) {
		return 0
	}
	return kv
}

// derivedBatch is the requests one engine rank holds in steady state at
// batch cap S under params, its token budget spent as budget says
// (steadyBatch): S where params state no budget. 0 where the derived k2 has
// nothing to price (estimateCapacityAtCapBudget).
func derivedBatch(params *capacity.EngineParams, batchCap int64, avgInput, avgOutput float64, budget stepBudget) float64 {
	if batchCap <= 0 || avgOutput <= 0 || (params != nil && params.EffectiveMaxBatchedTokens <= 0) {
		return 0
	}
	if params == nil {
		return float64(batchCap)
	}
	return steadyBatch(float64(params.EffectiveMaxBatchedTokens), float64(batchCap), avgInput, avgOutput, budget)
}

// k2AtResidentKV is the derived k2 of a replica of ranks engine ranks with
// each resident request priced at residentKV, not at its context
// (SwitchK2DerivedAtResidentKV), held at k1, and never below atContext, the
// figure priced at the context. It is atContext where residentKV is 0 or
// the batch is.
func k2AtResidentKV(params *capacity.EngineParams, batchCap pricedBatchCap, avgInput, avgOutput float64,
	ranks int, k1, atContext int64) int64 {
	if !(batchCap.residentKV > 0) {
		return atContext
	}
	n := derivedBatch(params, batchCap.perRank, avgInput, avgOutput, batchCap.budget)
	if !(n > 0) {
		return atContext
	}
	at := n * batchCap.residentKV * float64(max(ranks, 1))
	if !(at < float64(k1)) {
		return max(atContext, k1)
	}
	return max(atContext, int64(at))
}
