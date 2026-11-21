package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The fixtures here are synthetic: no committed recording has a prefix hit
// (issue llm-d-incubation/llm-d-scaling-manager#311).

// hitState is learnedState with the prefill variant's measured shape: whole
// prompts of 8000 tokens at a hit rate of prefillHit. on is
// slo.SwitchTTFTPrefixAttention as the replay read it.
func hitState(prefillHit float64, on bool) *learnreplay.State {
	st := learnedState()
	st.Prefill[0].PrefixAttention = on
	st.Variants = []learnreplay.VariantState{{Variant: "pre", Role: domain.RolePrefill,
		Shape: learnreplay.MeasuredShape{InputTokens: 8000, PrefixHitRate: prefillHit, FromCycle: 21, ToCycle: 40, Cycles: 20}}}
	return st
}

// hitDecode is a decode variant that served il-token prompts at hit rate
// hit, read as asked, with the KV transfer measured at transfer seconds.
func hitDecode(il, hit, transfer float64) *pricedVariant {
	return &pricedVariant{
		state: learnreplay.VariantState{Variant: "dec", Role: domain.RoleDecode,
			Shape:      learnreplay.MeasuredShape{InputTokens: il, OutputTokens: 200, PrefixHitRate: hit, Cycles: 20},
			DecodeTTFT: learnreplay.DecodeTTFTState{TransferSeconds: transfer}},
		shape: shape.New(il, 200, hit),
	}
}

// hitFleetOf is fleetOf with a decode variant.
func hitFleetOf(t *testing.T, st *learnreplay.State, pv *pricedVariant, f answerFlags, set ...string) *pricedFleet {
	t.Helper()
	f.set = map[string]bool{}
	for _, name := range set {
		f.set[name] = true
	}
	ps, err := f.prefillState(st)
	require.NoError(t, err)
	return f.fleetFor(st, ps, ttftQuestion{q: 0.9}, pv)
}

// An assumed prompt is netted by the decode variant's hit rate, as before;
// with the switch on, by the prefill variant's, which is what the prefill
// pods skip. Unread, the prefill variant's leaves the decode one's.
func TestTTFT_PrefixAttentionPricesThePrefillVariantsHit(t *testing.T) {
	pv := hitDecode(8000, 0.75, 0)
	off := hitFleetOf(t, hitState(0.5, false), pv, answerFlags{inputTokens: 8000}, "assume-input-tokens")
	require.Nil(t, off.refused)
	p := off.fleet.Prefill
	assert.Equal(t, 2000.0, p.PromptTokens, "off: the decode variant's hit")
	assert.Equal(t, 0.75, p.PromptHitRate)
	assert.Equal(t, 8000.0, p.WholePromptTokens)
	assert.False(t, p.PrefixAttention)

	on := hitFleetOf(t, hitState(0.5, true), pv, answerFlags{inputTokens: 8000}, "assume-input-tokens")
	require.Nil(t, on.refused)
	p = on.fleet.Prefill
	assert.Equal(t, 4000.0, p.PromptTokens, "on: the prefill variant's hit")
	assert.Equal(t, 0.5, p.PromptHitRate)
	assert.Equal(t, 8000.0, p.WholePromptTokens)
	assert.True(t, p.PrefixAttention)

	unread := hitState(0.5, true)
	unread.Variants[0].Shape = learnreplay.MeasuredShape{}
	pf := hitFleetOf(t, unread, pv, answerFlags{inputTokens: 8000}, "assume-input-tokens")
	assert.Equal(t, 2000.0, pf.fleet.Prefill.PromptTokens, "no prefill shape read: as before")
}

// The whole prompt and the hit rate sit beside the measured prompt, on
// whatever the switch says, so V_P is flagged VPShapeMoved where it was
// learned at other hits; a state that recorded none (an older one) prices
// and flags as before.
func TestTTFT_TheHitBesideThePromptRaisesVPShapeMoved(t *testing.T) {
	moved := func(st *learnreplay.State) bool {
		pf := fleetOf(t, st, answerFlags{})
		require.Nil(t, pf.refused)
		k, ok := whatif.PrefillKnee(pf.fleet.Prefill).(whatif.Answer)
		require.True(t, ok)
		return k.HasFlag(whatif.VPShapeMoved)
	}
	st := hitState(0.75, false)
	p := fleetOf(t, st, answerFlags{}).fleet.Prefill
	assert.Equal(t, 2000.0, p.PromptTokens, "the measured computed prompt")
	assert.Equal(t, 8000.0, p.WholePromptTokens)
	assert.Equal(t, 0.75, p.PromptHitRate)
	assert.False(t, moved(st), "V_P recorded no whole prompt: as before")

	// V_P learned on fresh 2k prompts: the same 2k computed, 8k at 75% asked.
	st.Prefill[0].Estimate.VPWholePromptTokens = 2000
	assert.True(t, moved(st), "the hit moved from 0 to 0.75")
	st.Prefill[0].Estimate.VPWholePromptTokens, st.Prefill[0].Estimate.VPPromptHitRate = 8000, 0.75
	assert.False(t, moved(st), "learned at the same hits")
}

