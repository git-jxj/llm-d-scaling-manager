package itl

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The floor is the BelowBandQuantile quantile, so with ten readings the
// least is passed over; with fewer than BelowBandMinReadings there is none.
func TestBelowBandFloor(t *testing.T) {
	xs := []float64{0.001, 0.012, 0.013, 0.014, 0.015, 0.016, 0.017, 0.018, 0.019, 0.020}
	if got := BelowBandFloor(xs); got != 0.012 {
		t.Errorf("floor %g, want 0.012: the outlier is passed over", got)
	}
	if got := BelowBandFloor(xs[:BelowBandMinReadings-1]); got != 0 {
		t.Errorf("floor %g on %d readings, want none", got, BelowBandMinReadings-1)
	}
	var kept []Observation
	for i := range BelowBandWindow + 5 {
		kept = keepBelowBand(kept, Observation{K: 0.05, ITLSec: float64(i)})
	}
	if len(kept) != BelowBandWindow || kept[0].ITLSec != 5 {
		t.Errorf("kept %d from %g, want the latest %d", len(kept), kept[0].ITLSec, BelowBandWindow)
	}
	p, ok := belowBandPoint([]Observation{{K: 0.01, ITLSec: 0.001}, {K: 0.02, ITLSec: 0.012}, {K: 0.03, ITLSec: 0.013},
		{K: 0.04, ITLSec: 0.014}, {K: 0.05, ITLSec: 0.015}, {K: 0.06, ITLSec: 0.016}, {K: 0.07, ITLSec: 0.017},
		{K: 0.08, ITLSec: 0.018}, {K: 0.09, ITLSec: 0.019}, {K: 0.10, ITLSec: 0.020}})
	if !ok || p.ITLSec != 0.012 || p.K != 0.02 {
		t.Errorf("point %+v, want 0.012 at its own fill 0.02", p)
	}
}

// floorRows is three replicas at fill k on the line ITL = a k + b, Ready an
// hour before start.
func floorRows(k, a, b float64, start time.Time) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, 3)
	for i := range 3 {
		out = append(out, domain.ReplicaMetrics{PodName: fmt.Sprintf("pod-%d", i), VariantName: "v", Ready: true,
			ReadySince: start.Add(-time.Hour), KvCacheUsage: k, KvUsageInstant: k, AvgITL: a*k + b})
	}
	return out
}

// floorTrue is the line the floor tests' engines run on: a B of 12 ms, twice
// the default.
var floorTrue = Model{A: 0.05, B: 0.012}

// floorRun observes 20 cycles at fill below, under the band, then 4 in it at
// 0.30 to 0.40, too narrow a spread for a full fit, and returns the last
// reading.
func floorRun(tr *Tracker, adm Admission, below func(c int) float64, start time.Time) Reading {
	var r Reading
	c := 0
	for ; c < 20; c++ {
		r = tr.Observe("k", "v", floorRows(below(c), floorTrue.A, floorTrue.B, start), adm, start.Add(time.Duration(c)*15*time.Second))
	}
	for i, k := range []float64{0.30, 0.35, 0.40, 0.35} {
		r = tr.Observe("k", "v", floorRows(k, floorTrue.A, floorTrue.B, start), adm, start.Add(time.Duration(c+i)*15*time.Second))
	}
	return r
}

