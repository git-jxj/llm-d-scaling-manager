package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// dayArgs are the day plan's golden question on the guide's sample
// recording: V_P assumed, as the guide's sample assumes it.
var dayArgs = []string{"day-plan", "--recording", olsSweep, "--model", "model-02", "--assume-vp", "30000",
	"--ttft", "500ms@p95", "--tpot", "4ms"}

// The day plan's goldens on the sample recording, text and JSON. go test
// ./cmd/capacity-planner -run TestDayPlanGolden -update rewrites them.
func TestDayPlanGolden(t *testing.T) {
	checkGolden(t, "day-plan-ols-sweep", 0, dayArgs)
	checkGolden(t, "day-plan-ols-sweep-json", 0, append(slices.Clone(dayArgs), "-o", "json"))
}

// The CLI and POST /v1/dayplan give the same plan: the API's body is the
// CLI's JSON, its ids aside.
func TestDayPlanAPIParity(t *testing.T) {
	h, _ := servedAPI(t)
	in, _ := apiInputs(t, h, "ols-sweep.jsonl.gz", "model-02")
	code, body := apiPost(t, h, "/v1/dayplan", whatifapi.DayPlanRequest{InputsID: in.InputsID,
		SLO: &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p95", Target: fp(0.5)},
			TPOT: &whatifapi.Objective{Statistic: statMean, Target: fp(0.004)}},
		Assume: &whatifapi.Assume{VP: fp(30000)}, MinRequests: fp(whatifapi.DefaultMinRequests), Step: "5m0s"})
	require.Equal(t, 200, code, "%s", body)
	var api whatifapi.DayPlanResponse
	require.NoError(t, jsonStrict(body, &api))
	out, c := runCmd(t, append(slices.Clone(dayArgs), "-o", "json")...)
	require.Equal(t, 0, c, out)
	var cli whatifapi.DayPlanResponse
	require.NoError(t, jsonStrict([]byte(out), &cli))
	api.DayPlanID, cli.DayPlanID, api.WhatifSchema, cli.WhatifSchema = "", "", "", ""
	assert.Equal(t, cli, api)
	assert.NotEmpty(t, api.Buckets)
}

// day24h is a day of synthesized cycles every 30 s from 2026-11-01 00:00
// UTC: a diurnal rate from about 2 r/s at night to about 16 r/s at 16:00,
// with a jitter within each five minutes, every cycle at 1,000 prompt and
// 1,000 output tokens, and a fleet of 2 prefill and 3 decode replicas. A
// half hour from 03:00 has no cycles: a scrape gap.
func day24h() []whatifapi.SeriesSample {
	var out []whatifapi.SeriesSample
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 24*120; i++ {
		at := t0.Add(time.Duration(i) * 30 * time.Second)
		if at.Hour() == 3 && at.Minute() < 30 {
			continue
		}
		h := at.Sub(t0).Hours()
		rate := 2 + 14*(0.5-0.5*math.Cos(2*math.Pi*(h-4)/24))
		rate *= 1 + 0.15*math.Sin(float64(i)*1.7)
		out = append(out, whatifapi.SeriesSample{At: at, Rate: rate, Prefill: 2, Decode: 3, PrefillGPUs: 1, DecodeGPUs: 1,
			PromptTokens: 1000, OutputTokens: 1000, HasShape: true})
	}
	return out
}

// dayRequest is the 24-hour plan's question.
func dayRequest() whatifapi.DayPlanRequest {
	return whatifapi.DayPlanRequest{SLO: &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p95", Target: fp(0.5)},
		TPOT: &whatifapi.Objective{Statistic: statMean, Target: fp(0.004)}}, Assume: &whatifapi.Assume{VP: fp(30000)}}
}

// dayPlan24h is the day plan of day24h on the sample recording's learned
// state.
func dayPlan24h(t *testing.T) *whatifapi.DayPlanResponse {
	t.Helper()
	_, src := servedAPI(t)
	res, err := src.Resolve(context.Background(), whatifapi.InputsRequest{Model: "model-02",
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: "ols-sweep.jsonl.gz"}})
	require.NoError(t, err)
	res.Series = day24h()
	start := time.Now()
	p, err := whatifapi.DayPlan(context.Background(), res, dayRequest())
	require.NoError(t, err)
	t.Logf("288 buckets planned in %s, %d fleets priced", time.Since(start), p.Searched)
	return p
}

// A day of 5-minute buckets, held to its golden: the hours, the summary and
// the notes. go test ./cmd/capacity-planner -run TestDayPlan24hGolden -update
// rewrites it.
func TestDayPlan24hGolden(t *testing.T) {
	p := dayPlan24h(t)
	require.Len(t, p.Buckets, 288)
	assert.Equal(t, 6, p.Summary.Counts.NoData, "the half hour with no cycles")
	assert.Equal(t, 282, p.Summary.Counts.Sized)
	var b bytes.Buffer
	require.NoError(t, writeDayPlanText(&b, whatifapi.InputsResponse{Namespace: "ns", Model: "model-02"}, p))
	path := filepath.Join("testdata", "day-plan-24h.golden")
	if *update {
		require.NoError(t, os.WriteFile(path, b.Bytes(), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "go test ./cmd/capacity-planner -run TestDayPlan24hGolden -update writes it")
	assert.Equal(t, strings.ReplaceAll(string(want), "\r\n", "\n"), b.String())
}

// The replicas a bucket needs follow its own rate: the fleet's limit at the
// replicas found reaches the bucket's own p95, every bucket sized, and
// decode, sized first, never needs fewer replicas at a higher rate. Sizing
// every bucket at one rate, the window's mean, fails both: a bucket over the
// mean is short of it.
func TestDayPlanNeededFollowsRate(t *testing.T) {
	p := dayPlan24h(t)
	assert.Equal(t, whatifapi.SizeForP95, p.SizeFor, "30 s cycles hold ten a bucket: the p95 is the default")
	var sized []whatifapi.DayBucket
	for _, b := range p.Buckets {
		if b.Status != whatifapi.BucketSized {
			continue
		}
		sized = append(sized, b)
		require.NotNil(t, b.Limit.Value, "%s", b.Start)
		assert.GreaterOrEqual(t, *b.Limit.Value, b.Rate.P95*(1-1e-9), "%s: the fleet needed does not serve the bucket's p95", b.Start)
	}
	require.Len(t, sized, 282)
	sort.SliceStable(sized, func(i, j int) bool { return sized[i].Rate.P95 < sized[j].Rate.P95 })
	for i := 1; i < len(sized); i++ {
		assert.GreaterOrEqual(t, sized[i].Needed.Decode, sized[i-1].Needed.Decode,
			"decode needs fewer replicas at %.3g r/s (%s) than at %.3g r/s (%s)", sized[i].Rate.P95, sized[i].Start,
			sized[i-1].Rate.P95, sized[i-1].Start)
	}
	assert.Greater(t, p.Summary.PeakNeeded.Decode+p.Summary.PeakNeeded.Prefill,
		p.Summary.MinNeeded.Decode+p.Summary.MinNeeded.Prefill, "a day from 2 to 16 r/s needs the same fleet all day")
	assert.NotEmpty(t, p.Summary.UnderHours, "the peak needs more than 2 prefill and 3 decode")
	assert.NotEmpty(t, p.Summary.OverHours, "the night needs fewer")
}

// jsonStrict decodes b into v, refusing a field v does not name.
func jsonStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
