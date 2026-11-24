package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// A backtest step states the whole prompt and the prompt its prefill pods
// computed (whatifapi.Knobs.PrefillComputedTokens). With
// slo.SwitchTTFTPrefixAttention on, the prefill work is the computed prompt
// and the rest of the whole one is the cached context the attention spans
// (issue llm-d-incubation/llm-d-scaling-manager#311): a step at a 0.75 hit
// computes a quarter of its prompt and attends over the other three.
func TestTTFT_PrefixAttentionPricesTheStepsComputedPrompt(t *testing.T) {
	t.Parallel()
	st := tierState(t, tierRecording(t, true))
	const whole = 24000.0
	knobs := whatifapi.Knobs{Headroom: 0.8, PromptTokens: whole, PrefillComputedTokens: whole / 4}

	sf := tierFleet(t, st, false, true, false, knobs)
	p := sf.pf.fleet.Prefill
	assert.InDelta(t, whole/4, p.PromptTokens, 1e-9, "the prefill pods' computed prompt")
	assert.InDelta(t, whole, p.WholePromptTokens, 1e-9)
	assert.InDelta(t, 0.75, p.PromptHitRate, 1e-12)
	require.True(t, p.CachedContextRead, "the cached context is the whole prompt less the computed one")
	assert.InDelta(t, 0.75*whole, p.CachedContextTokens, 1e-9)
	assert.Contains(t, sf.pf.in.Assumed, "prompt 6000 tokens computed of 24000, as the step's prefill pods computed them")
}

// Off, the computed prompt moves nothing: the whole prompt is netted as
// before. On, a step whose hit is at most whatif.MaterialPrefixHitMove
// (the decode and prefill rows' readings agree only to within it with no
// cache) moves nothing either.
func TestTTFT_TheStepsComputedPromptMovesNothingOffOrImmaterial(t *testing.T) {
	t.Parallel()
	st := tierState(t, tierRecording(t, true))
	const whole = 24000.0
	plain := whatifapi.Knobs{Headroom: 0.8, PromptTokens: whole}

	withStep := plain
	withStep.PrefillComputedTokens = whole / 4
	assert.Equal(t, tierFleet(t, st, false, false, false, plain).pf.fleet.Prefill,
		tierFleet(t, st, false, false, false, withStep).pf.fleet.Prefill, "off")

	withStep.PrefillComputedTokens = 0.95 * whole
	assert.Equal(t, tierFleet(t, st, false, true, false, plain).pf.fleet.Prefill,
		tierFleet(t, st, false, true, false, withStep).pf.fleet.Prefill, "on, a 0.05 hit")
}
