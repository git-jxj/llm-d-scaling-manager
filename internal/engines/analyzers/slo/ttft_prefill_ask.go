package slo

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// A short-prompt P/D prefill below its queue gate
// (llm-d-incubation/llm-d-scaling-manager#131). Its engine never queues, so
// the saturation analyzer prices it at k1 with the busy-share floor, which
// asks for the fleet it runs: on the 8000/200 staircase that was the fleet
// that missed TTFT. This analyzer prices TTFT, but on such a prefill it
// priced nothing and asked nothing:
//
//   - P1's a needs low-load minutes over a spread of prompt lengths. A pod
//     that runs one prompt length, above LowLoadOccupancy, never gets them
//     (TooFewLowLoadMinutes, NoPromptSpread), so every cycle was
//     no-fixed-latency. The joint window's a₀ needs no low load: it is read
//     on queue-free minutes at any load (prefill.JointEstimate.Overhead).
//   - A priced prefill variant was left no-data, so its TTFT answer was a
//     series and a log line, never an ask.

// SwitchTTFTSizesPrefill is the replay switch for sizing a prefill variant on
// TTFT: on, the joint a₀ stands where P1 refused a (withJointFixedLatency),
// and a prefill variant whose TTFT answer is priced carries it as its
// capacity (sizePrefill): the per-replica rate at which the fleet meets the
// target, in requests per second, against the model's arrival rate as the
// prefill role's demand. Off, as before, a stays refused and a prefill
// variant is no-data. Off from the constructor; only SetSwitchesForReplay
// turns it on. The analyzer only observes, so on it moves no replica count
// either: it moves this analyzer's prefill demand and target series and its
// TTFT readings.
const SwitchTTFTSizesPrefill = "slo-ttft-sizes-prefill"

// reasonTTFTSized is the Reason of a prefill variant sized on TTFT.
const reasonTTFTSized = "slo-ttft"

// Where a came from, on the log line.
const (
	aSourceP1    = "p1"
	aSourceJoint = "joint-overhead"
	aSourceNone  = "none"
)

// withJointFixedLatency is est with the joint window's a₀ as a where P1
// refused a, and whether it was taken. a₀ is the prefill pod's overhead
// around its compute (AvgTTFT less the engine's queue and prefill time,
// unstretched), a high quantile of its queue-free minutes; it holds no
// fixed compute, which the unshared work S the inverted V_P is read from
// already carries. Beside a V_P of P1's it can read low by P1's intercept
// s0, a few milliseconds (8.46 ms on ols-sweep's line).
func withJointFixedLatency(est prefill.Estimate, j *jointCycle) (prefill.Estimate, bool) {
	if j == nil || est.A > 0 && est.ARefused == "" || j.est.OverheadRefused != "" ||
		!(j.est.Overhead > 0) || !whatif.Finite(j.est.Overhead) {
		return est, false
	}
	est.A, est.ARefused = j.est.Overhead, ""
	return est, true
}

// aSource is where the a priced came from.
func aSource(est prefill.Estimate, fromJoint bool) string {
	switch {
	case fromJoint:
		return aSourceJoint
	case est.A > 0 && est.ARefused == "":
		return aSourceP1
	}
	return aSourceNone
}

// modelArrival is the model's arrival rate, requests per second: the router's,
// or the prefill rows' with none.
func modelArrival(input domain.AnalyzerInput, fleet fleetReading) float64 {
	if arrival := input.ArrivalRate; arrival > 0 && whatif.Finite(arrival) {
		return arrival
	}
	return fleet.requestRate
}

// sizePrefill sets the capacity of each prefill variant whose TTFT reading is
// priced: per replica, the reading's rate (RateForTTFT at the current count
// over the model's Ready prefill replicas), and the model's arrival rate as
// the prefill role's demand, split over the variants by Ready replicas, so
// the role's need is arrival / per-replica rate. Requests per second, this
// role's own unit; decode keeps its tokens.
//
// Only a priced reading sizes: decode-bound and routing-bound say more
// prefill replicas would not meet the target, and a refusal has no rate.
// The need reads the rate as linear in the count, which holds the fixed
// terms (a, the decode side) and f where the count moves them; the
// unit tests price the count it asks for and hold that it meets the target.
func sizePrefill(result *domain.AnalyzerResult, readings []metrics.SLOTTFTReading, fleet fleetReading, arrival float64) {
	if !(arrival > 0) || !whatif.Finite(arrival) {
		return
	}
	readyPrefill := 0
	for _, vs := range fleet.variants {
		if domain.CanonicalRole(vs.Role) == domain.RolePrefill {
			readyPrefill += fleet.ready[vs.VariantName]
		}
	}
	if readyPrefill == 0 {
		return
	}
	priced := map[string]float64{}
	for _, r := range readings {
		if r.Reason == constants.SLOTTFTReasonPriced && r.PerReplica > 0 && !math.IsInf(r.PerReplica, 0) {
			priced[r.Variant] = r.PerReplica
		}
	}
	if len(priced) == 0 {
		return
	}
	for i := range result.VariantCapacities {
		vc := &result.VariantCapacities[i]
		rate, ok := priced[vc.VariantName]
		if !ok || domain.CanonicalRole(vc.Role) != domain.RolePrefill {
			continue
		}
		vc.PerReplicaCapacity = rate
		vc.Reason = reasonTTFTSized
		vc.TotalDemand = arrival * float64(fleet.ready[vc.VariantName]) / float64(readyPrefill)
		if vc.ObservedReplicas > 0 {
			vc.Utilization = vc.TotalDemand / (float64(vc.ObservedReplicas) * rate)
		}
	}
	if result.RoleDemand == nil {
		result.RoleDemand = map[string]float64{}
	}
	result.RoleDemand[domain.RolePrefill] = arrival
}
