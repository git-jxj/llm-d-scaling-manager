package main

import (
	"io"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Given, even empty, --prefill-engine-args reaches the capture; not given,
// the prefill role takes --engine-args.
func TestCapture_PrefillEngineArgsReachTheCapture(t *testing.T) {
	options := func(extra ...string) []string {
		t.Helper()
		fs := newFlags("at-replicas", io.Discard)
		f := answerFlags{question: "at-replicas", warn: io.Discard}
		f.register(fs)
		ok, err := parse(fs, slices.Concat(sweepWindow("http://prom.example:9090"), extra))
		require.True(t, ok)
		require.NoError(t, err)
		require.NoError(t, f.check(fs))
		opts, err := f.windowOptions()
		require.NoError(t, err)
		return opts.PrefillEngineArgs
	}
	assert.Nil(t, options(), "not given: the prefill role takes --engine-args")
	assert.Equal(t, []string{"--max-num-seqs", "4", "--max-num-batched-tokens", "8192"},
		options("--prefill-engine-args", "--max-num-seqs 4 --max-num-batched-tokens 8192"))
	got := options("--prefill-engine-args", "")
	assert.NotNil(t, got, "given empty: a prefill engine run with no arguments")
	assert.Empty(t, got)
}
