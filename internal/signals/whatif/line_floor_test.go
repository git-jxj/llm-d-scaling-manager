package whatif

import (
	"math"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The public staircase's warm-up (issue
// llm-d-incubation/llm-d-scaling-manager#274): the held line, A 31.25 ms
// per unit of fill and B 1.13 ms, read 1.15 ms at a fill of 0.0006 where
// the engines' floor read 1.75 ms. Lifted, it passes through the floor and
// keeps its slope, so at the B-off phase's fill of 0.18 it reads 7.36 ms,
// against the 7.6 ms measured there (6.75 ms before).
func TestLiftToFloor_ALineUnderItsFloorIsRaised(t *testing.T) {
	held := itl.Model{A: 0.03125, B: 0.00113}
	floor := LineFloor{ITL: 0.00175, Fill: 0.0006}
	for _, tier := range []string{TierOLS, TierPinnedB, TierHeld, TierAboveBand} {
		got, lift := LiftToFloor(tier, held, floor)
		if got.A != held.A {
			t.Errorf("%s: A %g, want the slope kept at %g", tier, got.A, held.A)
		}
		if math.Abs(got.ITLAt(floor.Fill)-floor.ITL) > 1e-12 {
			t.Errorf("%s: the lifted line reads %g s at the floor's fill, want the floor %g s", tier, got.ITLAt(floor.Fill), floor.ITL)
		}
		if want := floor.ITL - held.ITLAt(floor.Fill); math.Abs(lift-want) > 1e-12 || math.Abs(got.B-held.B-lift) > 1e-12 {
			t.Errorf("%s: lift %g, B %g; want B raised by %g", tier, lift, got.B, want)
		}
		if at := got.ITLAt(0.18); math.Abs(at-0.00735625) > 1e-9 {
			t.Errorf("%s: %g s at a fill of 0.18, want 7.36 ms", tier, at)
		}
	}
}

// A line at or over its floor, a floor that is not one, and a line whose
// intercept no engine measured are all kept as they are.
func TestLiftToFloor_ALineAtOrOverItsFloorIsKept(t *testing.T) {
	line := itl.Model{A: 0.03, B: 0.0018}
	cases := []struct {
		name  string
		tier  string
		line  itl.Model
		floor LineFloor
	}{
		{"over its floor", TierOLS, line, LineFloor{ITL: 0.0015, Fill: 0.001}},
		{"at its floor", TierOLS, line, LineFloor{ITL: line.ITLAt(0.01), Fill: 0.01}},
		{"no floor", TierOLS, line, LineFloor{}},
		{"a NaN floor", TierOLS, line, LineFloor{ITL: math.NaN(), Fill: 0.001}},
		{"a fill past a full cache", TierOLS, line, LineFloor{ITL: 0.5, Fill: 1}},
		{"a borrowed line", TierBorrowed, line, LineFloor{ITL: 0.005, Fill: 0.001}},
		{"a line of no known tier", "", line, LineFloor{ITL: 0.005, Fill: 0.001}},
		{"a pinned line on the default B", TierPinnedB, itl.Model{A: 0.03, B: itl.DefaultBaselineSec}, LineFloor{ITL: 0.01, Fill: 0.001}},
		{"a flat line", TierOLS, itl.Model{A: 0, B: 0.001}, LineFloor{ITL: 0.005, Fill: 0.001}},
	}
	for _, c := range cases {
		got, lift := LiftToFloor(c.tier, c.line, c.floor)
		if got != c.line || lift != 0 {
			t.Errorf("%s: %+v lifted by %g, want %+v kept", c.name, got, lift, c.line)
		}
	}
}

// A resample is lifted as the line was, so the band stays around the line
// the planner prices; with no floor nothing is lifted, as before.
func TestBootstrapLines_ResamplesAreLiftedAsTheLineIs(t *testing.T) {
	rs := readingsOn(worked, 40, 0.2, 0.7, 0.05)
	fit := Refit{Tier: TierOLS, Line: worked, Baseline: worked.B}
	plain := BootstrapLines(rs, fit, DefaultResampling())
	// Over every resample's line at a fill of 0.001.
	floor := LineFloor{ITL: 0.01, Fill: 0.001}
	fit.Floor = floor
	lifted := BootstrapLines(rs, fit, DefaultResampling())
	if len(plain.Lines) == 0 || len(lifted.Lines) != len(plain.Lines) {
		t.Fatalf("fitted %d and %d resamples, want the same number, not none", len(plain.Lines), len(lifted.Lines))
	}
	for i, p := range plain.Lines {
		l := lifted.Lines[i]
		want, _ := LiftToFloor(p.Tier, p.Line, floor)
		if l.Line != want || l.Tier != p.Tier {
			t.Fatalf("resample %d: %+v, want %+v (the plain resample %+v lifted)", i, l, want, p)
		}
		if math.Abs(l.Line.ITLAt(floor.Fill)-floor.ITL) > 1e-12 {
			t.Fatalf("resample %d reads %g s at the floor's fill, want %g s", i, l.Line.ITLAt(floor.Fill), floor.ITL)
		}
	}
}
