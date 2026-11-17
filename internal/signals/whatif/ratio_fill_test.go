package whatif

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// fillWindow is one TPOT window at fill k whose q quantile, read at its
// bucket's upper edge, is ratio times the window's 10 ms mean.
func fillWindow(k, ratio float64) TPOTWindow {
	const mean, count = 0.010, 100
	return TPOTWindow{
		Buckets: []HistogramBucket{{UpperEdge: 0.95 * ratio * mean, Count: 0}, {UpperEdge: ratio * mean, Count: count}, {UpperEdge: math.Inf(1), Count: count}},
		Sum:     mean * count, Count: count, Fill: k, FillKnown: true,
	}
}

// windowsAt is n windows at fill k, each reading ratio.
func windowsAt(k, ratio float64, n int) []TPOTWindow {
	out := make([]TPOTWindow, n)
	for i := range out {
		out[i] = fillWindow(k, ratio)
	}
	return out
}

// learned is R_q at p90 from windows, failing the test on a refusal.
func learned(t *testing.T, windows []TPOTWindow) TPOTRatio {
	t.Helper()
	r, _, u := TPOTRatioFromWindows(windows, 0.9)
	if u != nil {
		t.Fatalf("R_q refused: %v", *u)
	}
	return r
}

// A tail that grows with load is learned as one: each bucket reads its own
// windows, R_q rises across the buckets, and At reads a bucket's own ratio at
// its center. One R_q at every fill prices the low fills at the high fills'
// tail, and fails here.
func TestRatioByFill_RisesWithFill(t *testing.T) {
	steps := []struct{ k, ratio float64 }{{0.1, 1.1}, {0.3, 1.3}, {0.5, 1.5}, {0.7, 1.8}}
	windows := make([]TPOTWindow, 0, 8*len(steps))
	for _, s := range steps {
		windows = append(windows, windowsAt(s.k, s.ratio, 8)...)
	}
	r := learned(t, windows)
	for _, s := range steps {
		got, global := r.At(s.k)
		near(t, "R_q at its bucket's center", got, s.ratio, 1e-9)
		if global {
			t.Errorf("fill %g: a bucket of 8 windows fell back to the global R_q", s.k)
		}
	}
	prev := 0.0
	for k := 0.0; k <= 1; k += 0.01 {
		got, _ := r.At(k)
		if got < prev {
			t.Fatalf("R_q falls from %g to %g at fill %g", prev, got, k)
		}
		prev = got
	}
	// Between two centers R_q is read linearly.
	mid, _ := r.At(0.2)
	near(t, "R_q between 0.1 and 0.3", mid, 1.2, 1e-9)
}

// A bucket reading below the one under it is raised to it: R_q never falls
// as the fill rises, whatever the windows say.
func TestRatioByFill_NeverFalls(t *testing.T) {
	windows := append(windowsAt(0.1, 1.6, 8), windowsAt(0.5, 1.2, 8)...)
	r := learned(t, windows)
	requireBuckets(t, r)
	at, _ := r.At(0.5)
	near(t, "R_q at 0.5, raised", at, 1.6, 1e-9)
	if r.ByFill[2].Learned != 1.2 {
		t.Errorf("bucket 0.4-0.6 learned %g, want its own 1.2 kept beside the raise", r.ByFill[2].Learned)
	}
}

// A bucket with fewer than MinFillRatioWindows windows reads the global R_q,
// and the answer priced there says so; with no bucket learned at all, R_q is
// one ratio, unflagged, as before.
func TestRatioByFill_ThinBucketFallsBackToGlobal(t *testing.T) {
	windows := append(windowsAt(0.1, 1.1, 20), windowsAt(0.5, 3.0, MinFillRatioWindows-1)...)
	r := learned(t, windows)
	requireBuckets(t, r)
	if !r.ByFill[2].Thin || r.ByFill[2].Windows != MinFillRatioWindows-1 {
		t.Fatalf("bucket 0.4-0.6: %+v, want thin with %d windows", r.ByFill[2], MinFillRatioWindows-1)
	}
	at, global := r.At(0.5)
	near(t, "R_q in the thin bucket", at, r.Value, 1e-12)
	if !global {
		t.Error("the thin bucket's R_q is not flagged as the global one")
	}
	low, lowGlobal := r.At(0.1)
	near(t, "R_q in the learned bucket", low, 1.1, 1e-12)
	if lowGlobal {
		t.Error("the learned bucket is flagged as the global R_q")
	}

	d := olsSweep()
	slo := SLOAtReplicas(d, ReplicasQuery{Rate: 0.5 * mustAnswer(t, KneeRate(d)).Value, PercentileTarget: &TPOTTarget{Percentile: 0.9, Ratio: r}})
	p := mustAnswer(t, slo.PercentileTPOT)
	if k := p.Basis.Fill; !(k > 0.1) {
		t.Fatalf("p90 priced at fill %g: the fixture no longer reaches past the learned bucket's center", k)
	}
	if !p.HasFlag(GlobalTPOTRatio) {
		t.Errorf("p90 at fill %g, inside a thin bucket's reach, is not flagged %s: %v", p.Basis.Fill, GlobalTPOTRatio, p.Flags)
	}

	thin := learned(t, windowsAt(0.5, 1.4, MinFillRatioWindows-1))
	if thin.ByFill != nil {
		t.Errorf("no bucket learned, yet ByFill is %+v", thin.ByFill)
	}
	noFill := windowsAt(0.5, 1.4, 20)
	for i := range noFill {
		noFill[i].FillKnown = false
	}
	if r := learned(t, noFill); r.ByFill != nil {
		t.Errorf("windows with no fill gave ByFill %+v", r.ByFill)
	}
}