// A key with no learned B is pinned through the floor read below the band,
// at its fill, with BaselineFromFloor: on readings of a true line, its own B,
// so the line prices the truth past its readings. Without it, as before, at
// the default B, and the fit through that B prices ITL high there. Pinned at
// the floor's ITL as if read at no fill, B would be high by A times the
// floor's fill and the line under the truth past its readings (the negative
// control below).
func TestTracker_BaselineFromFloorPinsAnUnlearnedKey(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	steadyLow := func(int) float64 { return 0.05 }
	on := floorRun(NewTracker(), Admission{BaselineFromFloor: true}, steadyLow, start)
	off := floorRun(NewTracker(), Admission{}, steadyLow, start)
	if on.Tier != TierPinnedB || off.Tier != TierPinnedB {
		t.Fatalf("premise: a pinned fit both ways; got %q on, %q off", on.Tier, off.Tier)
	}
	wantFloor := floorTrue.ITLAt(0.05)
	if !on.PinnedAtFloor || math.Abs(on.Floor-wantFloor) > 1e-12 || on.FloorK != 0.05 {
		t.Errorf("on: floor %g at %g (pinned %v), want %g at 0.05", on.Floor, on.FloorK, on.PinnedAtFloor, wantFloor)
	}
	if math.Abs(on.Model.B-floorTrue.B) > 1e-9 || math.Abs(on.Model.A-floorTrue.A) > 1e-9 {
		t.Errorf("on: line %+v, want the truth %+v", on.Model, floorTrue)
	}
	// Negative control: the fit through the floor's ITL itself.
	naive, ok := FitPinnedB(spanReadings(floorTrue, 12, 0.30, 0.40), wantFloor)
	if !ok || !(naive.ITLAt(0.8) < floorTrue.ITLAt(0.8)) {
		t.Errorf("negative control: through the floor's ITL at no fill, %+v prices %.4f s at k 0.8, want under the truth %.4f s",
			naive, naive.ITLAt(0.8), floorTrue.ITLAt(0.8))
	}
	if off.PinnedAtFloor || off.Floor != 0 || off.Model.B != DefaultBaselineSec {
		t.Errorf("off: B %g (floor %g, pinned %v), want the default", off.Model.B, off.Floor, off.PinnedAtFloor)
	}
	truth := floorTrue.ITLAt(0.8)
	onErr, offErr := on.Model.ITLAt(0.8)-truth, off.Model.ITLAt(0.8)-truth
	if math.Abs(onErr) > 1e-9 || !(offErr > 0) {
		t.Errorf("at k 0.8, truth %.4f s: on errs %+.4f s, off %+.4f s; on should be exact, off high", truth, onErr, offErr)
	}
	t.Logf("at k 0.8, truth %.4f s: through the floor %+.4f s, default B %+.4f s, the floor's ITL at no fill %+.4f s",
		truth, onErr, offErr, naive.ITLAt(0.8)-truth)
}

// A learned B ends the floor: the key pins its own.
func TestTracker_ALearnedBaselineEndsTheFloor(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	tr.SetBaseline("k", 0.02)
	r := floorRun(tr, Admission{BaselineFromFloor: true}, func(int) float64 { return 0.05 }, start)
	if r.PinnedAtFloor || r.Model.B != 0.02 {
		t.Errorf("B %g (pinned at the floor %v), want the learned 0.02", r.Model.B, r.PinnedAtFloor)
	}
}

// A reading below the band from a replica whose fill is moving is a step,
// not the floor, and is not kept (only each replica's first, which has no
// history to move against); and the switch off keeps none at all.
func TestTracker_TheFloorReadsSteadyRowsOnly(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	jumping := func(c int) float64 { return []float64{0.02, 0.14}[c%2] }
	tr := NewTracker()
	r := floorRun(tr, Admission{BaselineFromFloor: true}, jumping, start)
	if kept := len(tr.belowBand["k"].readings); r.PinnedAtFloor || r.Floor != 0 || kept > 3 {
		t.Errorf("a moving fill set the floor: %g (%d kept)", r.Floor, kept)
	}
	off := NewTracker()
	floorRun(off, Admission{}, func(int) float64 { return 0.05 }, start)
	if len(off.belowBand) != 0 {
		t.Errorf("off: %d keys kept readings below the band", len(off.belowBand))
	}
}

// A sweep forgets a key's readings below the band with its window.
func TestTracker_SweepForgetsTheFloor(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tr := NewTracker()
	floorRun(tr, Admission{BaselineFromFloor: true}, func(int) float64 { return 0.05 }, start)
	if tr.belowBand["k"] == nil || len(tr.belowBand["k"].readings) == 0 {
		t.Fatal("premise: readings kept")
	}
	tr.Sweep(start.Add(time.Hour), func(string) bool { return true })
	if len(tr.belowBand) != 0 {
		t.Errorf("%d keys left after the sweep", len(tr.belowBand))
	}
}
