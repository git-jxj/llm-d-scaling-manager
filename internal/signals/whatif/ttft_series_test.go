package whatif

import (
	"math"
	"math/big"
	"testing"
	"time"
)

// refLogSeries is ln sum_{n>=0} y^n / (n! (n+k)) summed term by term in
// 256-bit floats: the reference logSeries is checked against.
func refLogSeries(t *testing.T, k, y float64) float64 {
	t.Helper()
	const prec = 256
	bk, by := new(big.Float).SetPrec(prec).SetFloat64(k), new(big.Float).SetPrec(prec).SetFloat64(y)
	p := new(big.Float).SetPrec(prec).SetInt64(1) // y^n / n!
	sum := new(big.Float).SetPrec(prec)
	term, den := new(big.Float).SetPrec(prec), new(big.Float).SetPrec(prec)
	for n := 0; ; n++ {
		if n > 0 {
			p.Mul(p, by)
			p.Quo(p, new(big.Float).SetPrec(prec).SetInt64(int64(n)))
		}
		den.SetInt64(int64(n))
		den.Add(den, bk)
		term.Quo(p, den)
		sum.Add(sum, term)
		if float64(n) > y && term.MantExp(nil) < sum.MantExp(nil)-200 {
			break
		}
		if n > 10_000_000 {
			t.Fatalf("reference sum at k %g, y %g did not converge", k, y)
		}
	}
	m := new(big.Float)
	e := sum.MantExp(m)
	mf, _ := m.Float64()
	return math.Log(mf) + float64(e)*math.Ln2
}

// logSeries is the sum's log to within float64 precision on both sides of
// asymptoticSeriesFrom, where it moves from the direct sum to the expansion,
// and at the y a long-prompt TTFT tail asks for, including k at y/4, where
// the expansion's first terms alternate in sign. The direct sum's own error
// grows with y (each term's Lgamma is good to a unit in the last place of a
// number near y ln y), so its bound is relative to that; the expansion is
// held to a few units in the last place of the log, tight enough that a
// wrong term of order up to y^-4 at y = 1000 fails.
func TestLogSeriesMatchesAHighPrecisionSum(t *testing.T) {
	for _, k := range []float64{0.001, 0.01, 0.05, 0.25, 1, 2.5, 7.3, 16, 40, 250} {
		for _, y := range []float64{1, 10, 63.9, 64, 64.5, 100, 160, 200, 300, 999.9, 1000, 18000, 1e5} {
			if y < 4*k && y > 1000 {
				continue // the direct sum, slow and checked at smaller y
			}
			got, want := logSeries(k, y), refLogSeries(t, k, y)
			tol := 4e-15 * max(1, math.Abs(want))
			if y >= asymptoticSeriesFrom && y >= 4*k {
				ulp := math.Nextafter(math.Abs(want), math.Inf(1)) - math.Abs(want)
				tol = 1e-14 + 4*ulp
			}
			if math.Abs(got-want) > tol {
				t.Errorf("logSeries(%g, %g) = %.17g, want %.17g (diff %.3g, tol %.3g)", k, y, got, want, got-want, tol)
			}
		}
	}
}

// The expansion takes a handful of terms where the direct sum took about y:
// 2e5 reads at y = 1e5, the size a long-prompt tail asks for, finish in well
// under a second, where term by term they took minutes.
func TestLogSeriesAtLargeYIsFast(t *testing.T) {
	const reads, limit = 200_000, 2 * time.Second
	start := time.Now()
	var s float64
	for i := range reads {
		s += logSeries(0.25, 1e5+float64(i))
		if i%1000 == 999 && time.Since(start) > limit {
			t.Fatalf("%d of %d reads at y near 1e5 took %s, want all under %s", i+1, reads, time.Since(start), limit)
		}
	}
	if math.IsNaN(s) {
		t.Fatal("a read at y near 1e5 was NaN")
	}
	t.Logf("%d reads in %s", reads, time.Since(start))
}
