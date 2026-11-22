package whatif

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// tierRow is a serving prefill row: rate requests a second of whole-token
// prompts, computed tokens a second computed, the cached tokens a second
// by tier, and the KV transfers it read.
func tierRow(rate, whole, computed float64, cached map[string]float64, tr ...domain.KVTransferRate) domain.ReplicaMetrics {
	rm := computedRow(computed, rate, whole)
	for _, s := range []string{SourceDevice, SourceDisk, SourceExternalUnspecified, SourceHost, SourceP2P} {
		if v, ok := cached[s]; ok {
			rm.CachedTokenRateBySource = append(rm.CachedTokenRateBySource, domain.SourceRate{Source: s, Rate: v})
		}
	}
	rm.KVTransferRates = tr
	return rm
}

// The window reads the load of a cached token from outside the GPU as the
// engines' own counters give it: each transfer's seconds over the tokens it
// moved, times the tokens a request had of those tiers.
//
// One pod, 2 requests a second of 85000-token prompts, 3% computed: 5100
// tokens a second computed, 40000 from the GPU, 100000 from the host tier
// and 20900 from a peer. The offload load moved the host and peer tokens,
// 120900 a second, at 64 KiB a token in 0.5 s a second: 4.1 us a token,
// and a request's 60450 of them 0.25 s. The tier read moved the peer's
// 20900 in 0.1 s a second: 0.05 s a request. NIXL beside an offload load is
// the P/D transfer and prices nothing.
func TestPromptWindow_TierLoad(t *testing.T) {
	const kib64 = 65536.0
	cached := map[string]float64{SourceDevice: 40000, SourceHost: 100000, SourceP2P: 20900}
	row := tierRow(2, 85000, 5100, cached,
		domain.KVTransferRate{Kind: domain.KVTransferOffloadLoad, ByteRate: 120900 * kib64, SecondsRate: 0.5},
		domain.KVTransferRate{Kind: domain.KVTransferTierRead, ByteRate: 20900 * kib64, SecondsRate: 0.1},
		domain.KVTransferRate{Kind: domain.KVTransferNIXL, ByteRate: 1e9, SecondsRate: 7})
	w := &PromptWindow{}
	for i := range 3 {
		w.Note([]domain.ReplicaMetrics{row}, i, time.Unix(int64(15*i), 0))
	}
	tl, ok := w.TierLoad()
	if !ok {
		t.Fatal("the rows read the split: want a tier load")
	}
	relNear(t, "whole prompt", tl.WholePromptTokens, 85000, 1e-12)
	relNear(t, "outer share", tl.OuterShare, 120900.0/170000, 1e-12)
	relNear(t, "host share", tl.Shares[SourceHost], 100000.0/170000, 1e-12)
	relNear(t, "load per request", tl.SecondsPerRequest, 0.25+0.05, 1e-12)
	if len(tl.Transfers) != 2 || tl.Transfers[0].Kind != domain.KVTransferOffloadLoad || tl.Transfers[1].Kind != domain.KVTransferTierRead {
		t.Fatalf("transfers %+v, want the offload load and the tier read, NIXL left out", tl.Transfers)
	}
	relNear(t, "bytes per token", tl.Transfers[0].BytesPerToken, kib64, 1e-12)
	relNear(t, "bandwidth", tl.Transfers[0].BytesPerSecond, 120900*kib64/0.5, 1e-12)
	relNear(t, "seconds per token is bytes over bandwidth", tl.Transfers[0].SecondsPerToken,
		tl.Transfers[0].BytesPerToken/tl.Transfers[0].BytesPerSecond, 1e-12)
	if len(tl.Unmeasured) != 0 {
		t.Errorf("unmeasured %v, want none", tl.Unmeasured)
	}
	// A clone carries it.
	if c, ok := w.Clone().TierLoad(); !ok || c.SecondsPerRequest != tl.SecondsPerRequest {
		t.Errorf("clone: %v %v", c.SecondsPerRequest, ok)
	}

	// A peer tier with no offload load, over NIXL: NIXL prices it.
	nixl := &PromptWindow{}
	nixl.Note([]domain.ReplicaMetrics{tierRow(1, 10000, 2000, map[string]float64{SourceP2P: 8000},
		domain.KVTransferRate{Kind: domain.KVTransferNIXL, ByteRate: 8000 * kib64, SecondsRate: 0.4})}, 0, time.Unix(0, 0))
	tl, _ = nixl.TierLoad()
	relNear(t, "NIXL alone prices the peer", tl.SecondsPerRequest, 0.4, 1e-12)

	// A host tier with no load reading: unmeasured, and nothing priced for
	// it. A tier under MinTierShare is not named.
	unread := &PromptWindow{}
	unread.Note([]domain.ReplicaMetrics{tierRow(1, 10000, 2000,
		map[string]float64{SourceHost: 7950, SourceExternalUnspecified: 50})}, 0, time.Unix(0, 0))
	tl, ok = unread.TierLoad()
	if !ok || len(tl.Unmeasured) != 1 || tl.Unmeasured[0] != SourceHost || tl.SecondsPerRequest != 0 {
		t.Errorf("unread load: %+v %v, want the host tier unmeasured", tl, ok)
	}

	// A disk tier with only its read into the CPU tier: the read alone does
	// not reach the GPU, so the tier stays unmeasured.
	readOnly := &PromptWindow{}
	readOnly.Note([]domain.ReplicaMetrics{tierRow(1, 10000, 2000, map[string]float64{SourceDisk: 8000},
		domain.KVTransferRate{Kind: domain.KVTransferTierRead, ByteRate: 1, SecondsRate: 0.2})}, 0, time.Unix(0, 0))
	if tl, _ = readOnly.TierLoad(); len(tl.Unmeasured) != 1 || tl.Unmeasured[0] != SourceDisk {
		t.Errorf("read only: unmeasured %v, want disk", tl.Unmeasured)
	}

	// No split read: no tier load.
	none := &PromptWindow{}
	none.Note([]domain.ReplicaMetrics{computedRow(2000, 1, 10000)}, 0, time.Unix(0, 0))
	if _, ok := none.TierLoad(); ok {
		t.Error("no split: want none")
	}
	var nilWindow *PromptWindow
	if _, ok := nilWindow.TierLoad(); ok {
		t.Error("nil window: want none")
	}
}

