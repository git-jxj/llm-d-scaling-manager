package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The prefill role's engine arguments are a capture flag of their own, so
// every command that captures a window takes them: capture, and with
// --prometheus at-replicas, replicas-to-slo, slo-to-rate, day-plan and
// serve. From a recording it is refused: the arguments were the capture's.
func TestCapture_PrefillEngineArgsFlag(t *testing.T) {
	for _, cmd := range []string{"capture", "at-replicas", "replicas-to-slo", "slo-to-rate", "day-plan", "serve"} {
		text, code := runCmd(t, cmd, "-h")
		require.Equal(t, 0, code, cmd)
		assert.Regexp(t, "(?m)^  -prefill-engine-args( |$)", text, "%s takes --prefill-engine-args", cmd)
	}
	_, code := runCmd(t, "at-replicas", "--recording", "x.jsonl", "--prefill-engine-args", "--max-num-seqs 4")
	assert.Equal(t, 2, code)
}
