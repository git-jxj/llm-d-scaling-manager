package learnreplay

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// MixtureSeriesFrom is every cycle of the model q names, oldest first, as the
// capacity planner's experimental rank-mixture TTFT model reads it
// (whatif.MixtureCycle): the prefill ranks of the variants v names, and the
// prefill and decode pods' pooled histograms and TTFT. It is its own pass,
// read only when that model is asked for, so nothing else a series feeds
// moves. Its cycles are SeriesFrom's, in the same order. q must name the
// namespace and the model.
func MixtureSeriesFrom(src Source, q StateQuery, v SeriesVariants) ([]whatif.MixtureCycle, error) {
	if q.Namespace == "" || q.ModelID == "" {
		return nil, errors.New("learnreplay: the rank mixture's series needs the namespace and the model")
	}
	var out []whatif.MixtureCycle
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
			out = append(out, mixtureCycleOf(in, ev.Now, v))
		}
		return true, nil
	})
	return out, err
}

// mixtureCycleOf is one cycle of in, read at at. A prefill row of a
// LeaderWorkerSet group whose engines each recorded their own rate
// (domain.EngineReading.RequestRateKnown) gives one rank per engine; any
// other row gives its ranks (domain.ReplicaMetrics.Ranks), the row's rate
// split evenly between them and each with the row's means, counted in
// EvenWithinRows.
func mixtureCycleOf(in domain.AnalyzerInput, at time.Time, v SeriesVariants) whatif.MixtureCycle {
	c := whatif.MixtureCycle{At: at, Computed: map[float64]float64{}, Prompt: map[float64]float64{}, Queue: map[float64]float64{}}
	roles := rolesOf(in)
	var pw, pt, dw, dt float64
	for _, rm := range in.ReplicaMetrics {
		role := domain.CanonicalRole(roles[rm.VariantName])
		switch role {
		case domain.RolePrefill:
			if v.Prefill != "" && rm.VariantName != v.Prefill {
				continue
			}
		case domain.RoleDecode:
			if v.Decode != "" && rm.VariantName != v.Decode {
				continue
			}
		default:
			continue
		}
		if !prefill.Serving(rm) {
			continue
		}
		rate := 0.0
		if rm.RequestRateKnown && whatif.Finite(rm.RequestRate) && rm.RequestRate > 0 {
			rate = rm.RequestRate
		}
		if role == domain.RoleDecode {
			if rm.AvgTTFTKnown && rate > 0 {
				dw += rate
				dt += rate * rm.AvgTTFT
			}
			continue
		}
		if rm.AvgTTFTKnown && rate > 0 {
			pw += rate
			pt += rate * rm.AvgTTFT
		}
		for _, b := range rm.ComputedTokensBuckets {
			c.Computed[b.UpperBound] += b.Rate
		}
		for _, b := range rm.PromptTokensBuckets {
			c.Prompt[b.UpperBound] += b.Rate
		}
		for _, b := range rm.QueueTimeBuckets {
			c.Queue[b.UpperBound] += b.Rate
		}
		if engines := rm.GroupEngines; len(engines) > 0 && engineRatesRead(engines) {
			for _, e := range engines {
				c.Ranks = append(c.Ranks, whatif.MixtureRankReading{Key: e.PodName, MixtureRank: rankOf(e.RequestRate,
					e.AvgPrefillTime, e.AvgPrefillTimeKnown, e.AvgQueueTime, e.AvgQueueTimeKnown, e.AvgTTFT, e.AvgTTFTKnown,
					e.PrefillComputedTokenRate, e.PrefillComputedTokenRateKnown, e.AvgInputTokens)})
			}
			continue
		}
		n := rm.Ranks()
		if n > 1 {
			c.EvenWithinRows += n
		}
		for i := range n {
			r := rankOf(rate/float64(n), rm.AvgPrefillTime, rm.AvgPrefillTimeKnown, rm.AvgQueueTime, rm.AvgQueueTimeKnown,
				rm.AvgTTFT, rm.AvgTTFTKnown, rm.PrefillComputedTokenRate/float64(n), rm.PrefillComputedTokenRateKnown, rm.AvgInputTokens)
			c.Ranks = append(c.Ranks, whatif.MixtureRankReading{Key: rm.PodName + "#" + strconv.Itoa(i), MixtureRank: r})
		}
	}
	if pw > 0 {
		c.PrefillTTFT = pt / pw
	}
	if dw > 0 {
		c.DecodeTTFT = dt / dw
	}
	return c
}

// engineRatesRead says every engine of a group recorded its own request rate.
func engineRatesRead(engines []domain.EngineReading) bool {
	for _, e := range engines {
		if !e.RequestRateKnown {
			return false
		}
	}
	return true
}

// rankOf is one rank's reading: a mean not known reads 0, a queue time not
// known -1, and the computed prompt is the computed rate over the request rate.
func rankOf(rate, pf float64, pfKnown bool, q float64, qKnown bool, ttft float64, ttftKnown bool,
	computed float64, computedKnown bool, prompt float64) whatif.MixtureRank {
	r := whatif.MixtureRank{Rate: max(rate, 0), QueueTime: -1}
	if !whatif.Finite(r.Rate) {
		r.Rate = 0
	}
	if pfKnown && whatif.Finite(pf) {
		r.PrefillTime = pf
	}
	if qKnown && whatif.Finite(q) && q >= 0 {
		r.QueueTime = q
	}
	if ttftKnown && whatif.Finite(ttft) {
		r.TTFT = ttft
	}
	if computedKnown && whatif.Finite(computed) && r.Rate > 0 {
		r.Computed = computed / r.Rate
	}
	if whatif.Finite(prompt) {
		r.Prompt = prompt
	}
	return r
}
