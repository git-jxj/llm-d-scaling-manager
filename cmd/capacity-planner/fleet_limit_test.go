package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// With no TPOT asked, decode's knee is all that bounds decode. A recording
// whose decode line is pinned on the default intercept (ttft-three-pods) or
// missing (ttft-saturated) prices no knee, so decode's limit and the fleet's
// are that refusal, never prefill's rate alone: slo-to-rate refuses both rows
// and exits 3, and /v1/plan refuses limits.decode, limits.fleet (with and
// without headroom) and both +1 steps, while the prefill limit still prices.
// Before, each fleet figure was the prefill limit, read as bounded.
func TestFleetLimitRefusesAnUnpricedDecodeKnee(t *testing.T) {
	for name, c := range map[string]struct{ recording, reason string }{
		"pinned line": {"ttft-three-pods", "PinnedDefaultB"},
		"no line":     {"ttft-saturated", "NoLine"},
	} {
		path := stair(c.recording)
		t.Run(name+"/slo-to-rate", func(t *testing.T) {
			out, code := runCmd(t, "slo-to-rate", "--recording", path, "--assume-a", "20ms", "--ttft", "2s@p90", "-o", "json")
			w := decodeOutput(t, out)
			fleet := role(t, w, "fleet").Results.Rate
			require.NotNil(t, fleet.Refused, "the fleet limit is prefill's alone: %+v", fleet.Answer)
			assert.Equal(t, c.reason, fleet.Refused.Reason)
			decode := role(t, w, roleDecode).Results.Rate
			require.NotNil(t, decode.Refused)
			assert.Equal(t, c.reason, decode.Refused.Reason)
			assert.NotNil(t, role(t, w, rolePrefill).Results.Rate.Answer, "the prefill limit still prices")
			assert.Equal(t, 3, code)
			text, _ := runCmd(t, "slo-to-rate", "--recording", path, "--assume-a", "20ms", "--ttft", "2s@p90")
			assert.Contains(t, text, "fleet    refused: "+c.reason)
		})
		t.Run(name+"/plan", func(t *testing.T) {
			h := servedOne(t, path)
			in, _ := apiInputs(t, h, "run.jsonl.gz", "")
			plan, _ := apiPlan(t, h, whatifapi.PlanRequest{InputsID: in.InputsID, Assume: &whatifapi.Assume{A: fp(0.02)},
				SLO: &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p90", Target: fp(2)},
					TPOT: &whatifapi.Objective{Statistic: "none"}}})
			l := plan.Limits
			for what, f := range map[string]whatifapi.Figure{"fleet": l.Fleet.Figure, "without headroom": l.Fleet.WithoutHeadroom,
				"+1 prefill": plan.Marginal.PrefillPlusOne.Limit, "+1 decode": plan.Marginal.DecodePlusOne.Limit} {
				if f.Value != nil {
					t.Errorf("%s: %.4g r/s, prefill's limit alone", what, *f.Value)
					continue
				}
				if assert.NotNil(t, f.Refused, what) {
					assert.Equal(t, c.reason, f.Refused.Reason, what)
				}
			}
			require.NotNil(t, l.Decode, "decode's limit is the knee's refusal")
			require.NotNil(t, l.Decode.Refused)
			assert.Equal(t, c.reason, l.Decode.Refused.Reason)
			assert.NotNil(t, l.Prefill.Value, "the prefill limit still prices: %+v", l.Prefill)
			assert.Equal(t, []string{roleDecode}, l.Fleet.Binding)
			assert.Nil(t, l.Chosen.Headroom.Value, "no headroom under a refused limit")
		})
	}
}

// The same question on a recording whose decode line prices: no decode row
// and no decode limit, and the fleet's limit is answered.
func TestFleetLimitWithAPricedDecodeKnee(t *testing.T) {
	out, code := runCmd(t, workedTTFT("slo-to-rate", "--ttft", "1s@p90", "-o", "json")...)
	require.Equal(t, 0, code, out)
	w := decodeOutput(t, out)
	for _, r := range w.Roles {
		assert.NotEqual(t, roleDecode, r.Role, "no TPOT asked and the knee priced: no decode row")
	}
	assert.NotNil(t, role(t, w, "fleet").Results.Rate.Answer)
}

// servedOne is the API on a directory holding one recording, as run.jsonl.gz.
func servedOne(t *testing.T, recording string) http.Handler {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile(recording)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "run.jsonl.gz"), b, 0o644))
	src := &servedSources{recordings: dir, warn: io.Discard}
	return whatifapi.New(whatifapi.Config{Resolver: src, WhatifSchema: "schema-v3"}).Handler()
}
