package whatifsource

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// A P/D fleet runs each role with arguments of its own. With
// PrefillEngineArgs the prefill variant is parsed from them, and the decode
// variant from EngineArgs and BatchCap; without, both take EngineArgs, as
// before.
func TestCapture_PrefillEngineArgsAreThePrefillRoles(t *testing.T) {
	decodeArgs := []string{"--tensor-parallel-size", "8", "--max-num-batched-tokens", "256"}
	prefillArgs := []string{"--tensor-parallel-size", "1", "--max-num-seqs", "4", "--max-num-batched-tokens", "8192"}
	params := func(edit func(*Options)) (dec, pre map[string]int64) {
		t.Helper()
		d := lwsFleetCapture(t, decodeRole(everyPod), prefillRole(everyPod), func(o *Options) {
			o.Engine, o.EngineArgs, o.BatchCap = inferenceengine.EngineVLLM, decodeArgs, 64
			edit(o)
		})
		require.Len(t, d.window.Variants, 2)
		of := func(i int) map[string]int64 {
			p := d.window.Variants[i].Params
			return map[string]int64{"seqs": p.MaxNumSeqs, "budget": p.MaxNumBatchedTokens, "tp": int64(p.TensorParallelSize)}
		}
		require.Equal(t, "m-decode", d.window.Variants[0].Name)
		return of(0), of(1)
	}

	dec, pre := params(func(o *Options) { o.PrefillEngineArgs = prefillArgs })
	assert.Equal(t, map[string]int64{"seqs": 64, "budget": 256, "tp": 8}, dec, "decode: --engine-args and --batch-cap")
	assert.Equal(t, map[string]int64{"seqs": 4, "budget": 8192, "tp": 1}, pre,
		"prefill: its own arguments, and --batch-cap is not added to them")

	dec, pre = params(func(*Options) {})
	assert.Equal(t, dec, pre, "without them, prefill runs the decode arguments")

	_, pre = params(func(o *Options) { o.PrefillEngineArgs = []string{} })
	assert.Equal(t, map[string]int64{"seqs": 256, "budget": 0, "tp": 1}, pre,
		"given empty, prefill runs the engine's defaults, not the decode arguments")
}