// The tier load is a request's own latency, as a is: added to every
// figure, its own part of the answer, and no part of the queue's work, so
// the prefill knee does not move.
func TestTTFT_TierLoadIsOwnLatency(t *testing.T) {
	fl := TTFTFleet{Prefill: hitFleet(8000, 0, 0)}
	off := mustTTFT(t, TTFTAt(fl, 5, 0.9))
	fl.Prefill.TierLoadSeconds = 0.3
	on := mustTTFT(t, TTFTAt(fl, 5, 0.9))
	relNear(t, "p90 moves by the load", on.Seconds-off.Seconds, 0.3, 1e-9)
	relNear(t, "the part", on.Parts.TierLoad, 0.3, 1e-12)
	relNear(t, "own prefill holds it", on.Parts.OwnPrefill-off.Parts.OwnPrefill, 0.3, 1e-9)
	kOff := mustAnswer(t, PrefillKnee(hitFleet(8000, 0, 0)))
	kOn := mustAnswer(t, PrefillKnee(fl.Prefill))
	relNear(t, "knee", kOn.Value, kOff.Value, 1e-12)

	fl.Prefill.TierLoadSeconds = -1
	if _, ok := TTFTAt(fl, 5, 0.9).(Unanswerable); !ok {
		t.Error("a negative load is refused")
	}
}

// With the cached context read (slo.SwitchTTFTTierLoad), the span is the
// computed prompt plus twice the cached context as read, every tier's; the
// prefix-cache hit rate, which sees the GPU's hits alone, is not read. At
// 32k with 8k computed and a GPU hit rate of 0.1 (the rest from a host
// tier), the span reads 24k cached; from the hit rate it would read none.
func TestShapedVP_CachedContextSpan(t *testing.T) {
	s := func(x float64) float64 { return hitCurveB + hitCurveC*x }
	p := hitFleet(8000, 32000, 0.1)
	p.PrefixAttention = true
	vp, _ := pricedVP(t, p)
	relNear(t, "from the GPU hit rate: no material hit, held", vp, 120000, 1e-12)

	p.CachedContextRead, p.CachedContextTokens = true, 24000
	vp, k := pricedVP(t, p)
	relNear(t, "from the cached context", vp, 120000*s(8000)/s(8000+2*24000), 1e-12)
	if !k.HasFlag(VPScaledToPrompt) {
		t.Errorf("flags %v, want VPScaledToPrompt", k.Flags)
	}

	// Without PrefixAttention the cached context is not read.
	p.PrefixAttention = false
	vp, _ = pricedVP(t, p)
	relNear(t, "prefix attention off", vp, 120000, 1e-12)

	// A cached context under MaterialPrefixHitMove of the prompt on both
	// sides is none.
	p.PrefixAttention, p.CachedContextTokens = true, 800
	vp, _ = pricedVP(t, p)
	relNear(t, "immaterial", vp, 120000, 1e-12)

	p.CachedContextTokens = -1
	if _, ok := PrefillKnee(p).(Unanswerable); !ok {
		t.Error("a negative cached context is refused")
	}
}
