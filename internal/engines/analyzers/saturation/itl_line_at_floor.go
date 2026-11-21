package saturation

// SwitchITLLineAtFloor is the replay switch for the capacity planner's
// decode line lifted to the floor read below the band
// (whatif.LiftToFloor): a line whose ITL at the floor's fill reads under the
// floor has its intercept B raised until it passes through it, slope kept.
// The analyzer accepts the switch and does nothing with it: the lift is the
// planner's reading of the line its replay learned (learnreplay.StatesFrom
// reads the switch from its own options), from the floor the replay reads
// itself (learnreplay's leastBelowBand), and the controller never prices
// from it. Planner-only and off from the
// constructor, as SwitchITLHoldSteadySlope is; the planner's replay does not
// turn it on yet (learnreplay.PlannerSwitches): it is the fix for the decode
// fill the held line predicts (issue
// llm-d-incubation/llm-d-scaling-manager#274), and comes on with that hold.
const SwitchITLLineAtFloor = "itl-line-at-floor"