// randomRisingWindows is windows over the whole band whose ratio rises with
// the fill, with noise, so that some buckets are learned and some thin.
func randomRisingWindows(rng *rand.Rand) []TPOTWindow {
	n := 10 + rng.IntN(60)
	out := make([]TPOTWindow, 0, n)
	for range n {
		k := rng.Float64()
		out = append(out, fillWindow(k, 1+k*(0.2+2*rng.Float64())+0.1*rng.Float64()))
	}
	return out
}

// The inverse stays monotone with R_q by fill: a looser target never buys a
// lower rate, the fill slo-to-rate answers at gives the target back, and a
// low-fill target is priced at the low fills' ratio, a higher rate than one
// R_q at every fill allows.
func TestRatioByFill_InverseIsMonotone(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	looser := 0
	for range 300 {
		d := randomDecode(rng)
		windows := randomRisingWindows(rng)
		r, _, u := TPOTRatioFromWindows(windows, 0.9)
		if u != nil || r.ByFill == nil {
			continue
		}
		floor, _ := d.tpotAtFill(d.Band.Lo, r)
		prev := 0.0
		for _, f := range []float64{1.01, 1.1, 1.3, 1.6, 2, 3, 5, 8} {
			target := TPOTTarget{Seconds: f * floor, Percentile: 0.9, Ratio: r}
			a, ok := rateFor(d, target, 1).(Answer)
			if !ok {
				continue
			}
			if a.Value < prev*(1-1e-9) {
				t.Fatalf("target %g s buys %g r/s, below %g at a tighter one", target.Seconds, a.Value, prev)
			}
			prev = a.Value
			if slices.Contains(a.Basis.BoundBy, BoundTPOT) {
				back, _ := d.tpotAtFill(max(a.Basis.Fill, d.Band.Lo), r)
				near(t, "TPOT back at the answered fill", back, target.Seconds, 1e-9*target.Seconds)
			}
		}
		// At the band's bottom the curve reads at or below the global R_q;
		// strictly below, the target is looser than one R_q makes it.
		atLo, _ := r.At(d.Band.Lo)
		if atLo >= r.Value {
			continue
		}
		target := TPOTTarget{Seconds: 1.05 * floor, Percentile: 0.9, Ratio: r}
		flat := target
		flat.Ratio = TPOTRatio{Value: r.Value}
		curve, ok := rateFor(d, target, 1).(Answer)
		if !ok {
			continue
		}
		if one, refused := rateFor(d, flat, 1).(Unanswerable); refused && one.Reason == TargetBelowFloor {
			looser++
			continue
		}
		// Where the curve reads below one R_q at one R_q's own answer fill, the
		// product is under the target there, so the curve's fill and rate are
		// at least as high.
		one := mustAnswer(t, rateFor(d, flat, 1))
		if at, _ := r.At(one.Basis.Fill); at > r.Value {
			continue
		}
		if curve.Value < one.Value*(1-1e-9) {
			t.Fatalf("by fill %g r/s, below one R_q's %g", curve.Value, one.Value)
		}
		looser++
	}
	if looser < 50 {
		t.Fatalf("only %d of 300 fleets priced a low-fill target below one R_q's tail; the property ran on too few", looser)
	}
}

