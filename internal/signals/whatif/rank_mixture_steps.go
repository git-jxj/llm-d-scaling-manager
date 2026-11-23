package whatif

import (
	"slices"
	"time"
)

// MixtureCycle is one cycle of a recording as the rank mixture reads it: what
// each prefill rank read over its window, and the prefill and decode pods'
// pooled figures (learnreplay.MixtureSeriesFrom). A cycle is a few seconds; a
// step (MixtureStepOf) pools the cycles of its window.
type MixtureCycle struct {
	At time.Time
	// Ranks is the prefill ranks that served, each keyed (Key) so that a
	// step can follow one rank across its cycles.
	Ranks []MixtureRankReading
	// Computed, Prompt and Queue are the prefill pods' histograms, summed
	// over them: computed tokens, whole prompt tokens and queue time.
	Computed, Prompt, Queue map[float64]float64
	// PrefillTTFT and DecodeTTFT are each role's mean TTFT, weighted by its
	// pods' request rates; 0 with none read.
	PrefillTTFT, DecodeTTFT float64
	// EvenWithinRows counts the ranks whose own rate was not recorded: a
	// row of several ranks split evenly between them (its share profile is
	// flatter than the fleet's).
	EvenWithinRows int
}

// MixtureRankReading is one rank's reading at one cycle: Key names the rank
// (its pod, and its index within a row of several).
type MixtureRankReading struct {
	Key string
	MixtureRank
}

// MixtureStepOf pools cycles into one step at at: each rank's rate is its mean
// over the cycles (absent counts as no request), its means weighted by its
// rate; each histogram the mean of the cycles'; each role's TTFT the mean of
// the cycles that read one. Rate is the ranks' rates summed and PromptMean
// their whole prompt weighted by rate. The router's figures are not read
// here: the caller sets them from its measurement.
func MixtureStepOf(cycles []MixtureCycle, at time.Time) MixtureStep {
	out := MixtureStep{At: at}
	n := float64(len(cycles))
	if n == 0 {
		return out
	}
	type acc struct {
		rate, pf, q, ttft, x, l float64
		wpf, wq, wttft, wx, wl  float64
	}
	byKey := map[string]*acc{}
	var keys []string
	out.Computed, out.Prompt, out.Queue = map[float64]float64{}, map[float64]float64{}, map[float64]float64{}
	var pTTFT, dTTFT, np, nd float64
	for _, c := range cycles {
		for _, r := range c.Ranks {
			a, ok := byKey[r.Key]
			if !ok {
				a = &acc{}
				byKey[r.Key] = a
				keys = append(keys, r.Key)
			}
			a.rate += r.Rate
			w := r.Rate
			if !(w > 0) {
				continue
			}
			if r.PrefillTime > 0 {
				a.pf += w * r.PrefillTime
				a.wpf += w
			}
			if r.QueueTime >= 0 && Finite(r.QueueTime) {
				a.q += w * r.QueueTime
				a.wq += w
			}
			if r.TTFT > 0 {
				a.ttft += w * r.TTFT
				a.wttft += w
			}
			if r.Computed > 0 {
				a.x += w * r.Computed
				a.wx += w
			}
			if r.Prompt > 0 {
				a.l += w * r.Prompt
				a.wl += w
			}
		}
		for b, v := range c.Computed {
			out.Computed[b] += v / n
		}
		for b, v := range c.Prompt {
			out.Prompt[b] += v / n
		}
		for b, v := range c.Queue {
			out.Queue[b] += v / n
		}
		if c.PrefillTTFT > 0 {
			pTTFT += c.PrefillTTFT
			np++
		}
		if c.DecodeTTFT > 0 {
			dTTFT += c.DecodeTTFT
			nd++
		}
	}
	slices.Sort(keys)
	div := func(a, b float64) float64 {
		if b > 0 {
			return a / b
		}
		return 0
	}
	var lw float64
	for _, k := range keys {
		a := byKey[k]
		q := div(a.q, a.wq)
		if !(a.wq > 0) {
			q = -1
		}
		r := MixtureRank{Rate: a.rate / n, PrefillTime: div(a.pf, a.wpf), QueueTime: q, TTFT: div(a.ttft, a.wttft),
			Computed: div(a.x, a.wx), Prompt: div(a.l, a.wl)}
		out.Ranks = append(out.Ranks, r)
		out.Rate += r.Rate
		if r.Prompt > 0 {
			out.PromptMean += r.Rate * r.Prompt
			lw += r.Rate
		}
	}
	out.PromptMean = div(out.PromptMean, lw)
	out.PrefillTTFT, out.DecodeTTFT = div(pTTFT, np), div(dTTFT, nd)
	return out
}
