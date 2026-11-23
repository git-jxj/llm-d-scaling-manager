package whatif

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// This file is the experimental per-request TPOT distribution
// (TPOTRenewalLognormal), priced only where a backtest asks for it. It models
// the whole distribution of one request's TPOT, not a ratio over the mean:
//
//   - The server is a decode group: the data-parallel ranks of one replica
//     step in lockstep (expert parallelism synchronises them), and a step
//     advances every running sequence by one token plus the draft tokens it
//     accepts. Far under max-num-seqs no sequence waits for a slot (Erlang C,
//     SlotWait), so the step time, not a queue, sets a token's time: an
//     infinite-server (M/G/inf) occupancy with a mildly load-dependent step.
//   - A request's TPOT is its decode time over its tokens after the first.
//     With speculative decoding a step emits 1 + A tokens, A the accepted
//     drafts; the steps a request of L tokens needs is a first passage of
//     that renewal process, so its TPOT is the step time times S/(L-1). By
//     the renewal central limit theorem (Cox, Renewal Theory, 1962, ch. 3;
//     Feller vol. II, XI.5) S/(L-1) has squared CV of about Var(1+A)/
//     ((1+E A)(L-1)): a long request is concentrated near the mean, a short
//     one spread. Over the output-length mix, the squared CV is C times
//     E[1/(L-1)], C fitted with RenewalCoefficient as its floor.
//   - The population mixes the groups (GroupCV2, measured) and the requests
//     (the renewal term). Per request the TPOT is lognormal around the
//     planner's mean with log-variance sigma^2 = GroupCV2 + C E[1/(L-1)]: a
//     two-moment fit to the mixture, as QNA fits each flow (Whitt, The
//     Queueing Network Analyzer, 1983).
//
// C is the one free parameter, fitted by interval-censored maximum
// likelihood over every bucket of the router's TPOT histogram (FitTPOTDist).

// TPOTRenewalLognormal names the experimental TPOT distribution.
const TPOTRenewalLognormal = "renewal-lognormal"

const (
	// TPOTDistribution is on every TPOT percentile priced by the
	// experimental per-request TPOT distribution (TPOTRenewalLognormal).
	TPOTDistribution Flag = "TPOTDistribution"
	// TPOTDistributionAbstained is a TPOT percentile the planner priced
	// itself where the experimental distribution was asked for and did not
	// price it.
	TPOTDistributionAbstained Flag = "TPOTDistributionAbstained"
)

// MaxRenewalC is the upper end of the renewal coefficient's search: a C
// there has not been fitted, it is the bound.
const MaxRenewalC = 50.0

// MaxSlotWait is the most of a decode rank's arrivals that may wait for a
// sequence slot (SlotWait) for the distribution to hold: past it a queue for
// slots, which the distribution leaves out, adds to the TPOT.
const MaxSlotWait = 0.01

// DecodeGroup is one decode replica as one cycle read it: its request rate,
// its step time over the tokens a step emits, and the accepted drafts per
// step, where read.
type DecodeGroup struct {
	Rate, TokenTime float64
	Accepted        float64
	AcceptedKnown   bool
}

// DecodeCycle is one cycle of the decode pods as the TPOT distribution reads
// it: the pooled output-length histogram (cumulative request rate by upper
// edge), each replica's reading, and the mean and largest running sequences
// of a data-parallel rank.
type DecodeCycle struct {
	At            time.Time
	Output        map[float64]float64
	Groups        []DecodeGroup
	MeanRunning   float64
	MostRunning   float64
	RanksReported int
}

// InverseOutputLength is E[1/(L-1)] over the requests of the cumulative
// output-length histogram out with two tokens or more (a one-token request
// has no TPOT), each bucket's lengths spread evenly in log (a mass 1/L at
// each integer L of the bucket). The mass past the last finite edge counts
// at 1/(L-1) = 0. ok is false with no request of two tokens or more.
func InverseOutputLength(out map[float64]float64) (float64, bool) {
	edges := make([]float64, 0, len(out))
	for e := range out {
		if !math.IsNaN(e) {
			edges = append(edges, e)
		}
	}
	slices.Sort(edges)
	var num, den, prevEdge, prevCum float64
	for _, e := range edges {
		cum := math.Max(prevCum, out[e])
		m := cum - prevCum
		lo := prevEdge
		prevEdge, prevCum = e, cum
		if !(m > 0) || !Finite(m) {
			continue
		}
		if math.IsInf(e, 1) {
			den += m
			continue
		}
		a, b := math.Max(2, math.Floor(lo)+1), math.Floor(e)
		if b < a {
			continue
		}
		// sum_{L=a}^{b} (1/L) (1/(L-1)) telescopes to 1/(a-1) - 1/b.
		num += m * (1/(a-1) - 1/b) / harmonicSpan(a, b)
		den += m
	}
	if !(den > 0) {
		return 0, false
	}
	return num / den, true
}

// harmonicSpan is sum_{L=a}^{b} 1/L for integers 1 <= a <= b.
func harmonicSpan(a, b float64) float64 {
	if b-a < 64 {
		s := 0.0
		for l := a; l <= b; l++ {
			s += 1 / l
		}
		return s
	}
	return harmonic(b) - harmonic(a-1)
}