// A by-fill curve that falls, or holds a ratio below 1, is refused: it would
// fold slo-to-rate's inverse over.
func TestRatioByFill_FallingCurveIsRefused(t *testing.T) {
	tile := func(values ...float64) []FillRatio {
		out := make([]FillRatio, len(values))
		for i, v := range values {
			out[i] = FillRatio{Lo: float64(i) * FillRatioBucketWidth, Hi: float64(i+1) * FillRatioBucketWidth, Value: v}
		}
		return out
	}
	gap := tile(1.2, 1.3, 1.4, 1.5, 1.6)
	gap[2].Lo = 0.45
	short := tile(1.2, 1.3, 1.4, 1.5)
	for name, curve := range map[string][]FillRatio{
		"falling":         tile(1.6, 1.2, 1.3, 1.4, 1.5),
		"below 1":         tile(0.9, 1.2, 1.3, 1.4, 1.5),
		"a gap":           gap,
		"short of a full": short,
	} {
		r := TPOTRatio{Value: 1.5, ByFill: curve}
		res := rateFor(olsSweep(), TPOTTarget{Seconds: 0.01, Percentile: 0.9, Ratio: r}, 1)
		if u, ok := res.(Unanswerable); !ok || u.Reason != InvalidInput {
			t.Errorf("%s curve priced: %#v", name, res)
		}
	}
	ok := TPOTRatio{Value: 1.5, ByFill: tile(1.2, 1.3, 1.4, 1.5, 1.6)}
	if a := mustAnswer(t, rateFor(olsSweep(), TPOTTarget{Seconds: 0.01, Percentile: 0.9, Ratio: ok}, 1)); !(a.Value > 0) {
		t.Errorf("a curve that tiles [0, 1] and rises priced %g r/s", a.Value)
	}
}

// slo-to-rate flags the global stand-in only where the target sets the rate:
// capped at the operating point, the target's fill is not the answer's.
func TestRatioByFill_GlobalFlagOnlyWhereTPOTBinds(t *testing.T) {
	d := olsSweep()
	thinTop := TPOTRatio{Value: 1.3, ByFill: []FillRatio{
		{Lo: 0, Hi: 0.2, Value: 1.1, Windows: 9}, {Lo: 0.2, Hi: 0.4, Value: 1.1, Windows: 9},
		{Lo: 0.4, Hi: 0.6, Value: 1.3, Thin: true}, {Lo: 0.6, Hi: 0.8, Value: 1.3, Thin: true}, {Lo: 0.8, Hi: 1, Value: 1.3, Thin: true},
	}}
	target := TPOTTarget{Seconds: 0.0045, Percentile: 0.9, Ratio: thinTop}
	free := mustAnswer(t, rateFor(d, target, 1))
	if !slices.Contains(free.Basis.BoundBy, BoundTPOT) || !free.HasFlag(GlobalTPOTRatio) {
		t.Fatalf("uncapped: bound by %v, flags %v; want TPOT and %s at fill %g", free.Basis.BoundBy, free.Flags, GlobalTPOTRatio, free.Basis.Fill)
	}
	capped := mustAnswer(t, rateFor(d, target, 0.5))
	if slices.Contains(capped.Basis.BoundBy, BoundTPOT) || capped.HasFlag(GlobalTPOTRatio) {
		t.Errorf("capped at the operating point: bound by %v, flags %v; want no %s", capped.Basis.BoundBy, capped.Flags, GlobalTPOTRatio)
	}
}

// requireBuckets stops the test unless r carries R_q by fill in every bucket.
func requireBuckets(t *testing.T, r TPOTRatio) {
	t.Helper()
	if len(r.ByFill) != fillRatioBuckets {
		t.Fatalf("ByFill holds %d buckets, want %d", len(r.ByFill), fillRatioBuckets)
	}
}

// Coarse router buckets (10, 20, 40 ms) around a 14 ms mean hold p50, p90 and
// p99 in one bucket. Read at its upper edge, all three give 20/14 and price
// one TPOT. The window's sum caps p50 lower: the 500 requests at or above the
// median would each be at least it, and the sum leaves 4 ms a request over
// the bucket's 10 ms floor, spread over those 500. p90 and p99 share the edge,
// which is all a 10 ms bucket can say about them without under-stating.
func TestRatio_CoarseBucketBoundByMean(t *testing.T) {
	w := TPOTWindow{
		Buckets: []HistogramBucket{{UpperEdge: 0.010, Count: 0}, {UpperEdge: 0.020, Count: 1000}, {UpperEdge: 0.040, Count: 1000}, {UpperEdge: math.Inf(1), Count: 1000}},
		Sum:     14, Count: 1000,
	}
	windows := []TPOTWindow{w, w, w}
	for _, c := range []struct {
		q       float64
		want    float64
		bounded bool
	}{
		{0.5, (0.010 + 4.0/500) / 0.014, true},
		{0.9, 0.020 / 0.014, false},
		{0.99, 0.020 / 0.014, false},
	} {
		r, flags, u := TPOTRatioFromWindows(windows, c.q)
		if u != nil {
			t.Fatalf("p%g refused: %v", 100*c.q, *u)
		}
		near(t, "R_q", r.Value, c.want, 1e-9)
		if !slices.Contains(flags, CoarseTPOTBucket) || slices.Contains(flags, MeanBoundedTPOTPercentile) != c.bounded {
			t.Errorf("p%g flags %v, want %s, and %s %v", 100*c.q, flags, CoarseTPOTBucket, MeanBoundedTPOTPercentile, c.bounded)
		}
	}
}