// The KV transfer moves the whole prompt less what decode holds: with the
// switch on, T scales by I*(1 - decode hit) asked over read; off, by the
// computed prompt, as before.
func TestTTFT_PrefixAttentionMovesTheTransferByTheDecodeHit(t *testing.T) {
	// Read: the prefill pods computed 2000 a request, decode served 8000 at
	// a 50% hit, the pull took 20 ms. Asked: 16000 at the same hits.
	const transfer = 0.02
	total := func(on bool) (float64, *pricedFleet) {
		pv := hitDecode(8000, 0.5, transfer)
		pv.shape = shape.New(16000, 200, 0.5)
		pf := hitFleetOf(t, hitState(0.75, on), pv, answerFlags{inputTokens: 16000}, "assume-input-tokens")
		require.Nil(t, pf.refused)
		p := pf.fleet.Prefill
		return p.TransferSecondsPerToken * p.PromptTokens, pf
	}
	off, pf := total(false)
	assert.Equal(t, 8000.0, pf.fleet.Prefill.PromptTokens)
	assert.InDelta(t, transfer*8000/2000, off, 1e-15, "off: T per computed token read, at the computed prompt asked")
	on, pf := total(true)
	assert.Equal(t, 4000.0, pf.fleet.Prefill.PromptTokens, "the prefill variant's 75%")
	assert.InDelta(t, transfer*(16000*0.5)/(8000*0.5), on, 1e-15, "on: T by the tokens moved, asked over read")
}

// --assume-transfer-base is a fixed part of the measured transfer per
// request; the rest goes with the prompt. 0, the default, prices as before;
// above the measured it is held at the measured; with nothing measured it
// is not applied; a negative one is refused.
func TestTTFT_TheTransferBaseIsAFixedPartOfTheMeasured(t *testing.T) {
	const transfer = 0.02
	at := func(base time.Duration, measured float64) (float64, *pricedFleet) {
		pv := hitDecode(8000, 0.5, measured)
		pv.shape = shape.New(16000, 200, 0.5)
		set := []string{"assume-input-tokens"}
		if base != 0 {
			set = append(set, "assume-transfer-base")
		}
		pf := hitFleetOf(t, hitState(0.75, false), pv, answerFlags{inputTokens: 16000, assumeTransferBase: base}, set...)
		require.Nil(t, pf.refused)
		p := pf.fleet.Prefill
		return p.TransferSecondsPerToken * p.PromptTokens, pf
	}
	none, _ := at(0, transfer)
	assert.InDelta(t, transfer*8000/2000, none, 1e-15)
	got, pf := at(5*time.Millisecond, transfer)
	assert.InDelta(t, 0.005+0.015*8000/2000, got, 1e-15)
	assert.Contains(t, strings.Join(pf.in.Assumed, "; "), "KV transfer base 5ms a request (--assume-transfer-base)")
	got, pf = at(30*time.Millisecond, transfer)
	assert.InDelta(t, transfer, got, 1e-15, "a base above the measured is held at it")
	assert.Contains(t, strings.Join(pf.in.Assumed, "; "), "KV transfer base 30ms above the 20ms measured")
	_, pf = at(5*time.Millisecond, 0)
	assert.Zero(t, pf.fleet.Prefill.TransferSecondsPerToken)
	assert.Contains(t, pf.in.Assumed, "--assume-transfer-base not applied: no KV transfer measured on the decode rows")

	f := answerFlags{assumeTransferBase: -time.Millisecond, set: map[string]bool{"assume-transfer-base": true}}
	require.ErrorContains(t, f.checkTTFT(), "--assume-transfer-base -1ms, want a duration of 0 or more")
}
