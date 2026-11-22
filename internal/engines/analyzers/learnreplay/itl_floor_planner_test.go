package learnreplay

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// floorPinnedLines counts, over every tenth cycle of a golden and its last,
// the decode lines pinned at an intercept B (whatif.TierPinnedB), and of
// those the ones on the 6 ms default B (whatif.OnDefaultB), in the planner's
// replay with sw over PlannerSwitches.
func floorPinnedLines(t *testing.T, fixture, ns, model string, sw map[string]map[string]bool) (pinned, onDefault int) {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join(goldenDir, fixture, RecordingName))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	every := func(last int) []int {
		var cs []int
		for c := 10; c < last; c += 10 {
			cs = append(cs, c)
		}
		return cs
	}
	st, early, err := StatesFrom(context.Background(), BytesSource(raw), Options{Switches: sw},
		StateQuery{Namespace: ns, ModelID: model}, every)
	require.NoError(t, err)
	states := []*State{st}
	for _, s := range early {
		states = append(states, s)
	}
	for _, s := range states {
		for _, vs := range s.Variants {
			if vs.Tier != whatif.TierPinnedB {
				continue
			}
			pinned++
			if whatif.OnDefaultB(vs.Tier, vs.Line) {
				onDefault++
			}
		}
	}
	return pinned, onDefault
}

// The planner's replay keeps a decode line with no learned B on the 6 ms
// default, and so refuses it PinnedDefaultB, whatever the controller's
// constructor says (saturation.SwitchITLBaselineFromFloor, on there since
// issue llm-d-incubation/llm-d-scaling-manager#318): on ttft-three-pods,
// pinned through the floor below the band, the planner read TPOT at 0.33 to
// 0.46 of what was measured, with no flag saying B was not measured. A caller
// that sets the switch still gets the floor's B, which is how the test knows
// the recording reads a floor at all.
func TestPlannerSwitches_TheFloorBIsOffInThePlanner(t *testing.T) {
	t.Parallel() // reads the goldens, writes nowhere
	on, set := PlannerSwitches()[PackageSaturation][saturation.SwitchITLBaselineFromFloor]
	assert.True(t, set, "PlannerSwitches names the floor switch")
	assert.False(t, on, "the planner's replay turns the floor's B off")

	pinned, onDefault := floorPinnedLines(t, "ttft-three-pods", "ns-01", "model-02", nil)
	require.Positive(t, pinned, "the fixture has pinned-B lines to read")
	assert.Equal(t, pinned, onDefault, "every pinned-B line of the planner's replay is on the default B")

	pinnedOn, onDefaultOn := floorPinnedLines(t, "ttft-three-pods", "ns-01", "model-02",
		map[string]map[string]bool{PackageSaturation: {saturation.SwitchITLBaselineFromFloor: true}})
	require.Positive(t, pinnedOn)
	assert.Less(t, onDefaultOn, pinnedOn, "with the switch set on, the floor pins some line off the default B")
}
