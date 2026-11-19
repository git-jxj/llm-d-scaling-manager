package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// Issue llm-d-incubation/llm-d-scaling-manager#188: an engine can hold more KV
// per running request than its context (a sparse-attention indexer's keys, a
// speculative draft model's KV). With SwitchKVPerRequestAboveContext on, the
// reading is priced as read above the context, by the capacity planner's rule
// (shape.PriceResident). The tests set the switch by its name, so they run on
// a commit that predates it, and fail there.

// aboveOn turns the switch on, as a replay does.
func aboveOn(t *testing.T) func(*SaturationAnalyzer) {
	return func(a *SaturationAnalyzer) {
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{"kv-per-request-above-context": true}))
	}
}

// aboveOnSGLang is aboveOn on an SGLang engine.
func aboveOnSGLang(t *testing.T) func(*SaturationAnalyzer) {
	return func(a *SaturationAnalyzer) {
		aboveOn(t)(a)
		p := declaredParams(1024)
		p.Engine = inferenceengine.EngineSGLang
		a.capacityStore.Update("test-ns", "test-model", "variant-d", capacity.Record{
			AcceleratorName: "H200", GpuCount: 1, EngineParams: p,
		}, time.Now())
	}
}

// Thirty running requests hold 90,000 tokens, 3000 each, against a context of
// 1050 (cachingRow): an engine holding about 2.9 capacity tokens a context
// token. On, the derived mu is priced at 3000 a request, 0.68 x 100,000 /
// 3000 = 22.7 sequences, where the bound priced 64.8: a KV-bound mu 2.9x
// high. The log marks it above the context. Off, the bound holds, as before.
func TestKVAboveContext_AReadingAboveTheContextIsPricedAsRead(t *testing.T) {
	on := hitMu(t, cachingRow(0.9, 30), aboveOn(t))
	assert.InDelta(t, 3000, on["kvReqPerSeq"], 1e-9)
	assert.InDelta(t, 3000, on["kvReqFleet"], 1e-9, "the GPS check prices the same request")
	assert.InDelta(t, 68_000.0/3000, on["seqs"], 1e-9)
	assert.Equal(t, "above-context", on["residentKVBound"])
	assert.Equal(t, "above-context", on["residentKVBoundFleet"])

	off := hitMu(t, cachingRow(0.9, 30), nil)
	assert.InDelta(t, 1050, off["kvReqPerSeq"], 1e-9)
	assert.Equal(t, "upper", off["residentKVBound"])
	assert.Less(t, on["rate"], off["rate"], "more KV a request, fewer sequences, a lower mu")
}

// Ten running requests hold 30,000 tokens at a fill of 0.3, 3000 each,
// against a discount of 550 and a context of 1050. The count gate at the
// discount asks for 0.15 x 100,000 / 550 = 28 requests, more than a cache
// filled to 0.3 holds at 3000 each, so it would read nothing at any fill
// below 0.82. On, the gate is taken at the reading, 0.15 x 100,000 / 3000 =
// 5 requests: the reading is taken and marked above the context. Off, the
// gate stays at the discount and there is no reading, as before.
func TestKVAboveContext_TheCountGateTakesTheReading(t *testing.T) {
	r := residentReading{tokens: 30_000, running: 10, capTokens: 100_000, minK: 0.15}
	s := shape.New(1000, 100, 0.5)
	got, bound := withResidentKV(s, r, residentRule{aboveContext: true})
	assert.InDelta(t, 3000, got.KVreq, 1e-9)
	assert.Equal(t, residentBoundAboveContext, bound)
	got, bound = withResidentKV(s, r, residentRule{})
	assert.InDelta(t, 550, got.KVreq, 1e-9)
	assert.Equal(t, residentBoundNoReading, bound)

	on := hitMu(t, cachingRow(0.3, 10), aboveOn(t))
	assert.InDelta(t, 3000, on["kvReqPerSeq"], 1e-9)
	assert.Equal(t, "above-context", on["residentKVBound"])
	assert.InDelta(t, 68_000.0/3000, on["seqs"], 1e-9)

	off := hitMu(t, cachingRow(0.3, 10), nil)
	assert.InDelta(t, 550, off["kvReqPerSeq"], 1e-9)
	assert.Equal(t, "no-reading", off["residentKVBound"])
}

// A reading below the discount keeps the gate at the discount, on or off:
// 2000 tokens over 10 requests, 200 each, reads nothing at a gate of 28.
func TestKVAboveContext_ALowReadingKeepsTheGate(t *testing.T) {
	r := residentReading{tokens: 2000, running: 10, capTokens: 100_000, minK: 0.15}
	s := shape.New(1000, 100, 0.5)
	_, bound := withResidentKV(s, r, residentRule{aboveContext: true})
	assert.Equal(t, residentBoundNoReading, bound)
}

// Between the context and twice it the reading is priced as read and the
// bound is none: 60,000 over 40, 1500 each. Below the discount it is held at
// the discount, as before: 20,000 over 50, 400 against 550.
func TestKVAboveContext_TheFloorStaysAndANearContextReadingIsPlain(t *testing.T) {
	near := hitMu(t, cachingRow(0.6, 40), aboveOn(t))
	assert.InDelta(t, 1500, near["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundNone, near["residentKVBound"])

	low := hitMu(t, cachingRow(0.2, 50), aboveOn(t))
	assert.InDelta(t, 550, low["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundLower, low["residentKVBound"])
}

// SGLang reads the busiest rank against the pod's whole capacity, so above
// the context its reading is held at the context, switch on or off.
func TestKVAboveContext_SGLangIsHeldAtTheContext(t *testing.T) {
	sg := hitMu(t, cachingRow(0.9, 30), aboveOnSGLang(t))
	assert.InDelta(t, 1050, sg["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundUpper, sg["residentKVBound"])
}

// A shape with no prefix-cache hit takes a reading too: its discount is its
// context, and the engine's own KV is above it. Off, a no-hit shape is not
// read at all.
func TestKVAboveContext_ANoHitShapeIsReadAboveItsContext(t *testing.T) {
	noHit := cachingRow(0.9, 30)
	noHit.PrefixCacheHitRate = 0
	on := hitMu(t, noHit, aboveOn(t))
	assert.InDelta(t, 3000, on["kvReqPerSeq"], 1e-9)
	assert.Equal(t, "above-context", on["residentKVBound"])

	below := cachingRow(0.5, 50)
	below.PrefixCacheHitRate = 0
	held := hitMu(t, below, aboveOn(t))
	assert.InDelta(t, 1050, held["kvReqPerSeq"], 1e-9, "1000 a request is below the context: held at it")
	assert.Equal(t, residentBoundLower, held["residentKVBound"])

	off := hitMu(t, noHit, nil)
	assert.InDelta(t, 1050, off["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundNoHit, off["residentKVBound"])
}

// The switch needs SwitchKVPerRequestHitsResident: with that off, nothing is
// read, and the price is the shape's.
func TestKVAboveContext_NothingWithoutHitsResident(t *testing.T) {
	got := hitMu(t, cachingRow(0.9, 30), func(a *SaturationAnalyzer) {
		aboveOn(t)(a)
		hitsOff(a)
	})
	assert.InDelta(t, 550, got["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundOff, got["residentKVBound"])
}

// The controller keeps the bound: the switch is off in a new analyzer.
func TestKVAboveContext_OffFromTheConstructor(t *testing.T) {
	fields := NewSaturationAnalyzer(capacity.NewStore()).switchFields()
	require.Contains(t, fields, "kv-per-request-above-context")
	assert.False(t, *fields["kv-per-request-above-context"])
}
