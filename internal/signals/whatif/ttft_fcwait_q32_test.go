package whatif

import (
	"maps"
	"slices"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The Qwen3-32B staircase (2 prefill and 2 decode pods at TP 2, an H200
// cluster) sent flow-control-on steps past the prefill onset where the router
// really held: the first run to measure the wait TTFTFleet.PriceRouterWait
// prices. With the switch off (the default, DefaultPriceRouterWait) the price
// reads short there; on, with the gate priced (ttft_gate.go), it holds the
// client. One held step does not make it the default.

// q32Fleet is that run's frozen fleet for bucket B (8000-token prompts,
// Poisson) or C (the same at Ca^2 4), on the processor-sharing model with
// flow control off: the freeze's fleet to the bit
// (TestStaircaseQ32_TheFixtureIsTheFrozenCurve).
//
// Where it comes from: frozen.json in the run's directory (not in the
// repository; the staircase writes it beside the run's recordings), whose
// recordingSha256 names the
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
	rate       float64
	frozen     [3]float64
	client     [3]float64
	routerWait float64
	failed     int
	// pastTheOnset is at or past the prefill onset; waitPriced is where the
	// switch takes the wait as priced (FlowControlWaitPriced).
	pastTheOnset bool
	waitPriced   bool
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
	for _, bucket := range slices.Sorted(maps.Keys(q32Steps)) {
		for _, s := range q32Steps[bucket] {
			for i, q := range []float64{0, 0.9, 0.95} {
				got := psTTFT(t, q32Fleet(bucket), s.rate, q).Seconds
				nearRel(t, bucket+" frozen curve", got, s.frozen[i], 1e-9)
			}
		}
	}
}

// q32Tolerance is the rule the gate's price has to pass: past the onset, at a
// step the fleet held, the price may read at most 10 % under the client.
const q32Tolerance = 0.9

// q32HeldStep is bucket C's step past the prefill onset that the fleet held
// (3.24 r/s, 0.80 of the prefill knee, inside MaxRouterWaitKneeShare).
func q32HeldStep(t *testing.T) q32Step {
	t.Helper()
	for _, s := range q32Steps["C"] {
		if s.pastTheOnset && s.insideTheTake && s.failed == 0 {
			return s
		}
	}
	t.Fatal("no held step past the onset in bucket C")
	return q32Step{}
}

// The default (switch off) reads short at the held step: a known failure,
// asserted so that a change shows. The figure is the frozen curve's, flagged
// AboveFlowControlOnset, and at C 3.24 r/s, where the router held 19 % of
// requests for a mean of 1.40 s, it reads 0.80 of the client's mean, 0.84 of
// its p90 and 0.57 of its p95. The flag makes it a miss, so question 1 still
// caps at the onset (3.10 r/s, inside the measured crossing).
func TestStaircaseQ32_SwitchOff_ReadsShortAtTheHeldStep(t *testing.T) {
	if DefaultPriceRouterWait {
		t.Fatal("the gate's price is opt-in until a cluster staircase validates it: DefaultPriceRouterWait is off")
	}
	s := q32HeldStep(t)
	fl := fcFleet(q32Fleet("C"), DefaultPriceRouterWait)
	for i, q := range []float64{0, 0.9, 0.95} {
		got := mustTTFT(t, ttftAt(t, fl, s.rate, q))
		nearRel(t, "C price, switch off", got.Seconds, s.frozen[i], 1e-9)
		if !got.HasFlag(AboveFlowControlOnset) {
			t.Errorf("q %g: flags %v, want AboveFlowControlOnset", q, got.Flags)
		}
		t.Logf("switch off, C %.3f r/s q %g: model %.2f s, client %.2f s (%.2fx)", s.rate, q, got.Seconds, s.client[i], got.Seconds/s.client[i])
		if got.Seconds >= q32Tolerance*s.client[i] {
			t.Errorf("the known failure is gone: switch off, q %g reads %.2f s, within %.0f%% of the client's %.2f s; revisit the TTFT page",
				q, got.Seconds, 100*(1-q32Tolerance), s.client[i])
		}
	}
	target := TTFTTarget{Seconds: 22.71371427515482, Percentile: 0.95}
	a := mustAnswer(t, rateForTTFT(t, fl, target))
	if !(a.Value < s.rate) {
		t.Errorf("switch off: C p95 rate %.3f r/s, want it under the held step's %.3f r/s (the onset caps it)", a.Value, s.rate)
	}
}

// With the switch on, the gate priced as a stage (ttft_gate.go) holds the
// client at the step past the prefill onset the fleet held, and moves nothing
// below the onsets.
//
// At C's held step the price reads at least 0.9 of the client at the mean,
// p90 and p95 (1.77, 1.53 and 0.97), and at most twice it (q32Ceiling), and
// bucket C's rate for its frozen p95 target is the onset itself, inside the
// measured crossing, as is its p90 target's rate; the switch as it was (the
// router's wait taken as inside the prefill stage, no figure moved) answered
// 3.44 r/s, past them.
//
// Below each onset the figures are the frozen curve's, to the bit; they read
// 1.5 to 2.7 times the client, and the test holds them to the same 0.9 floor.
// B's step past the onset (3.85 r/s) is past MaxRouterWaitKneeShare: it keeps
// AboveFlowControlOnset and the frozen figure. The step after C's (3.34 r/s)
// is void (127 evicted): logged, not judged; its client figures are the
// dispatched requests' only. With the switch off every figure is the frozen
// curve's.
func TestStaircaseQ32_SwitchOn_TheGateStageHoldsTheClient(t *testing.T) {
	// q32Ceiling bounds how long the priced gate may read at the held step.
	const q32Ceiling = 2.0
	for _, bucket := range slices.Sorted(maps.Keys(q32Steps)) {
		fl := fcFleet(q32Fleet(bucket), true)
		for _, s := range q32Steps[bucket] {
			mean := mustTTFT(t, ttftAt(t, fl, s.rate, 0))
			if s.pastTheOnset != (mean.HasFlag(FlowControlWaitPriced) || mean.HasFlag(AboveFlowControlOnset)) ||
				s.waitPriced != mean.HasFlag(FlowControlWaitPriced) {
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
				// 0 is unmeasured: the void B 7's p90 and p95 lie past the
				// client's histogram.
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
				if s.failed == 0 && s.insideTheTake && got > q32Ceiling*s.client[i] {
					t.Errorf("%s %.3f r/s q %g reads %.2f s, over %g times the client's %.2f s", bucket, s.rate, q, got, q32Ceiling, s.client[i])
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
		if c.target.Percentile == 0.95 {
			// The gate's jump at the onset already misses the p95 target, so
			// the answer is the onset itself (priced just under it).
			nearRel(t, "C p95 rate, the onset", on.Value, mustOnsets(t, fcFleet(q32Fleet("C"), true)).Prefill, 1e-9)
		}
	}
}
