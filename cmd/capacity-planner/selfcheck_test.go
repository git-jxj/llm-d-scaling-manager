package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// withSelfCheck turns the self-check on, as it is outside the tests, with a
// fixed release.
func withSelfCheck(t *testing.T) {
	t.Helper()
	defaultSelfCheck, selfCheckVersion = true, func() string { return "vtest" }
	t.Cleanup(func() { defaultSelfCheck, selfCheckVersion = false, plannerVersion })
}

// selfCheckQ32 is at-replicas on the q32 staircase's warm-up recording.
var selfCheckQ32 = []string{"at-replicas", "--recording", q32Recording, "--ttft", "2s@p90"}

// The answer carries its self-check: the line, why, and what to trust,
// ahead of the answer. The JSON answer carries it too. On the q32 warm-up
// the mean TTFT reads within 20% in 8 of 11 steps (median ratio 1.18) and
// its p90, scored in 5 steps (2 more fall in a bucket too wide to place
// it, 4 hold too few requests), reads low in none and is too few for the
// p90-low rule: weak, not inconsistent, and no issue is offered. (Each
// run replays the q32 warm-up: two runs, not more; the issue's privacy is
// internal/whatifapi's TestSelfCheck_IssueHoldsNoName.)
func TestSelfCheckGolden(t *testing.T) {
	withSelfCheck(t)
	// The load is the warm-up's 1000/1000 phase, the shape priced its last
	// cycles' 8k/26: of different traffic, so no verdict is given (#297;
	// at its own shape the window's 2 + 2 ran at 0.8x the knee, not past
	// it) and none is judged for trust (#298; TestReport_JudgeTrust holds
	// the exit 4).
	checkGolden(t, "at-replicas-q32-self-check", whatif.ExitRefused, selfCheckQ32)
	out, code := runCmd(t, append(selfCheckQ32, "-o", "json")...)
	require.Equal(t, whatif.ExitRefused, code, out)
	var rep whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	assert.Equal(t, whatif.ExitRefused, rep.ExitCode)
	sc := rep.SelfCheck
	require.NotNil(t, sc)
	assert.Equal(t, whatifapi.SelfCheckWeak, sc.Status, sc.Line)
	assert.Empty(t, sc.Untrusted)
	assert.Empty(t, sc.IssueURL)
	require.NotNil(t, sc.TTFT)
	assert.Equal(t, "mean", sc.TTFT.Statistic)
	assert.False(t, slices.ContainsFunc(sc.Checks, func(r whatifapi.SelfCheckRule) bool { return !r.Passed }), "%+v", sc.Checks)
	require.NotEmpty(t, rep.Shapes)
	assert.Equal(t, whatifapi.VerdictNotJudged, rep.Shapes[0].Verdict.Verdict)
	assert.Contains(t, rep.Shapes[0].Verdict.Summary, phasesWords)
	assert.NotContains(t, rep.Shapes[0].Verdict.Summary, contradictsWords, "its hot cycles are not at the shape priced")
	assert.Empty(t, rep.Shapes[0].Verdict.Trust, "a verdict not given is not judged for trust")

	// --no-self-check: no self-check, in either output.
	out, code = runCmd(t, append(selfCheckQ32, "-o", "json", "--no-self-check")...)
	require.Equal(t, whatif.ExitRefused, code, out)
	var off whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(out), &off))
	assert.Nil(t, off.SelfCheck)
}
