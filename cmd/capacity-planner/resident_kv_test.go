package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The decode KV a request holds is the whole context, whatever the prefix-cache
// hit rate: a hit saves KV only where another running request holds the same
// blocks (saturation's prefix_cache_kv.go). The planner prices decode from the
// KV per running request its rows measured.

// residentAt is the clock of the synthetic recording's first cycle.
var residentAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// residentWorkload is a decode fleet whose prompts are long and hit the prefix
// cache 92% of the time, on blocks no running request shares: each running
// request holds its whole context, times overhead (an engine's own KV beyond
// the context, 1 for none).
type residentWorkload struct {
	il, ol, hit, overhead float64
	capTokens             int64
	steps                 int
}

// context is IL + OL/2, the KV a request holds on average over its life.
func (w residentWorkload) context() float64 { return w.il + w.ol/2 }

// row is a Ready decode pod at fill k, its running requests the ones that
// fill the cache at the workload's resident KV.
func (w residentWorkload) row(pod string, k float64) domain.ReplicaMetrics {
	running := int(math.Floor(k * float64(w.capTokens) / (w.context() * w.overhead)))
	itl := 0.008 + 0.03*k
	tokens := float64(running) / itl
	return domain.ReplicaMetrics{
		PodName: pod, VariantName: "dec", Namespace: "ns", ModelID: "m",
		Ready: true, ReadySince: residentAt.Add(-time.Hour), StartSeconds: 60, Revision: "r1",
		KvCacheUsage: k, KvCacheUsageKnown: true, KvUsageInstant: k, KvUsageInstantKnown: true,
		MaxRankKvCacheUsage: k, EngineRanks: 1, NumGpuBlocks: w.capTokens / 16, BlockSize: 16, TotalKvCapacityTokens: w.capTokens,
		TokensInUse: int64(k * float64(w.capTokens)), RunningRequests: running, RunningRequestsKnown: true,
		AvgInputTokens: w.il, AvgOutputTokens: w.ol, AvgInputTokensRecent: w.il, AvgOutputTokensRecent: w.ol,
		PrefixCacheHitRate: w.hit,
		AvgITL:             itl, AvgITLKnown: true, GenerationTokenRate: tokens, GenerationTokenRateKnown: true,
		RequestRate: tokens / w.ol, RequestRateKnown: true, QueueLengthKnown: true,
	}
}

// recording is the workload captured over its steps, six pods at fills 0.3
// to 0.55, and generated into a recording.
func (w residentWorkload) recording(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	rec := learnrec.New(&buf, 0)
	h := capacity.DefaultHorizons()
	rec.Record(learnrec.KindStart, residentAt, learnrec.Start{
		Saturation: learnrec.SaturationStart{EpisodeGap: h.EpisodeGap, VariantTimeout: h.VariantTimeout,
			BucketRetention: h.BucketRetention, BorrowTickGap: saturation.BorrowedK2TickGap},
		ThroughputConfig: &learnrec.ThroughputStart{SlopeHoldFor: time.Hour, OptimizationInterval: 15 * time.Second,
			SettleView: true, CapacityStore: true}})
	params := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 1024, BlockSize: 16, TensorParallelSize: 1}
	rec.Record(learnreplay.KindCaptureWindow, residentAt, learnreplay.CaptureWindow{Namespace: "ns", From: residentAt,
		To: residentAt.Add(time.Duration(w.steps-1) * 15 * time.Second), Step: 15 * time.Second, Roles: "role-of",
		Variants: []learnreplay.CaptureVariant{
			{ModelID: "m", Name: "dec", Role: domain.RoleDecode, Accelerator: "H100", GpuCount: 1, Params: params},
		}})
	policy := &config.ScalingPolicy{KvCacheThreshold: 0.8, QueueLengthThreshold: 5, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.7,
		Analyzers: []config.AnalyzerScoreConfig{{Name: "saturation"}}}
	for s := range w.steps {
		at := residentAt.Add(time.Duration(s) * 15 * time.Second)
		rows := make([]domain.ReplicaMetrics, 0, 6)
		for j := range 6 {
			rows = append(rows, w.row(fmt.Sprintf("dec-%d", j), 0.3+0.05*float64(j)+0.002*float64(s%3)))
		}
		rec.Record(learnreplay.KindCaptureCycle, at, learnreplay.CaptureCycle{Input: learnrec.InputOf(domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns", ReplicaMetrics: rows, Config: policy,
			VariantStates: []domain.VariantReplicaState{{VariantName: "dec", CurrentReplicas: 6, DesiredReplicas: 6,
				GPUsPerReplica: 1, Role: domain.RoleDecode, AcceleratorName: "H100"}},
		})})
	}
	require.NoError(t, rec.Err())
	rep, err := generateRecording(context.Background(), buf.Bytes())
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "resident.jsonl")
	require.NoError(t, os.WriteFile(path, rep.Recording, 0o600))
	return path
}

