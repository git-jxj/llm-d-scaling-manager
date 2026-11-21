package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statedCheck is the self-check of an -o json answer, read by key, so that
// the test reads the parent's output too.
type statedCheck struct {
	Status string   `json:"status"`
	Line   string   `json:"line"`
	Stated []string `json:"stated"`
	TTFT   *struct {
		Bias *float64 `json:"bias"`
	} `json:"ttft"`
	TPOT *struct {
		Steps   int            `json:"steps"`
		Refused map[string]int `json:"refused"`
	} `json:"tpot"`
}

// selfCheckOf runs args with -o json and returns the answer's self-check.
func selfCheckOf(t *testing.T, args ...string) statedCheck {
	t.Helper()
	out, code := runCmd(t, append(args, "-o", "json")...)
	require.NotEqual(t, 2, code, out)
	var doc struct {
		SelfCheck *statedCheck `json:"selfCheck"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc), out)
	require.NotNil(t, doc.SelfCheck, "no self-check")
	return *doc.SelfCheck
}

// The self-check replays the recording with the prompt spread the answer
// states: on the q32 warm-up (prompts sent at cv 0.06, no computed-prompt
// histogram, so the default cv 2) --assume-prompt-cv 0.06 moves the
// self-check's TTFT ratio with the answer's figures, and its line names
// the input. Before issue llm-d-incubation/llm-d-scaling-manager#277 the line
// was the same with and without it.
func TestSelfCheckReplaysTheStatedPromptCV(t *testing.T) {
	withSelfCheck(t)
	plain := selfCheckOf(t, selfCheckQ32...)
	cv := selfCheckOf(t, append(selfCheckQ32, "--assume-prompt-cv", "0.06")...)
	assert.Empty(t, plain.Stated)
	assert.True(t, strings.HasPrefix(plain.Line, "Self-check: tested on the last "), plain.Line)
	assert.Equal(t, []string{"--assume-prompt-cv 0.06"}, cv.Stated)
	assert.True(t, strings.HasPrefix(cv.Line, "Self-check, replayed with the stated --assume-prompt-cv 0.06 (flagged as stated): tested on the last "), cv.Line)
	require.NotNil(t, plain.TTFT)
	require.NotNil(t, cv.TTFT)
	require.NotNil(t, plain.TTFT.Bias)
	require.NotNil(t, cv.TTFT.Bias)
	assert.Less(t, *cv.TTFT.Bias, *plain.TTFT.Bias, "a narrower spread prices a lower TTFT, at the fits as in the answer")
}

// The self-check replays the recording with the ITL B the answer states, and
// names it. On the b-on staircase the decode line is pinned through the floor
// below the band (saturation.SwitchITLBaselineFromFloor, issue
// llm-d-incubation/llm-d-scaling-manager#169), so no step is refused
// PinnedDefaultB: the plain self-check scores 6 p90 TPOT steps, 5 within
// 20 %. Before the floor, every step was refused PinnedDefaultB and only the
// stated B scored them. A stated B is used only on a line still on the
// default (itl_b.go), so here it changes no step, and the self-check still
// replays and names it. TestAssumeITLB_PricesALineOnTheDefaultIntercept
// holds the stated B on a line on the default.
func TestSelfCheckReplaysTheStatedITLB(t *testing.T) {
	withSelfCheck(t)
	recording := stair("ttft-staircase-b-on")
	plain := selfCheckOf(t, "at-replicas", "--recording", recording)
	require.NotNil(t, plain.TPOT, plain.Line)
	assert.Empty(t, plain.Stated)
	assert.Zero(t, plain.TPOT.Refused["PinnedDefaultB"], "the floor's B prices the steps: %s", plain.Line)
	assert.Positive(t, plain.TPOT.Steps, plain.Line)

	sc := selfCheckOf(t, "at-replicas", "--recording", recording, "--assume-itl-b", "floor")
	assert.Equal(t, []string{"--assume-itl-b floor"}, sc.Stated)
	assert.True(t, strings.HasPrefix(sc.Line, "Self-check, replayed with the stated --assume-itl-b floor (flagged as stated, outside the validated region): "), sc.Line)
	require.NotNil(t, sc.TPOT, sc.Line)
	assert.Zero(t, sc.TPOT.Refused["PinnedDefaultB"], sc.Line)
	assert.Equal(t, plain.TPOT.Steps, sc.TPOT.Steps, "no line is on the default, so the stated B moves no step: %s", sc.Line)
}
