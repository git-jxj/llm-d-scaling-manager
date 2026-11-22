package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// psState is a state whose prefill variant runs seqs prompts a rank on ranks
// ranks in steps of budget tokens, as its flags state them.
func psState(seqs, budget int64, ranks int) *learnreplay.State {
	p := &capacity.EngineParams{MaxNumSeqs: seqs, MaxNumSeqsSet: seqs > 0, MaxNumBatchedTokens: budget}
	return &learnreplay.State{Variants: []learnreplay.VariantState{{Variant: "pre", Params: p, Ranks: ranks}}}
}

// The engine's flags decide: max_num_seqs above 1 and a step budget of at
// least two mean computed prompts price processor sharing, with the pod's
// slots its ranks' sequences; anything else leaves the fleet as it was, and
// says why.
func TestTTFT_PSByEngineReadsTheFlags(t *testing.T) {
	for name, c := range map[string]struct {
		st     *learnreplay.State
		prompt float64
		on     bool
		slots  int
	}{
		"client":         {psState(4, 8192, 4), 2664, true, 16},
		"one rank":       {psState(4, 8192, 1), 2664, true, 4},
		"step too tight": {psState(4, 8192, 4), 5000, false, 0},
		"one sequence":   {psState(1, 8192, 4), 2664, false, 0},
		"budget unread":  {psState(4, 0, 4), 2664, false, 0},
		"no params":      {&learnreplay.State{Variants: []learnreplay.VariantState{{Variant: "pre"}}}, 2664, false, 0},
	} {
		pf := &pricedFleet{}
		pf.fleet.Prefill = whatif.Prefill{PromptTokens: c.prompt}
		pf.psByEngine(c.st, "pre")
		assert.Equal(t, c.on, pf.fleet.Prefill.Queue.Model == whatif.QueueProcessorSharing, name)
		assert.Equal(t, c.slots, pf.fleet.Prefill.Queue.Slots, name)
		assert.Equal(t, c.on, len(pf.flags) == 1 && pf.flags[0] == psByEngineFlag, name)
		words := strings.Join(pf.in.Assumed, "; ")
		if c.on {
			assert.Contains(t, words, "prefill queue model processor-sharing (engine config: max-num-seqs", name)
		} else {
			assert.Contains(t, words, "prefill queue model by the spread rule (engine config:", name)
		}
	}
}
