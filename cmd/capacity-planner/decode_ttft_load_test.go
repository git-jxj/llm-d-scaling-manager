package main

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// stairGolden is a staircase golden's recording.
func stairGolden(phase string) string {
	return "../../internal/engines/analyzers/learnreplay/testdata/goldens/ttft-staircase-" + phase + "/recording.jsonl.gz"
}

// stairFloorArgs price a 2 s TTFT with V_P and a stated, so the floor is
// what the decode side sets.
var stairFloorArgs = []string{"--ttft", "2s", "--tpot", "100ms", "--assume-vp", "200000", "--assume-a", "20ms", "-o", "json"}

// stairPrefill is the prefill role of slo-to-rate on a staircase phase at a
// cycle ("" the last).
func stairPrefill(t *testing.T, phase, cycle string) wireRole {
	t.Helper()
	args := append([]string{"slo-to-rate", "--recording", stairGolden(phase)}, stairFloorArgs...)
	if cycle != "" {
		args = append(args, "--cycle", cycle)
	}
	out, _ := runCmd(t, args...)
	for _, r := range decodeOutput(t, out).Roles {
		if r.Role == "prefill" {
			require.NotNil(t, r.TTFT, out)
			require.NotNil(t, r.Results.Rate, out)
			return r
		}
	}
	t.Fatalf("no prefill role:\n%s", out)
	return wireRole{}
}

// b-off and c-on end in a collapse: decode queued hundreds of requests and
// its engine TTFT read 16.6 s and 9.0 s. Their decode line is refused
// (PinnedDefaultB), so the decode side is that measured TTFT, and held at
// every rate it set the zero-load floor at 16.6 s and 9.1 s, where the client
// read 0.12 to 0.18 s at the lowest step. At the last cycle the planner
// refuses it and names the latest calm cycle; at cycle 240, before the
// collapse, the floor is under 0.3 s.
func TestDecodeTTFTUnderLoad_StaircaseCollapse(t *testing.T) {
	for _, c := range []struct {
		phase   string
		waiting string
		calm    int
	}{
		{"b-off", "16.6 s, 283 requests waiting", 249},
		{"c-on", "9.0 s, 136 requests waiting", 249},
	} {
		t.Run(c.phase, func(t *testing.T) {
			last := stairPrefill(t, c.phase, "")
			require.NotNil(t, last.Results.Rate.Refused, "the last cycle is answered")
			assert.Equal(t, string(whatif.DecodeTTFTUnderLoad), last.Results.Rate.Refused.Reason)
			assert.Contains(t, last.Results.Rate.Refused.Detail, c.waiting)
			assert.Contains(t, last.Results.Rate.Refused.Detail, fmt.Sprintf("--cycle %d", c.calm))
			assert.Equal(t, c.calm, last.TTFT.DecodeTTFTCalmCycle)
			assert.Equal(t, "decode engine", last.TTFT.DecodeSide)

			for _, cycle := range []string{"240", fmt.Sprint(c.calm)} {
				r := stairPrefill(t, c.phase, cycle)
				require.Nil(t, r.Results.Rate.Refused, "cycle %s", cycle)
				require.NotNil(t, r.Results.Rate.Answer, "cycle %s", cycle)
				assert.Zero(t, r.TTFT.DecodeTTFTCalmCycle, "cycle %s", cycle)
				if cycle == "240" {
					assert.Less(t, r.Results.Rate.Answer.Basis.Floor, 0.3, "the floor at cycle 240")
				}
			}
		})
	}
}

// b-on held its last step: its decode line prices, so the check never reads.
func TestDecodeTTFTUnderLoad_LinePricedIsUntouched(t *testing.T) {
	r := stairPrefill(t, "b-on", "")
	require.NotNil(t, r.Results.Rate.Answer)
	assert.Equal(t, "line", r.TTFT.DecodeSide)
	assert.Less(t, r.Results.Rate.Answer.Basis.Floor, 0.3)
}

// The negative control: without the check the collapse's queue is the floor,
// 9 s and more, and a 2 s target is refused as below it.
func TestDecodeTTFTUnderLoad_WithoutTheCheck(t *testing.T) {
	checkDecodeTTFTLoad = false
	t.Cleanup(func() { checkDecodeTTFTLoad = true })
	for _, phase := range []string{"b-off", "c-on"} {
		r := stairPrefill(t, phase, "")
		require.NotNil(t, r.Results.Rate.Refused, phase)
		assert.Equal(t, string(whatif.TargetBelowFloor), r.Results.Rate.Refused.Reason, phase)
		assert.Greater(t, r.TTFT.DecodeTTFTSeconds, 9.0, phase)
		assert.Zero(t, r.TTFT.DecodeTTFTCalmCycle, phase)
	}
}
