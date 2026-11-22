package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// specRun feeds a window 30 minutes of one decode pod that speculates: a
// measured TTFT of 0.35 s at a whole prompt of 40000 tokens, a step of 40 ms
// emitting 2.5 tokens (1.5 accepted), and with quiet, a pod beside it that
// does not speculate, with a TTFT of 0.2 s.
func specRun(c DecodeTermCycle, quiet bool) DecodeTermEstimate {
	w := NewDecodeTermWindow()
	for i := range 30 {
		row := decodeRow("d", 0.35, 0.04, 40000)
		row.SpecAcceptedPerStep, row.SpecAcceptedPerStepKnown = 1.5, true
		rows := []domain.ReplicaMetrics{row}
		if quiet {
			rows = append(rows, decodeRow("q", 0.2, 0.01, 40000))
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
	assert.Equal(t, 0.35, on.Held.Seconds, "the decode engine's measured TTFT")
	assert.Equal(t, 40000.0, on.Held.PromptTokens)
	assert.InDelta(t, 0.04/2.5, on.Held.ITLSeconds, 1e-12, "the step over the tokens it emits")
	assert.InDelta(t, 2.5, on.Held.StepTokens, 1e-12)
	assert.Equal(t, TooFewDecodeMinutes, on.Decode.Refused, "never in D's fit")
	assert.Zero(t, on.Decode.Minutes)

	perStep := specRun(DecodeTermCycle{SpeculatingHeld: true}, false)
	assert.Equal(t, HeldDecode{Seconds: 0.35, PromptTokens: 40000, ITLSeconds: 0.04}, perStep.Held,
		"a per-step line reads the step as it is")
}

// A quiet row that does not speculate is held before a speculating one, even
// with a lower TTFT: the switch only fills where nothing else stands.
func TestHeld_ARowThatDoesNotSpeculateComesFirst(t *testing.T) {
	on := specRun(DecodeTermCycle{SpeculatingHeld: true, ITLPerToken: true}, true)
	assert.Equal(t, HeldDecode{Seconds: 0.2, PromptTokens: 40000, ITLSeconds: 0.01}, on.Held)
	assert.Equal(t, 30, on.Decode.Minutes, "the quiet row's minutes alone are D's")
}

// Decode is counted once. R is the router's mean less both engines' (hopOver),
// the decode engine's TTFT taken out whether its rows speculate or not, so R
// never holds it: with the switch the held D supplies it, and D + R is the
// router's mean less the prefill engine's. The switch does not touch R.
func TestHeld_ASpeculatingDAndTheHopCountDecodeOnce(t *testing.T) {
	const prefillTTFT, decodeTTFT, hop = 1.1, 0.35, 0.25
	run := func(on bool) DecodeTermEstimate {
		w := NewDecodeTermWindow()
		at := termT0
		for range 30 * 4 {
			c := hopCycle(at, prefillTTFT+decodeTTFT+hop, 1.5, 2600)
			c.Prefill[0].AvgTTFT = prefillTTFT
			dec := decodeRow("d", decodeTTFT, 0.04, 40000)
			dec.RequestRate = 1.5
			dec.SpecAcceptedPerStep, dec.SpecAcceptedPerStepKnown = 1.5, true
			c.Decode = []domain.ReplicaMetrics{dec}
			c.SpeculatingHeld, c.ITLPerToken = on, true
			w.Observe(c)
			at = at.Add(15 * time.Second)
		}
		return w.Estimate()
	}
	off, on := run(false), run(true)
	assert.Empty(t, off.Hop.Refused)
	assert.InDelta(t, hop, off.Hop.Base, 1e-9, "R holds no decode TTFT with the switch off")
	assert.Equal(t, off.Hop, on.Hop, "the switch does not touch R")
	assert.Zero(t, off.Held.Seconds, "off: no D, the old sum stands for the decode engine")
	assert.InDelta(t, prefillTTFT+decodeTTFT+hop-prefillTTFT, on.Held.Seconds+on.Hop.Base, 1e-9,
		"on: D + R is the router's mean less the prefill engine's, decode counted once")
}

// Every other gate holds for a speculating row: a slot queue keeps it out.
func TestHeld_ASpeculatingRowKeepsTheOtherGates(t *testing.T) {
	w := NewDecodeTermWindow()
	for i := range 10 {
		row := decodeRow("d", 0.35, 0.04, 40000)
		row.SpecAcceptedPerStep, row.SpecAcceptedPerStepKnown = 1.5, true
		row.CapacityQueueLength = 2
		w.Observe(DecodeTermCycle{Now: termT0.Add(time.Duration(i) * time.Minute), Decode: []domain.ReplicaMetrics{row},
			PromptTokens: 2600, SpeculatingHeld: true, ITLPerToken: true})
	}
	assert.Equal(t, HeldDecode{}, w.Estimate().Held)
}
