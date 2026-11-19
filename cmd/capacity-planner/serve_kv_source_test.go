package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// muPaths is the synthetic mu-paths golden: its rows hold 2,500 KV tokens a
// request against the 1,050 its context says, so its decode KV is priced as
// measured (and marked above the context), not floored, and a stated shape
// scales it.
const muPaths = "../../internal/engines/analyzers/learnreplay/testdata/goldens/mu-paths/recording.jsonl.gz"

// muVariant is the mu-paths variant whose line prices a knee.
const muVariant = "variant-03"

// servedMuPaths is the API on a directory holding the mu-paths recording.
func servedMuPaths(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile(muPaths)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mu-paths.jsonl.gz"), b, 0o644))
	src := &servedSources{recordings: dir, warn: io.Discard}
	return whatifapi.New(whatifapi.Config{Resolver: src, WhatifSchema: "schema-v3"}).Handler()
}

// cliDecode is the CLI's replicas-to-slo on mu-paths' variant, with extra
// flags: its role with inputs.
func cliDecode(t *testing.T, extra ...string) wireRole {
	t.Helper()
	out, code := runCmd(t, slices.Concat([]string{"replicas-to-slo", "--recording", muPaths, "--variant", muVariant, "-o", "json"}, extra)...)
	require.Equal(t, 0, code, out)
	w := decodeOutput(t, out)
	i := slices.IndexFunc(w.Roles, func(r wireRole) bool { return r.Inputs != nil })
	require.GreaterOrEqual(t, i, 0, out)
	return w.Roles[i]
}

// The page's kvPerRequest term carries the source the CLI's JSON names, at the
// same figure; and a plan at a stated shape prices the decode knee the CLI
// prices at that shape, from the measured reading scaled to it.
func TestServeInputs_TheKVSourceIsTheCLIs(t *testing.T) {
	h := servedMuPaths(t)
	code, body := apiPost(t, h, "/v1/inputs", whatifapi.InputsRequest{DecodeVariant: muVariant,
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: "mu-paths.jsonl.gz"}})
	require.Equal(t, http.StatusOK, code, "%s", body)
	var in whatifapi.InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))

	observed := cliDecode(t)
	require.Equal(t, kvMeasured+kvAboveContext, observed.Inputs.KVPerRequestSource, "mu-paths is priced as measured, 2.4x its context")
	i := slices.IndexFunc(in.Terms, func(term whatifapi.Term) bool { return term.Name == "kvPerRequest" })
	require.GreaterOrEqual(t, i, 0, "%s", body)
	term := in.Terms[i]
	assert.Equal(t, observed.Inputs.KVPerRequestSource, term.Source)
	require.NotNil(t, term.Value)
	assert.InDelta(t, observed.Inputs.KVPerRequest, *term.Value, 1e-6*observed.Inputs.KVPerRequest)

	stated := cliDecode(t, "--assume-input-tokens", "1800", "--assume-output-tokens", "600")
	require.Equal(t, kvMeasuredScaled+kvAboveContext, stated.Inputs.KVPerRequestSource)
	assert.InEpsilon(t, observed.Inputs.KVPerRequest*(1800+300)/(900+150), stated.Inputs.KVPerRequest, 1e-6,
		"the reading scales by the context asked over the measured one")
	require.NotNil(t, stated.Results.Knee.Answer, "the stated shape prices a knee")

	rate := 0.5 * stated.Results.Knee.Answer.Value
	out, planBody := apiPlan(t, h, whatifapi.PlanRequest{InputsID: in.InputsID,
		Traffic: &whatifapi.Traffic{Classes: &whatifapi.Classes{Critical: &whatifapi.ClassRate{Rate: fp(rate)}},
			Mix: &whatifapi.Mix{PromptTokens: 1800, OutputTokens: 600}},
		SLO: &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p90", Target: fp(1)},
			TPOT: &whatifapi.Objective{Statistic: statMean, Target: fp(0.05)}}})
	require.NotNil(t, out.TTFT.Markers.DecodeKnee, "%s", planBody)
	sameFigure(t, "decode knee at the stated shape", wantOf(stated.Results.Knee), *out.TTFT.Markers.DecodeKnee)
}
