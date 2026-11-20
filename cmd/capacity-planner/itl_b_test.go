package main

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// pinnedRecording is a staircase recording whose decode line is pinned-B on
// the default intercept at its last cycle (PinnedDefaultB).
var pinnedRecording = stair("ttft-three-pods")

// A line on the default intercept is refused, and the refusal says how to
// state B. --assume-itl-b 12ms prices it: B is 12 ms, the slope is refitted
// on the same readings, so the new line and the default one cross at the
// readings' mean fill, inside the fill they were read at; every figure is
// flagged AssumedITLB and is outside the validated region, and the ITL B row
// and the JSON say what was assumed. Before #225 no flag priced it.
func TestAssumeITLB_PricesALineOnTheDefaultIntercept(t *testing.T) {
	out, code := runCmd(t, "replicas-to-slo", "--recording", pinnedRecording, "-o", "json")
	before := role(t, decodeOutput(t, out), roleDecode)
	require.NotNil(t, before.Results.Knee.Refused, "the default intercept is refused")
	assert.Equal(t, string(whatif.PinnedDefaultB), before.Results.Knee.Refused.Reason)
	assert.Contains(t, before.Results.Knee.Refused.Detail, "--assume-itl-b", "the refusal says how to state B")
	assert.Equal(t, 3, code)
	assert.Nil(t, before.Inputs.Line.AssumedB)

	out, code = runCmd(t, "replicas-to-slo", "--recording", pinnedRecording, "--assume-itl-b", "12ms", "-o", "json")
	after := role(t, decodeOutput(t, out), roleDecode)
	knee := after.Results.Knee.Answer
	require.NotNil(t, knee, "the stated B prices the knee: %+v", after.Results.Knee.Refused)
	assert.Contains(t, knee.Flags, string(assumedITLB))
	require.NotNil(t, knee.Validation)
	assert.False(t, knee.Validation.Validated, "an assumed B is outside the validated region")
	require.NotNil(t, after.Results.MeanTPOT.Answer)
	assert.Contains(t, after.Results.MeanTPOT.Answer.Flags, string(assumedITLB))
	assert.NotEqual(t, 3, code)

	line, was := after.Inputs.Line, before.Inputs.Line
	assert.InDelta(t, 0.012, line.B, 1e-12)
	assert.Equal(t, whatif.TierPinnedB, line.Tier)
	a := line.AssumedB
	require.NotNil(t, a)
	assert.Equal(t, itlBStated, a.Source)
	assert.True(t, a.Applied)
	assert.InDelta(t, was.A, a.DefaultA, 1e-12, "the slope it had on the default")
	// Both lines run through the readings' mean (itl.FitPinnedB): they cross
	// there, at the fill (0.006 - 0.012) / (A' - A).
	require.NotNil(t, line.ObservedFill)
	cross := (itl.DefaultBaselineSec - 0.012) / (line.A - was.A)
	assert.GreaterOrEqual(t, cross, line.ObservedFill.Lo)
	assert.LessOrEqual(t, cross, line.ObservedFill.Hi)
	assert.Less(t, line.A, was.A, "a higher B, a lower slope")

	text, _ := runCmd(t, "replicas-to-slo", "--recording", pinnedRecording, "--assume-itl-b", "12ms")
	assert.Contains(t, text, "ITL B           ITL B 12.0 ms, stated, in place of the 6 ms default")
	assert.Contains(t, text, "AssumedITLB (12.0 ms, stated)")

	// Stating the default itself is a statement too: priced on the same line,
	// flagged, not refused as an unmeasured B.
	out, _ = runCmd(t, "replicas-to-slo", "--recording", pinnedRecording, "--assume-itl-b", "6ms", "-o", "json")
	same := role(t, decodeOutput(t, out), roleDecode)
	require.NotNil(t, same.Results.Knee.Answer, "a stated 6 ms prices: %+v", same.Results.Knee.Refused)
	assert.Contains(t, same.Results.Knee.Answer.Flags, string(assumedITLB))
	assert.InDelta(t, was.A, same.Inputs.Line.A, 1e-12, "the line on the default, refitted through it, is itself")
}

// --assume-itl-b floor takes B from the floor read below the band, through
// the readings' mean (itl.FloorIntercept), and says what it read; where the
// recording has no floor it says why and the line stays refused.
func TestAssumeITLB_FloorSaysWhatItRead(t *testing.T) {
	out, _ := runCmd(t, "replicas-to-slo", "--recording", pinnedRecording, "--assume-itl-b", "floor", "-o", "json")
	r := role(t, decodeOutput(t, out), roleDecode)
	a := r.Inputs.Line.AssumedB
	require.NotNil(t, a)
	assert.Equal(t, itlBFromFloor, a.Source)
	if !a.Applied {
		assert.NotEmpty(t, a.Why)
		require.NotNil(t, r.Results.Knee.Refused)
		assert.Contains(t, r.Results.Knee.Refused.Detail, a.Why)
		return
	}
	assert.Positive(t, a.FloorSeconds)
	assert.Positive(t, a.FloorFill)
	assert.Greater(t, a.FloorSeconds, a.Seconds, "B is under the floor it was read through")
	assert.InDelta(t, a.Seconds, r.Inputs.Line.B, 1e-12)
	require.NotNil(t, r.Results.Knee.Answer)
	assert.Contains(t, r.Results.Knee.Answer.Flags, string(assumedITLB))
}

