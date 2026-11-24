package whatifapi

import (
	"errors"
	"fmt"
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// Load as concurrency. Little's law ties a rate to the mean number of
// requests in the system, L = rate x W, with W a request's mean time in it:
// TTFT mean + (OL - 1) x TPOT mean, the report's e2eMeanSum. A load generator
// that keeps N requests open (closed loop) drives the rate at which L = N.

// unitRequests is a concurrency's unit: requests in the system.
const unitRequests = "requests"

// MaxConcurrency is the largest concurrency a load may ask, in requests.
const MaxConcurrency = 1 << 20

// Concurrency is the mean number of requests in the system at a rate, by
// Little's law: an average over time, not a peak. Each part is refused where
// a mean it rests on is, and every part is refused at or past the knee,
// where requests queue without bound (Unbounded).
type Concurrency struct {
	// Fleet is rate x (TTFT mean + (OL - 1) x TPOT mean): the requests in
	// the system on average, from arrival to the last token.
	Fleet Figure `json:"fleet"`
	// Prefill is rate x the prefill part of the mean TTFT: the wait on the
	// prefill side (engine queue, the router's gate and its burst) and the
	// prompt's own prefill as the planner prices it. Absent where the TTFT
	// carries no parts.
	Prefill *Figure `json:"prefill,omitempty"`
	// Decode is rate x OL x TPOT mean, the sequences decoding at once over
	// the fleet, and DecodePerPod the same over the decode replicas.
	Decode       *Figure `json:"decode,omitempty"`
	DecodePerPod *Figure `json:"decodePerPod,omitempty"`
	// Unbounded says the rate is at or past the knee: the queue grows
	// without bound there, and no mean concurrency holds.
	Unbounded bool `json:"unbounded,omitempty"`
}

// unboundedDetail is what a concurrency at or past the knee says.
const unboundedDetail = "at or past the knee requests queue without bound: no mean concurrency holds"

// ConcurrencyAt is the mean concurrency at rate, from the mean TTFT and the
// mean TPOT priced there, ol output tokens a request and decodeReplicas
// decode replicas (0 leaves the per-pod figure out). pastKnee says the rate
// is at or past the fleet's knee: every part is then refused as unbounded.
// The figures carry no flags of their own: they rest on the mean TTFT and
// TPOT beside them, whose flags say what those rest on.
func ConcurrencyAt(rate float64, ttftMean, tpotMean whatif.Result, ol float64, decodeReplicas int, pastKnee bool) *Concurrency {
	if pastKnee {
		return &Concurrency{Fleet: refusedRequests(string(whatif.Saturated), unboundedDetail), Unbounded: true}
	}
	if !(ol > 0) {
		return &Concurrency{Fleet: refusedRequests(string(whatif.InvalidInput), "no output length to price the time in the system at")}
	}
	c := &Concurrency{}
	t, tok := valueOf(ttftMean)
	p, pok := valueOf(tpotMean)
	switch {
	case !tok:
		c.Fleet = refusedConcurrency(ttftMean, "the mean TTFT")
	case !pok:
		c.Fleet = refusedConcurrency(tpotMean, "the mean TPOT")
	default:
		c.Fleet = requestsFigure(rate * (t + max(ol-1, 0)*p))
		c.Fleet.UpperBound = upperBoundTTFT(ttftMean)
	}
	if v, ok := ttftMean.(whatif.TTFT); ok {
		pp := v.Parts
		if part := pp.EngineWait + pp.RouterWait + pp.BurstWait + pp.OwnPrefill; part > 0 && rate > 0 {
			f := requestsFigure(rate * part)
			f.UpperBound = TTFTUpperBound(v.Flags)
			c.Prefill = &f
		}
	}
	if pok {
		d := requestsFigure(rate * ol * p)
		c.Decode = &d
		if decodeReplicas > 0 {
			per := requestsFigure(*d.Value / float64(decodeReplicas))
			c.DecodePerPod = &per
		}
	}
	return c
}

// requestsFigure is n requests.
func requestsFigure(n float64) Figure {
	return Figure{Value: ptr(n), Unit: unitRequests}
}

// refusedRequests is a concurrency refused for reason.
func refusedRequests(reason, detail string) Figure {
	return Figure{Refused: &Refusal{Reason: reason, Detail: detail}}
}

// refusedConcurrency is a concurrency refused for r's reason, what naming
// the mean refused.
func refusedConcurrency(r whatif.Result, what string) Figure {
	if u, ok := r.(whatif.Unanswerable); ok {
		return refusedRequests(string(u.Reason), what+" is refused: "+u.Detail)
	}
	return refusedRequests(string(whatif.InvalidInput), what+" is not priced")
}

// upperBoundTTFT says a mean TTFT is priced as an upper bound.
func upperBoundTTFT(r whatif.Result) bool {
	v, ok := r.(whatif.TTFT)
	return ok && TTFTUpperBound(v.Flags)
}

// ConcurrencyRate is a concurrency asked, translated into the rate that
// carries it: Rate, at which rate x W(rate) is Concurrency. AtKnee says the
// fleet holds fewer requests than that wherever its time in system prices:
// Rate is then the highest rate it prices, the knee, or short of it the
// rate where the mean time in system is first refused. A closed-loop load
// at that concurrency saturates the fleet there: more concurrency adds
// queueing, not throughput.
type ConcurrencyRate struct {
	Concurrency float64 `json:"concurrency"`
	Rate        float64 `json:"rate"`
	AtKnee      bool    `json:"atKnee,omitempty"`
}

// errConcurrencyUnpriced is a time in system refused at every rate under the
// knee.
var errConcurrencyUnpriced = errors.New("the mean time in system is refused")

// concurrencyTolerance is the bisection's stop, a share of the knee.
const concurrencyTolerance = 1e-7

// RateAtConcurrency solves rate x w(rate) = l for the rate on [0, knee):
// w is the mean time in system at a rate, in seconds, ok false where it is
// refused, which the search takes as past the answer (w rises with the rate,
// and a refusal under the knee is the queue saturating). The solution is
// unique since rate x w(rate) rises with the rate. Where no rate that
// prices holds l requests, the answer is AtKnee (ConcurrencyRate). It is an
// error where l or knee is not a finite number above 0, or where w is
// refused near a rate of 0, which leaves nothing to translate with.
func RateAtConcurrency(l, knee float64, w func(rate float64) (float64, bool)) (ConcurrencyRate, error) {
	out := ConcurrencyRate{Concurrency: l}
	switch {
	case !(l > 0) || math.IsInf(l, 0):
		return out, fmt.Errorf("concurrency %g, want a finite number of requests above 0", l)
	case !(knee > 0) || math.IsInf(knee, 0):
		return out, errors.New("no knee to bound the rate a concurrency translates into")
	}
	if _, ok := w(knee * concurrencyTolerance); !ok {
		return out, errConcurrencyUnpriced
	}
	lo, hi, reached := 0.0, knee, false
	for i := 0; i < 100 && hi-lo > concurrencyTolerance*knee; i++ {
		mid := (lo + hi) / 2
		v, ok := w(mid)
		switch {
		case ok && mid*v >= l:
			hi, reached = mid, true
		case ok:
			lo = mid
		default:
			hi = mid
		}
	}
	out.Rate, out.AtKnee = (lo+hi)/2, !reached
	if !reached && hi == knee {
		out.Rate = knee
	}
	return out, nil
}

// ConcurrencyUnpriced says err is RateAtConcurrency's refusal for a time in
// system refused at every rate.
func ConcurrencyUnpriced(err error) bool { return errors.Is(err, errConcurrencyUnpriced) }

// ConcurrencyWords is a concurrency in plain words: "about 12 requests in the
// system on average", "unbounded (at or past the knee)", or the refusal.
func ConcurrencyWords(c *Concurrency) string {
	switch {
	case c == nil:
		return ""
	case c.Unbounded:
		return "unbounded (at or past the knee, requests queue without bound)"
	case c.Fleet.Value != nil:
		return fmt.Sprintf("about %s requests in the system on average", ConcurrencyNumber(*c.Fleet.Value))
	case c.Fleet.Refused != nil:
		return "refused (" + c.Fleet.Refused.Reason + ")"
	}
	return ""
}

// ConcurrencyNumber is a mean concurrency as printed: one decimal under 10,
// else whole requests.
func ConcurrencyNumber(x float64) string {
	if x < 10 {
		return fmt.Sprintf("%.1f", x)
	}
	return fmt.Sprintf("%.0f", x)
}