// priced is the decode variant as the planner prices it from the workload's
// recording, at the lengths set, if any.
func (w residentWorkload) priced(t *testing.T, il, ol float64) pricedVariant {
	t.Helper()
	f := answerFlags{recording: w.recording(t), model: "m", cycle: "last", window: captureFlags{}, set: map[string]bool{}}
	if il > 0 {
		f.inputTokens, f.set["assume-input-tokens"] = il, true
	}
	if ol > 0 {
		f.outputTokens, f.set["assume-output-tokens"] = ol, true
	}
	st, err := f.readState(context.Background())
	require.NoError(t, err)
	variants, err := f.decodeVariants(st)
	require.NoError(t, err)
	require.Len(t, variants, 1)
	return variants[0]
}

// highHit is the negative control's workload: 20,000-token prompts, 92% of
// them a prefix hit, each request still holding its whole context.
var highHit = residentWorkload{il: 20000, ol: 400, hit: 0.92, overhead: 1, capTokens: 1_000_000, steps: 40}

// On a high-hit workload the decode side is priced at the KV its rows held,
// the whole context, not at the hit's discount. Priced at the discount,
// IL x 0.08 + OL/2 = 1,800 tokens a request against the 20,200 held, the
// planner fits 11x the requests a cache holds: 11x the KV headroom, and a
// knee as far above the measured one. That is the base's answer.
func TestResidentKV_AHighHitIsPricedAtTheContextItHolds(t *testing.T) {
	pv := highHit.priced(t, 0, 0)
	discounted := highHit.il*(1-highHit.hit) + highHit.ol/2
	assert.InEpsilon(t, highHit.context(), pv.decode.KVPerRequest, 0.05,
		"decode KV a request %.0f, held %.0f; the hit's discount is %.0f", pv.decode.KVPerRequest, highHit.context(), discounted)
	slots := highHit.capTokens * 8 / 10
	assert.Less(t, float64(slots)/pv.decode.KVPerRequest, 1.1*float64(slots)/highHit.context(),
		"requests a cache holds at the band's top")
}

// A shape the flags state scales the measured KV by its context over the
// measured one, keeping the engine's overhead: an engine that holds 1.5x the
// context still holds 1.5x at another prompt length, never the discount.
func TestResidentKV_AStatedShapeKeepsTheMeasuredOverhead(t *testing.T) {
	w := highHit
	w.overhead = 1.5
	measured := w.priced(t, 0, 0)
	assert.InEpsilon(t, 1.5*w.context(), measured.decode.KVPerRequest, 0.05)
	assert.Equal(t, kvMeasured, measured.kvSource)

	stated := w.priced(t, 40000, 400)
	assert.InEpsilon(t, 1.5*(40000+200), stated.decode.KVPerRequest, 0.05)
	assert.Equal(t, kvMeasuredScaled, stated.kvSource)
}

// The overhead is not capped at the context: a reading of 3.8x the context is
// priced at 3.8x, as the engine holds it.
func TestResidentKV_NotCappedAtTheContext(t *testing.T) {
	w := highHit
	w.overhead, w.capTokens = 3.8, 10_000_000
	pv := w.priced(t, 0, 0)
	assert.InEpsilon(t, 3.8*w.context(), pv.decode.KVPerRequest, 0.05)
}

// With no reading, the decode side is priced at the whole context, with no
// discount, and says so.
func TestResidentKV_NoReadingIsTheWholeContext(t *testing.T) {
	pv := pricedVariant{
		state: learnreplay.VariantState{Shape: learnreplay.MeasuredShape{InputTokens: 20000, OutputTokens: 400, PrefixHitRate: 0.92}},
	}
	pv.shape = shape.New(highHit.il, highHit.ol, highHit.hit).WithOutputDispersion(1)
	assert.InDelta(t, 20200, pv.residentKV(), 1e-9)
	assert.Equal(t, kvContext, pv.kvSource)

	// Too few cycles is no reading.
	pv.state.Resident = learnreplay.ResidentKV{PerRequest: 50000, Cycles: 2, FromCycle: 1, ToCycle: 2}
	assert.InDelta(t, 20200, pv.residentKV(), 1e-9)
	assert.Equal(t, kvContext, pv.kvSource)
}

// The knee moves with the KV price: at the context the slots fall by the
// discount's factor. The synthetic recording learns only a pinned line, which
// prices no knee, so the line here is the one its rows were drawn on, fitted.
func TestResidentKV_TheKneeFollowsTheSlots(t *testing.T) {
	pv := highHit.priced(t, 0, 0)
	d := pv.decode
	d.Line, d.Tier = itl.Model{A: 0.03, B: 0.008}, whatif.TierOLS
	res := whatif.KneeRate(d)
	knee, ok := res.(whatif.Answer)
	require.True(t, ok, "knee %+v", res)
	d.KVPerRequest = highHit.il*(1-highHit.hit) + highHit.ol/2
	res = whatif.KneeRate(d)
	discounted, ok := res.(whatif.Answer)
	require.True(t, ok, "knee %+v", res)
	assert.Greater(t, discounted.Value, 5*knee.Value, "the discount over-states the knee")
}
