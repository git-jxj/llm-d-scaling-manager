package whatif

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// A D held from a speculating row: the measured TTFT, climbing from the ITL per
// token it was read at by the assumed slope per second of the step, the tokens
// a step emits times AssumedDecodeStepSlope a second of time per token.
func TestTermFromEstimate_ASpeculatingRow(t *testing.T) {
	held := prefill.HeldDecode{Seconds: 0.48, PromptTokens: 85000, ITLSeconds: 0.04 / 2.85, StepTokens: 2.85}
	got := TermFromEstimate(prefill.DecodeTermEstimate{Decode: prefill.DecodeFit{Refused: prefill.TooFewDecodeMinutes}, Held: held,
		Hop: prefill.HopFit{Refused: prefill.NoRouterMean}})
	want := DecodeTerm{Engine: PromptLine{Base: 0.48, HeldPromptTokens: 85000}, PerStep: AssumedDecodeStepSlope * 2.85,
		FromITL: 0.04 / 2.85, StepTokens: 2.85, StepAssumed: true, Hop: PromptLine{Base: AssumedRouterHopSeconds}, HopAssumed: true,
		EngineRefused: prefill.TooFewDecodeMinutes, HopRefused: prefill.NoRouterMean}
	if got != want {
		t.Errorf("held from a speculating row: %+v", got)
	}
	if _, ok := got.withoutAssumed(); !ok {
		t.Error("a held D is assumed")
	}
	if u := got.check(); u != nil {
		t.Errorf("a sound term: %+v", u)
	}
}

// Priced: at a fill whose ITL is under the one the row was read at, D is the
// measured TTFT itself, held at the shorter prompt; the old sum's ITL is not.
func TestTerm_ASpeculatingRowPricesTheMeasuredTTFT(t *testing.T) {
	fl := termFleet(&DecodeTerm{Engine: PromptLine{Base: 0.48, HeldPromptTokens: 85000}, PerStep: AssumedDecodeStepSlope * 2.85,
		FromITL: 1, StepTokens: 2.85, StepAssumed: true})
	v := mustTTFT(t, ttftAt(t, fl, 8, 0))
	relNear(t, "D", v.Parts.DecodeEngine, 0.48, 1e-12)
	if !v.HasFlag(DecodeStepSlopeAssumed) || v.HasFlag(DecodeTermOmitted) || v.Parts.FirstStep != 0 {
		t.Errorf("flags %v, first step %g", v.Flags, v.Parts.FirstStep)
	}
}
