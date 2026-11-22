package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The planner's own decode term windows read slo.SwitchTTFTDecodeSpeculatingHeld:
// with it, a fleet whose every decode row speculates holds the decode engine's
// measured TTFT for D, its ITL per accepted token as the planner's line is;
// without it, D is omitted and the old sum T_xfer + ITL stands for it.
func TestTerm_TheDecodeSpeculatingHeldSwitch(t *testing.T) {
	term := func(on, perToken bool) whatif.DecodeTerm {
		s := newTTFTSamples(true)
		s.specHeld, s.itlPerToken = on, perToken
		args := func(string) string { return "" }
		for i := range 40 {
			pre := prefillRow("p0", 2600, 1)
			pre.AvgInputTokens, pre.AvgInputTokensRecent = 2600, 2600
			dec := decodeRow("d0", 0.48, 1.5)
			dec.AvgITL, dec.AvgITLKnown, dec.CapacityQueueKnown = 0.04, true, true
			dec.AvgInputTokens, dec.AvgInputTokensRecent = 85000, 85000
			dec.SpecAcceptedPerStep, dec.SpecAcceptedPerStepKnown = 1.85, true
			s.noteTerm(pdInput(pre, dec), time.Unix(int64(15*i), 0), args)
		}
		return whatif.TermFromEstimate(s.terms["dec"].Estimate())
	}
	off := term(false, true)
	assert.True(t, off.EngineOmitted, "off, as before: the old sum stands for D")
	assert.Equal(t, prefill.TooFewDecodeMinutes, off.EngineRefused)

	on := term(true, true)
	assert.False(t, on.EngineOmitted)
	assert.True(t, on.StepAssumed)
	assert.Equal(t, prefill.TooFewDecodeMinutes, on.EngineRefused, "never in D's fit")
	assert.Equal(t, 0.48, on.Engine.Base, "the decode engine's measured TTFT")
	assert.InDelta(t, 0.04/2.85, on.FromITL, 1e-12)
	assert.InDelta(t, whatif.AssumedDecodeStepSlope*2.85, on.PerStep, 1e-9)

	step := term(true, false)
	assert.InDelta(t, 0.04, step.FromITL, 1e-12, "a per-step line reads the step as it is")
	assert.Equal(t, whatif.AssumedDecodeStepSlope, step.PerStep)
}
