package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// stairDir is the staircase recordings' directory: real P/D runs, one
// prefill and one decode variant, two pods each.
const stairDir = "../../internal/engines/analyzers/learnreplay/testdata/goldens/"

// The day plan's goldens, text and JSON, on the sample recording (its 4k-token
// buckets put the 500 ms TTFT under the zero-load floor: prefill refused
// there, exit 3) and on real
// P/D staircases: b-on with V_P and a assumed sizes every bucket; b-on as
// recorded learned no V_P, so decode is sized alone and the plan exits 3
// naming the refusal; b-off as recorded refuses every term that sizes a
// role, and says so, exit 3, with no figure of a need. go test
// ./cmd/capacity-planner -run TestDayPlanGolden -update rewrites them.
func TestDayPlanGolden(t *testing.T) {
	stair := func(name string) string { return stairDir + name + "/recording.jsonl.gz" }
	assumed := []string{"--assume-vp", "200000", "--assume-a", "20ms", "--ttft", "2s@p90", "--tpot", "30ms"}
	for _, c := range []struct {
		name string
		args []string
		code int
	}{
		{"day-plan-ols-sweep", dayArgs, 3},
		{"day-plan-ols-sweep-json", append(slices.Clone(dayArgs), "-o", "json"), 3},
		{"day-plan-staircase-b-on-assumed", slices.Concat([]string{"day-plan", "--recording", stair("ttft-staircase-b-on")}, assumed), 0},
		{"day-plan-staircase-b-on", []string{"day-plan", "--recording", stair("ttft-staircase-b-on")}, 3},
		{"day-plan-staircase-b-on-json", []string{"day-plan", "-o", "json", "--recording", stair("ttft-staircase-b-on")}, 3},
		{"day-plan-staircase-b-off", []string{"day-plan", "--recording", stair("ttft-staircase-b-off")}, 3},
	} {
		t.Run(c.name, func(t *testing.T) { checkGolden(t, c.name, c.code, c.args) })
	}
}

// On real P/D staircases a refusal is the source's, not a bucket's: no
// bucket is unmet, a refused prefill leaves decode sized alone with no
// prefill figure, and a refused decode knee sizes nothing and names why.
func TestDayPlanStaircaseRefusals(t *testing.T) {
	decode := func(name string, args ...string) (*whatifapi.DayPlanResponse, int) {
		out, code := runCmd(t, slices.Concat([]string{"day-plan", "-o", "json", "--recording", stairDir + name + "/recording.jsonl.gz"}, args)...)
		var p whatifapi.DayPlanResponse
		require.NoError(t, jsonStrict([]byte(out), &p), out)
		return &p, code
	}
	onlyDecode, code := decode("ttft-staircase-b-on")
	assert.Equal(t, 3, code)
	require.Len(t, onlyDecode.Refused, 1)
	assert.Equal(t, "NoPrefillCapacity", onlyDecode.Refused[0].Reason)
	assert.Contains(t, onlyDecode.Refused[0].Hint, "--assume-vp")
	assert.Zero(t, onlyDecode.Summary.Counts.Unmet, "a refusal of the source is no bucket's")
	assert.Positive(t, onlyDecode.Summary.Counts.Sized)
	for _, b := range onlyDecode.Buckets {
		if b.Needed != nil {
			assert.Nil(t, b.Needed.Prefill, "%s: prefill sized without V_P", b.Start)
			assert.Contains(t, b.Flags, whatifapi.FlagPrefillNotPriced)
		}
	}
	assert.Nil(t, onlyDecode.Summary.NeededGPUHours, "half a fleet's GPUs are not set beside a whole fleet's")
	assert.Nil(t, onlyDecode.Summary.NeededReplicaHours.Prefill)

	none, code := decode("ttft-staircase-b-off")
	assert.Equal(t, 3, code)
	assert.Zero(t, none.Summary.Counts.Sized)
	assert.Zero(t, none.Summary.Counts.Unmet)
	assert.Positive(t, none.Summary.Counts.Refused)
	assert.Nil(t, none.Summary.PeakNeeded)
	assert.Nil(t, none.Summary.NeededReplicaHours, "no need is printed as 0")
	assert.Empty(t, none.Summary.UnderHours)
	assert.NotEmpty(t, none.Summary.RefusedHours)
	reasons := make([]string, 0, len(none.Refused))
	for _, r := range none.Refused {
		reasons = append(reasons, r.Term+":"+r.Reason)
		assert.NotEmpty(t, r.Hint)
		assert.Positive(t, r.Buckets, r.Term)
	}
	assert.Equal(t, []string{"ttft:NoFixedLatency", "decodeKnee:PinnedDefaultB"}, reasons)

	full, code := decode("ttft-staircase-b-on", "--assume-vp", "200000", "--assume-a", "20ms", "--ttft", "2s@p90", "--tpot", "30ms")
	assert.Equal(t, 0, code)
	assert.Empty(t, full.Refused)
	assert.Equal(t, len(full.Buckets), full.Summary.Counts.Sized)
	for _, b := range full.Buckets {
		require.NotNil(t, b.Needed.Prefill, "%s", b.Start)
		assert.GreaterOrEqual(t, *b.Limit.Value, *b.SizedRate*(1-1e-9), "%s", b.Start)
	}
}

