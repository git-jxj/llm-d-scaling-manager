package whatif

import (
	"testing"
)

// The Qwen3-32B flow-control A/B ran router v0.11.0 and v1.0.0 on one standup
// (2 prefill and 2 decode pods at TP 2, H200; 8000-token prompts, 200-token
// outputs, Ca^2 4 asked; flow control on, defaultRequestTTL 30 s), each at
// 3.17, 3.39 and 3.54 r/s for 600 s with seeds 5001 and 5003, so the two
// routers saw the same arrivals. It had no warm-up and no freeze: the fleet
// priced here is the earlier staircase's frozen bucket C on the same model,
// shape and hardware (q32Fleet), at the rate and Ca^2 asked and at those the
// load generator sent.
//
// Client TTFT is over each step's requests scheduled 60 s or more into it;
// the sent rate and Ca^2 are of the same requests' scheduled times. The
// router wait is the EPP's flow_control_request_queue_duration_seconds mean
// for dispatched requests over the step.

// abStep is one step of the A/B: the rate asked, the rate and Ca^2 sent, the
// client's TTFT mean, p90 and p95 (seconds), the router's mean wait
// (seconds) and the requests the router evicted on its TTL.
type abStep struct {
	seed                 int
	asked, sent, sentCa2 float64
	client               [3]float64
	routerWait           float64
	evicted              int
}

// routerV1Steps are router v1.0.0's steps: every one held (none evicted, the
// fleet 2+2 throughout), and every one past bucket C's prefill onset
// (3.098 r/s). With flow control off at seed 5001 and 3.39 r/s the client
// read 3.93 / 7.48 / 12.54 s, against 3.87 / 9.02 / 11.81 s with it on: the
// router held each request 1.49 s on average and the TTFT did not grow.
var routerV1Steps = []abStep{
	{5001, 3.17, 3.450, 3.57, [3]float64{3.61, 7.01, 8.75}, 1.2797, 0},
	{5001, 3.39, 3.475, 3.87, [3]float64{3.87, 9.02, 11.81}, 1.4930, 0},
	{5001, 3.54, 3.574, 4.22, [3]float64{4.56, 10.34, 12.60}, 2.0335, 0},
	{5003, 3.17, 3.027, 3.84, [3]float64{2.48, 4.74, 5.40}, 0.5448, 0},
	{5003, 3.39, 3.558, 3.30, [3]float64{3.23, 5.79, 6.63}, 0.9216, 0},
	{5003, 3.54, 3.367, 4.61, [3]float64{3.62, 6.66, 7.79}, 1.3130, 0},
}

// routerV1Fleet is bucket C's frozen fleet at the arrivals' Ca^2, flow
// control on, the router's wait taken as inside the prefill price.
func routerV1Fleet(ca2 float64, inPrefill bool) TTFTFleet {
	fl := fcFleet(q32Fleet("C"), false)
	fl.Prefill.ArrivalCV2 = ca2
	fl.RouterWaitInPrefill = inPrefill
	return fl
}

