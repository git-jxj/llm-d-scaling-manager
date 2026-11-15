package whatif

import (
	"math"
	"testing"
)

// The gate's stage at bucket C's step past the onset (3.24 r/s), from the
// plan's own figures: the staleness Delta is the prefill sojourn (its own
// work and its mean stretch, both at the margined V_P), the burst a
// staleness period's arrivals, the drain one pod's time on it, and the
// closed share drain/(drain + Delta). The mean TTFT is the frozen curve's
// plus W_router (closed share times half the drain) and W_burst (half the
// drain), and the parts still sum to it.
func TestGateStage_TheFiguresFollowTheMechanism(t *testing.T) {
	const rate = 3.238087631893702
	fl := fcFleet(q32Fleet("C"), true)
	pl, u := fl.plan()
	if u != nil {
		t.Fatal(u)
	}
	pf := pl.pf.atRate(rate)
	_, parts, uu := pf.psStages(rate, pl.decodePods(), false)
	if uu != nil {
		t.Fatal(uu)
	}
	g := newGateStage(pf, rate, parts.prefill)
	delta := pf.work + parts.prefill
	nearRel(t, "staleness", g.staleness, delta, 1e-12)
	nearRel(t, "burst", g.burst, rate*delta, 1e-12)
	nearRel(t, "drain", g.drain, rate*delta*pf.work, 1e-12)
	nearRel(t, "closed", g.closed, g.drain/(g.drain+delta), 1e-12)
	t.Logf("Delta %.2f s, burst %.1f requests, drain %.2f s, closed %.2f", g.staleness, g.burst, g.drain, g.closed)

	got := mustTTFT(t, ttftAt(t, fl, rate, 0))
	nearRel(t, "router wait", got.Parts.RouterWait, g.closed*g.drain/2, 1e-12)
	nearRel(t, "burst wait", got.Parts.BurstWait, g.drain/2, 1e-12)
	frozen := mustTTFT(t, ttftAt(t, fcFleet(q32Fleet("C"), false), rate, 0))
	nearRel(t, "mean", got.Seconds, frozen.Seconds+got.Parts.RouterWait+got.Parts.BurstWait, 1e-9)
	p := got.Parts
	nearRel(t, "parts", p.EngineWait+p.KVPullWait+p.OwnPrefill+p.DecodeWait+p.FirstStep+p.DecodeEngine+p.RouterHop+p.RouterWait+p.BurstWait, got.Seconds, 1e-9)

	// The wait's tail: W_router + W_burst, independent, at P(> 0) = 1. The
	// two share a rate, which waitTail.sum parts by a relative 1e-6, so the
	// sum's figures are within about that of the parts'.
	w := g.wait()
	var p0 float64
	for _, x := range w.w {
		p0 += x
	}
	nearRel(t, "P(wait > 0)", p0, 1, 1e-9)
	nearRel(t, "wait mean", w.mean(), g.closed*g.drain/2+g.drain/2, 1e-5)
}

// The staleness is never under the EPP's poll interval: at a prompt short
// enough that the pod's sojourn is under it, Delta is the poll.
func TestGateStage_StalenessIsAtLeastThePoll(t *testing.T) {
	fl := fcFleet(q32Fleet("C"), true)
	pl, u := fl.plan()
	if u != nil {
		t.Fatal(u)
	}
	pf := pl.pf
	pf.work = 0.001
	g := newGateStage(pf, 10, 0.002)
	if g.staleness != EPPRefreshSeconds {
		t.Errorf("staleness %g s, want the poll %g s", g.staleness, EPPRefreshSeconds)
	}
}

// Past the onset the priced TTFT rises with the rate, at the mean, p90 and
// p95, so question 1's bisection finds one edge; and it jumps up at the
// onset, never down. Bucket C's frozen fleet and its shape on four decode
// pods (psFleet), whose onsets sit under the share (bucket B's do not: its
// onset is past 0.9 of the knee on both); each checks at least one priced
// rate.
func TestGateStage_TTFTRisesPastTheOnset(t *testing.T) {
	c4 := psFleet(0.0577, 4)
	c4.Decode.Replicas = 4
	for _, c := range []struct {
		name string
		fl   TTFTFleet
	}{{"C", q32Fleet("C")}, {"C, four decode pods", c4}} {
		fl := fcFleet(c.fl, true)
		on := mustOnsets(t, fl)
		knee := mustTTFT(t, ttftAt(t, fl, 1, 0)).PrefillKnee
		checked := 0
		for _, q := range []float64{0, 0.9, 0.95} {
			prev := mustTTFT(t, ttftAt(t, fl, on.Prefill*(1-1e-9), q)).Seconds
			for i := 0; i <= 20; i++ {
				r := on.Prefill + float64(i)*(MaxRouterWaitKneeShare*knee-on.Prefill)/20
				got := mustTTFT(t, ttftAt(t, fl, r, q))
				if !got.HasFlag(FlowControlWaitPriced) {
					continue
				}
				checked++
				if !(got.Seconds >= prev) || math.IsInf(got.Seconds, 0) {
					t.Errorf("%s q %g at %.4f r/s: %g s after %g s", c.name, q, r, got.Seconds, prev)
				}
				prev = got.Seconds
			}
		}
		if checked == 0 {
			t.Errorf("%s: no priced rate checked (onset %g, %g of the knee %g)", c.name, on.Prefill, MaxRouterWaitKneeShare, knee)
		}
	}
}

// On P/D the independent figure carries the gate as the union bound does: the
// gate's wait and the decode admission wait sum in front of every part
// (stageMix.withWait). Bucket C's held step and its four-decode-pod shape,
// past the onset, at p90 and p95: Independent never above Seconds, and at
// most a hair under it where the decode wait is all but never met.
func TestGateStage_IndependentCarriesTheGate(t *testing.T) {
	c4 := psFleet(0.0577, 4)
	c4.Decode.Replicas = 4
	for _, c := range []struct {
		name string
		fl   TTFTFleet
	}{{"C", q32Fleet("C")}, {"C, four decode pods", c4}} {
		fl := fcFleet(c.fl, true)
		on := mustOnsets(t, fl)
		rate := on.Prefill * 1.02
		for _, q := range []float64{0.9, 0.95} {
			got := mustTTFT(t, ttftAt(t, fl, rate, q))
			if !got.HasFlag(FlowControlWaitPriced) {
				t.Fatalf("%s: precondition, flags %v at %g r/s", c.name, got.Flags, rate)
			}
			t.Logf("%s q %g: Seconds %.4f, Independent %.4f, decode wait probability %.3g", c.name, q, got.Seconds, got.Independent, got.DecodeWaitProbability)
			if got.Independent > got.Seconds*(1+1e-9) || got.Independent < got.Seconds*(1-1e-3) {
				t.Errorf("%s q %g: Independent %.6f s against the union bound's %.6f s: want the gate in both", c.name, q, got.Independent, got.Seconds)
			}
		}
	}
}
