package whatif

import (
	"math"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// loadAwarePrefill is three prefill pods at V_P 30,000 tok/s, 1000-token
// prompts at cv 1, f 1.3 read at a mean pod utilization of 0.4: the pooled
// knee is 90 r/s.
func loadAwarePrefill(on bool, margin float64) Prefill {
	return Prefill{
		VP: 30000, FixedLatency: 0.0126, Replicas: 3, PromptTokens: 1000, PromptCV: 1,
		Imbalance: 1.3, ImbalanceUtilization: 0.4, LoadAwareRouter: on, VPMargin: margin,
	}
}

func kneeOf(t *testing.T, p Prefill) float64 {
	t.Helper()
	a, ok := PrefillKnee(p).(Answer)
	if !ok {
		t.Fatalf("no knee: %#v", PrefillKnee(p))
	}
	return a.Value
}

// With a load-aware router and no margin the knee is the pooled one,
// N_p V_P / E[I]; off, it is that over f. With the margin it is where the
// worst pod at f(rho) fills the margined V_P.
func TestLoadAware_TheKneeIsPooled(t *testing.T) {
	relNear(t, "knee, load-aware", kneeOf(t, loadAwarePrefill(true, 0)), 90, 1e-12)
	relNear(t, "knee, off", kneeOf(t, loadAwarePrefill(false, 0)), 90/1.3, 1e-12)

	p := loadAwarePrefill(true, DefaultVPMargin)
	k := kneeOf(t, p)
	pl, u := p.check()
	if u != nil {
		t.Fatal(u)
	}
	rho := k / 90
	relNear(t, "f(rho) rho at the margined knee", pl.fAt(k)*rho, 1-DefaultVPMargin, 1e-9)
	if !(k > 90*(1-DefaultVPMargin)/1.3) || !(k < 90) {
		t.Errorf("margined knee %.6g, want between the held one %.6g and the pooled 90", k, 90*(1-DefaultVPMargin)/1.3)
	}
}

// f is held up to the utilization it was read at, and the TTFT there is the
// one with the switch off; past it f falls, to 1 at the pooled knee.
func TestLoadAware_FIsHeldBelowWhereItWasRead(t *testing.T) {
	on, off := loadAwarePrefill(true, DefaultVPMargin), loadAwarePrefill(false, DefaultVPMargin)
	plOn, _ := on.check()
	for _, rate := range []float64{5, 20, 35, 36} { // rho up to 0.4 is 36 r/s
		if got := plOn.fAt(rate); got != 1.3 {
			t.Errorf("f at %g r/s = %g, want 1.3 held", rate, got)
		}
		a := mustTTFT(t, TTFTAt(TTFTFleet{Prefill: on}, rate, 0.9))
		b := mustTTFT(t, TTFTAt(TTFTFleet{Prefill: off}, rate, 0.9))
		if a.Seconds != b.Seconds {
			t.Errorf("TTFT p90 at %g r/s: %.9g on, %.9g off; want equal below rho_m", rate, a.Seconds, b.Seconds)
		}
	}
	relNear(t, "f at rho 0.7", plOn.fAt(63), 1+0.3*0.3/0.6, 1e-12)
	if got := plOn.fAt(90); got != 1 {
		t.Errorf("f at the pooled knee = %g, want 1", got)
	}
}

// Past rho_m the price is never above the held one, and it still rises with
// the rate up to the knee, so RateForTTFT's bisection holds.
func TestLoadAware_TTFTRisesWithTheRate(t *testing.T) {
	on, off := loadAwarePrefill(true, DefaultVPMargin), loadAwarePrefill(false, DefaultVPMargin)
	k := kneeOf(t, on)
	prev := 0.0
	for i := 1; i < 200; i++ {
		rate := k * float64(i) / 200
		a := mustTTFT(t, TTFTAt(TTFTFleet{Prefill: on}, rate, 0.9)).Seconds
		if !(a >= prev) {
			t.Fatalf("TTFT p90 falls at %.4g r/s: %.6g after %.6g", rate, a, prev)
		}
		prev = a
		if b, ok := TTFTAt(TTFTFleet{Prefill: off}, rate, 0.9).(TTFT); ok && a > b.Seconds*(1+1e-12) {
			t.Errorf("at %.4g r/s load-aware %.6g above held %.6g", rate, a, b.Seconds)
		}
	}
}

// A pod hotter than the router can steer away from: f 1.8 read at 0.5 has
// an excess of 0.8 against 0.5 of idle capacity, and saturates first, at
// rho 0.625 (k = 1.6: the smaller root of 1.6 rho^2 - 2.6 rho + 1).
func TestLoadAware_AHotPodSaturatesBeforeThePooledKnee(t *testing.T) {
	p := loadAwarePrefill(true, 0)
	p.Imbalance, p.ImbalanceUtilization = 1.8, 0.5
	relNear(t, "knee", kneeOf(t, p), 90*0.625, 1e-12)
	// Read where it already saturates the busiest pod: the held knee.
	p.Imbalance, p.ImbalanceUtilization = 2.5, 0.5
	relNear(t, "knee held", kneeOf(t, p), 90/2.5, 1e-12)
}

// No change where there is no imbalance to fall: one pod, f 1, or the
// switch off.
func TestLoadAware_NothingToFall(t *testing.T) {
	one := loadAwarePrefill(true, 0)
	one.Replicas, one.Imbalance = 1, 0
	relNear(t, "one pod", kneeOf(t, one), 30, 1e-12)
	even := loadAwarePrefill(true, 0)
	even.Imbalance = 1
	relNear(t, "f 1", kneeOf(t, even), 90, 1e-12)
	unknown := loadAwarePrefill(true, 0)
	unknown.ImbalanceUtilization = 0
	pl, _ := unknown.check()
	relNear(t, "rho_m unknown: f falls from no load", pl.fAt(45), 1+0.3*0.5, 1e-12)
}

// ImbalanceUtilizationAt reads the load at the count P1 learned f at, else
// the load F is read at, over V_P, held below 1; 0 with none.
func TestImbalanceUtilizationAt(t *testing.T) {
	est := prefill.Estimate{FLoad: 3000, FByPods: map[int]float64{2: 1.1}, FLoadByPods: map[int]float64{2: 6000}}
	relNear(t, "learned count", ImbalanceUtilizationAt(est, 2, 10000), 0.6, 1e-12)
	relNear(t, "another count", ImbalanceUtilizationAt(est, 3, 10000), 0.3, 1e-12)
	relNear(t, "over the V_P priced", ImbalanceUtilizationAt(est, 3, 20000), 0.15, 1e-12)
	est.FLoadByPods[2] = 20000
	relNear(t, "held below 1", ImbalanceUtilizationAt(est, 2, 10000), MaxImbalanceUtilization, 1e-12)
	if got := ImbalanceUtilizationAt(prefill.Estimate{}, 2, 10000); got != 0 {
		t.Errorf("none read: %g", got)
	}
	if got := ImbalanceUtilizationAt(prefill.Estimate{FLoad: 1}, 2, 0); got != 0 || math.IsNaN(got) {
		t.Errorf("no V_P: %g", got)
	}
}
