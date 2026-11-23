package whatifapi

import (
	"fmt"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// A report's OVER-PROVISIONED verdict says a role has a replica to spare. The
// model can say so of a fleet whose TTFT, as the router measured it at the
// load judged, was over its target: the fleet missed the target it would be
// cut on. WithholdOverOnMeasured makes such a verdict NOT JUDGED.

// measuredOverWords says why an over-provisioned verdict is withheld.
const measuredOverWords = "a fleet that missed its TTFT target has nothing to spare"

// MeasuredTTFTAtLoad is the TTFT the router measured at the statistic q (0
// the mean) over the cycles a load judged at stat was read from: every cycle
// at or above rate for the p95 and the peak, every one with traffic for the
// mean. The cycles' router histograms are pooled, so a busy cycle weighs by
// its requests. ok is false with no router histogram at those cycles, or
// too few requests in one cycle's window to read the statistic.
func MeasuredTTFTAtLoad(series []SeriesSample, latency []LatencySample, stat string, rate, q float64) (float64, bool) {
	if len(series) != len(latency) || !(rate > 0) {
		return 0, false
	}
	floor := rate
	if stat == LoadMean {
		floor = 0
	}
	pooled := map[float64]float64{}
	var sum, count, window float64
	n := 0
	for i, x := range series {
		l := latency[i].Router
		if l == nil || !(x.Rate > 0) || x.Rate < floor {
			continue
		}
		for _, b := range l.TTFTBuckets {
			pooled[b.UpperBound] += b.Rate
		}
		sum, count = sum+l.TTFTSumRate, count+l.TTFTCountRate
		window += measureWindowOf(latency[i], MeasuredRouter)
		n++
	}
	if n == 0 {
		return 0, false
	}
	if q == 0 {
		// The pooled rate over one cycle's window: windows overlap, so the
		// sum over cycles is not a count.
		if !(count > 0) || count/float64(n)*window/float64(n) < minRequestsFor(0) {
			return 0, false
		}
		return sum / count, true
	}
	h, ok := histOf(bucketsOf(pooled))
	if !ok || h.total/float64(n)*window/float64(n) < minRequestsFor(q) {
		return 0, false
	}
	v := h.quantile(q)
	if !whatif.Finite(v) {
		// Past the top bucket: at least the top bound.
		return h.top(), h.top() > 0
	}
	return v, true
}

// bucketsOf is a pooled histogram, bound to rate, as buckets.
func bucketsOf(pooled map[float64]float64) []domain.LatencyBucket {
	out := make([]domain.LatencyBucket, 0, len(pooled))
	for ub, rate := range pooled {
		out = append(out, domain.LatencyBucket{UpperBound: ub, Rate: rate})
	}
	return out
}

// WithholdOverOnMeasured makes NOT JUDGED each shape's OVER-PROVISIONED
// verdict at the load judged where the router measured the TTFT over its
// target there (MeasuredTTFTAtLoad), and flags the shape
// FlagMeasuredTTFTOverTarget. A growth line is a what-if at a rate no cycle
// ran at, and is left as it is.
func (r *ReportResponse) WithholdOverOnMeasured(series []SeriesSample, latency []LatencySample) {
	t := r.SLO.TTFT
	if t == nil || r.Load.Seen == nil || r.Load.Source != "observed" {
		return
	}
	q, err := parseStatistic("ttft.statistic", t.Statistic)
	if err != nil {
		return
	}
	m, ok := MeasuredTTFTAtLoad(series, latency, r.Load.Statistic, *r.Load.Seen, q)
	if !ok || !(m > t.Target) {
		return
	}
	for i := range r.Shapes {
		s := &r.Shapes[i]
		v := &s.Verdict
		if v.Verdict != VerdictOver {
			continue
		}
		v.Verdict = VerdictNotJudged
		why := fmt.Sprintf("the router measured TTFT %s %s against a %s target at the load judged", t.Statistic, secondsWords(m), secondsWords(t.Target))
		v.Reason += ". " + why + ": " + measuredOverWords
		v.Summary = fmt.Sprintf("%s: the model finds a replica to spare, but %s; %s.", v.Verdict, why, measuredOverWords)
		v.Shrink = []string{}
		s.Flags = appendNew(s.Flags, FlagMeasuredTTFTOverTarget)
		r.ExitCode = max(r.ExitCode, whatif.ExitRefused)
	}
}
