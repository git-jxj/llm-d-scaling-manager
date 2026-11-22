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

// slo.SwitchTTFTComputedHit reaches the planner through the prefill state,
// off unless the replay's options turn it on, and the prefill state's
// prompt window reads the computed-token hit either way: on the committed
// recordings, which have no prefix hit, it is 0.
func TestStateAt_TheComputedHitSwitchReachesThePrefillState(t *testing.T) {
	gz, err := os.ReadFile(filepath.Join(goldenDir, ttftSaturatedFixture, RecordingName))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	for _, on := range []bool{false, true} {
		opts := Options{}
		if on {
			opts.Switches = map[string]map[string]bool{PackageSLO: {slo.SwitchTTFTComputedHit: true}}
		}
		st, err := StateAt(context.Background(), raw, opts, StateQuery{Cycle: 0})
		require.NoError(t, err)
		require.Len(t, st.Prefill, 1)
		assert.Equal(t, on, st.Prefill[0].ComputedHit)
		hit, ok := st.Prefill[0].PromptWindow.ComputedHit()
		require.True(t, ok, "the recording reads the one-minute prompt on its prefill rows")
		assert.Less(t, hit, 0.01, "no prefix hit in the recording")
	}
}
