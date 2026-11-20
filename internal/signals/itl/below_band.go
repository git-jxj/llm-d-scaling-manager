package itl

import (
	"math"
	"slices"
	"time"
)

// The floor below the band: a low quantile of the mean ITLs a variant's
// replicas read at a fill under the band's bottom, the readings a fit drops
// as idle. Each is A times its small fill above B, so the floor is an upper
// bound on the engine's own floor B, and close to it.
//
// Two readers share it. The capacity planner reads the line below the band
// down to it (whatif.BelowBand.LeastITL). The tracker pins a key with no
// learned B through it, read at its fill (Admission.BaselineFromFloor).

const (
	// BelowBandWindow is how many of a variant's latest readings below the
	// band its floor is read from: an hour of one replica at the 15 s cycle.
	// The capacity planner's replay bounds them by this count alone; the
	// tracker ages them out at DefaultObservationMaxAge too (pruneBelowBand),
	// and drops them with the key's window on a rollout or a shape move.
	BelowBandWindow = 240
	// BelowBandMinReadings is the fewest readings a floor is read from;
	// fewer is no floor.
	BelowBandMinReadings = 10
	// BelowBandQuantile is the floor's quantile: a low one, not the least,
	// so one window that read low does not set it.
	BelowBandQuantile = 0.10
)

// belowBandIndex is the index, in n readings sorted by ITL, of the floor:
// floor(BelowBandQuantile*n), so that with ten readings the least is passed
// over; -1 with fewer than BelowBandMinReadings.
func belowBandIndex(n int) int {
	if n < BelowBandMinReadings {
		return -1
	}
	return int(BelowBandQuantile * float64(n))
}

// BelowBandFloor is the floor below the band read from xs, mean ITLs in
// seconds: their BelowBandQuantile quantile (belowBandIndex). 0 with fewer
// than BelowBandMinReadings.
func BelowBandFloor(xs []float64) float64 {
	i := belowBandIndex(len(xs))
	if i < 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[i]
}

// belowBandPoint is the floor of the readings under lo, the band's bottom
// now, as BelowBandFloor reads it, and the fill it was read at: the reading
// at belowBandIndex by ITL. A reading kept under an older, higher bottom is
// not below this band, and is not read. ok is false with too few.
func belowBandPoint(readings []Observation, lo float64) (Observation, bool) {
	s := make([]Observation, 0, len(readings))
	for _, o := range readings {
		if o.K < lo {
			s = append(s, o)
		}
	}
	i := belowBandIndex(len(s))
	if i < 0 {
		return Observation{}, false
	}
	slices.SortStableFunc(s, func(a, b Observation) int {
		switch {
		case a.ITLSec < b.ITLSec:
			return -1
		case a.ITLSec > b.ITLSec:
			return 1
		}
		return 0
	})
	return s[i], true
}

// pruneBelowBand is xs without the readings older than
// DefaultObservationMaxAge at now. That is the main window's horizon, the
// readings the pinned fit takes: the floor and the fit's mean set one line,
// so both are read over the same stretch. The hour BelowBandWindow holds at
// one replica would let a floor outlive every reading it is paired with.
func pruneBelowBand(xs []Observation, now time.Time) []Observation {
	return slices.DeleteFunc(xs, func(o Observation) bool { return now.Sub(o.Timestamp) > DefaultObservationMaxAge })
}

// floorMinGap is how far above the floor's fill the pinned fit's mean fill
// must sit, as a multiple of it (floorIntercept).
const floorMinGap = 2.0

// keepBelowBand is xs with x appended, holding the latest BelowBandWindow.
func keepBelowBand(xs []Observation, x Observation) []Observation {
	xs = append(xs, x)
	if len(xs) > BelowBandWindow {
		xs = xs[len(xs)-BelowBandWindow:]
	}
	return xs
}

// floorIntercept is the B that puts the pinned fit over pinned through the
// floor reading at its own fill: the pinned fit's line passes through the
// mean point (kbar, ITLbar) of its readings (pinnedSlope), so the line
// through that point and (floor.K, floor.ITLSec) has intercept
// (floor.ITLSec*kbar - ITLbar*floor.K) / (kbar - floor.K). On readings of a
// true line that is its B: pinned at the floor's ITL itself, as if read at
// no fill, B is high by A times the floor's fill and the slope low, so the
// line prices under the truth past its readings.
//
// The intercept's gain on an error e in the readings' mean is
// floor.K / (kbar - floor.K), and on an error d in the floor's ITL
// kbar / (kbar - floor.K): a floor read just under the band against readings
// just above it turns a 0.4 ms error into several. So the mean must sit at
// floorMinGap times the floor's fill or more, where the first gain is at
// most 1 and the second at most 2: B errs by no more than the readings do.
//
// ok is false when the mean is under that, or the intercept is not in
// (0, floor.ITLSec].
func floorIntercept(pinned []Observation, floor Observation) (float64, bool) {
	var k, y float64
	n := 0
	for _, o := range pinned {
		if !(o.K > 0) || !(o.ITLSec > 0) || math.IsInf(o.K, 0) || math.IsInf(o.ITLSec, 0) {
			continue
		}
		k += o.K
		y += o.ITLSec
		n++
	}
	if n == 0 {
		return 0, false
	}
	kbar, ybar := k/float64(n), y/float64(n)
	if !(kbar >= floorMinGap*floor.K) || !(kbar > floor.K) {
		return 0, false
	}
	b := (floor.ITLSec*kbar - ybar*floor.K) / (kbar - floor.K)
	if !(b > 0) || b > floor.ITLSec || math.IsInf(b, 0) {
		return 0, false
	}
	return b, true
}
