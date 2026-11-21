package saturation

// SwitchITLHoldSteadySlope is the replay switch for the ITL line's held
// slope (itl.Admission.HoldsSteadySlope). On, a key whose last A was fitted
// on steady readings keeps it where the pinned fit would otherwise refit on
// every reading of the window, the fallback for fewer than
// itl.DefaultMinSamples steady ones. After a load stops, the window's last
// readings are the ramp-down's: each minute's ITL still covers the load that
// stopped while its mean fill falls, so they read a slope too steep, and the
// fit took them once the plateau's steady readings aged out. On the public
// staircase that raised A from 31 to 42 ms per unit of fill and put the
// decode knee about 25% low (issue
// llm-d-incubation/llm-d-scaling-manager#266). Off, the fallback refits, as
// before this switch. Planner-only and off from the constructor; the
// planner's replay does not turn it on yet (learnreplay.PlannerSwitches):
// the line it holds has the B of the plateau's last full fit, under the
// engines' floor, so the planner predicts less decode KV fill than the pods
// held and reads TTFT and TPOT under what was measured
// (SwitchITLLineAtFloor lifts it; issue
// llm-d-incubation/llm-d-scaling-manager#274).
const SwitchITLHoldSteadySlope = "itl-hold-steady-slope"
