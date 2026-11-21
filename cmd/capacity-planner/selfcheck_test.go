package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
// p90-low rule: weak, not inconsistent, so the verdict is not called
// untrusted, only not covered (it is at a rate the check did not test),
// and no issue is offered. (Each run replays the q32 warm-up: two
// runs, not more; the issue's privacy is TestSelfCheck_IssueHoldsNoName's.)
func TestSelfCheckGolden(t *testing.T) {
	withSelfCheck(t)
	checkGolden(t, "at-replicas-q32-self-check", 0, selfCheckQ32)
	out, code := runCmd(t, append(selfCheckQ32, "-o", "json")...)
	require.Equal(t, 0, code, out)
	var rep whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	sc := rep.SelfCheck
	require.NotNil(t, sc)
	assert.Equal(t, whatifapi.SelfCheckWeak, sc.Status, sc.Line)
	assert.Empty(t, sc.Untrusted)
	assert.Empty(t, sc.IssueURL)
	require.NotNil(t, sc.TTFT)
	assert.Equal(t, "mean", sc.TTFT.Statistic)
	assert.False(t, slices.ContainsFunc(sc.Checks, func(r whatifapi.SelfCheckRule) bool { return !r.Passed }), "%+v", sc.Checks)
	require.NotEmpty(t, rep.Shapes)
	assert.True(t, strings.HasPrefix(rep.Shapes[0].Verdict.Trust, whatifapi.TrustNotCovered+": "), rep.Shapes[0].Verdict.Trust)

	// --no-self-check: no self-check, in either output.
	out, code = runCmd(t, append(selfCheckQ32, "-o", "json", "--no-self-check")...)
	require.Equal(t, 0, code, out)
	var off whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(out), &off))
	assert.Nil(t, off.SelfCheck)
}
