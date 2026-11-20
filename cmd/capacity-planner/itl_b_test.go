package main

import (
	"context"
	"io"
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
// state B. --assume-itl-b 2ms (under the 2.5 ms floor this recording's decode
// Pods read below the band) prices it: B is 2 ms, the slope is refitted on the
// same readings, so the new line and the default one cross at the readings'
// mean fill, inside the fill they were read at; every figure is flagged
// AssumedITLB and is outside the validated region, and the ITL B row and the
// JSON say what was assumed. A B above that floor contradicts the readings and
// is not used. Before #225 no flag priced it.
func TestAssumeITLB_PricesALineOnTheDefaultIntercept(t *testing.T) {
	out, code := runCmd(t, "replicas-to-slo", "--recording", pinnedRecording, "-o", "json")
	before := role(t, decodeOutput(t, out), roleDecode)
	require.NotNil(t, before.Results.Knee.Refused, "the default intercept is refused")
	assert.Equal(t, string(whatif.PinnedDefaultB), before.Results.Knee.Refused.Reason)
	assert.Contains(t, before.Results.Knee.Refused.Detail, "--assume-itl-b", "the refusal says how to state B")
	assert.Equal(t, 3, code)
	assert.Nil(t, before.Inputs.Line.AssumedB)

	out, code = runCmd(t, "replicas-to-slo", "--recording", pinnedRecording, "--assume-itl-b", "2ms", "-o", "json")
	after := role(t, decodeOutput(t, out), roleDecode)
	knee := after.Results.Knee.Answer
	require.NotNil(t, knee, "the stated B prices the knee: %+v", after.Results.Knee.Refused)
	assert.Contains(t, knee.Flags, string(assumedITLB))
	require.NotNil(t, knee.Validation)
	assert.False(t, knee.Validation.Validated, "an assumed B is outside the validated region")
	assert.NotEmpty(t, knee.Validation.Outside)
	// The stated B is in the figures' class as stated, not only the arrivals:
	// it alone puts them outside.
	f := answerFlags{recording: pinnedRecording, cycle: "last", assumeITLB: "2ms", warn: io.Discard,
		set: map[string]bool{"assume-itl-b": true}}
	st, err := f.readState(context.Background())
	require.NoError(t, err)
	vars, err := f.decodeVariants(st)
	require.NoError(t, err)
	require.Len(t, vars, 1)
	assert.Contains(t, vars[0].class().Stated, "--assume-itl-b")
	f.assumeITLB = "12ms"
	vars, err = f.decodeVariants(st)
	require.NoError(t, err)
	assert.NotContains(t, vars[0].class().Stated, "--assume-itl-b", "not used, not stated")
	require.NotNil(t, after.Results.MeanTPOT.Answer)
	assert.Contains(t, after.Results.MeanTPOT.Answer.Flags, string(assumedITLB))
	assert.NotEqual(t, 3, code)

	line, was := after.Inputs.Line, before.Inputs.Line
	assert.InDelta(t, 0.002, line.B, 1e-12)
	assert.Equal(t, whatif.TierPinnedB, line.Tier)
	a := line.AssumedB
	require.NotNil(t, a)
	assert.Equal(t, itlBStated, a.Source)
	assert.True(t, a.Applied)
	assert.InDelta(t, was.A, a.DefaultA, 1e-12, "the slope it had on the default")
	// Both lines run through the readings' mean (itl.FitPinnedB): they cross
	// there, at the fill (0.006 - 0.002) / (A' - A).
	require.NotNil(t, line.ObservedFill)
	cross := (itl.DefaultBaselineSec - 0.002) / (line.A - was.A)
	assert.GreaterOrEqual(t, cross, line.ObservedFill.Lo)
	assert.LessOrEqual(t, cross, line.ObservedFill.Hi)
	assert.Greater(t, line.A, was.A, "a lower B, a steeper slope")

	text, _ := runCmd(t, "replicas-to-slo", "--recording", pinnedRecording, "--assume-itl-b", "2ms")
	assert.Contains(t, text, "ITL B           2.0 ms, stated, in place of the 6.0 ms default")
	assert.Contains(t, text, "AssumedITLB (2.0 ms, stated)")

	// Above the floor: not used, said so, still refused and not outside on
	// its account.
	out, _ = runCmd(t, "replicas-to-slo", "--recording", pinnedRecording, "--assume-itl-b", "12ms", "-o", "json")
	above := role(t, decodeOutput(t, out), roleDecode)
	require.NotNil(t, above.Results.Knee.Refused)
	assert.Contains(t, above.Results.Knee.Refused.Detail, "is above the floor")
	require.NotNil(t, above.Inputs.Line.AssumedB)
	assert.False(t, above.Inputs.Line.AssumedB.Applied)
	assert.NotContains(t, above.Results.Knee.Refused.Flags, string(assumedITLB))
}

// A Decode whose B the caller stated is priced even on the default
// intercept itself; without AssumedB it is refused as PinnedDefaultB.
func TestAssumeITLB_StatedDefaultIsPriced(t *testing.T) {
	d := whatif.Decode{Line: itl.Model{A: 0.04, B: itl.DefaultBaselineSec}, Band: whatif.DefaultBand(), CapacityTokens: 500000,
		KVPerRequest: 2000, OutputTokens: 500, ReplicaCap: 256, Replicas: 2, Tier: whatif.TierPinnedB}
	u, ok := whatif.KneeRate(d).(whatif.Unanswerable)
	require.True(t, ok)
	assert.Equal(t, whatif.PinnedDefaultB, u.Reason)
	d.AssumedB = true
	_, ok = whatif.KneeRate(d).(whatif.Answer)
	assert.True(t, ok, "a stated B prices")
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
	require.True(t, a.Applied, a.Why)
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
	for _, bad := range []string{"0s", "-1ms", "100ns", "2s", "12", "Floor", "nan", ""} {
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
		"ols":          {learnreplay.VariantState{Tier: whatif.TierOLS, Line: itl.Model{A: 0.04, B: 0.009}}, "not refused on the default intercept"},
		"learned B":    {learnreplay.VariantState{Tier: whatif.TierPinnedB, Line: itl.Model{A: 0.04, B: 0.009}}, "not refused on the default intercept"},
		"held":         {learnreplay.VariantState{Tier: whatif.TierHeld, Line: itl.Model{A: 0.04, B: itl.DefaultBaselineSec}}, "held line has no readings"},
		"borrowed":     {learnreplay.VariantState{Tier: whatif.TierBorrowed, Line: itl.Model{A: 0.04, B: itl.DefaultBaselineSec}}, "borrowed line has no readings"},
		"no line":      {learnreplay.VariantState{}, "no ITL line"},
		"not refitted": {learnreplay.VariantState{Tier: whatif.TierPinnedB, Line: itl.Model{A: 0.04, B: itl.DefaultBaselineSec}}, "does not reproduce"},
	} {
		pv := pricedVariant{state: c.vs}
		f.assumeIntercept(&pv)
		assert.True(t, pv.itlB.Asked, name)
		assert.False(t, pv.itlB.Applied, name)
		assert.Contains(t, pv.itlB.Why, c.why, name)
		assert.Equal(t, c.vs.Line, pv.state.Line, "%s: the line is not changed", name)
		assert.Contains(t, pv.itlBNotUsed(), "ITL B not assumed (--assume-itl-b)", name)
		assert.Empty(t, pv.itlBWords(), "%s: nothing assumed", name)
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
		Assume: &whatifapi.Assume{A: fp(0.02), ITLB: fp(0.002)}})
	require.NotNil(t, plan.Limits.Decode)
	require.NotNil(t, plan.Limits.Decode.Value, "decode prices: %+v", plan.Limits.Decode.Refused)
	assert.True(t, slices.ContainsFunc(plan.Flags, func(u whatifapi.FlagUse) bool { return u.Flag == string(assumedITLB) }), "%+v", plan.Flags)
	assert.True(t, slices.ContainsFunc(plan.Assumed, func(s string) bool { return strings.HasPrefix(s, "ITL B 2.0 ms, stated") }), "%v", plan.Assumed)
	// Prefill refused (no assume.a): decode still prices on the stated B, and
	// the plan still lists it as assumed.
	alone, _ := apiPlan(t, h, whatifapi.PlanRequest{InputsID: in.InputsID, SLO: slo, Assume: &whatifapi.Assume{ITLB: fp(0.002)}})
	require.NotNil(t, alone.Limits.Decode)
	require.NotNil(t, alone.Limits.Decode.Value, "%+v", alone.Limits.Decode.Refused)
	assert.True(t, slices.ContainsFunc(alone.Assumed, func(s string) bool { return strings.HasPrefix(s, "ITL B 2.0 ms, stated") }), "%v", alone.Assumed)
	for name, a := range map[string]whatifapi.Assume{
		"both":            {ITLB: fp(0.012), ITLBFromFloor: true},
		"too large":       {ITLB: fp(2)},
		"zero":            {ITLB: fp(0)},
		"sub-microsecond": {ITLB: fp(1e-10)},
	} {
		code, body := apiPost(t, h, "/v1/plan", whatifapi.PlanRequest{InputsID: in.InputsID, SLO: slo, Assume: &a})
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", name, body)
	}
}
