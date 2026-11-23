package main

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// vpPlateauArgs prices the staircase's b-on fleet, whose prefill pods never
// queued (V_P refused), with a stated a and targets the fleet can meet, so
// that only V_P decides whether the prefill side prices.
var vpPlateauArgs = []string{"--recording", stairBOn, "--assume-a", "20ms", "--ttft", "2s@p90"}

// --prefill-vp-plateau on at-replicas: the b-on fleet, refused V_P without
// it, prices TTFT from the plateau's lower bound, at least (#280), flagged
// VPFromPlateau, with the note. Without the flag nothing moves: the other
// goldens hold that.
func TestVPPlateau_AtReplicasGolden(t *testing.T) {
	checkGolden(t, "at-replicas-vp-plateau", 0,
		slices.Concat([]string{"at-replicas"}, vpPlateauArgs, []string{"--tpot", "30ms", "--" + vpPlateauFlag}))
}

// Off by default: V_P is refused on b-on, and no figure says plateau. On,
// V_P is learned from the plateau, a lower bound there, and every figure
// priced from it carries VPFromPlateau, reads OUTSIDE VALIDATED REGION by
// it, and the answer's notes and footer say where V_P came from.
func TestVPPlateau_OffByDefaultOnWhenAsked(t *testing.T) {
	prefillRole := func(t *testing.T, w wireOutput) wireRole {
		t.Helper()
		i := slices.IndexFunc(w.Roles, func(r wireRole) bool { return r.Role == rolePrefill })
		require.GreaterOrEqual(t, i, 0, "no prefill role")
		require.NotNil(t, w.Roles[i].TTFT, "the prefill role has no TTFT inputs")
		return w.Roles[i]
	}

	off, code := runCmd(t, slices.Concat([]string{"slo-to-rate", "-o", "json"}, vpPlateauArgs)...)
	assert.NotContains(t, off, string(whatif.VPFromPlateau))
	assert.NotContains(t, off, string(prefill.SourceOccupancyPlateau))
	assert.NotContains(t, off, vpPlateauFlag)
	p := prefillRole(t, decodeOutput(t, off))
	require.NotNil(t, p.Results.Rate)
	assert.Nil(t, p.Results.Rate.Answer, "off, b-on's V_P is refused (exit %d)", code)

	on, code := runCmd(t, slices.Concat([]string{"slo-to-rate", "-o", "json", "--" + vpPlateauFlag}, vpPlateauArgs)...)
	require.Equal(t, 0, code, on)
	w := decodeOutput(t, on)
	p = prefillRole(t, w)
	assert.Equal(t, string(prefill.SourceOccupancyPlateau), p.TTFT.VPOrigin)
	assert.Equal(t, sourceLearned, p.TTFT.VPSource)
	assert.Positive(t, p.TTFT.VP)
	assert.Contains(t, p.TTFT.Assumed, vpPlateauNote)
	for _, r := range w.Roles {
		if r.Results.Rate == nil || r.Results.Rate.Answer == nil {
			continue
		}
		a := r.Results.Rate.Answer
		assert.Contains(t, a.Flags, string(whatif.VPFromPlateau), "role %s", r.Role)
		// b-on's plateau has not flattened: a lower bound, as #280 reads it.
		assert.Contains(t, a.Flags, string(whatif.VPLowerBound), "role %s", r.Role)
		require.NotNil(t, a.Validation, "role %s", r.Role)
		assert.False(t, a.Validation.Validated, "role %s", r.Role)
		assert.True(t, strings.HasPrefix(a.Validation.Label, "OUTSIDE VALIDATED REGION (prefill speed from the occupancy plateau"),
			"role %s: %s", r.Role, a.Validation.Label)
	}
	assert.Contains(t, w.Note, "--"+vpPlateauFlag)
}

// Every command that replays a recording takes the flag, with one help.
func TestVPPlateau_EveryReplayingCommandTakesIt(t *testing.T) {
	for _, cmd := range []string{"at-replicas", "slo-to-rate", "replicas-to-slo", "backtest", "predict", "day-plan"} {
		out, _ := runCmd(t, cmd, "-h")
		assert.Contains(t, out, "-"+vpPlateauFlag, cmd)
		assert.Contains(t, out, "experimental", cmd)
		assert.Contains(t, out, "never queues prefill", cmd)
	}
}

// The server reads the request's prefillVPPlateau, and keeps its inputs
// apart from the default's; backtest, predict and day-plan resolve through
// it with their flag.
func TestVPPlateau_TheServerAndTheResolvingCommandsReadIt(t *testing.T) {
	ctx := context.Background()
	src := &servedSources{warn: io.Discard, recordings: filepath.Dir(stairBOn)}
	req := whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(stairBOn)}}
	off, err := src.Resolve(ctx, req)
	require.NoError(t, err)
	req.PrefillVPPlateau = true
	on, err := src.Resolve(ctx, req)
	require.NoError(t, err)
	assert.False(t, off.Inputs.PrefillVPPlateau)
	assert.NotContains(t, off.Inputs.Assumed, vpPlateauNote)
	assert.True(t, on.Inputs.PrefillVPPlateau)
	assert.Contains(t, on.Inputs.Assumed, vpPlateauNote)
	assert.NotEqual(t, off.Content, on.Content, "the plateau's inputs share the default's id")

	bt := backtestFlags{vpPlateau: true}
	res, err := bt.resolve(ctx, stairBOn, 0, io.Discard)
	require.NoError(t, err)
	assert.True(t, res.Inputs.PrefillVPPlateau, "backtest and predict")
	dp := dayPlanFlags{recording: stairBOn, vpPlateau: true, noSelfCheck: true}
	res, err = dp.resolve(ctx, io.Discard)
	require.NoError(t, err)
	assert.True(t, res.Inputs.PrefillVPPlateau, "day-plan")
}