// harmonic is H(n) = sum_{L=1}^{n} 1/L, exact under 64 and the
// Euler-Maclaurin expansion above.
func harmonic(n float64) float64 {
	if n < 64 {
		s := 0.0
		for l := 1.0; l <= n; l++ {
			s += 1 / l
		}
		return s
	}
	const eulerGamma = 0.5772156649015329
	return math.Log(n) + eulerGamma + 1/(2*n) - 1/(12*n*n)
}

// RenewalCoefficient is Var(1+A)/(1+E A), the squared CV of S/(L-1) times
// L-1 by the renewal central limit theorem, for k draft tokens a step
// accepted as a chain (P(A >= j) = p^j for j <= k) with mean accepted.
// Without speculation (k 0) a step emits one token and it is 0.
func RenewalCoefficient(k int, accepted float64) float64 {
	if k <= 0 || !(accepted > 0) || !Finite(accepted) {
		return 0
	}
	p := chainAcceptance(k, accepted)
	var ea, ea2 float64
	for j := 1; j <= k; j++ {
		t := math.Pow(p, float64(j))
		ea += t
		ea2 += float64(2*j-1) * t
	}
	return math.Max(0, ea2-ea*ea) / (1 + ea)
}

// chainAcceptance is p with p + p^2 + ... + p^k = accepted, by bisection.
func chainAcceptance(k int, accepted float64) float64 {
	if accepted >= float64(k) {
		return 1
	}
	lo, hi := 0.0, 1.0
	for range 60 {
		m := (lo + hi) / 2
		s := 0.0
		for j := 1; j <= k; j++ {
			s += math.Pow(m, float64(j))
		}
		if s < accepted {
			lo = m
		} else {
			hi = m
		}
	}
	return (lo + hi) / 2
}

// GroupCV2 is the squared coefficient of variation of the groups' token
// time, weighted by their request rates; ok is false with fewer than two
// groups read.
func GroupCV2(groups []DecodeGroup) (float64, bool) {
	var w, s, s2 float64
	n := 0
	for _, g := range groups {
		if !(g.Rate > 0) || !(g.TokenTime > 0) || !Finite(g.Rate) || !Finite(g.TokenTime) {
			continue
		}
		w += g.Rate
		s += g.Rate * g.TokenTime
		s2 += g.Rate * g.TokenTime * g.TokenTime
		n++
	}
	if n < 2 {
		return 0, false
	}
	m := s / w
	return math.Max(0, s2/w-m*m) / (m * m), true
}

// SlotWait is the Erlang C probability that an arrival waits for one of n
// slots at offered load a (Erlang, 1917; Halfin and Whitt, 1981, for its
// many-server limit): the share of a rank's sequences that queue for
// max-num-seqs. It is 1 at a >= n.
func SlotWait(n int, a float64) float64 {
	if n <= 0 || !(a > 0) {
		return 0
	}
	if a >= float64(n) {
		return 1
	}
	// Erlang B by its recursion, then C from B.
	b := 1.0
	for k := 1; k <= n; k++ {
		b = a * b / (float64(k) + a*b)
	}
	return b / (1 - (a/float64(n))*(1-b))
}

// TPOTDist is the fitted distribution: log-variance GroupCV2 + C
// E[1/(L-1)] around the mean, with Renewal the floor C was fitted above.
type TPOTDist struct {
	GroupCV2, C, Renewal float64
}

// Sigma2 is the log-variance at E[1/(L-1)] inv.
func (d TPOTDist) Sigma2(inv float64) float64 { return d.GroupCV2 + d.C*inv }

// Quantile is the q quantile of a request's TPOT at mean and inv.
func (d TPOTDist) Quantile(mean, inv, q float64) float64 {
	s2 := d.Sigma2(inv)
	if !(s2 > 0) {
		return mean
	}
	return mean * math.Exp(math.Sqrt(s2)*math.Sqrt2*math.Erfinv(2*q-1)-s2/2)
}

// CDF is the share of requests with a TPOT at or under x at mean and inv.
func (d TPOTDist) CDF(mean, inv, x float64) float64 {
	return lognormalCDF(mean, d.Sigma2(inv), x)
}

// lognormalCDF is P(X <= x) for a lognormal of mean and log-variance s2.
func lognormalCDF(mean, s2, x float64) float64 {
	switch {
	case math.IsInf(x, 1):
		return 1
	case !(x > 0):
		return 0
	case !(s2 > 0):
		if x >= mean {
			return 1
		}
		return 0
	}
	s := math.Sqrt(s2)
	z := (math.Log(x/mean) + s2/2) / s
	return 0.5 * math.Erfc(-z/math.Sqrt2)
}

// TPOTDistStep is one step the distribution is fitted on or scored at: the
// router's measured mean, the step's E[1/(L-1)] and group CV^2, and its
// histogram as upper edges (the last may be +Inf) and the requests in each
// bucket.
type TPOTDistStep struct {
	Mean, Inv, GroupCV2 float64
	Edges, Counts       []float64
}

