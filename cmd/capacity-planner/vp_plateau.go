package main

import (
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// --prefill-vp-plateau turns the occupancy plateau on for one answer (issue
// llm-d-incubation/llm-d-scaling-manager#368): the replay switch
// saturation.SwitchPrefillVPOccupancyPlateau, which is not in
// learnreplay.PlannerSwitches, so it is off unless asked. It is read in one
// place, the replay (answerFlags.replayOptions): every command that replays
// a recording reads its state there, the server and day-plan, backtest and
// predict through whatifapi.InputsRequest.PrefillVPPlateau. A figure priced
// from the plateau's V_P is flagged whatif.VPFromPlateau, which no envelope
// row allows, so it reads OUTSIDE VALIDATED REGION.

// vpPlateauFlag is the flag's name.
const vpPlateauFlag = "prefill-vp-plateau"

// vpPlateauUsage is the flag's help, on every command that takes it.
const vpPlateauUsage = "experimental, opt-in, unvalidated: where no prefill pod queued, so V_P is refused or only a lower bound, " +
	"read V_P from the plateau of the served prompt tokens against the prefill occupancy; for a fleet that never queues prefill. " +
	"Figures priced from it are flagged VPFromPlateau and read OUTSIDE VALIDATED REGION; a plateau that has not flattened " +
	"stays a lower bound (VPLowerBound). Off by default"

// vpPlateauNote is the note an answer priced from the plateau's V_P carries.
const vpPlateauNote = "prefill speed from the occupancy plateau (experimental, --" + vpPlateauFlag + ")"

// plateauValidation labels a report row priced from the plateau's V_P with
// the envelope's verdict on it, OUTSIDE VALIDATED REGION by VPFromPlateau.
// A row priced otherwise, or one already judged, is left as it was.
func plateauValidation(row *whatifapi.ReportRate, kind whatif.FigureKind, res whatif.Result) {
	flags := resultFlags(res)
	if row.Value == nil || row.Validation != nil || !slices.Contains(flags, whatif.VPFromPlateau) {
		return
	}
	row.Validation = whatifapi.ValidationOf(whatif.DefaultEnvelope().Judge(whatif.Figure{Kind: kind, Flags: flags}))
}

// replayOptions is the options the answer's replay runs with: the planner's
// switches (learnreplay.PlannerSwitches), and with --prefill-vp-plateau the
// occupancy plateau on as well. Off, they are the zero options, as before
// the flag.
func (f *answerFlags) replayOptions() learnreplay.Options {
	if !f.vpPlateau {
		return learnreplay.Options{}
	}
	return learnreplay.Options{Switches: map[string]map[string]bool{
		learnreplay.PackageSaturation: {saturation.SwitchPrefillVPOccupancyPlateau: true},
	}}
}
