package whatif

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The Qwen3-32B staircase (2 prefill and 2 decode pods at TP 2, an H200
// cluster) sent flow-control-on steps past the prefill onset where the router
// really held: the first run to measure the wait TTFTFleet.PriceRouterWait
// takes as priced. It reads short there, so the switch stays off
// (DefaultPriceRouterWait).

// q32Fleet is that run's frozen fleet for bucket B (8000-token prompts,
// Poisson) or C (the same at Ca^2 4), on the processor-sharing model with
// flow control off: the freeze's fleet to the bit
// (TestStaircaseQ32_TheFixtureIsTheFrozenCurve).
func q32Fleet(bucket string) TTFTFleet {
	ca2 := 0.0
	if bucket == "C" {
		ca2 = 4
	}
	return TTFTFleet{
		Prefill: Prefill{
			VP: 17046.48, VPLowerBound: true, FixedLatency: 0.009011691426531434, Imbalance: 1, Replicas: 2,
			PromptTokens: 8000, PromptCV: 0.0577, ArrivalCV2: ca2, TransferSecondsPerToken: 2.8114833006122766e-06,
			VPPromptTokens: 7984.303256867742, CurveSlope: 5.135289702755557e-05, CurveCurvature: 8.506104476023039e-10,
			VPMargin: DefaultVPMargin,
		},
		Decode: &Decode{
			Line: itl.Model{A: 0.04249685049858612, B: 0.010425088112190157}, Band: FillBand{Lo: 0.15, Hi: 0.8},
			CapacityTokens: 764672, KVPerRequest: 8114.099089776806, OutputTokens: 200, ReplicaCap: 256, Replicas: 2, Tier: "held",
		},
		DecodeTerm: &DecodeTerm{
			Engine:  PromptLine{Base: -0.08874498911329295, PerToken: 1.2311228655172654e-05},
			PerStep: 8.627566545897512,
			Hop:     PromptLine{Base: -0.02158598458382821, PerToken: 2.559333408175386e-05},
		},
		ProcessorSharing: true,
		FlowControl:      FlowControl{State: FlowControlOff},
	}
}

// q32Step is one flow-control-on step: the rate asked, the frozen curve's
// mean, p90 and p95 there (seconds), the client's over the step's judged
// window (seconds), and the router's mean wait for the requests it
// dispatched (seconds). failed is the requests the router evicted on its
// TTL; a step with any is void for the gate, and its client figures are the
// dispatched requests' only, so they read low.
type q32Step struct {
	rate          float64
	frozen        [3]float64
	client        [3]float64
	routerWait    float64
	failed        int
	pastTheOnset  bool
	insideTheTake bool
}

// q32Steps are the steps at and next below each prefill onset (B 3.707 r/s,
// C 3.098 r/s): the router held nothing below them and a wait of seconds at
// and past them.
var q32Steps = map[string][]q32Step{
	"B": {
		{3.6428485858804147, [3]float64{5.273274550350335, 11.409593807675499, 14.419681319288916}, [3]float64{2.1215, 4.4565, 5.2679}, 0, 0, false, false},
		{3.845229062873771, [3]float64{10.215706637866445, 22.81984585535476, 29.25444764090113}, [3]float64{15.7473, 0, 0}, 4.1838, 709, true, false},
	},
	"C": {
		{3.0357071549003454, [3]float64{4.522326965600122, 10.102519918021411, 12.840557197577244}, [3]float64{3.0083, 6.1311, 7.3965}, 0, 0, false, false},
		{3.238087631893702, [3]float64{5.756415452073837, 12.954067517566163, 16.543123941994388}, [3]float64{7.2284, 15.3978, 28.8396}, 1.4025, 0, true, true},
		{3.33927787039038, [3]float64{6.637648400406939, 14.988989867359779, 19.187698547244437}, [3]float64{11.7434, 42.3026, 49.5451}, 3.1681, 127, true, true},
	},
}

// The fixture is the frozen fleet: its TTFT at every step is the frozen
// curve's, to the bit printed.
func TestStaircaseQ32_TheFixtureIsTheFrozenCurve(t *testing.T) {
	for bucket, steps := range q32Steps {
		for _, s := range steps {
			for i, q := range []float64{0, 0.9, 0.95} {
				got := psTTFT(t, q32Fleet(bucket), s.rate, q).Seconds
				nearRel(t, bucket+" frozen curve", got, s.frozen[i], 1e-9)
			}
		}
	}
}

// A known failure, asserted so that a fix shows. Below each onset the price
// reads well above the client (1.5 to 2.6 times). At C's first step past it
// (3.24 r/s, 0.80 of the prefill knee, inside MaxRouterWaitKneeShare, so the
// switch takes it as FlowControlWaitPriced) the router held 19 % of requests
// for a mean of 1.40 s, and the price reads 0.80 of the client's mean, 0.84
// of its p90 and 0.57 of its p95. At the next step (3.34 r/s, void: 127
// evicted) it reads 0.57, 0.35 and 0.39 of what the dispatched requests
// alone saw. The gate does not only move the wait, as the switch argues: at
// the same rates with flow control off the client read 3.14 s and 3.60 s,
// under the price. Taken as priced, it would also lift bucket C's rate for
// its frozen p95 target from the onset (3.10 r/s, inside the measured
// crossing, 3.04 to 3.24) to 3.44 r/s, past it.
func TestStaircaseQ32_TheRouterWaitStepsReadShort(t *testing.T) {
	if DefaultPriceRouterWait {
		t.Fatal("PriceRouterWait reads short of the Qwen3-32B staircase's client past the prefill onset: keep it off")
	}
	for bucket, steps := range q32Steps {
		fl := fcFleet(q32Fleet(bucket), true)
		for _, s := range steps {
			mean := mustTTFT(t, ttftAt(t, fl, s.rate, 0))
			if s.pastTheOnset != (mean.HasFlag(FlowControlWaitPriced) || mean.HasFlag(AboveFlowControlOnset)) ||
				s.insideTheTake != mean.HasFlag(FlowControlWaitPriced) {
				t.Errorf("%s %.3f r/s: flags %v", bucket, s.rate, mean.Flags)
			}
			for i, q := range []float64{0, 0.9, 0.95} {
				if s.client[i] == 0 {
					continue
				}
				got := mustTTFT(t, ttftAt(t, fl, s.rate, q)).Seconds
				t.Logf("%s %.3f r/s q %g: model %.2f s, client %.2f s (%.2fx), router wait %.2f s, %d evicted",
					bucket, s.rate, q, got, s.client[i], got/s.client[i], s.routerWait, s.failed)
				if s.pastTheOnset && got >= s.client[i] {
					t.Errorf("the known failure is gone: %s %.3f r/s q %g reads %.2f s, at or over the client's %.2f s; revisit DefaultPriceRouterWait and the TTFT page", bucket, s.rate, q, got, s.client[i])
				}
				if !s.pastTheOnset && got < s.client[i] {
					t.Errorf("%s %.3f r/s q %g: below the onset the model reads %.2f s, under the client's %.2f s", bucket, s.rate, q, got, s.client[i])
				}
			}
		}
	}
	// C's frozen p95 target: the measured crossing is between the steps at
	// 3.036 r/s (7.40 s) and 3.238 r/s (28.84 s).
	target := TTFTTarget{Seconds: 22.71371427515482, Percentile: 0.95}
	const crossingTop = 3.238087631893702
	off := RateForTTFT(fcFleet(q32Fleet("C"), false), target).(Answer)
	on := RateForTTFT(fcFleet(q32Fleet("C"), true), target).(Answer)
	t.Logf("C p95 %.2f s: %.3f r/s with the wait flagged, %.3f r/s taken as priced, measured crossing under %.3f r/s", target.Seconds, off.Value, on.Value, crossingTop)
	if !(off.Value < crossingTop && on.Value > crossingTop) {
		t.Errorf("the known failure is gone: C p95 rate %.3f r/s flagged, %.3f r/s priced, against a crossing under %.3f r/s", off.Value, on.Value, crossingTop)
	}
}
