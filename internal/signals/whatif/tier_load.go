package whatif

import (
	"maps"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The cost of a cached prompt token that came from outside the GPU (issue
// llm-d-incubation/llm-d-scaling-manager#311 follow-up, the tier load). A
// hit in the GPU prefix cache costs only the attention over it. A hit from
// a host, disk or peer tier is loaded first: the engine moves its KV into
// the GPU, and with vLLM's OffloadingConnector the request waits for that
// load before its prefill is scheduled. The window reads what the load
// cost from the engines' own counters, per transfer kind
// (domain.KVTransferRate): bytes and seconds of transfer, over the cached
// tokens by tier (vllm:prompt_tokens_cached_by_source) the kind moved.

// The tiers vllm:prompt_tokens_cached_by_source names (its source label).
const (
	// SourceDevice is the GPU prefix cache: no load.
	SourceDevice = "device"
	// SourceHost is a CPU tier on the same pod.
	SourceHost = "host"
	// SourceDisk is a file-system tier.
	SourceDisk = "disk"
	// SourceP2P is a peer's tier, pulled over the network.
	SourceP2P = "p2p"
	// SourceExternalUnspecified is a connector that does not name its tier:
	// no transfer series prices it.
	SourceExternalUnspecified = "external_unspecified"
)

// MinTierShare is the least share of the whole prompt a tier must serve for
// its load to count: below it no load is priced, and a tier with no
// transfer reading does not hold the term back. Stated, not measured: 1% of
// the prompt.
const MinTierShare = 0.01

// tierKind is what one transfer kind moves: the tiers whose cached tokens
// it carries.
var tierKind = map[string][]string{
	domain.KVTransferOffloadLoad: {SourceHost, SourceDisk, SourceP2P},
	domain.KVTransferTierRead:    {SourceDisk, SourceP2P},
	// NIXL carries a peer's tokens only where no offload load is read: on a
	// pod with an OffloadingConnector its peer tier loads through it, and a
	// NixlConnector beside it is the P/D transfer, not a hit.
	domain.KVTransferNIXL: {SourceP2P},
}

// kindReading is one transfer kind over a tier sample: its bytes and
// seconds a second, and the cached tokens a second of the tiers it moved,
// on the rows that read it.
type kindReading struct {
	bytes, seconds, tokens float64
}

// tierSample is one cycle's tier readings, over the counted rows that read
// both the cached tokens by tier and the one-minute whole prompt.
type tierSample struct {
	requests, whole float64
	// cached is the cached tokens a second by tier; moved the tokens a
	// second each kind is to move, over every row of the sample, whether
	// the row read that kind or not; kinds each kind's reading, over the
	// rows that read it.
	cached map[string]float64
	moved  map[string]float64
	kinds  map[string]kindReading
}

// movedBy is the cached tokens a second kind moves on a row with the given
// split, and whether the row read an offload load (tierKind).
func movedBy(kind string, cached map[string]float64, offloadLoad bool) float64 {
	if kind == domain.KVTransferNIXL && offloadLoad {
		return 0
	}
	var n float64
	for _, s := range tierKind[kind] {
		n += cached[s]
	}
	return n
}

// noteTiers folds one counted row into the cycle's tier sample (Note).
func (s *tierSample) noteTiers(rm domain.ReplicaMetrics, rate float64) {
	il := rm.AvgInputTokensRecent
	if len(rm.CachedTokenRateBySource) == 0 || !Finite(il) || !(il > 0) {
		return
	}
	cached := map[string]float64{}
	for _, sr := range rm.CachedTokenRateBySource {
		if Finite(sr.Rate) && sr.Rate >= 0 {
			cached[sr.Source] += sr.Rate
		}
	}
	if s.cached == nil {
		s.cached, s.moved, s.kinds = map[string]float64{}, map[string]float64{}, map[string]kindReading{}
	}
	s.requests += rate
	s.whole += rate * il
	for src, v := range cached {
		s.cached[src] += v
	}
	offloadLoad := false
	for _, t := range rm.KVTransferRates {
		if t.Kind == domain.KVTransferOffloadLoad {
			offloadLoad = true
		}
	}
	for kind := range tierKind {
		s.moved[kind] += movedBy(kind, cached, offloadLoad)
	}
	for _, t := range rm.KVTransferRates {
		if _, ok := tierKind[t.Kind]; !ok || !Finite(t.ByteRate) || t.ByteRate < 0 || !Finite(t.SecondsRate) || t.SecondsRate < 0 {
			continue
		}
		k := s.kinds[t.Kind]
		k.bytes += t.ByteRate
		k.seconds += t.SecondsRate
		k.tokens += movedBy(t.Kind, cached, offloadLoad)
		s.kinds[t.Kind] = k
	}
}

// TierTransfer is one transfer kind as a window read it: the bandwidth,
// the bytes it moved per cached token, and so the seconds per token, which
// is BytesPerToken / BytesPerSecond.
type TierTransfer struct {
	Kind            string
	BytesPerSecond  float64
	BytesPerToken   float64
	SecondsPerToken float64
}

// TierLoad is what a window read of the load of cached tokens from outside
// the GPU.
type TierLoad struct {
	// Shares is each tier's cached tokens over the whole prompt, the GPU's
	// (SourceDevice) included; OuterShare is the share of every tier but the
	// GPU's.
	Shares     map[string]float64
	OuterShare float64
	// WholePromptTokens is the mean whole prompt of the rows read.
	WholePromptTokens float64
	// Transfers is each kind read that moved tokens, by kind.
	Transfers []TierTransfer
	// SecondsPerRequest is the mean load a request paid at the traffic
	// read: each kind's tokens a request times its seconds per token,
	// summed. Its own seconds over its own tokens, so a row that did not
	// read a kind is priced at the rows that did.
	SecondsPerRequest float64
	// Unmeasured is each tier, past the GPU, serving at least MinTierShare
	// of the whole prompt that no transfer read prices, sorted. With one,
	// SecondsPerRequest leaves its load out, and the term is not priced.
	Unmeasured []string
}

// TierLoad is the window's tier load, false where no counted row read the
// cached tokens by tier with a one-minute whole prompt (a recording made
// before the collector read them, or an engine that does not publish
// vllm:prompt_tokens_cached_by_source).
func (w *PromptWindow) TierLoad() (TierLoad, bool) {
	if w == nil {
		return TierLoad{}, false
	}
	var requests, whole float64
	cached, moved, kinds := map[string]float64{}, map[string]float64{}, map[string]kindReading{}
	for _, s := range w.samples {
		requests += s.tiers.requests
		whole += s.tiers.whole
		for k, v := range s.tiers.cached {
			cached[k] += v
		}
		for k, v := range s.tiers.moved {
			moved[k] += v
		}
		for k, v := range s.tiers.kinds {
			r := kinds[k]
			r.bytes += v.bytes
			r.seconds += v.seconds
			r.tokens += v.tokens
			kinds[k] = r
		}
	}
	if !(requests > 0) || !(whole > 0) {
		return TierLoad{}, false
	}
	tl := TierLoad{Shares: map[string]float64{}, WholePromptTokens: whole / requests}
	for src, v := range cached {
		tl.Shares[src] = v / whole
		if src != SourceDevice {
			tl.OuterShare += v / whole
		}
	}
	priced := map[string]bool{}
	for _, kind := range slices.Sorted(maps.Keys(kinds)) {
		r := kinds[kind]
		if !(r.tokens > 0) {
			continue
		}
		t := TierTransfer{Kind: kind, BytesPerToken: r.bytes / r.tokens, SecondsPerToken: r.seconds / r.tokens}
		if r.seconds > 0 {
			t.BytesPerSecond = r.bytes / r.seconds
		}
		tl.Transfers = append(tl.Transfers, t)
		tl.SecondsPerRequest += moved[kind] / requests * t.SecondsPerToken
		for _, src := range tierKind[kind] {
			// The tier read alone does not reach the GPU: it prices a tier
			// only with the load beside it.
			if kind != domain.KVTransferTierRead {
				priced[src] = true
			}
		}
	}
	for _, src := range slices.Sorted(maps.Keys(tl.Shares)) {
		if src != SourceDevice && tl.Shares[src] >= MinTierShare && !priced[src] {
			tl.Unmeasured = append(tl.Unmeasured, src)
		}
	}
	return tl, true
}
