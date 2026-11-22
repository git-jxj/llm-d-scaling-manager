package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// specRun feeds a window 30 minutes of one decode pod that speculates: a
// measured TTFT of 0.48 s at a whole prompt of 85000 tokens, a step of 40 ms
// emitting 2.85 tokens (1.85 accepted), and with quiet, a pod beside it that
// does not speculate, with a TTFT of 0.2 s.
func specRun(c DecodeTermCycle, quiet bool) DecodeTermEstimate {
	w := NewDecodeTermWindow()
	for i := range 30 {
		row := decodeRow("d", 0.48, 0.04, 85000)
		row.SpecAcceptedPerStep, row.SpecAcceptedPerStepKnown = 1.85, true
		rows := []domain.ReplicaMetrics{row}
		if quiet {
			rows = append(rows, decodeRow("q", 0.2, 0.01, 85000))
		}
		c.Now, c.Decode, c.PromptTokens = termT0.Add(time.Duration(i)*time.Minute), rows, 2600
		w.Observe(c)
	}
	return w.Estimate()
}

// Off, a speculating row is no reading: with every row speculating no D is
// held, and the term is omitted. On, the row's measured TTFT is held, its ITL
// read per token where the line is, and it is never read for D's fit.
func TestHeld_ASpeculatingRowIsHeldWithTheSwitch(t *testing.T) {
	off := specRun(DecodeTermCycle{ITLPerToken: true}, false)
	assert.Equal(t, HeldDecode{}, off.Held, "off, as before: no reading")
	assert.Equal(t, TooFewDecodeMinutes, off.Decode.Refused)
	assert.Zero(t, off.Decode.Minutes)

	on := specRun(DecodeTermCycle{SpeculatingHeld: true, ITLPerToken: true}, false)
	assert.Equal(t, 0.48, on.Held.Seconds, "the decode engine's measured TTFT")
	assert.Equal(t, 85000.0, on.Held.PromptTokens)
	assert.InDelta(t, 0.04/2.85, on.Held.ITLSeconds, 1e-12, "the step over the tokens it emits")
	assert.InDelta(t, 2.85, on.Held.StepTokens, 1e-12)
	assert.Equal(t, TooFewDecodeMinutes, on.Decode.Refused, "never in D's fit")
	assert.Zero(t, on.Decode.Minutes)

	perStep := specRun(DecodeTermCycle{SpeculatingHeld: true}, false)
	assert.Equal(t, HeldDecode{Seconds: 0.48, PromptTokens: 85000, ITLSeconds: 0.04}, perStep.Held,
		"a per-step line reads the step as it is")
}

// A quiet row that does not speculate is held before a speculating one, even
// with a lower TTFT: the switch only fills where nothing else stands.
func TestHeld_ARowThatDoesNotSpeculateComesFirst(t *testing.T) {
	on := specRun(DecodeTermCycle{SpeculatingHeld: true, ITLPerToken: true}, true)
	assert.Equal(t, HeldDecode{Seconds: 0.2, PromptTokens: 85000, ITLSeconds: 0.01}, on.Held)
	assert.Equal(t, 30, on.Decode.Minutes, "the quiet row's minutes alone are D's")
}

// Every other gate holds for a speculating row: a slot queue keeps it out.
func TestHeld_ASpeculatingRowKeepsTheOtherGates(t *testing.T) {
	w := NewDecodeTermWindow()
	for i := range 10 {
		row := decodeRow("d", 0.48, 0.04, 85000)
		row.SpecAcceptedPerStep, row.SpecAcceptedPerStepKnown = 1.85, true
		row.CapacityQueueLength = 2
		w.Observe(DecodeTermCycle{Now: termT0.Add(time.Duration(i) * time.Minute), Decode: []domain.ReplicaMetrics{row},
			PromptTokens: 2600, SpeculatingHeld: true, ITLPerToken: true})
	}
	assert.Equal(t, HeldDecode{}, w.Estimate().Held)
}
