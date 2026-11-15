package whatif

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The Qwen3-32B staircase (2 prefill and 2 decode pods at TP 2, an H200
// cluster) sent flow-control-on steps past the prefill onset where the router
// really held: the first run to measure the wait TTFTFleet.PriceRouterWait
// prices. Without a stage of its own the price read short there; with the
// gate priced (ttft_gate.go) it holds the client, so the switch is on by
// default (DefaultPriceRouterWait).

// q32Fleet is that run's frozen fleet for bucket B (8000-token prompts,
// Poisson) or C (the same at Ca^2 4), on the processor-sharing model with
// flow control off: the freeze's fleet to the bit
// (TestStaircaseQ32_TheFixtureIsTheFrozenCurve).
//
// Where it comes from: the run's frozen.json, whose recordingSha256 names the
// warm-up recording it was frozen from
// (4693f3bbbd8322c77af2a1fbcddeaa4cf3ce92f7d6eb66e8437ec2e3900ff291), written
// by `capacity-planner freeze` from that warm-up recording and the run's
// spec (hack/benchmark/staircase_spec_qwen32b.yaml). To regenerate the
// constants, build the fleet as freezeFlags.priceBucket does for the bucket
// (answerFlags.fleetFor on the recording at cycle last, prompt cv set to the
// spec's 0.0577, ArrivalCV2 to the bucket's Ca^2) and print its Prefill,
// Decode and DecodeTerm with %+v; the frozen curve's mean, p90 and p95 are
// the q32Steps' frozen figures.
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

// q32Tolerance is the rule the gate's price had to pass to be the default:
// past the onset, at a step the fleet held, the price may read at most 10 %
// under the client.
const q32Tolerance = 0.9

// The gate priced as a stage (ttft_gate.go) holds the client at the step past
// the prefill onset the fleet held, and moves nothing below the onsets.
//
// Before the stage the switch moved no figure: at C's first step past the
// onset (3.24 r/s, 0.80 of the prefill knee, inside MaxRouterWaitKneeShare)
// the router held 19 % of requests for a mean of 1.40 s, and the price, the
// frozen curve's, read 0.80 of the client's mean, 0.84 of its p90 and 0.57 of
// its p95, and bucket C's rate for its frozen p95 target rose past the
// measured crossing. With the stage the price reads at least 0.9 of the
// client there at the mean, p90 and p95, and the rate answer stays at the
// onset, inside the crossing.
//
// Below each onset the figures are the frozen curve's, to the bit, and at or
// above the client (1.5 to 2.7 times). B's step past the onset (3.85 r/s) is
// past MaxRouterWaitKneeShare: it keeps AboveFlowControlOnset and the frozen
// figure. The step after C's (3.34 r/s) is void (127 evicted): logged, not
// judged; its client figures are the dispatched requests' only. With the
// switch off (the opt-out) every figure is the frozen curve's.
func TestStaircaseQ32_TheGateStageHoldsTheClient(t *testing.T) {
	if !DefaultPriceRouterWait {
		t.Fatal("the gate's price meets its rule on the Qwen3-32B staircase: DefaultPriceRouterWait is on")
	}
	for bucket, steps := range q32Steps {
		fl := fcFleet(q32Fleet(bucket), true)
		for _, s := range steps {
			mean := mustTTFT(t, ttftAt(t, fl, s.rate, 0))
			if s.pastTheOnset != (mean.HasFlag(FlowControlWaitPriced) || mean.HasFlag(AboveFlowControlOnset)) ||
				s.insideTheTake != mean.HasFlag(FlowControlWaitPriced) {
				t.Errorf("%s %.3f r/s: flags %v", bucket, s.rate, mean.Flags)
			}
			if gate := mean.Parts.RouterWait > 0 && mean.Parts.BurstWait > 0; gate != s.insideTheTake {
				t.Errorf("%s %.3f r/s: router wait %g s, burst wait %g s, want the gate priced only where it takes", bucket, s.rate, mean.Parts.RouterWait, mean.Parts.BurstWait)
			}
			for i, q := range []float64{0, 0.9, 0.95} {
				got := mustTTFT(t, ttftAt(t, fl, s.rate, q)).Seconds
				unswitched := mustTTFT(t, ttftAt(t, fcFleet(q32Fleet(bucket), false), s.rate, q)).Seconds
				nearRel(t, bucket+" price, switch off", unswitched, s.frozen[i], 1e-9)
				if !s.insideTheTake {
					nearRel(t, bucket+" price where the gate is not priced", got, s.frozen[i], 1e-9)
				}
				if s.client[i] == 0 {
					continue
				}
				t.Logf("%s %.3f r/s q %g: model %.2f s (frozen %.2f s), client %.2f s (%.2fx), router wait %.2f s priced, %.2f s measured, %d evicted",
					bucket, s.rate, q, got, s.frozen[i], s.client[i], got/s.client[i], mean.Parts.RouterWait, s.routerWait, s.failed)
				// Only a step the fleet held is judged: a void step's client
				// figures are the dispatched requests' only, and read low.
				if s.failed == 0 && got < q32Tolerance*s.client[i] {
					t.Errorf("%s %.3f r/s q %g reads %.2f s, more than %.0f%% under the client's %.2f s",
						bucket, s.rate, q, got, 100*(1-q32Tolerance), s.client[i])
				}
			}
		}
	}
	// C's frozen p95 and p90 targets: the measured crossings are between the
	// steps at 3.036 r/s (p95 7.40 s) and 3.238 r/s (28.84 s), and between
	// 3.238 r/s (p90 15.40 s) and 3.339 r/s.
	for _, c := range []struct {
		target      TTFTTarget
		crossingTop float64
	}{
		{TTFTTarget{Seconds: 22.71371427515482, Percentile: 0.95}, 3.238087631893702},
		{TTFTTarget{Seconds: 17.700823769319424, Percentile: 0.9}, 3.33927787039038},
	} {
		off := mustAnswer(t, rateForTTFT(t, fcFleet(q32Fleet("C"), false), c.target))
		on := mustAnswer(t, rateForTTFT(t, fcFleet(q32Fleet("C"), true), c.target))
		t.Logf("C q %g, %.2f s: %.3f r/s with the gate flagged, %.3f r/s priced, measured crossing under %.3f r/s",
			c.target.Percentile, c.target.Seconds, off.Value, on.Value, c.crossingTop)
		if !(on.Value < c.crossingTop) {
			t.Errorf("C q %g: the priced rate %.3f r/s is past the measured crossing (under %.3f r/s)", c.target.Percentile, on.Value, c.crossingTop)
		}
	}
}
