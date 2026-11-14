package whatifapi

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// stubResolver resolves every request to one synthetic model: prefill knee
// 10 r/s per prefill replica, decode knee 12 r/s per decode replica, a TTFT
// and a TPOT rising with the rate. With refusePrefill, V_P is refused, as a
// recording that learned none: every TTFT figure and the fleet's limit are
// refusals.
type stubResolver struct {
	refusePrefill bool
	// fleets counts the fleets priced.
	fleets atomic.Int64
	// gate, when set, holds every Fleet until it is closed; onPlainLimits,
	// when set, is called with the count of PlainLimits calls so far.
	gate          chan struct{}
	onPlainLimits func(n int64)
	plainLimits   atomic.Int64
	// dip, when set, cuts the prefill knee of the fleets it holds for: a
	// limit that does not rise with the replicas.
	dip func(prefill, decode int) bool
	// onFleet, when set, is called with the knobs of every fleet priced.
	onFleet func(k Knobs)
	// loRefused, when set, refuses the low end of every fleet limit's span,
	// as a corner of V_P and f under a TTFT floor does.
	loRefused whatif.Reason
}

func (r *stubResolver) Resolve(_ context.Context, req InputsRequest) (*Resolved, error) {
	switch req.Model {
	case "bad":
		return nil, InputErrorf("no model bad")
	case "boom":
		return nil, errors.New("the source broke")
	}
	in := InputsResponse{Model: "model-01", Namespace: "ns-01", Source: SourceInfo{Kind: SourceRecording, Name: "run.jsonl.gz", Cycle: 10, TotalCycles: 10},
		Fleet: FleetInfo{Prefill: &RoleInfo{Variant: "p", Replicas: 1}, Decode: RoleInfo{Variant: "d", Replicas: 1}},
		Observed: Observed{Rate: Figure{Refused: &Refusal{Reason: "NotRecorded", Detail: "stub"}}, Buckets: Buckets(),
			Mix: ObservedMix{Refused: &Refusal{Reason: "NoPerRequestLengths", Detail: "stub"}}},
		Policy: PolicyInfo{TTFT: &PolicyObjective{Statistic: "p90", Seconds: 0.2}, TPOT: &PolicyObjective{Statistic: "mean", Seconds: 0.012},
			Headroom: 0.85, FlowControl: "off, inferred"},
		Terms: []Term{}, Assumed: []string{"Poisson arrivals"}}
	if req.Model == "decode-only" {
		in.Fleet.Prefill = nil
	}
	// ctx-16k is a source whose engines' arguments set a 16k context window.
	// ctx-refused and ctx-zero carry the term with no window: refused, and 0.
	switch req.Model {
	case "ctx-16k":
		in.Terms = []Term{{Name: TermMaxModelLen, Source: "engine arguments", Figure: Figure{Value: ptr(16384.0), Unit: "tokens"}}}
	case "ctx-refused":
		in.Terms = []Term{{Name: TermMaxModelLen, Figure: Figure{Refused: &Refusal{Reason: "NotRecorded", Detail: "stub"}}}}
	case "ctx-zero":
		in.Terms = []Term{{Name: TermMaxModelLen, Figure: Figure{Value: ptr(0.0), Unit: "tokens"}}}
	}
	return &Resolved{Inputs: in, Content: fmt.Sprintf("stub %v %s", r.refusePrefill, req.Model), Pricer: r}, nil
}

func (r *stubResolver) Fleet(k Knobs) (Fleet, error) {
	if k.Assume.VP != nil && *k.Assume.VP > 1e9 {
		return nil, InputErrorf("--assume-vp %g: too large", *k.Assume.VP)
	}
	if r.gate != nil {
		<-r.gate
	}
	if r.onFleet != nil {
		r.onFleet(k)
	}
	r.fleets.Add(1)
	return &stubFleet{k: k, refusePrefill: r.refusePrefill, r: r}, nil
}

// stubFleet is one synthetic fleet.
type stubFleet struct {
	k             Knobs
	refusePrefill bool
	r             *stubResolver
}

var noVP = whatif.Unanswerable{Reason: whatif.NoPrefillCapacity, Detail: "no P1 estimate"}

// prefillKnee is 10 r/s a replica at prompts up to 1000 tokens, less above,
// and a hundredth of it where the resolver's dip holds.
func (f *stubFleet) prefillKnee() float64 {
	k := 10 * float64(f.k.Prefill) * 1000 / max(f.k.PromptTokens, 1000)
	if f.r.dip != nil && f.r.dip(f.k.Prefill, f.k.Decode) {
		k /= 100
	}
	return k
}

// decodeKnee is 12 r/s a replica.
func (f *stubFleet) decodeKnee() float64 { return 12 * float64(f.k.Decode) }

func (f *stubFleet) PrefillKnee() whatif.Result {
	if f.refusePrefill {
		return noVP
	}
	return whatif.Answer{Value: f.prefillKnee(), Flags: []whatif.Flag{whatif.UnvalidatedTTFT}}
}

func (f *stubFleet) DecodeKnee() whatif.Result {
	return whatif.Answer{Value: f.decodeKnee(), Basis: whatif.Basis{BoundBy: []whatif.Bound{whatif.BoundBatchCap}}}
}

