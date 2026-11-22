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

// slo.SwitchTTFTTierLoad reaches the planner through the prefill state, off
// unless the replay's options turn it on, and is not among the planner's
// switches; the committed recordings read no cached tokens by tier, so the
// prefill state's window reads no tier load either way.
func TestStateAt_TheTierLoadSwitchReachesThePrefillState(t *testing.T) {
	assert.NotContains(t, PlannerSwitches()[PackageSLO], slo.SwitchTTFTTierLoad, "planner-only and off")
	gz, err := os.ReadFile(filepath.Join(goldenDir, ttftSaturatedFixture, RecordingName))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	for _, on := range []bool{false, true} {
		opts := Options{}
		if on {
			opts.Switches = map[string]map[string]bool{PackageSLO: {slo.SwitchTTFTTierLoad: true}}
		}
		st, err := StateAt(context.Background(), raw, opts, StateQuery{Cycle: 0})
		require.NoError(t, err)
		require.Len(t, st.Prefill, 1)
		assert.Equal(t, on, st.Prefill[0].TierLoad)
		_, ok := st.Prefill[0].PromptWindow.TierLoad()
		assert.False(t, ok, "no cached tokens by tier in the recording")
	}
}
