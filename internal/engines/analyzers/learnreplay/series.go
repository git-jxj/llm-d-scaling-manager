package learnreplay

import (
	"fmt"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// CycleSample is what one cycle of a recording saw of a model: its arrival
// rate, the pods of each role that served, and the traffic's shape. It is
// read from the cycle's input alone, with no replay, so a day of cycles is
// read in one pass.
type CycleSample struct {
	At time.Time
	// ArrivalRate is the model's request rate, requests per second, as the
	// cycle's input carries it; RateKnown is false when the input carries
	// none (no router series), and the rate is then the decode rows' sum.
	ArrivalRate float64
	RateKnown   bool
	// PrefillPods and DecodePods are the pods of each role that served at
	// the cycle (prefill.Serving): ready, not a bridge.
	PrefillPods, DecodePods int
	// PrefillGPUs and DecodeGPUs are the GPUs per replica of each role, as
	// the cycle's variants state them; 0 where none is stated.
	PrefillGPUs, DecodeGPUs int
	// PromptTokens, OutputTokens and PrefixHitRate are the decode rows'
	// means, weighted by each row's request rate; HasShape is false when no
	// decode row read an output length.
	PromptTokens, OutputTokens, PrefixHitRate float64
	HasShape                                  bool
}

// Series is every cycle of the model q names, oldest first, read from the
// recording's saturation Analyze events without replaying them. q.Cycle is
// ignored: the series is the whole recording. A *QueryError is a model the
// recording does not hold or names ambiguously.
func Series(recording []byte, q StateQuery) ([]CycleSample, error) {
	ns, model, _, err := resolveModel(recording, q)
	if err != nil {
		return nil, err
	}
	read, err := learnrec.Read(recording)
	if err != nil {
		return nil, err
	}
	var out []CycleSample
	for _, ev := range read.Events {
		if ev.Kind != saturation.KindAnalyze {
			continue
		}
		var d saturation.AnalyzeEvent
		if err := ev.DecodeData(&d); err != nil {
			return nil, fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
		in, err := decodeInput(d.Input)
		if err != nil {
			return nil, fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
		if in.Namespace != ns || in.ModelID != model {
			continue
		}
		out = append(out, sampleOf(in, ev.Now))
	}
	return out, nil
}

// sampleOf is one cycle's sample of in, read at at.
func sampleOf(in domain.AnalyzerInput, at time.Time) CycleSample {
	s := CycleSample{At: at, ArrivalRate: in.ArrivalRate, RateKnown: in.ArrivalRate > 0}
	roles := rolesOf(in)
	for _, vs := range in.VariantStates {
		switch domain.CanonicalRole(vs.Role) {
		case domain.RolePrefill:
			s.PrefillGPUs = max(s.PrefillGPUs, vs.GPUsPerReplica)
		case domain.RoleDecode:
			s.DecodeGPUs = max(s.DecodeGPUs, vs.GPUsPerReplica)
		}
	}
	var weight, il, ol, hit, decodeRate float64
	for _, rm := range in.ReplicaMetrics {
		role := domain.CanonicalRole(roles[rm.VariantName])
		if prefill.Serving(rm) {
			switch role {
			case domain.RolePrefill:
				s.PrefillPods++
			case domain.RoleDecode:
				s.DecodePods++
			}
		}
		if role != domain.RoleDecode {
			continue
		}
		w := 0.0
		if rm.RequestRateKnown && rm.RequestRate > 0 {
			w = rm.RequestRate
			decodeRate += w
		}
		if !(rm.AvgOutputTokens > 0) || !(w > 0) {
			continue
		}
		weight += w
		il += w * rm.AvgInputTokens
		ol += w * rm.AvgOutputTokens
		hit += w * rm.PrefixCacheHitRate
	}
	if weight > 0 {
		s.PromptTokens, s.OutputTokens, s.PrefixHitRate, s.HasShape = il/weight, ol/weight, hit/weight, true
	}
	if !s.RateKnown {
		s.ArrivalRate = decodeRate
	}
	return s
}
