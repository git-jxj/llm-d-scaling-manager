package whatif

import (
	"math"
	"testing"
)

// Within rounding of a prefill pod's knee its utilization reads exactly 1, the
// processor-sharing stretch overflows, and a stage's mean is +Inf. Its
// quantile is +Inf, read at once: searched, the bracket started at +Inf and
// doubled 200 times, every read of the tail ran the gamma series to
// maxSeriesTerms for a NaN, and a plan at 12 prefill and 19 decode replicas
// ran past 30 minutes.
func TestTailQuantileOfAnInfiniteMeanReadsNoTail(t *testing.T) {
	reads := 0
	exceeds := func(float64) float64 { reads++; return 1 }
	if got := tailQuantile(exceeds, math.Inf(1), 0.1); !math.IsInf(got, 1) {
		t.Fatalf("quantile %g, want +Inf", got)
	}
	if reads != 0 {
		t.Fatalf("the tail was read %d times for a mean of +Inf, want 0", reads)
	}
	// The stage itself: a gamma part of infinite mean and shape 0.25, as the
	// 12/19 plan built it at a utilization of exactly 1.
	m := stageMix{w: []float64{1}, b: []block{{own: newOwnTime(math.Inf(1), 2)}}}
	if got := m.quantile(0.1); !math.IsInf(got, 1) {
		t.Fatalf("stage quantile %g, want +Inf", got)
	}
}

// countingQuantiler is block 1 of the union bound, +Inf at every alpha, that
// records the alphas asked.
type countingQuantiler struct{ alphas []float64 }

func (c *countingQuantiler) quantile(alpha float64) float64 {
	c.alphas = append(c.alphas, alpha)
	return math.Inf(1)
}

// When no point of the split's grid is finite the split is +Inf, at the beta
// the search returned before, and block 1 is never asked a quantile outside
// (0, 1-q): the golden section started from log(beta) = 0 and asked alphas
// below 0, each a bracket doubled 200 times.
func TestSplitOfAnInfiniteBlockStaysInItsDomain(t *testing.T) {
	q := 0.9
	b1 := &countingQuantiler{}
	pd := 0.5
	v, beta := split(b1, expWait(pd, 5), q)
	if !math.IsInf(v, 1) {
		t.Fatalf("split %g, want +Inf", v)
	}
	if want := (1 - q) * (1 - 1e-9); beta != want {
		t.Fatalf("beta %g, want %g (the grid's top)", beta, want)
	}
	for _, a := range b1.alphas {
		if !(a > 0 && a < 1-q) {
			t.Fatalf("block 1 asked at alpha %g, outside (0, %g); %d asks", a, 1-q, len(b1.alphas))
		}
	}
	if n := len(b1.alphas); n != 49 {
		t.Fatalf("%d quantiles asked, want the grid's 49", n)
	}
}

// A gamma tail at a non-finite argument is NaN at once, the value it reached
// after maxSeriesTerms steps.
func TestGammaAtNonFiniteArgumentsIsNaN(t *testing.T) {
	for _, c := range []struct{ k, x float64 }{
		{0.25, math.Inf(1)}, {0.25, math.NaN()}, {math.Inf(1), 3}, {math.NaN(), 3},
	} {
		if v := lnGammaQFraction(c.k, c.x); !math.IsNaN(v) {
			t.Errorf("lnGammaQFraction(%g, %g) = %g, want NaN", c.k, c.x, v)
		}
		if v := lnGammaPSeries(c.k, c.x); !math.IsNaN(v) {
			t.Errorf("lnGammaPSeries(%g, %g) = %g, want NaN", c.k, c.x, v)
		}
	}
}

// gammaTermsBound is the most terms or steps one gamma read may take at the
// smallest spread priced as a gamma: under 1e5 are needed at k = 1e8, and
// maxSeriesTerms, 1e6, is a read that never converged.
const gammaTermsBound = 200_000

// The least spread the model prices as a gamma is minPromptCV, shape
// k = 1/minPromptCV^2 = 1e8; below it every builder prices the time as
// constant. Near x = k a gamma read takes about sqrt(k) terms, so each read
// at that shape converges well inside maxSeriesTerms; a smaller floor
// (cv 1e-7, k 1e14) would need some 1e7 and run every read to the bound.
func TestGammaReadsAreBoundedAtTheSmallestSpread(t *testing.T) {
	k := 1 / (minPromptCV * minPromptCV)
	s := math.Sqrt(k)
	for _, x := range []float64{k - 3*s, k - 1, k, k + 1, k + 1.5, k + 3*s, k + 10*s} {
		var v float64
		var n int
		if x < k+1 {
			v, n = lnGammaPSeriesTerms(k, x)
		} else {
			v, n = lnGammaQFractionSteps(k, x)
		}
		t.Logf("k %.3g, x - k %+.4g: ln %.6g in %d terms", k, x-k, v, n)
		if !Finite(v) || n > gammaTermsBound {
			t.Errorf("k %g, x %g: %g in %d terms, want a finite value within %d", k, x, v, n, gammaTermsBound)
		}
	}
	// Every builder of a gamma own time floors the spread at minPromptCV.
	if o := gammaPart(1, 1e-12); !o.det {
		t.Errorf("gammaPart at cv 1e-6 is a gamma of shape %g, want constant", o.k)
	}
	pl, u := Prefill{VP: 30000, FixedLatency: 0.01, Replicas: 1, PromptTokens: 1024, PromptCV: 1e-6}.check()
	if u != nil {
		t.Fatalf("prefill refused: %v", u)
	}
	if o := pl.own(); !o.det {
		t.Errorf("a prefill at cv 1e-6 owns a gamma of shape %g, want constant", o.k)
	}
}
