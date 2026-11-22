package learnreplay

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
)

// slo.SwitchTTFTCachedTokenCost reaches the planner through the prefill
// state, off unless the replay's options turn it on, and it is not among the
// planner's switches.
func TestStateAt_TheCachedTokenCostSwitchReachesThePrefillState(t *testing.T) {
	gz, err := os.ReadFile(filepath.Join(goldenDir, ttftSaturatedFixture, RecordingName))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	for _, on := range []bool{false, true} {
		opts := Options{}
		if on {
			opts.Switches = map[string]map[string]bool{PackageSLO: {slo.SwitchTTFTCachedTokenCost: true}}
		}
		st, err := StateAt(context.Background(), raw, opts, StateQuery{Cycle: 0})
		require.NoError(t, err)
		require.Len(t, st.Prefill, 1)
		assert.Equal(t, on, st.Prefill[0].CachedTokenCost)
	}
	assert.False(t, PlannerSwitches()[PackageSLO][slo.SwitchTTFTCachedTokenCost], "off in the planner by default")
}
