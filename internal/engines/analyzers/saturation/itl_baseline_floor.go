package saturation

// SwitchITLBaselineFromFloor is the replay switch for the ITL line's
// intercept while the key has no learned B (issue
// llm-d-incubation/llm-d-scaling-manager#169, item 1). A key learns B only
// from a full fit, and many never make one: on the e1 and staircase
// recordings nearly every published line sits on the 6 ms default
// (itl.DefaultBaselineSec). Through readings at a fill k, a fit through a B
// under the true one prices ITL high past them, so the derived mu is low and
// the fleet over-provisioned.
//
// On, such a key is pinned through the floor read below the band instead
// (itl.Admission.BaselineFromFloor): the 10th percentile of the steady mean
// ITLs its replicas read at a fill under the band's bottom, the readings the
// fit drops as idle, once there are itl.BelowBandMinReadings of them
// (itl.BelowBandFloor, the floor the capacity planner reads too,
// whatif.BelowBand.LeastITL). The floor is read at its own fill: B is the
// intercept of the line through it and the mean of the pinned fit's
// readings, which on a true line is its B. Pinned at the floor's ITL as if
// read at no fill, B would be high by A times that fill, and the line would
// price under the truth past its readings. The one-sided rule
// (SwitchITLOneSidedAtCap) is set against the pinned fit as against the
// default. A learned B ends it. Off, as before, the default. Off from the
// constructor: the controller keeps the default until a GPU check clears it.
const SwitchITLBaselineFromFloor = "saturation-itl-baseline-from-floor"

// baselineFromFloorOn reports SwitchITLBaselineFromFloor.
func (a *SaturationAnalyzer) baselineFromFloorOn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.itlBaselineFromFloor
}
