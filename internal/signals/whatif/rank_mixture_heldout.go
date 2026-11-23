package whatif

import (
	"math"
	"time"
)

// HeldOutScore is a model's mean TTFT scored on held-out steps: Within the
// steps whose predicted mean is within the tolerance of the measured one
// (|predicted/measured - 1| <= tolerance), Low those read more than the
// tolerance low, of Steps scored; MedianRatio the median of predicted over
// measured; RateLo and RateHi the lowest and highest rate scored.
type HeldOutScore struct {
	Within, Low, Steps int
	MedianRatio        float64
	RateLo, RateHi     float64
}

// RankMixtureHeldOut scores the model on the self-check folds FitRankMixture
// cuts its fit window into (half to three quarters, three quarters to the
// end): each fold's core is fitted on the steps before its cut alone, priced
// at spread, its A0 fitted on those same steps, and its mean TTFT compared
// with the router's measured mean on each step after the cut. The spread is
// the window's (RankMixture.Spread), which the folds' p90 calibrated, so the
// p90 is not scored here; the mean scored is never fitted on the steps it is
// scored on. A fold that does not fit scores nothing.
func RankMixtureHeldOut(steps []MixtureStep, o RankMixtureOptions, spread, tolerance float64) HeldOutScore {
	var out HeldOutScore
	if len(steps) < 8 {
		return out
	}
	start := steps[0].At.Add(-1)
	span := steps[len(steps)-1].At.Sub(start)
	cut := func(f float64) time.Time { return start.Add(time.Duration(f * float64(span))) }
	var ratios []float64
	for _, c := range [][3]float64{{0, 0.5, 0.75}, {0, 0.75, 1}} {
		train := stepsBetween(steps, cut(c[0]), cut(c[1]))
		test := stepsBetween(steps, cut(c[1]), cut(c[2]))
		fm, _, err := FitCore(train, o)
		if err != nil {
			continue
		}
		fm.Spread = spread
		a0, ok := a0Of(drawSteps(fm, train), spread)
		if !ok {
			continue
		}
		for i, p := range drawSteps(fm, test) {
			if p.priceError || !(p.mean > 0) {
				continue
			}
			m, _ := p.d.Stats(a0, spread*p.beta)
			if !Finite(m) {
				continue
			}
			r := m / p.mean
			ratios = append(ratios, r)
			switch {
			case math.Abs(r-1) <= tolerance:
				out.Within++
			case r < 1:
				out.Low++
			}
			rate := test[i].Rate
			if out.Steps == 0 || rate < out.RateLo {
				out.RateLo = rate
			}
			out.RateHi = max(out.RateHi, rate)
			out.Steps++
		}
	}
	if len(ratios) > 0 {
		out.MedianRatio = medianOf(ratios)
	}
	return out
}