// The flag's values: a duration above 0 and at most a second, or floor.
func TestAssumeITLB_Parse(t *testing.T) {
	floor, b, err := parseAssumeITLB("floor")
	require.NoError(t, err)
	assert.True(t, floor)
	assert.Zero(t, b)
	_, b, err = parseAssumeITLB("12ms")
	require.NoError(t, err)
	assert.Equal(t, "12ms", b.String())
	for _, bad := range []string{"0s", "-1ms", "2s", "12", "Floor", "nan"} {
		_, _, err := parseAssumeITLB(bad)
		assert.Error(t, err, bad)
	}
	_, code := runCmd(t, "replicas-to-slo", "--recording", pinnedRecording, "--assume-itl-b", "12")
	assert.Equal(t, 2, code, "a bare number is a usage error")
}

// A stated B touches only a line on the default intercept: a measured B is
// priced as it is, and a held or borrowed line, with no readings here to
// refit, stays refused; each says why, and the line is not changed.
func TestAssumeITLB_LeavesOtherLines(t *testing.T) {
	f := &answerFlags{assumeITLB: "12ms"}
	for name, c := range map[string]struct {
		vs  learnreplay.VariantState
		why string
	}{
		"ols":          {learnreplay.VariantState{Tier: whatif.TierOLS, Line: itl.Model{A: 0.04, B: 0.009}}, "was measured"},
		"learned B":    {learnreplay.VariantState{Tier: whatif.TierPinnedB, Line: itl.Model{A: 0.04, B: 0.009}}, "was measured"},
		"held":         {learnreplay.VariantState{Tier: whatif.TierHeld, Line: itl.Model{A: 0.04, B: itl.DefaultBaselineSec}}, "held line has no readings"},
		"borrowed":     {learnreplay.VariantState{Tier: whatif.TierBorrowed, Line: itl.Model{A: 0.04, B: itl.DefaultBaselineSec}}, "borrowed line has no readings"},
		"no line":      {learnreplay.VariantState{}, "no ITL line"},
		"not refitted": {learnreplay.VariantState{Tier: whatif.TierPinnedB, Line: itl.Model{A: 0.04, B: itl.DefaultBaselineSec}}, "no readings"},
	} {
		pv := pricedVariant{state: c.vs}
		f.assumeIntercept(&pv)
		assert.True(t, pv.itlB.Asked, name)
		assert.False(t, pv.itlB.Applied, name)
		assert.Contains(t, pv.itlB.Why, c.why, name)
		assert.Equal(t, c.vs.Line, pv.state.Line, "%s: the line is not changed", name)
		assert.Contains(t, pv.itlBWords(), "ITL B not assumed (--assume-itl-b)", name)
	}
	floor := &answerFlags{assumeITLB: "floor"}
	pv := pricedVariant{state: learnreplay.VariantState{Tier: whatif.TierPinnedB, Line: itl.Model{A: 0.04, B: itl.DefaultBaselineSec},
		Refitted: true, LineReadings: []itl.Observation{{K: 0.5, ITLSec: 0.026}}}}
	floor.assumeIntercept(&pv)
	assert.False(t, pv.itlB.Applied)
	assert.Contains(t, pv.itlB.Why, "no floor below the band")
}

// The API states B as assume.itlB (seconds) or assume.itlBFromFloor, as the
// CLI's flag: the plan prices decode, flags it and lists it as assumed; both
// at once, or a B past a second, is a 400. The inputs name the B the floor
// gives where the line is refused.
func TestAssumeITLB_API(t *testing.T) {
	h := servedOne(t, pinnedRecording)
	in, _ := apiInputs(t, h, "run.jsonl.gz", "")
	i := slices.IndexFunc(in.Terms, func(t whatifapi.Term) bool { return t.Name == "itlLine" })
	require.GreaterOrEqual(t, i, 0, "the line is refused at the inputs")
	require.NotNil(t, in.Terms[i].Refused)
	assert.Contains(t, in.Terms[i].Refused.Detail, "assume.itlB", "in the API's words")
	slo := &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p90", Target: fp(2)},
		TPOT: &whatifapi.Objective{Statistic: "mean", Target: fp(0.05)}}
	plan, _ := apiPlan(t, h, whatifapi.PlanRequest{InputsID: in.InputsID, SLO: slo,
		Assume: &whatifapi.Assume{A: fp(0.02), ITLB: fp(0.012)}})
	require.NotNil(t, plan.Limits.Decode)
	require.NotNil(t, plan.Limits.Decode.Value, "decode prices: %+v", plan.Limits.Decode.Refused)
	assert.True(t, slices.ContainsFunc(plan.Flags, func(u whatifapi.FlagUse) bool { return u.Flag == string(assumedITLB) }), "%+v", plan.Flags)
	assert.True(t, slices.ContainsFunc(plan.Assumed, func(s string) bool { return strings.HasPrefix(s, "ITL B 12.0 ms, stated") }), "%v", plan.Assumed)
	for name, a := range map[string]whatifapi.Assume{
		"both":      {ITLB: fp(0.012), ITLBFromFloor: true},
		"too large": {ITLB: fp(2)},
		"zero":      {ITLB: fp(0)},
	} {
		code, body := apiPost(t, h, "/v1/plan", whatifapi.PlanRequest{InputsID: in.InputsID, SLO: slo, Assume: &a})
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", name, body)
	}
}
