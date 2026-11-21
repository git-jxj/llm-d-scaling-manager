package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// residenceState is a P/D state (or a decode-only one) whose decode engine
// read a TTFT of 40 ms at 8000 prompt tokens.
func residenceState(pd, on bool) *learnreplay.State {
	d := learnreplay.VariantState{Variant: "d", Role: string(domain.RoleDecode), Replicas: 2,
		Line: itl.Model{A: 0.03125, B: 0.00172}, Tier: whatif.TierOLS, CapacityTokens: 1.16e6, BatchCap: 256, Ranks: 1,
		Shape: learnreplay.MeasuredShape{InputTokens: 8000, OutputTokens: 26}}
	d.DecodeTTFT.EngineTTFT = whatif.DecodeTTFT{Seconds: 0.040, PromptTokens: 8000}
	st := &learnreplay.State{DecodeResidencePull: on, Variants: []learnreplay.VariantState{d}}
	if pd {
		st.Variants = append(st.Variants, learnreplay.VariantState{Variant: "p", Role: string(domain.RolePrefill)})
	}
	return st
}

// With saturation.SwitchDecodeResidencePull a P/D decode's residence holds
// the decode engine's TTFT at the prompt priced (#301); off, or with no
// prefill variant, it holds nothing.
func TestDecodeResidence_TheDecodeEngineTTFTOnTheSwitch(t *testing.T) {
	for name, c := range map[string]struct {
		pd, on bool
		want   float64
	}{
		"P/D, on":          {true, true, 0.040},
		"P/D, off":         {true, false, 0},
		"decode only, on":  {false, true, 0},
		"decode only, off": {false, false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := answerFlags{set: map[string]bool{}}
			vs, err := f.decodeVariants(residenceState(c.pd, c.on))
			require.NoError(t, err)
			require.Len(t, vs, 1)
			assert.InDelta(t, c.want, vs[0].decode.HeldBefore, 1e-12)
		})
	}
}
