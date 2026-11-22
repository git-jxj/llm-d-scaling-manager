package main

import (
	"encoding/json"
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// --load-aware-router is off by default and set by the flag.
func TestTTFT_LoadAwareRouterFlag(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{{nil, false}, {[]string{"--load-aware-router"}, true}, {[]string{"--load-aware-router=false"}, false}} {
		var f answerFlags
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		f.register(fs)
		require.NoError(t, fs.Parse(c.args))
		assert.Equal(t, c.want, f.loadAwareRouter, "%v", c.args)
	}
	require.False(t, whatif.DefaultPlannerLoadAwareRouter, "no recording says what the router scores")
}

// On the staircase at three prefill pods with the 1.5 floor, the flag lifts
// the prefill knee toward the pooled one, N_p V_P (1 - margin)/E[I], and the
// TTFT below the load f was read at holds; f stays 1.5 in the inputs, and the
// inputs note how it is read. Off, the knee is the held one.
func TestTTFT_LoadAwareRouterOnTheStaircase(t *testing.T) {
	read := func(extra ...string) (knee, ttft, imb float64, notes []string) {
		args := append([]string{"replicas-to-slo", "--recording", stairBOn, "--assume-vp", "400000", "--assume-a", "20ms",
			"--prefill", "3", "--imbalance-across-counts=false", "--rate", "30", "--ttft-percentile", "90", "-o", "json"}, extra...)
		out, _ := runCmd(t, args...)
		var w wireOutput
		require.NoError(t, json.Unmarshal([]byte(out), &w), out)
		var raw any
		require.NoError(t, json.Unmarshal([]byte(out), &raw))
		for _, r := range w.Roles {
			if r.Role == rolePrefill && r.TTFT != nil {
				require.NotNil(t, r.Results.TTFT, out)
				require.NotNil(t, r.Results.TTFT.Answer, out)
				k, ok := firstNumber(raw, "prefillKnee")
				require.True(t, ok, out)
				return k, r.Results.TTFT.Answer.Value, r.TTFT.Imbalance, r.TTFT.Assumed
			}
		}
		require.FailNow(t, "no prefill role", out)
		return 0, 0, 0, nil
	}
	offKnee, offTTFT, offF, offNotes := read()
	onKnee, onTTFT, onF, onNotes := read("--load-aware-router")
	t.Logf("knee %.3f -> %.3f, TTFT p90 %.4f -> %.4f s, f %.3f; %s", offKnee, onKnee, offTTFT, onTTFT, onF, notesText(onNotes))
	assert.Equal(t, whatif.ImbalanceFloor, offF)
	assert.Equal(t, offF, onF, "f in the inputs is the f below the knee")
	assert.Greater(t, onKnee, 1.2*offKnee, "the knee lifts toward the pooled one")
	assert.Equal(t, offTTFT, onTTFT, "30 r/s is below the load f was read at: f is held there")
	assert.NotContains(t, notesText(offNotes), "--load-aware-router")
	assert.Contains(t, notesText(onNotes), "--load-aware-router")
}

// firstNumber is the first number keyed name anywhere in v, depth first.
func firstNumber(v any, name string) (float64, bool) {
	switch x := v.(type) {
	case map[string]any:
		if n, ok := x[name].(float64); ok {
			return n, true
		}
		for _, c := range x {
			if n, ok := firstNumber(c, name); ok {
				return n, true
			}
		}
	case []any:
		for _, c := range x {
			if n, ok := firstNumber(c, name); ok {
				return n, true
			}
		}
	}
	return 0, false
}

// notesText is the notes, one a line.
func notesText(notes []string) string {
	return strings.Join(notes, "\n")
}
