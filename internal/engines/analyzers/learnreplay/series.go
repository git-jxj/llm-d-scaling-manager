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
	// cycle's input carries it. RateKnown is false when the input carries
	// none (no router series) while decode rows served requests: the rate is
	// then the request rates of every decode row of the model, summed. A
	// cycle with neither is idle, its rate a known 0.
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
	// OtherPods is the serving pods of prefill and decode variants other
	// than the counted ones: the model's rate served by pods the counts
	// leave out.
	OtherPods int
}

// SeriesVariants names the variants a series counts: the prefill and the
// decode variant a plan prices. An empty name counts every variant of the
// role.
type SeriesVariants struct {
	Prefill, Decode string
}

// Series is every cycle of the model q names, oldest first, read from the
// recording's saturation Analyze events without replaying them. Pods, GPUs
// and lengths are the variants v names, so that what the fleet had is
// counted for the variants a plan sizes. q.Cycle is ignored: the series is
// the whole recording. A *QueryError is a model the recording does not hold
// or names ambiguously.
func Series(recording []byte, q StateQuery, v SeriesVariants) ([]CycleSample, error) {
	return SeriesFrom(BytesSource(recording), q, v)
}

// SeriesFrom is Series of the recording src opens, never held. A query that
// names the namespace and the model is read in one pass, and resolved
// (resolveModel) only when the pass found none of its cycles, for the
// error; any other in two, resolved first.
func SeriesFrom(src Source, q StateQuery, v SeriesVariants) ([]CycleSample, error) {
	ns, model := q.Namespace, q.ModelID
	if ns == "" || model == "" {
		var err error
		if ns, model, _, err = resolveModel(src, q); err != nil {
			return nil, err
		}
	}
	var out []CycleSample
	err := scan(src, func(ev learnrec.RawEvent) (bool, error) {
		if ev.Kind != saturation.KindAnalyze {
			return true, nil
		}
		var d saturation.AnalyzeEvent
		if err := ev.DecodeData(&d); err != nil {
			return false, fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
		in, err := decodeInput(d.Input)
		if err != nil {
			return false, fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
		if in.Namespace == ns && in.ModelID == model {
			out = append(out, sampleOf(in, ev.Now, v))
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		if _, _, _, err := resolveModel(src, q); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// sampleOf is one cycle's sample of in, read at at, of the variants v
// names.
func sampleOf(in domain.AnalyzerInput, at time.Time, v SeriesVariants) CycleSample {
	s := CycleSample{At: at, ArrivalRate: in.ArrivalRate}
	roles := rolesOf(in)
	counted := func(variant, role string) bool {
		switch role {
		case domain.RolePrefill:
			return v.Prefill == "" || variant == v.Prefill
		case domain.RoleDecode:
			return v.Decode == "" || variant == v.Decode
		}
		return false
	}
	for _, vs := range in.VariantStates {
		if !counted(vs.VariantName, domain.CanonicalRole(vs.Role)) {
			continue
		}
		switch domain.CanonicalRole(vs.Role) {
		case domain.RolePrefill:
			s.PrefillGPUs = max(s.PrefillGPUs, vs.GPUsPerReplica)
		case domain.RoleDecode:
			s.DecodeGPUs = max(s.DecodeGPUs, vs.GPUsPerReplica)
		}
	}
	var weight, il, ol, hit, modelRate float64
	for _, rm := range in.ReplicaMetrics {
		role := domain.CanonicalRole(roles[rm.VariantName])
		if role == domain.RoleDecode && rm.RequestRateKnown && rm.RequestRate > 0 {
			modelRate += rm.RequestRate
		}
		if !counted(rm.VariantName, role) {
			if (role == domain.RolePrefill || role == domain.RoleDecode) && prefill.Serving(rm) {
				s.OtherPods++
			}
			continue
		}
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
	s.RateKnown = in.ArrivalRate > 0 || modelRate == 0
	if !s.RateKnown {
		s.ArrivalRate = modelRate
	}
	return s
}
