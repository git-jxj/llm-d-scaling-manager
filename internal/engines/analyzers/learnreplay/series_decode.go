package learnreplay

import (
	"errors"
	"fmt"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// DecodeSeriesFrom is every cycle of the model q names, oldest first, as the
// capacity planner's experimental TPOT distribution reads it
// (whatif.DecodeCycle): the decode pods' pooled output-length histogram, each
// decode replica's token time and accepted drafts, and the running sequences
// of a rank. It is its own pass, read only when that model is asked for, so
// nothing else a series feeds moves. Its cycles are SeriesFrom's, in the
// same order. q must name the namespace and the model.
func DecodeSeriesFrom(src Source, q StateQuery, v SeriesVariants) ([]whatif.DecodeCycle, error) {
	if q.Namespace == "" || q.ModelID == "" {
		return nil, errors.New("learnreplay: the decode series needs the namespace and the model")
	}
	var out []whatif.DecodeCycle
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
		if in.Namespace == q.Namespace && in.ModelID == q.ModelID {
			out = append(out, decodeCycleOf(in, ev.Now, v))
		}
		return true, nil
	})
	return out, err
}

// decodeCycleOf is one cycle of in, read at at: the decode rows of the
// variant v names (every decode row where it names none), or, on a fleet
// with no decode role, its serving rows.
func decodeCycleOf(in domain.AnalyzerInput, at time.Time, v SeriesVariants) whatif.DecodeCycle {
	c := whatif.DecodeCycle{At: at, Output: map[float64]float64{}}
	roles := rolesOf(in)
	decode := false
	for _, rm := range in.ReplicaMetrics {
		if domain.CanonicalRole(roles[rm.VariantName]) == domain.RoleDecode {
			decode = true
			break
		}
	}
	var running float64
	for _, rm := range in.ReplicaMetrics {
		role := domain.CanonicalRole(roles[rm.VariantName])
		switch {
		case decode && role != domain.RoleDecode:
			continue
		case !decode && role == domain.RolePrefill:
			continue
		case v.Decode != "" && rm.VariantName != v.Decode:
			continue
		}
		if !prefill.Serving(rm) {
			continue
		}
		for _, b := range rm.OutputTokensBuckets {
			c.Output[b.UpperBound] += b.Rate
		}
		if rm.AvgITLKnown && whatif.Finite(rm.AvgITL) && rm.AvgITL > 0 && rm.RequestRateKnown && whatif.Finite(rm.RequestRate) && rm.RequestRate > 0 {
			g := whatif.DecodeGroup{Rate: rm.RequestRate, TokenTime: rm.AvgITL}
			if rm.SpecAcceptedPerStepKnown && whatif.Finite(rm.SpecAcceptedPerStep) && rm.SpecAcceptedPerStep >= 0 {
				g.Accepted, g.AcceptedKnown = rm.SpecAcceptedPerStep, true
				g.TokenTime = rm.AvgITL / (1 + rm.SpecAcceptedPerStep)
			}
			c.Groups = append(c.Groups, g)
		}
		if engines := rm.GroupEngines; len(engines) > 0 {
			for _, e := range engines {
				if e.RunningRequestsKnown {
					running += float64(e.RunningRequests)
					c.MostRunning = max(c.MostRunning, float64(e.RunningRequests))
					c.RanksReported++
				}
			}
			continue
		}
		if rm.RunningRequestsKnown {
			n := max(rm.Ranks(), 1)
			running += float64(rm.RunningRequests)
			c.MostRunning = max(c.MostRunning, float64(rm.RunningRequests)/float64(n))
			c.RanksReported += n
		}
	}
	if c.RanksReported > 0 {
		c.MeanRunning = running / float64(c.RanksReported)
	}
	return c
}
