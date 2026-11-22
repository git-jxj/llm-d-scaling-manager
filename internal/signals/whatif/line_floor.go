package whatif

import "github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"

// LineFloor is the floor read below the band, as a point: the mean ITL a low
// quantile of the variant's readings under the band's bottom read
// (itl.BelowBandPoint), in seconds, at the fill it was read at. The zero
// value is no floor.
type LineFloor struct {
	ITL, Fill float64
}

// ok reports a floor a line can be set against: a positive, finite ITL at a
// fill in [0, 1).
func (f LineFloor) ok() bool {
	return Finite(f.ITL) && f.ITL > 0 && Finite(f.Fill) && f.Fill >= 0 && f.Fill < 1
}

// LiftToFloor is line raised, by a shift of its intercept B alone, to pass
// through floor where it read under it there, and the shift in seconds; line
// itself and 0 where it did not, where the floor is not ok, or where the
// line's intercept is not a measured one (MeasuredIntercept).
//
// The floor is a low quantile of what the engines read at a fill under the
// band, so a line under it at that fill reads under nearly every reading
// there. Its B is then not the engine's: a full fit over a narrow spread of
// fill pivots about the readings' mean, and B moves with the fit's slope. On
// the public staircase's warm-up the last full fit, at the top of a plateau,
// put B at 1.13 ms where the engines read 1.75 ms at a fill of 0.0006 to
// 0.0011, and
// the planner's decode fill at a rate is B / (C/KVreq - lambda*OL*A) times
// lambda*OL (Decode.fillAtReplicaRate; a burst adds S*A*e to B there):
// proportional to B, so the fill and
// TPOT it predicted were about 0.7 of what the pods held (issue
// llm-d-incubation/llm-d-scaling-manager#274).
//
// Only B moves: the slope was fitted over the band, where the line is read,
// and a shift up prices a higher ITL at every fill, never a lower one. That
// is not itl.FloorIntercept's rule, which pivots the line through the floor
// and its readings' mean and so lowers A: on the staircase's B-off phase the
// shifted line reads 7.34 ms at the fill of 0.18 where 7.6 ms was measured
// (7.36 ms on the unit test's floor at 0.0006), and the pivoted one about
// 7.1 ms. A held line has no readings to pivot
// about either.
func LiftToFloor(tier string, line itl.Model, floor LineFloor) (itl.Model, float64) {
	if !floor.ok() || !MeasuredIntercept(tier, line) || !itl.ValidModel(line.A, line.B) {
		return line, 0
	}
	at := line.ITLAt(floor.Fill)
	if !(at < floor.ITL) {
		return line, 0
	}
	lift := floor.ITL - at
	line.B += lift
	return line, lift
}
