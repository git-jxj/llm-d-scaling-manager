package whatif

// StepBudgetCap is the most sequences one engine rank runs in a decode step:
// its batch cap, perRank, never more than its step token budget holds.
//
// vLLM's Scheduler.schedule() takes 1 + k tokens of max_num_batched_tokens
// for each running request, k the speculative tokens a step proposes
// (num_speculative_tokens), and skips the rest of the running requests once
// the budget is spent. So a rank with budget B runs at most floor(B / (1 + k))
// sequences a step, whatever its max_num_seqs: a wide-EP decode with the
// batched all-to-all's default budget of 256 and MTP k = 3 runs 64, against a
// cap of 256 (issue llm-d-incubation/llm-d-scaling-manager#302).
//
// The accepted draft tokens do not enter: they set how many tokens a running
// sequence emits a step, which the measured ITL (seconds per emitted token)
// already carries, not how many sequences run.
//
// A budget of 0 or below is unknown and caps nothing; a perRank of 0 or below
// is no cap, and the budget's batch is returned. k below 0 is read as 0.
func StepBudgetCap(perRank, budget int64, specTokens int) int64 {
	if budget <= 0 {
		return perRank
	}
	byBudget := budget / int64(1+max(specTokens, 0))
	if perRank <= 0 {
		return byBudget
	}
	return min(perRank, byBudget)
}
