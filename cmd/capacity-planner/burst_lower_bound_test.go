package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// On the synthetic lower-bound fleet (lowerBoundRecording) the prefill knee
// rests on a lower bound of V_P, so "a burst near the knee" is not known
// there: the steps whose windows burst past that knee, and not past
// decode's, are judged, with their TPOT scored and their TTFT not
// (UnscoredBurst); before, the whole step was excluded. None of those TTFT
// figures is scored, so none can read under what was measured.
func TestBacktest_ABurstPastALowerBoundKneeWithholdsOnlyTheTTFT(t *testing.T) {
	rec := lowerBoundRecording(t)
	out, _ := runCmd(t, "backtest", "--recording", rec, "--split", "50%", "-o", "json")
	var bt whatifapi.BacktestResponse
	require.NoError(t, json.Unmarshal([]byte(out), &bt))
	bursts := 0
	for _, st := range bt.Steps {
		assert.NotEqual(t, whatifapi.ExcludedBurst, st.Excluded, "%s: a burst against a lower-bound knee excludes no step", st.Start)
		if !st.TTFTBurst {
			continue
		}
		bursts++
		assert.Empty(t, st.Excluded, st.Start)
		for _, p := range st.TTFT {
			assert.Nil(t, p.Ratio, "%s %s: no TTFT scored in a burst", st.Start, p.Statistic)
			if p.Predicted.Value != nil && p.Measured != nil {
				assert.Equal(t, whatifapi.UnscoredBurst, p.Unscored, "%s %s", st.Start, p.Statistic)
			}
		}
		scored := 0
		for _, p := range st.TPOT {
			if p.Ratio != nil {
				scored++
			}
		}
		assert.Positive(t, scored, "%s: the step's TPOT is scored", st.Start)
	}
	require.Positive(t, bursts, "the synthetic fleet has burst steps past its lower-bound knee")
}