// TPOTDistStepOf is a step from a cumulative histogram of request rates
// (buckets, with their upper edges) over windowSeconds, its mean, and the
// decode cycles of its window. ok is false where the histogram, the mean or
// the output lengths are not read.
func TPOTDistStepOf(edges, cum []float64, windowSeconds, mean float64, cycles []DecodeCycle) (TPOTDistStep, bool) {
	if len(edges) == 0 || len(edges) != len(cum) || !(mean > 0) || !(windowSeconds > 0) {
		return TPOTDistStep{}, false
	}
	inv, g2, ok := DecodeWindow(cycles)
	if !ok {
		return TPOTDistStep{}, false
	}
	st := TPOTDistStep{Mean: mean, Inv: inv, GroupCV2: g2, Edges: slices.Clone(edges), Counts: make([]float64, len(edges))}
	prev := 0.0
	for i, c := range cum {
		c = math.Max(prev, c)
		st.Counts[i] = (c - prev) * windowSeconds
		prev = c
	}
	if !(prev > 0) {
		return TPOTDistStep{}, false
	}
	return st, true
}

// DecodeWindow is E[1/(L-1)] over the output lengths pooled over cycles,
// and the mean of their groups' CV^2 (0 where no cycle reads two groups).
func DecodeWindow(cycles []DecodeCycle) (inv, groupCV2 float64, ok bool) {
	pooled := map[float64]float64{}
	n := 0
	for _, c := range cycles {
		for e, r := range c.Output {
			pooled[e] += r
		}
		if v, gok := GroupCV2(c.Groups); gok {
			groupCV2 += v
			n++
		}
	}
	if n > 0 {
		groupCV2 /= float64(n)
	}
	inv, ok = InverseOutputLength(pooled)
	return inv, groupCV2, ok
}

// logLik is the steps' interval-censored log-likelihood at C, each step's
// lognormal at its own mean: sum over buckets of the requests in a bucket
// times the log of the mass the distribution puts there.
func logLik(steps []TPOTDistStep, c float64) float64 {
	const floor = 1e-9
	ll := 0.0
	for _, st := range steps {
		d := TPOTDist{GroupCV2: st.GroupCV2, C: c}
		prev := 0.0
		for i, e := range st.Edges {
			f := d.CDF(st.Mean, st.Inv, e)
			if st.Counts[i] > 0 {
				ll += st.Counts[i] * math.Log(math.Max(f-prev, floor))
			}
			prev = f
		}
	}
	return ll
}

// ErrNoTPOTSteps is a fit given no step to fit on.
var ErrNoTPOTSteps = errors.New("no step reads the router's TPOT histogram, its mean and the decode pods' output lengths")

// FitTPOTDist fits C in [floor, MaxRenewalC] by interval-censored maximum
// likelihood over every bucket of every step (golden-section search on the
// log-likelihood, concave in C here as it is in a lognormal's variance).
// atBound says C ended at MaxRenewalC: the bound, not an estimate.
func FitTPOTDist(steps []TPOTDistStep, floor float64) (c float64, atBound bool, err error) {
	if len(steps) == 0 {
		return 0, false, ErrNoTPOTSteps
	}
	if !(floor >= 0) || floor >= MaxRenewalC {
		return 0, false, fmt.Errorf("renewal floor %g, want 0 <= floor < %g", floor, MaxRenewalC)
	}
	lo, hi := floor, MaxRenewalC
	g := (math.Sqrt(5) - 1) / 2
	a, b := hi-g*(hi-lo), lo+g*(hi-lo)
	fa, fb := logLik(steps, a), logLik(steps, b)
	for hi-lo > 1e-4 {
		if fa >= fb {
			hi, b, fb = b, a, fa
			a = hi - g*(hi-lo)
			fa = logLik(steps, a)
		} else {
			lo, a, fa = a, b, fb
			b = lo + g*(hi-lo)
			fb = logLik(steps, b)
		}
	}
	c = (lo + hi) / 2
	return c, MaxRenewalC-c < 1e-2, nil
}

// BucketScore compares the distribution with one step's histogram at its own
// resolution: KS is the largest gap between the predicted share at or under
// an edge and the measured share, over the step's finite edges, at the
// predicted mean; Inside[i] says the predicted q-th quantile (qs[i]) lies in
// the bucket the measured one fell in.
func (d TPOTDist) BucketScore(st TPOTDistStep, predictedMean float64, qs ...float64) (ks float64, inside []bool) {
	total := 0.0
	for _, c := range st.Counts {
		total += c
	}
	inside = make([]bool, len(qs))
	if !(total > 0) {
		return 0, inside
	}
	cum, lo := 0.0, 0.0
	for i, e := range st.Edges {
		prev := cum / total
		cum += st.Counts[i]
		f := cum / total
		if !math.IsInf(e, 1) {
			ks = math.Max(ks, math.Abs(d.CDF(predictedMean, st.Inv, e)-f))
		}
		for j, q := range qs {
			if prev < q && f >= q {
				v := d.Quantile(predictedMean, st.Inv, q)
				inside[j] = v > lo && v <= e
			}
		}
		lo = e
	}
	return ks, inside
}
