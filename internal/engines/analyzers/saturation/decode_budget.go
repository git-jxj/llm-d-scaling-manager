package saturation

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// SwitchK2DecodeBudgetAtComputedPrompt is the replay switch for the step
// budget a derived k2 (P3) charges a decode replica of a P/D model (issue
// llm-d-incubation/llm-d-scaling-manager#201).
//
// The derived k2 sets the requests one engine rank holds at its token budget
// B (--max-num-batched-tokens) to B x O / (I + O): each request takes I
// prompt tokens of budget to prefill and one per output token. A decode of a
// P/D model does not prefill the prompt: its KV arrives from prefill, and the
// engine computes only what the transfer and its own prefix cache did not
// cover. Charged the whole prompt, a decode with B = 256 and an 82,000-token
// prompt is priced at 2.2 requests per rank against a cap of 64, its k2 at a
// fortieth of its KV cache, and the fleet's measured occupancy then reads
// ten to twenty times over capacity.
//
// On, a decode replica's budget is charged what the replica computes per
// request: the prompt tokens it computed (vllm:request_prefill_kv_computed_tokens)
// over the requests it completed, never more than the whole prompt, and per
// step what a running request takes with speculative decoding: 1 +
// num_speculative_tokens of budget, for 1 + the accepted draft tokens
// emitted. A replica that reports neither rate is priced as before. Off, as
// before, at I + O.
//
// Off from the constructor: on the controller's goldens it moves decode's
// derived k2, and the default changes only after review.
const SwitchK2DecodeBudgetAtComputedPrompt = "k2-decode-budget-at-computed-prompt"

// stepBudget is how one engine rank's token budget is spent per request, for
// the derived k2's batch at the budget (estimateCapacityAtCapBudget). The zero
// value is the whole prompt and one token per step: I + O.
type stepBudget struct {
	// set is false for the zero value: the budget is charged I + O.
	set bool
	// prompt is the prompt tokens the rank computes per request.
	prompt float64
	// stepTokens is the budget one running request takes each step: 1, or
	// 1 + num_speculative_tokens.
	stepTokens float64
	// emitted is the output tokens one running request gains each step: 1,
	// or 1 + the draft tokens accepted per step.
	emitted float64
}

// decodeStepBudget is the step budget of a decode replica rm under params
// (SwitchK2DecodeBudgetAtComputedPrompt), and the zero value -- I + O -- for
// any other role, or a replica that reports no computed prompt or no
// completions. The prompt is held to [0, AvgInputTokens]: a replica cannot
// compute more of the prompt than there is. Speculation is priced only where
// the flags state num_speculative_tokens and the replica reports its
// acceptance; otherwise a step takes one token and emits one.
func decodeStepBudget(rm domain.ReplicaMetrics, role string, params *capacity.EngineParams) stepBudget {
	if domain.CanonicalRole(role) != domain.RoleDecode {
		return stepBudget{}
	}
	if !rm.PrefillComputedTokenRateKnown || !rm.RequestRateKnown || !(rm.RequestRate > 0) ||
		!whatif.Finite(rm.RequestRate) || !finiteNonNegative(rm.PrefillComputedTokenRate) {
		return stepBudget{}
	}
	prompt := rm.PrefillComputedTokenRate / rm.RequestRate
	if rm.AvgInputTokens > 0 {
		prompt = min(prompt, rm.AvgInputTokens)
	}
	b := stepBudget{set: true, prompt: prompt, stepTokens: 1, emitted: 1}
	if k := params.SpeculativeTokens(); k > 0 && rm.SpecAcceptedPerStepKnown &&
		finiteNonNegative(rm.SpecAcceptedPerStep) {
		b.stepTokens = float64(1 + k)
		b.emitted = 1 + min(rm.SpecAcceptedPerStep, float64(k))
	}
	return b
}

// finiteNonNegative reports whether v is a finite number at or above zero.
func finiteNonNegative(v float64) bool {
	return whatif.Finite(v) && v >= 0
}

// steadyBatch is the requests one engine rank holds at its token budget B,
// capped at its batch cap S. A request is resident for O / emitted steps and
// takes prompt + stepTokens x O / emitted of budget over its life, so a rank
// spending B per step holds B x (O / emitted) / (prompt + stepTokens x O /
// emitted). The zero budget is prompt I, one token per step: B x O / (I + O).
func steadyBatch(B, S, avgInput, avgOutput float64, budget stepBudget) float64 {
	if !budget.set {
		return min(B*avgOutput/(avgInput+avgOutput), S)
	}
	steps := avgOutput / budget.emitted
	return min(B*steps/(budget.prompt+budget.stepTokens*steps), S)
}
