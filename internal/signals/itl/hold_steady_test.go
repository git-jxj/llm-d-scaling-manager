package itl

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// holdStart is when the hold tests' replicas came up: an hour before their
// first cycle, so none is inside its settle.
var holdStart = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// holdRows is three replicas at fill k reading ITL itl.
func holdRows(k, itl float64) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, 3)
	for i := range 3 {
		out = append(out, domain.ReplicaMetrics{PodName: fmt.Sprintf("pod-%d", i), VariantName: "v", Ready: true,
			ReadySince: holdStart.Add(-time.Hour), KvCacheUsage: k, KvUsageInstant: k, AvgITL: itl})
	}
	return out
}

// holdSlope is the slope of the plateau's line, through DefaultBaselineSec.
const holdSlope = 0.03

// rampDown observes a plateau at fill 0.75 on the line A holdSlope through
// DefaultBaselineSec for five minutes, then a ramp-down: four cycles whose
// fill falls 0.6 to 0.3 while each still reads the plateau's ITL, as a
// minute's ITL does when every token in it was generated before the load
// stopped. Then the fleet idles for 40 minutes (no fill, no ITL) and the
// readings age out. It returns each cycle's reading from the ramp on.
func rampDown(tr *Tracker, adm Admission) []Reading {
	const cycle = 15 * time.Second
	itl := holdSlope*0.75 + DefaultBaselineSec
	now := holdStart
	for range 20 {
		tr.Observe("k", "v", holdRows(0.75, itl), adm, now)
		now = now.Add(cycle)
	}
	out := make([]Reading, 0, 164)
	for _, k := range []float64{0.6, 0.5, 0.4, 0.3} {
		out = append(out, tr.Observe("k", "v", holdRows(k, itl), adm, now))
		now = now.Add(cycle)
	}
	for range 160 {
		out = append(out, tr.Observe("k", "v", holdRows(0, 0), adm, now))
		now = now.Add(cycle)
	}
	return out
}

// Issue llm-d-incubation/llm-d-scaling-manager#266: once the plateau's
// steady readings leave the window, the pinned fit takes every reading, and
// the ramp-down's read a slope far steeper than the plateau's. With
// HoldsSteadySlope the plateau's A holds through to the end; without it (the
// negative control), the ramp-down's A is fitted and then held.
func TestTracker_ASteadySlopeHoldsThroughARampDown(t *testing.T) {
	on := rampDown(NewTracker(), Admission{HoldsSteadySlope: true})
	off := rampDown(NewTracker(), Admission{})

	steep, everyReading := 0.0, false
	for _, r := range off {
		if r.Tier == TierPinnedB && !r.SlopeHeld && r.SteadyHeld < DefaultMinSamples {
			everyReading = true
		}
		steep = max(steep, r.Model.A)
	}
	if !everyReading || steep < 1.3*holdSlope {
		t.Fatalf("premise: off, the ramp-down is fitted on every reading (%v) and reads steep, A %.4f against %.4f",
			everyReading, steep, holdSlope)
	}
	if last := off[len(off)-1]; !last.SlopeHeld || last.Model.A < 1.3*holdSlope {
		t.Errorf("off: the last line holds A %.4f (held %v), want the ramp-down's", last.Model.A, last.SlopeHeld)
	}

	held := 0
	for i, r := range on {
		if r.Tier != TierPinnedB {
			t.Fatalf("on, cycle %d of the ramp: tier %q, want a line", i, r.Tier)
		}
		if d := r.Model.A - holdSlope; d > 1e-9 || d < -1e-9 {
			t.Fatalf("on, cycle %d of the ramp: A %.5f, want the plateau's %.5f (steady %d, held %d)",
				i, r.Model.A, holdSlope, r.SteadyHeld, r.Held)
		}
		if r.SlopeHeld {
			held++
		}
	}
	if !on[len(on)-1].SlopeHeld || held == 0 {
		t.Errorf("on: the plateau's A is held (%d cycles held)", held)
	}
}

// A key that has only ever been fitted on every reading holds nothing the
// switch keeps: its fleet has not settled, and the fit follows its readings
// as it did without the switch.
func TestTracker_AnUnsteadySlopeIsNotHeld(t *testing.T) {
	run := func(adm Admission) Reading {
		tr := NewTracker()
		now := holdStart
		var r Reading
		// The fill swings 0.3 to 0.6 every cycle: steady on the first cycle
		// alone, too few for a steady fit.
		for i := range 8 {
			k := 0.3 + 0.3*float64(i%2)
			r = tr.Observe("k", "v", holdRows(k, 0.02*k+DefaultBaselineSec), adm, now)
			now = now.Add(15 * time.Second)
		}
		if r.SteadyHeld >= DefaultMinSamples || r.Tier != TierPinnedB || r.SlopeHeld {
			t.Fatalf("premise: a pinned fit on unsteady readings; got tier %q, steady %d, held %v", r.Tier, r.SteadyHeld, r.SlopeHeld)
		}
		for i := range 8 {
			k := 0.3 + 0.3*float64(i%2)
			r = tr.Observe("k", "v", holdRows(k, 0.04*k+DefaultBaselineSec), adm, now)
			now = now.Add(15 * time.Second)
		}
		return r
	}
	on, off := run(Admission{HoldsSteadySlope: true}), run(Admission{})
	if on.SlopeHeld || on.Model != off.Model {
		t.Errorf("on: %+v (held %v), want the fit off makes: %+v", on.Model, on.SlopeHeld, off.Model)
	}
	if on.Model.A <= 0.02 {
		t.Errorf("on: A %.4f, want it to follow the steeper readings", on.Model.A)
	}
}

// The hold ends where every hold does: the plateau's A is held while the
// ramp-down's readings are still in the window, and past SlopeHoldFor the
// every-reading fit is taken again.
func TestTracker_ASteadySlopeHoldEnds(t *testing.T) {
	adm := Admission{HoldsSteadySlope: true, SlopeHoldFor: 15 * time.Minute}
	got := rampDown(NewTracker(), adm)
	heldAt, refitAt := -1, -1
	for i, r := range got {
		steady := r.SteadyHeld >= DefaultMinSamples
		if heldAt < 0 && r.SlopeHeld && r.Held >= DefaultMinSamples && !steady && math.Abs(r.Model.A-holdSlope) < 1e-9 {
			heldAt = i
		}
		if refitAt < 0 && r.Tier == TierPinnedB && !r.SlopeHeld && r.Model.A > 1.3*holdSlope {
			refitAt = i
		}
	}
	if heldAt < 0 {
		t.Fatalf("premise: the plateau's A is held over the ramp-down's readings before SlopeHoldFor")
	}
	if refitAt <= heldAt {
		t.Errorf("past SlopeHoldFor the every-reading fit is taken again: held at cycle %d, refit at %d", heldAt, refitAt)
	}
}
