package learnreplay

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The planner's own decode term windows read slo.SwitchTTFTHopWindowMeans:
// with it, a fleet whose rate moves every cycle teaches the hop from its
// windows' means; without it, no window passes the per-cycle rate gate.
func TestTerm_TheHopWindowMeansSwitch(t *testing.T) {
	hop := func(on bool) prefill.HopFit {
		s := newTTFTSamples(true)
		s.hopWindowMeans = on
		args := func(string) string { return "" }
		for i := range 4 * 90 {
			rate := 1.0
			if i%2 == 1 {
				rate = 1.6
			}
			pre := prefillRow("p0", 2000*rate, rate)
			pre.AvgTTFT, pre.AvgTTFTKnown, pre.AvgInputTokens, pre.AvgInputTokensRecent = 1.4, true, 85000, 85000
			pre.CapacityQueueKnown = true
			in := pdInput(pre, decodeRow("d0", 0.48, rate))
			in.RouterLatency = &domain.RouterLatency{WindowSeconds: 300, TTFTSumRate: 2.48, TTFTCountRate: 1}
			s.noteTerm(in, time.Unix(int64(15*i), 0), args)
		}
		return s.terms["dec"].Estimate().Hop
	}
	off := hop(false)
	assert.Equal(t, prefill.TooFewHopWindows, off.Refused)
	assert.Zero(t, off.Windows)
	on := hop(true)
	require.Empty(t, on.Refused)
	assert.True(t, on.FromWindowMeans)
	assert.InDelta(t, 0.6, on.Base, 1e-9, "the router's 2.48 s less the engines' 1.4 and 0.48")
}

// StateAt reads the switch from its options: on the repo's Qwen3-32B warm-up,
// a P/D recording with the router's mean, the hop's source follows it.
func TestStateAt_TheHopWindowMeansSwitchReachesTheTerm(t *testing.T) {
	gz, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "cmd", "capacity-planner", "testdata", "staircase-q32", "warmup.jsonl.gz"))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	assert.NotContains(t, PlannerSwitches()[PackageSLO], slo.SwitchTTFTHopWindowMeans, "planner-only and off")
	for _, on := range []bool{false, true} {
		opts := Options{Switches: map[string]map[string]bool{PackageSLO: {slo.SwitchTTFTHopWindowMeans: on}}}
		st, err := StateAt(context.Background(), raw, opts, StateQuery{})
		require.NoError(t, err)
		i := slices.IndexFunc(st.Variants, func(v VariantState) bool { return domain.CanonicalRole(v.Role) == domain.RoleDecode })
		require.GreaterOrEqual(t, i, 0)
		term := st.Variants[i].DecodeTTFT.Term
		t.Logf("on=%v hop %+v assumed=%v refused=%s", on, term.Hop, term.HopAssumed, term.HopRefused)
		assert.Equal(t, on, term.HopFromWindowMeans)
		assert.False(t, term.HopAssumed)
	}
}
