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
	g, ok := newGateStage(pf, rate, parts.prefill)
	if !ok {
		t.Fatal("no gate at the held step")
	}
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
	g, ok := newGateStage(pf, 10, 0.002)
	if !ok || g.staleness != EPPRefreshSeconds {
		t.Errorf("staleness %g s (ok %v), want the poll %g s", g.staleness, ok, EPPRefreshSeconds)
	}
}

// A burst under one request is not raised to one: the drain and both waits
// shrink with it, to under half a request's own work.
func TestGateStage_ASmallBurstIsNotRaised(t *testing.T) {
	fl := fcFleet(q32Fleet("C"), true)
	pl, u := fl.plan()
	if u != nil {
		t.Fatal(u)
	}
	pf := pl.pf
	pf.work = 0.01
	g, ok := newGateStage(pf, 2, 0.01) // Delta 0.05 s (the poll), burst 0.1
	if !ok {
		t.Fatal("no gate")
	}
	nearRel(t, "burst", g.burst, 0.1, 1e-12)
	nearRel(t, "drain", g.drain, 0.1*0.01, 1e-12)
	if r, b := g.routerWait().mean(), g.burstWait().mean(); !(r < pf.work/2 && b < pf.work/2) {
		t.Errorf("router wait %g s, burst wait %g s: want both under half the work %g s", r, b, pf.work)
	}
}

// A gate with no finite, positive drain is not priced: NaN or infinite work,
// stretch or rate, and no work, give ok false, and unpriceGate trades the
// priced flag back for AboveFlowControlOnset, the rest kept.
func TestGateStage_NonFiniteInputsAreNotPriced(t *testing.T) {
	fl := fcFleet(q32Fleet("C"), true)
	pl, u := fl.plan()
	if u != nil {
		t.Fatal(u)
	}
	nan, inf := math.NaN(), math.Inf(1)
	for _, c := range []struct {
		name                string
		work, stretch, rate float64
	}{
		{"NaN work", nan, 1, 3}, {"Inf work", inf, 1, 3}, {"NaN stretch", 0.47, nan, 3},
		{"Inf stretch", 0.47, inf, 3}, {"NaN rate", 0.47, 1, nan}, {"Inf rate", 0.47, 1, inf},
		{"no work", 0, 1, 3}, {"no rate", 0.47, 1, 0},
	} {
		pf := pl.pf
		pf.work = c.work
		if g, ok := newGateStage(pf, c.rate, c.stretch); ok {
			t.Errorf("%s: priced %+v", c.name, g)
		}
	}
	got := unpriceGate([]Flag{UnvalidatedTTFT, FlowControlWaitPriced, AssumedVP})
	want := []Flag{UnvalidatedTTFT, AboveFlowControlOnset, AssumedVP}
	if len(got) != len(want) {
		t.Fatalf("flags %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("flags %v, want %v", got, want)
		}
	}
}

// Past MaxRouterWaitKneeShare of the knee the gate is not priced even with
// the switch on: just above the share the flag is AboveFlowControlOnset, the
// gate's parts are 0 and the figure is the switch-off one; just under it the
// gate is priced.
func TestGateStage_NotPricedPastTheShare(t *testing.T) {
	c4 := psFleet(0.0577, 4)
	c4.Decode.Replicas = 4
	fl := fcFleet(c4, true)
	on := mustOnsets(t, fl)
	share := MaxRouterWaitKneeShare * mustTTFT(t, ttftAt(t, fl, 1, 0)).PrefillKnee
	if !(on.Prefill < share) {
		t.Fatalf("precondition: onset %g under the share %g", on.Prefill, share)
	}
	under := mustTTFT(t, ttftAt(t, fl, share*(1-1e-6), 0.9))
	if !under.HasFlag(FlowControlWaitPriced) || !(under.Parts.RouterWait > 0) {
		t.Errorf("just under the share: flags %v, router wait %g s", under.Flags, under.Parts.RouterWait)
	}
	over := mustTTFT(t, ttftAt(t, fl, share*(1+1e-6), 0.9))
	off := mustTTFT(t, ttftAt(t, fcFleet(c4, false), share*(1+1e-6), 0.9))
	if !over.HasFlag(AboveFlowControlOnset) || over.HasFlag(FlowControlWaitPriced) || over.Parts.RouterWait != 0 || over.Parts.BurstWait != 0 || over.Seconds != off.Seconds {
		t.Errorf("just over the share: flags %v, router wait %g s, burst wait %g s, %g s against %g s off",
			over.Flags, over.Parts.RouterWait, over.Parts.BurstWait, over.Seconds, off.Seconds)
	}
}

// withWait refuses a wait it cannot sum onto a part's own (a shifted decode
// wait onto the gate's three pieces), and two waits of two unshifted pieces
// each sum: their means add, and their atom is the product of the atoms.
func TestGateStage_WaitsSumOrRefuse(t *testing.T) {
	gate := gateStage{drain: 4, closed: 0.5}.wait()
	mix := stageMix{w: []float64{1}, b: []block{{own: newOwnTime(1, 0)}}}
	gated, ok := mix.withWait(gate)
	if !ok {
		t.Fatal("the gate did not take on an empty part")
	}
	if _, ok := gated.withWait(boundWait(0.2, 0.5, 3)); ok {
		t.Error("a shifted wait summed onto the gate's")
	}
	a, _ := expWait(0.3, 2).sum(expWait(0.4, 5))
	b, _ := expWait(0.2, 1).sum(expWait(0.5, 3))
	s, ok := a.sum(b)
	if !ok {
		t.Fatal("two two-piece unshifted waits did not sum")
	}
	relNear(t, "mean", s.mean(), a.mean()+b.mean(), 1e-5)
	tail := func(w waitTail, u float64) float64 {
		var p float64
		for i, x := range w.w {
			p += x * math.Exp(-w.r[i]*u)
		}
		return p
	}
	relNear(t, "P(W > 0)", tail(s, 0), 1-(1-tail(a, 0))*(1-tail(b, 0)), 1e-9)
	// P(A + B > u) >= P(A > u) and P(B > u), and below P(A > u) + P(B > u) +
	// P(A > 0) P(B > 0), at a few points.
	for _, u := range []float64{0.1, 1, 3} {
		if p := tail(s, u); !(p >= max(tail(a, u), tail(b, u))-1e-12 && p <= tail(a, u)+tail(b, u)+tail(a, 0)*tail(b, 0)) {
			t.Errorf("P(W > %g) = %g, against %g and %g", u, p, tail(a, u), tail(b, u))
		}
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
