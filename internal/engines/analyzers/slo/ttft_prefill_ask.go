package slo

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
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
// and each prefill variant whose TTFT answer is priced is sized at the least
// pod count the model prices under the target (prefillAsk, sizePrefill), in
// requests per second against the model's arrival rate as the prefill role's
// demand. Off, as before, a stays refused and a prefill variant is no-data;
// the slo-ttft-variant line carries aSource either way. Off from the
// constructor; only SetSwitchesForReplay turns it on. The analyzer only
// observes, so on it moves no replica count either: it moves this analyzer's
// prefill demand and target series and its TTFT readings.
//
// a₀ comes from the joint window, which is kept only for a model of one
// prefill and one decode variant and with SwitchTTFTJointInputs on
// (noteJoint); otherwise P1's a alone prices, as off. A model of several
// prefill variants is not sized: each variant's ask is priced with the others
// at their current count, and no one prices the asks together.
const SwitchTTFTSizesPrefill = "slo-ttft-sizes-prefill"

// reasonTTFTSized is the Reason of a prefill variant sized on TTFT.
const reasonTTFTSized = "slo-ttft"

// maxPrefillAskFactor bounds the search for a prefill count: at most this many
// times the variant's Ready pods. Near the floor the fixed terms (a, the
// decode engine's TTFT) leave the prefill stage almost no budget, and the
// least count that meets climbs without bound; that is the decode side's
// answer, not prefill's, so past the bound nothing is asked. Stated, not
// measured.
const maxPrefillAskFactor = 4

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
//
// P1's low-load curve is dropped with it: P1 may have fitted the curve and
// then refused its intercept (NoShortPrompts, NonPositiveIntercept,
// AUncertain), and V_P is not rescaled by a curve P1 did not stand behind.
func withJointFixedLatency(est prefill.Estimate, j *jointCycle) (prefill.Estimate, bool) {
	if j == nil || est.A > 0 && est.ARefused == "" || j.est.OverheadRefused != "" ||
		!(j.est.Overhead > 0) || !whatif.Finite(j.est.Overhead) {
		return est, false
	}
	est.A, est.ARefused = j.est.Overhead, ""
	est.AModel, est.ASlope, est.ACurve = "", 0, 0
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

// prefillAsk is the least count of the variant's pods, the model's other
// prefill pods held, at which the model prices the TTFT at rate under the
// target, searched from one pod to maxPrefillAskFactor times the Ready pods;
// false when none there does. It prices each count as S2's search does
// (ttftFleet), so f moves with the count, which a rate read at the current
// count and divided out would not.
func prefillAsk(in ttftInputs, rate float64, target whatif.TTFTTarget) (int, bool) {
	if in.pods < 1 || !(rate > 0) || !whatif.Finite(rate) {
		return 0, false
	}
	for n := 1; n <= maxPrefillAskFactor*in.pods; n++ {
		fl, _ := ttftFleet(in, n)
		if met, known := whatif.TTFTMeets(whatif.TTFTAt(fl, rate, target.Percentile), target); known && met {
			return n, true
		}
	}
	return 0, false
}

// sizePrefill sets the capacity of the model's one prefill variant with a
// Ready pod from its ask (prefillAsk): per replica, the arrival rate over
// the ask, so the need the engine reads (role demand over it,
// TotalReplicas) is the ask; the role's demand and the variant's are the
// arrival rate. Requests per second, this role's own unit; decode keeps its
// tokens. A model with several prefill variants serving is not sized
// (SwitchTTFTSizesPrefill).
//
// On a P/D model this analyzer's model-level supply then adds requests per
// second to decode's tokens per second: only the per-role figures mean
// anything there, and slo is in no composite.
func sizePrefill(result *domain.AnalyzerResult, asks map[string]int, fleet fleetReading, arrival float64) {
	if !(arrival > 0) || !whatif.Finite(arrival) {
		return
	}
	variant := ""
	for _, vs := range fleet.variants {
		if domain.CanonicalRole(vs.Role) != domain.RolePrefill || fleet.ready[vs.VariantName] == 0 {
			continue
		}
		if variant != "" {
			return
		}
		variant = vs.VariantName
	}
	ask := asks[variant]
	if variant == "" || ask < 1 {
		return
	}
	for i := range result.VariantCapacities {
		vc := &result.VariantCapacities[i]
		if vc.VariantName != variant || domain.CanonicalRole(vc.Role) != domain.RolePrefill {
			continue
		}
		rate := arrival / float64(ask)
		vc.PerReplicaCapacity = rate
		vc.Reason = reasonTTFTSized
		vc.TotalDemand = arrival
		if vc.ObservedReplicas > 0 {
			vc.Utilization = vc.TotalDemand / (float64(vc.ObservedReplicas) * rate)
		}
	}
	if result.RoleDemand == nil {
		result.RoleDemand = map[string]float64{}
	}
	result.RoleDemand[domain.RolePrefill] = arrival
}
