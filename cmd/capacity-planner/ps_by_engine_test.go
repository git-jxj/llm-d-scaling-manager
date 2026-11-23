package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// psState is a state whose prefill variant runs seqs prompts a rank, with
// ranks data-parallel ranks, in steps of budget tokens, chunked or not, as
// its flags state them.
func psState(seqs, budget int64, ranks int, chunked bool) *learnreplay.State {
	p := &capacity.EngineParams{MaxNumSeqs: seqs, MaxNumSeqsSet: seqs > 0, MaxNumBatchedTokens: budget, ChunkedPrefillEnabled: chunked}
	return &learnreplay.State{Variants: []learnreplay.VariantState{{Variant: "pre", Params: p, Ranks: ranks}}}
}

// The engine's flags decide, not the prompt a step asks: chunked prefill with
// max_num_seqs above 1 prices processor sharing, with the pod's slots its
// ranks' sequences, whatever the prompt priced; a step budget under the mean
// computed prompt measured is the one sanity check. Anything else leaves the
// fleet as it was, and says why.
func TestTTFT_PSByEngineReadsTheFlags(t *testing.T) {
	for name, c := range map[string]struct {
		st       *learnreplay.State
		measured float64
		on       bool
		slots    int
	}{
		"several ranks": {psState(8, 4096, 2, true), 1500, true, 16},
		"one rank":      {psState(8, 4096, 1, true), 1500, true, 8},
		"budget unread": {psState(8, 0, 2, true), 1500, true, 16},
		"budget under":  {psState(8, 1024, 2, true), 1500, false, 0},
		"not chunked":   {psState(8, 4096, 2, false), 1500, false, 0},
		"one sequence":  {psState(1, 4096, 2, true), 1500, false, 0},
		"no params":     {&learnreplay.State{Variants: []learnreplay.VariantState{{Variant: "pre"}}}, 1500, false, 0},
	} {
		for _, priced := range []float64{1000, 8000} {
			pf := &pricedFleet{}
			pf.fleet.Prefill = whatif.Prefill{PromptTokens: priced}
			pf.psByEngine(c.st, &learnreplay.PrefillState{Variant: "pre", PromptTokens: c.measured})
			assert.Equal(t, c.on, pf.fleet.Prefill.Queue.Model == whatif.QueueProcessorSharing, "%s at %g", name, priced)
			assert.Equal(t, c.slots, pf.fleet.Prefill.Queue.Slots, name)
			assert.Equal(t, c.on, len(pf.flags) == 1 && pf.flags[0] == psByEngineFlag, name)
			words := strings.Join(pf.in.Assumed, "; ")
			if c.on {
				assert.Contains(t, words, "prefill queue model processor-sharing (engine config: chunked prefill, max-num-seqs", name)
			} else {
				assert.Contains(t, words, "prefill queue model by the spread rule (engine config:", name)
			}
		}
	}
}
