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
// states, and its line names the input. Before issue
// llm-d-incubation/llm-d-scaling-manager#277 the line was the same with and
// without it.
//
// The self-check scores the mean (#291). On the q32 warm-up each step reads
// its own prompt cv, 0.3 to 0.45, from the prefill pods' histogram (the
// answer's default cv 2 is not the steps'). At a cv past
// whatif.MaxPSPromptCV the larger of processor sharing's and first come's
// mean is priced; first come's lies above processor sharing's only past
// cv 1, and processor sharing's mean does not depend on the spread. So
// --assume-prompt-cv 0.06 leaves the mean bias where it was (both read
// 1.6890; it moves the percentiles), and the stated cv is shown to reach
// the replay's pricing by a cv past 1: --assume-prompt-cv 3 prices first
// come's larger mean on every step, and the bias rises.
func TestSelfCheckReplaysTheStatedPromptCV(t *testing.T) {
	withSelfCheck(t)
	plain := selfCheckOf(t, selfCheckQ32...)
	narrow := selfCheckOf(t, append(selfCheckQ32, "--assume-prompt-cv", "0.06")...)
	wide := selfCheckOf(t, append(selfCheckQ32, "--assume-prompt-cv", "3")...)
	assert.Empty(t, plain.Stated)
	assert.True(t, strings.HasPrefix(plain.Line, "Self-check: tested on the last "), plain.Line)
	assert.Equal(t, []string{"--assume-prompt-cv 0.06"}, narrow.Stated)
	assert.True(t, strings.HasPrefix(narrow.Line, "Self-check, replayed with the stated --assume-prompt-cv 0.06 (flagged as stated): tested on the last "), narrow.Line)
	assert.Equal(t, []string{"--assume-prompt-cv 3"}, wide.Stated)
	for _, sc := range []statedCheck{plain, narrow, wide} {
		require.NotNil(t, sc.TTFT, sc.Line)
		require.NotNil(t, sc.TTFT.Bias, sc.Line)
	}
	assert.InDelta(t, *plain.TTFT.Bias, *narrow.TTFT.Bias, 1e-9, "below cv 1 the priced mean is processor sharing's, which the spread does not move")
	assert.Greater(t, *wide.TTFT.Bias, *plain.TTFT.Bias, "the stated cv 3 reaches the replay's pricing: first come's mean, above processor sharing's")
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