// With RouterWaitInPrefill the price past the prefill onset is the
// processor-sharing figure, unchanged to the bit, flagged
// FlowControlWaitPriced with no gate stage; and at every step of router
// v1.0.0 it reads at least q32Tolerance of the client's mean, p90 and p95,
// at the rate and Ca^2 asked and at those sent. On router v0.11.0 the same
// figure reads short at a step its fleet held (C at 3.24 r/s: 0.80, 0.84
// and 0.57 of the client, TestStaircaseQ32_SwitchOff_ReadsShortAtTheHeldStep),
// because that router's gate adds time: the switch is for v1.0.0 only.
func TestRouterV1_TheWaitIsInsidePrefill(t *testing.T) {
	onset := mustOnsets(t, routerV1Fleet(4, true)).Prefill
	knee := mustTTFT(t, ttftAt(t, routerV1Fleet(4, true), 3, 0)).PrefillKnee
	for _, s := range routerV1Steps {
		if s.evicted != 0 {
			t.Fatalf("seed %d %.2f r/s: %d evicted, want only held steps", s.seed, s.asked, s.evicted)
		}
		for _, at := range []struct {
			name      string
			rate, ca2 float64
		}{{"asked", s.asked, 4}, {"sent", s.sent, s.sentCa2}} {
			if !(at.rate <= MaxRouterWaitKneeShare*knee) {
				t.Fatalf("seed %d %s %.3f r/s: want it inside %.2f of the knee %.3f", s.seed, at.name, at.rate, MaxRouterWaitKneeShare, knee)
			}
			// Seed 5003's first step sent under the onset (3.027 r/s): no
			// flag either way there.
			past := at.rate >= onset
			if at.name == "asked" && !past {
				t.Fatalf("seed %d asked %.3f r/s: want it past the onset %.3f", s.seed, at.rate, onset)
			}
			for i, q := range []float64{0, 0.9, 0.95} {
				got := mustTTFT(t, ttftAt(t, routerV1Fleet(at.ca2, true), at.rate, q))
				off := mustTTFT(t, ttftAt(t, routerV1Fleet(at.ca2, false), at.rate, q))
				if got.HasFlag(FlowControlWaitPriced) != past || got.HasFlag(AboveFlowControlOnset) {
					t.Errorf("seed %d %s q %g: flags %v, want FlowControlWaitPriced past the onset only", s.seed, at.name, q, got.Flags)
				}
				if off.HasFlag(AboveFlowControlOnset) != past {
					t.Errorf("seed %d %s q %g: switch off, flags %v, want AboveFlowControlOnset past the onset only", s.seed, at.name, q, off.Flags)
				}
				if got.Seconds != off.Seconds || got.Parts.RouterWait != 0 || got.Parts.BurstWait != 0 {
					t.Errorf("seed %d %s q %g: %.6f s (router %g, burst %g), want the processor-sharing figure %.6f s with no gate stage",
						s.seed, at.name, q, got.Seconds, got.Parts.RouterWait, got.Parts.BurstWait, off.Seconds)
				}
				t.Logf("v1.0.0 seed %d %s %.3f r/s Ca2 %.2f q %g: model %.2f s, client %.2f s (%.2fx), router wait %.2f s measured",
					s.seed, at.name, at.rate, at.ca2, q, got.Seconds, s.client[i], got.Seconds/s.client[i], s.routerWait)
				if got.Seconds < q32Tolerance*s.client[i] {
					t.Errorf("seed %d %s %.3f r/s q %g reads %.2f s, more than %.0f%% under the client's %.2f s",
						s.seed, at.name, at.rate, q, got.Seconds, 100*(1-q32Tolerance), s.client[i])
				}
			}
		}
	}
}

// With RouterWaitInPrefill a rate answer may stand past the prefill onset, up
// to MaxRouterWaitKneeShare of the knee, and meets its own verdict. Bucket
// C's frozen p95 target (22.7 s), which the default caps at the onset,
// answers past it, and under the highest rate v1.0.0 held (3.54 r/s asked),
// where the client's p95 read at most 12.6 s.
func TestRouterV1_TheRateAnswerPassesTheOnset(t *testing.T) {
	target := TTFTTarget{Seconds: 22.71371427515482, Percentile: 0.95}
	fl := routerV1Fleet(4, true)
	onset := mustOnsets(t, fl).Prefill
	off := mustAnswer(t, rateForTTFT(t, routerV1Fleet(4, false), target))
	on := mustAnswer(t, rateForTTFT(t, fl, target))
	knee := mustTTFT(t, ttftAt(t, fl, 3, 0)).PrefillKnee
	t.Logf("C p95 %.2f s: %.3f r/s default, %.3f r/s with the wait in prefill; onset %.3f, %.2f of the knee %.3f",
		target.Seconds, off.Value, on.Value, onset, MaxRouterWaitKneeShare, MaxRouterWaitKneeShare*knee)
	if !(off.Value < onset) {
		t.Errorf("default: %.3f r/s, want it under the onset %.3f", off.Value, onset)
	}
	if !(on.Value > onset && on.Value <= MaxRouterWaitKneeShare*knee) {
		t.Errorf("in prefill: %.3f r/s, want it past the onset %.3f and at most %.3f", on.Value, onset, MaxRouterWaitKneeShare*knee)
	}
	if !(on.Value < 3.54) {
		t.Errorf("in prefill: %.3f r/s, want it under the highest rate measured held, 3.54 r/s", on.Value)
	}
	if met, known := TTFTMeets(ttftAt(t, fl, on.Value, target.Percentile), target); !met || !known {
		t.Errorf("the rate answered does not meet its own verdict (met %v, known %v)", met, known)
	}
}

// With both switches set the gate stage takes: PriceRouterWait's figure, not
// the processor-sharing one.
func TestRouterV1_TheGateStageTakesWithBoth(t *testing.T) {
	both := routerV1Fleet(4, true)
	both.PriceRouterWait = true
	gate := routerV1Fleet(4, false)
	gate.PriceRouterWait = true
	got := mustTTFT(t, ttftAt(t, both, 3.3, 0))
	want := mustTTFT(t, ttftAt(t, gate, 3.3, 0))
	if got.Seconds != want.Seconds || got.Parts.RouterWait != want.Parts.RouterWait || !(got.Parts.RouterWait > 0) {
		t.Errorf("both: %.4f s, router wait %g; want the gate stage's %.4f s, router wait %g", got.Seconds, got.Parts.RouterWait, want.Seconds, want.Parts.RouterWait)
	}
}
