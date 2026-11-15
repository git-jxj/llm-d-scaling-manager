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
// onset, never down.
func TestGateStage_TTFTRisesPastTheOnset(t *testing.T) {
	for _, bucket := range []string{"B", "C"} {
		fl := fcFleet(q32Fleet(bucket), true)
		on := mustOnsets(t, fl)
		knee := mustTTFT(t, ttftAt(t, fl, 1, 0)).PrefillKnee
		for _, q := range []float64{0, 0.9, 0.95} {
			prev := mustTTFT(t, ttftAt(t, fl, on.Prefill*(1-1e-9), q)).Seconds
			for r := on.Prefill; r <= MaxRouterWaitKneeShare*knee; r += (MaxRouterWaitKneeShare*knee - on.Prefill) / 20 {
				got := mustTTFT(t, ttftAt(t, fl, r, q))
				if !got.HasFlag(FlowControlWaitPriced) {
					continue
				}
				if !(got.Seconds >= prev) || math.IsInf(got.Seconds, 0) {
					t.Errorf("%s q %g at %.4f r/s: %g s after %g s", bucket, q, r, got.Seconds, prev)
				}
				prev = got.Seconds
			}
		}
	}
}
