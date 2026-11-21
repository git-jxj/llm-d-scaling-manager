package learnreplay

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
)

// decodeOf is st's decode variant.
func decodeOf(t *testing.T, st *State) VariantState {
	t.Helper()
	for _, v := range st.Variants {
		if domain.CanonicalRole(v.Role) != domain.RolePrefill {
			return v
		}
	}
	t.Fatalf("no decode variant at cycle %d", st.Cycle)
	return VariantState{}
}

// On the B-on staircase golden, the held line (saturation.SwitchITLHoldSteadySlope)
// reads under the floor its engines read below the band: 1.66 ms of B
// against 3.07 ms at a fill of 0.042. With saturation.SwitchITLLineAtFloor
// the line passes through that floor, its slope and the fit it came from
// (Baseline, LineReadings) unchanged; off, it is the line as before. With the
// hold off the line is the steeper pinned fit, which reads over the floor and
// is not lifted.
func TestITLLineAtFloor_TheStaircaseLineThroughItsFloor(t *testing.T) {
	rec := readGoldenRecording(t, "ttft-staircase-b-on")
	at := func(hold, lift bool) VariantState {
		t.Helper()
		sw := map[string]map[string]bool{PackageSaturation: {
			saturation.SwitchITLHoldSteadySlope: hold, saturation.SwitchITLLineAtFloor: lift}}
		st, err := StateAt(context.Background(), rec, Options{Switches: sw}, StateQuery{})
		require.NoError(t, err)
		return decodeOf(t, st)
	}

	off, on := at(true, false), at(true, true)
	require.Greater(t, off.BelowBandITL, 0.0, "the golden reads a floor below the band")
	assert.Less(t, off.Line.ITLAt(off.BelowBandFill), off.BelowBandITL, "held, the line reads under its floor")
	assert.Zero(t, off.LineLift)

	assert.Equal(t, off.Line.A, on.Line.A, "the slope is kept")
	assert.InDelta(t, on.BelowBandITL, on.Line.ITLAt(on.BelowBandFill), 1e-12, "lifted, the line passes through its floor")
	assert.InDelta(t, off.BelowBandITL-off.Line.ITLAt(off.BelowBandFill), on.LineLift, 1e-12)
	assert.InDelta(t, off.Line.B+on.LineLift, on.Line.B, 1e-12)
	assert.Equal(t, off.Baseline, on.Baseline)
	assert.Equal(t, off.Tier, on.Tier)
	assert.Len(t, on.LineReadings, len(off.LineReadings))

	steep := at(false, true)
	assert.Zero(t, steep.LineLift, "the pinned fit reads over its floor: nothing to lift")
	assert.GreaterOrEqual(t, steep.Line.ITLAt(steep.BelowBandFill), steep.BelowBandITL)
}
