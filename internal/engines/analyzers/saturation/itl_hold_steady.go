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
// with the line held, the planner's predicted decode KV fill reads about
// half what the pods held, and TTFT and TPOT read under what was measured.
const SwitchITLHoldSteadySlope = "itl-hold-steady-slope"