// olsRes is the sample recording resolved once per test binary: each
// resolve replays it.
var olsRes = sync.OnceValues(func() (*whatifapi.Resolved, error) {
	src := &servedSources{recordings: filepath.Dir(olsSweep), warn: io.Discard}
	return src.Resolve(context.Background(), whatifapi.InputsRequest{Model: "model-02",
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(olsSweep)}})
})

// resolved is olsRes for t, with series in place of the recording's when
// given: a copy, so that no test changes another's.
func resolved(t *testing.T, series []whatifapi.SeriesSample) *whatifapi.Resolved {
	t.Helper()
	res, err := olsRes()
	require.NoError(t, err)
	out := *res
	if series != nil {
		out.Series = series
	}
	return &out
}

// stubResolver resolves every request to one Resolved.
type stubResolver struct{ res *whatifapi.Resolved }

func (s stubResolver) Resolve(context.Context, whatifapi.InputsRequest) (*whatifapi.Resolved, error) {
	return s.res, nil
}

// POST /v1/dayplan's body is the CLI's JSON golden, byte for byte but for
// the ids: one code path.
func TestDayPlanAPIParity(t *testing.T) {
	h := whatifapi.New(whatifapi.Config{Resolver: stubResolver{resolved(t, nil)}, WhatifSchema: "schema-v" + strconv.Itoa(schemaVersion)}).Handler()
	code, body := apiPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Model: "model-02"})
	require.Equal(t, 200, code, "%s", body)
	var in whatifapi.InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	code, body = apiPost(t, h, "/v1/dayplan", whatifapi.DayPlanRequest{InputsID: in.InputsID,
		SLO: &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p95", Target: fp(0.5)},
			TPOT: &whatifapi.Objective{Statistic: statMean, Target: fp(0.004)}},
		Assume: &whatifapi.Assume{VP: fp(30000)}, MinRequests: fp(whatifapi.DefaultMinRequests), Step: "5m0s"})
	require.Equal(t, 200, code, "%s", body)
	var api whatifapi.DayPlanResponse
	require.NoError(t, jsonStrict(body, &api))
	golden, err := os.ReadFile(filepath.Join("testdata", "day-plan-ols-sweep-json.golden"))
	require.NoError(t, err)
	var cli whatifapi.DayPlanResponse
	require.NoError(t, jsonStrict(golden, &cli))
	assert.NotEmpty(t, cli.WhatifSchema, "the CLI names the schema its figures follow")
	api.DayPlanID, cli.DayPlanID, api.InputsID, cli.InputsID = "", "", "", ""
	assert.Equal(t, cli, api)
	assert.NotEmpty(t, api.Buckets)
}

// day24h is a day of synthesized cycles every 15 s from 2026-11-01 00:00
// UTC: a diurnal rate from about 2 r/s at night to about 16 r/s at 16:00,
// with a jitter within each five minutes, every cycle at 1,000 prompt and
// 1,000 output tokens, and a fleet of 2 prefill and 3 decode replicas. A
// half hour from 03:00 has no cycles: a scrape gap.
func day24h() []whatifapi.SeriesSample {
	var out []whatifapi.SeriesSample
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 24*240; i++ {
		at := t0.Add(time.Duration(i) * 15 * time.Second)
		if at.Hour() == 3 && at.Minute() < 30 {
			continue
		}
		h := at.Sub(t0).Hours()
		rate := 2 + 14*(0.5-0.5*math.Cos(2*math.Pi*(h-4)/24))
		rate *= 1 + 0.15*math.Sin(float64(i)*1.7)
		out = append(out, whatifapi.SeriesSample{At: at, Rate: rate, RateKnown: true, Prefill: 2, Decode: 3, PrefillGPUs: 1, DecodeGPUs: 1,
			PromptTokens: 1000, OutputTokens: 1000, HasShape: true})
	}
	return out
}

// dayRequest is the 24-hour plan's question.
func dayRequest() whatifapi.DayPlanRequest {
	return whatifapi.DayPlanRequest{SLO: &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p95", Target: fp(0.5)},
		TPOT: &whatifapi.Objective{Statistic: statMean, Target: fp(0.004)}}, Assume: &whatifapi.Assume{VP: fp(30000)}}
}

// day24hPlan is the day plan of day24h on the sample recording's learned
// state, planned once per test binary.
var day24hPlan = sync.OnceValues(func() (*whatifapi.DayPlanResponse, error) {
	res, err := olsRes()
	if err != nil {
		return nil, err
	}
	r := *res
	r.Series = day24h()
	return whatifapi.DayPlan(context.Background(), &r, dayRequest(), "schema-v3")
})

// dayPlan24h is day24hPlan for t.
func dayPlan24h(t *testing.T) *whatifapi.DayPlanResponse {
	t.Helper()
	p, err := day24hPlan()
	require.NoError(t, err)
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
	assert.Equal(t, 282, p.Summary.Counts.SizedForP95, "20 cycles a bucket: every bucket sized for its p95")
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
	assert.Equal(t, whatifapi.SizeForP95, p.SizeFor)
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
	peak, low := p.Summary.PeakNeeded, p.Summary.MinNeeded
	assert.Greater(t, peak.Decode+*peak.Prefill, low.Decode+*low.Prefill, "a day from 2 to 16 r/s needs the same fleet all day")
	assert.NotEmpty(t, p.Summary.UnderHours, "the peak needs more than 2 prefill and 3 decode")
	assert.NotEmpty(t, p.Summary.OverHours, "the night needs fewer")
}

// jsonStrict decodes b into v, refusing a field v does not name.
func jsonStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