func (f *stubFleet) Onsets() whatif.Result { return nil }

func (f *stubFleet) TTFTAt(rate float64) whatif.Result {
	if f.refusePrefill {
		return noVP
	}
	if rate >= min(f.prefillKnee(), f.decodeKnee()) {
		return whatif.Unanswerable{Reason: whatif.Saturated, Detail: fmt.Sprintf("%g r/s at or above the knee", rate)}
	}
	s := 0.1 / (1 - rate/min(f.prefillKnee(), f.decodeKnee()))
	return whatif.TTFT{Seconds: s, Floor: 0.1, Flags: []whatif.Flag{whatif.UnvalidatedTTFT},
		Sensitivity: &whatif.Sensitivity{Lo: 0.9 * s, Hi: 1.1 * s}}
}

func (f *stubFleet) TTFTPoint(rate float64) (whatif.Result, []string) {
	return f.TTFTAt(rate), []string{"a stub's note at the rate"}
}

func (f *stubFleet) TPOTAt(rate float64) (mean, percentile whatif.Result) {
	if rate >= f.decodeKnee() {
		u := whatif.Unanswerable{Reason: whatif.Saturated, Detail: "at the decode knee"}
		return u, nil
	}
	mean = whatif.Answer{Value: 0.005 + rate/1000, Basis: whatif.Basis{Floor: 0.005}, Band: &whatif.Interval{Lo: 0.004, Hi: 0.02}}
	if f.k.TPOTPercentile != 0 {
		percentile = whatif.Unanswerable{Reason: whatif.NoTPOTHistogram, Detail: "no TPOT histogram"}
		if f.k.Assume.TPOTRatio != nil {
			percentile = whatif.Answer{Value: *f.k.Assume.TPOTRatio * (0.005 + rate/1000), Flags: []whatif.Flag{whatif.AssumedTPOTRatio}}
		}
	}
	return mean, percentile
}

func (f *stubFleet) Limits(ttftSeconds, tpotSeconds, headroom float64) LimitSet {
	d, p, fl := f.PlainLimits(ttftSeconds, tpotSeconds, headroom)
	_, _, uncapped := f.PlainLimits(ttftSeconds, tpotSeconds, 1)
	// The fleet's limit spans 10% either way over V_P and f.
	if a, ok := fl.(whatif.Answer); ok {
		a.Sensitivity = &whatif.Sensitivity{Lo: 0.9 * a.Value, Hi: 1.1 * a.Value, Refused: f.r.loRefused}
		fl = a
	}
	return LimitSet{Decode: d, Prefill: p, Fleet: fl, WithoutHeadroom: uncapped, Notes: []string{"a stub's note at the target"}}
}

func (f *stubFleet) PlainLimits(ttftSeconds, tpotSeconds, headroom float64) (decode, prefill, fleet whatif.Result) {
	if n := f.r.plainLimits.Add(1); f.r.onPlainLimits != nil {
		f.r.onPlainLimits(n)
	}
	knee := min(f.prefillKnee(), f.decodeKnee())
	if f.refusePrefill {
		prefill = noVP
	} else {
		prefill = whatif.Answer{Value: knee * (1 - 0.1/ttftSeconds), Basis: whatif.Basis{BoundBy: []whatif.Bound{whatif.BoundTTFT}}}
	}
	if tpotSeconds > 0 {
		decode = whatif.Answer{Value: min(headroom*f.decodeKnee(), (tpotSeconds-0.005)*1000),
			Basis: whatif.Basis{BoundBy: []whatif.Bound{whatif.BoundTPOT}, Uncapped: (tpotSeconds - 0.005) * 1000}}
	}
	if u, ok := prefill.(whatif.Unanswerable); ok {
		return decode, prefill, u
	}
	// A percentile with no ratio refuses decode's limit, and the fleet's.
	if tpotSeconds > 0 && f.k.TPOTPercentile != 0 && f.k.Assume.TPOTRatio == nil {
		u := whatif.Unanswerable{Reason: whatif.NoTPOTHistogram, Detail: "no TPOT histogram"}
		return u, prefill, u
	}
	p, _ := valueOf(prefill)
	lowest, by := headroom*knee, whatif.BoundOperatingPoint
	if p < lowest {
		lowest, by = p, whatif.BoundTTFT
	}
	if d, ok := valueOf(decode); ok && d < lowest {
		lowest, by = d, "decode TPOT"
	}
	return decode, prefill, whatif.Answer{Value: lowest, Basis: whatif.Basis{BoundBy: []whatif.Bound{by}}}
}

func (f *stubFleet) TPOTRatio() Ratio {
	if f.k.TPOTPercentile == 0 {
		return Ratio{}
	}
	if f.k.Assume.TPOTRatio == nil {
		return Ratio{Refused: &Refusal{Reason: string(whatif.NoTPOTHistogram), Detail: "no TPOT histogram"}}
	}
	return Ratio{Value: f.k.Assume.TPOTRatio, Source: "assumed", Flags: []string{string(whatif.AssumedTPOTRatio)}}
}

func (f *stubFleet) Assumed() []string { return []string{"Poisson arrivals"} }
