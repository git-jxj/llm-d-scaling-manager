package learnreplay

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// ServedWorkWindow is the span the prefill fleet's served work is averaged
// over (servedWork.peaks): the planner's busiest stretch of load
// (whatifapi.RollingWindow), about the scale-up lag. Over two minutes a
// one-minute rate's counting noise is averaged down, while a stretch that
// long is work the pods kept up.
const ServedWorkWindow = 2 * time.Minute

// servedWorkMaxGap is the widest gap between two cycles that still continues
// a stretch: a missed cycle at the 15 s default, not a missing minute.
const servedWorkMaxGap = time.Minute

// servedWorkSample is one cycle of a prefill variant: its serving pods, and
// the computed-token rate summed over them, ok when every serving pod read
// one.
type servedWorkSample struct {
	at   time.Time
	pods int
	sum  float64
	ok   bool
}

// servedWork is a prefill variant's computed-token work over prefill.Retention,
// cycle by cycle: what the fleet's prefill pods were seen to compute, from
// the engines' own counter (domain.ReplicaMetrics.PrefillComputedTokenRate).
type servedWork struct {
	samples []servedWorkSample
}

// note keeps one cycle and forgets what is older than prefill.Retention.
func (w *servedWork) note(s servedWorkSample) {
	w.samples = append(w.samples, s)
	cut := s.at.Add(-prefill.Retention)
	i := 0
	for i < len(w.samples) && w.samples[i].at.Before(cut) {
		i++
	}
	w.samples = w.samples[i:]
}

// peaks is, at each pod count, the highest mean computed-token rate the
// variant's serving pods sustained over a stretch of ServedWorkWindow or
// more: consecutive cycles, each read on every pod, at that one count, none
// more than servedWorkMaxGap apart. A stretch shorter than the window, or
// broken, is no reading. The prefill pods compute no faster than their
// capacity, queued or not, so each is a lower bound on what that many pods
// can compute at the traffic they ran: the floor under a prefill knee priced
// from a lower bound (the planner's servedKneeFloor).
func (w *servedWork) peaks() map[int]float64 {
	out := map[int]float64{}
	n := len(w.samples)
	for i := range n {
		first := w.samples[i]
		if !first.ok || first.pods < 1 {
			continue
		}
		sum := 0.0
		for j := i; j < n; j++ {
			s := w.samples[j]
			if !s.ok || s.pods != first.pods || (j > i && s.at.Sub(w.samples[j-1].at) > servedWorkMaxGap) {
				break
			}
			sum += s.sum
			if s.at.Sub(first.at) >= ServedWorkWindow {
				if mean := sum / float64(j-i+1); mean > out[first.pods] {
					out[first.pods] = mean
				}
				break
			}
		}
	}
	return out
}
