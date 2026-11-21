package whatif

import (
	"math"
	"slices"
)

// The censored fit's search range for the lognormal's sigma: a TPOT
// spread from almost none to a tail far past any engine's.
const (
	censoredSigmaLo = 0.01
	censoredSigmaHi = 2.5
)

// TPOTRatioCensored learns R_q as TPOTRatioFromWindows does, but reads each
// window by interval-censored maximum likelihood rather than at the q
// bucket's upper edge (#279): a lognormal with the window's own mean
// (_sum/_count) is fitted to its bucket counts, each count the mass between
// two edges, and the window's ratio is that lognormal's q-quantile over its
// mean, exp(sigma*z_q - sigma^2/2). The upper edge stays the ratio's top:
// a window never reads above the edge reading (readWindow). R_q is the
// median of the windows' ratios, with no order-statistic margin: the fit
// already reads the bucket's mass, not its edge. The flags and the
// refusals are TPOTRatioFromWindows's, and ByFill is learned the same way
// by fill.
func TPOTRatioCensored(windows []TPOTWindow, q float64) (TPOTRatio, []Flag, *Unanswerable) {
	edge, flags, refused := TPOTRatioFromWindows(windows, q)
	if refused != nil {
		return edge, flags, refused
	}
	global, ok := censoredMedian(windows, q)
	if !ok {
		return edge, flags, nil
	}
	r := TPOTRatio{Value: max(1, min(global, edge.Value))}
	if len(edge.ByFill) > 0 {
		r.ByFill = make([]FillRatio, len(edge.ByFill))
		for i, b := range edge.ByFill {
			r.ByFill[i] = b
			var in []TPOTWindow
			for _, w := range windows {
				if w.FillKnown && Finite(w.Fill) && min(fillRatioBuckets-1, max(0, int(math.Floor(w.Fill/FillRatioBucketWidth)))) == i {
					in = append(in, w)
				}
			}
			r.ByFill[i].Value = r.Value
			if v, ok := censoredMedian(in, q); ok && !b.Thin {
				r.ByFill[i].Value = max(1, min(v, b.Learned))
			}
		}
		// Raised, never falling, as the fill rises: as fillRatios keeps it.
		for i := 1; i < len(r.ByFill); i++ {
			r.ByFill[i].Value = max(r.ByFill[i].Value, r.ByFill[i-1].Value)
		}
	}
	return r, flags, nil
}

// censoredMedian is the median of the windows' censored ratios, ok false
// where no window fits.
func censoredMedian(windows []TPOTWindow, q float64) (float64, bool) {
	var rs []float64
	for _, w := range windows {
		if r, ok := censoredWindow(w, q); ok {
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 {
		return 0, false
	}
	slices.Sort(rs)
	n := len(rs)
	if n%2 == 1 {
		return rs[n/2], true
	}
	return (rs[n/2-1] + rs[n/2]) / 2, true
}

// censoredWindow is one window's q-quantile over its mean, from the
// lognormal with the window's mean that best explains its bucket counts,
// capped at the edge reading; ok false for a window with no observation,
// no positive mean, or buckets that do not read.
func censoredWindow(w TPOTWindow, q float64) (float64, bool) {
	if !(w.Count > 0) || !(w.Sum > 0) || len(w.Buckets) == 0 {
		return 0, false
	}
	rd, empty, err := readWindow(w, q)
	if err != nil || empty {
		return 0, false
	}
	mean := w.Sum / w.Count
	type cell struct{ lo, hi, n float64 }
	var cells []cell
	lo, prev, running := 0.0, 0.0, 0.0
	for _, b := range w.Buckets {
		running = max(running, b.Count)
		if n := running - prev; n > 0 {
			cells = append(cells, cell{lo: lo, hi: b.UpperEdge, n: n})
		}
		lo, prev = b.UpperEdge, running
	}
	if len(cells) == 0 {
		return 0, false
	}
	ll := func(sigma float64) float64 {
		mu := math.Log(mean) - sigma*sigma/2
		cdf := func(x float64) float64 {
			switch {
			case x <= 0:
				return 0
			case math.IsInf(x, 1):
				return 1
			}
			return 0.5 * math.Erfc(-(math.Log(x)-mu)/(sigma*math.Sqrt2))
		}
		s := 0.0
		for _, c := range cells {
			p := cdf(c.hi) - cdf(c.lo)
			s += c.n * math.Log(max(p, 1e-300))
		}
		return s
	}
	// A grid in log sigma, then a golden-section search around its best.
	const steps = 64
	best, bestLL := censoredSigmaLo, math.Inf(-1)
	ratio := math.Pow(censoredSigmaHi/censoredSigmaLo, 1.0/steps)
	for i, s := 0, censoredSigmaLo; i <= steps; i, s = i+1, s*ratio {
		if v := ll(s); v > bestLL {
			best, bestLL = s, v
		}
	}
	a, b := max(best/ratio, censoredSigmaLo), min(best*ratio, censoredSigmaHi)
	g := (math.Sqrt(5) - 1) / 2
	for range 60 {
		c, d := b-g*(b-a), a+g*(b-a)
		if ll(c) > ll(d) {
			b = d
		} else {
			a = c
		}
	}
	sigma := (a + b) / 2
	z := math.Sqrt2 * math.Erfinv(2*q-1)
	r := math.Exp(sigma*z - sigma*sigma/2)
	if !Finite(r) || !(r > 0) {
		return 0, false
	}
	return min(r, rd.ratio), true
}
