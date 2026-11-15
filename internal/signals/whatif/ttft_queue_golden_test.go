package whatif

import (
	"fmt"
	"testing"
)

// queueGolden is one finite quantile or split, by name.
type queueGolden struct {
	name string
	got  func() float64
}

// queueGoldens are tailQuantile and split on finite inputs: a wait in front
// of a gamma own time at several spreads, a two-part stage mixture, and the
// split against a decode wait at two percentiles.
func queueGoldens() []queueGolden {
	var out []queueGolden
	for _, cv := range []float64{0, 0.25, 1, 2} {
		bl := block{wait: expWait(0.5, 5), own: newOwnTime(1, cv)}
		for _, alpha := range []float64{0.1, 0.01} {
			out = append(out, queueGolden{name: fmt.Sprintf("block cv %g alpha %g", cv, alpha),
				got: func() float64 { return bl.quantile(alpha) }})
		}
		for _, q := range []float64{0.9, 0.99} {
			out = append(out, queueGolden{name: fmt.Sprintf("split cv %g q %g", cv, q),
				got: func() float64 { v, _ := split(bl, expWait(0.3, 8), q); return v }})
		}
	}
	mix := stageMix{w: []float64{0.7, 0.3}, b: []block{{own: newOwnTime(0.2, 0.5)}, {wait: expWait(0.4, 3), own: newOwnTime(0.6, 1.5)}}}
	out = append(out,
		queueGolden{name: "mix alpha 0.05", got: func() float64 { return mix.quantile(0.05) }},
		queueGolden{name: "mix split q 0.95", got: func() float64 { v, _ := split(mix, expWait(0.2, 4), 0.95); return v }})
	return out
}

// The finite path of tailQuantile and split is the parent's to the last bit:
// the knee-rounding fix changes only a +Inf mean and a grid with no finite
// point.
func TestQueueQuantilesUnchanged(t *testing.T) {
	gs := queueGoldens()
	if len(queueGoldenValues) != len(gs) {
		t.Fatalf("%d golden values for %d cases", len(queueGoldenValues), len(gs))
	}
	for i, g := range gs {
		if got, want := g.got(), queueGoldenValues[i]; got != want {
			t.Errorf("%s: %.17g, want %.17g", g.name, got, want)
		}
	}
}

// queueGoldenValues are the values, in queueGoldens' order, computed on the
// parent of the knee-rounding fix (99c17a38f).
var queueGoldenValues = []float64{
	1.3218875824868854, // block cv 0 alpha 0.1
	1.7824046010856711, // block cv 0 alpha 0.01
	1.6757546123551137, // split cv 0 q 0.9
	2.4240947675782607, // split cv 0 q 0.99
	1.4977606526906015, // block cv 0.25 alpha 0.1
	1.9803538786717696, // block cv 0.25 alpha 0.01
	1.8611622407490778, // split cv 0.25 q 0.9
	2.6227091920891605, // split cv 0.25 q 0.99
	2.420361191821974,  // block cv 1 alpha 0.1
	4.722953220950991,  // block cv 1 alpha 0.01
	2.9501264419636466, // split cv 1 q 0.9
	5.540539001742648,  // split cv 1 q 0.99
	3.110020341562314,  // block cv 2 alpha 0.1
	9.840733865381253,  // block cv 2 alpha 0.01
	3.7492225785419135, // split cv 2 q 0.9
	10.799584210474947, // split cv 2 q 0.99
	1.3568952588135657, // mix alpha 0.05
	2.3203840833607976, // mix split q 0.95
}
