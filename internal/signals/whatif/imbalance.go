package whatif

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The imbalance f across pod counts (llm-d-incubation/llm-d-scaling-manager#169, item 5). P1 learns f only
// at a count the fleet has run at (prefill.Estimate.FByPods); the planner
// asks about the counts it has not. With DefaultImbalanceAcrossCounts on,
// both the SLO analyzer and the CLI read f at any count through
// ImbalanceAcrossCounts: a count P1 learned keeps its own f, any other is
// read off a curve fitted to the counts learned, and with none learned f is
// FreshImbalance.
//
// The curve. Each pod takes a share of the prefill work that departs from
// the mean by a fixed relative spread c, from the router's scoring, a slower
// node, or a prefix the router keeps sending to one pod. The busiest of n
// such pods reads, on average, 1 + c·E[max of n standard normals] of the
// mean: f(n) = 1 + c·g(n), g = prefill.NormalMaxMean (0.564 at two pods,
// 0.846 at three, 1.029 at four, 1.424 at eight). P1's f is already
// de-biased for chance (Poisson counts), so c is what stays once chance is
// taken off. c is the least-squares fit through f = 1 of every count
// learned: one count gives c = (f - 1)/g(n) exactly.
//
// Measured on Qwen3-32B TP2 prefill pods on H200 (8000-token prompts): P1
// learned f = 1.069 over 29 minutes at two pods (2+2), c = 0.123, so the
// curve reads 1.104 at three. At three pods (3+1) the busiest pod's share of
// prefill busy time read 1.12 to 1.14 at the five steps below saturation,
// and of prompt tokens 1.08 to 1.10.

// DefaultImbalanceAcrossCounts is the default of reading f across pod counts
// (ImbalanceAcrossCounts), for the CLI (--imbalance-across-counts), the
// planner API and the SLO analyzer (slo.SwitchTTFTImbalanceAcrossCounts)
// alike: off, so an unlearned count is priced at min(N, max(ImbalanceFloor,
// f)) as before. Opt-in first: one GPU run at three pods backs the curve and
// FreshImbalance; the default flips after a check at a count none of the
// learned ones covers.
const DefaultImbalanceAcrossCounts = false

// FreshImbalance is f at a count above one with no count learned, with
// DefaultImbalanceAcrossCounts on. Not a constant of any fleet: it stands
// above what every Qwen3-32B run measured, 1.00 and 1.07 learned at two pods
// and a busiest share of 1.08 to 1.14 at three, and above three of the four
// staircase phases of a smaller model at two pods (1.00 to 1.03). The fourth
// learned 1.36 there: a fleet like that is priced short until P1 learns its
// own f, and the answer is flagged AssumedImbalance either way.
const FreshImbalance = 1.15

// Where an f read across counts came from.
const (
	// ImbalanceLearned is P1's own f at the count.
	ImbalanceLearned = "learned"
	// ImbalanceDefault is FreshImbalance: no count learned.
	ImbalanceDefault = "default"
	// ImbalanceRawShare is the raw share at the count it was read at, above
	// the f read across counts (Imbalance.WithRawShare).
	ImbalanceRawShare = "raw share"
	// imbalanceFitted prefixes the counts the curve was fitted from.
	imbalanceFitted = "fitted from counts"
)

// Imbalance is f at one pod count, read across counts.
type Imbalance struct {
	// F is f at the count, at least 1 and at most the count.
	F float64
	// Assumed is any f not learned at the count: fitted or the default. The
	// caller flags it AssumedImbalance.
	Assumed bool
	// Source is ImbalanceLearned, ImbalanceDefault, or "fitted from counts
	// {2, 4}" naming the counts behind the curve.
	Source string
	// Counts are the counts the curve was fitted from, ascending; Spread is
	// its c. Both empty unless fitted.
	Counts []int
	Spread float64
}

// ImbalanceAcrossCounts is f at pods, more than one, from P1's f by count
// (prefill.Estimate.FByPods): the count's own when learned, else the curve
// fitted to every count learned, else FreshImbalance; never below 1 nor
// above pods. A count of one, or an f under 1, is not a reading of the
// curve: one pod has no imbalance, and P1 never reports under 1.
func ImbalanceAcrossCounts(byPods map[int]float64, pods int) Imbalance {
	if f, ok := byPods[pods]; ok && f >= 1 && !math.IsInf(f, 0) {
		return Imbalance{F: f, Source: ImbalanceLearned}
	}
	n := float64(max(pods, 1))
	counts, spread := imbalanceCurve(byPods)
	if len(counts) == 0 {
		return Imbalance{F: min(n, FreshImbalance), Assumed: true, Source: ImbalanceDefault}
	}
	f := 1.0
	if pods > 1 {
		f = 1 + spread*prefill.NormalMaxMean(pods)
	}
	return Imbalance{F: min(n, max(1, f)), Assumed: true, Source: fittedSource(counts), Counts: counts, Spread: spread}
}

// WithRawShare is i at the count the raw share was read at: an f not learned
// is raised to raw, at most pods, where raw reads higher. The raw share is a
// reading of that count, not de-biased, so it errs high: the side that asks
// for more replicas. A learned f is kept.
func (i Imbalance) WithRawShare(raw float64, pods int) Imbalance {
	if !i.Assumed || !(raw > i.F) {
		return i
	}
	return Imbalance{F: min(float64(pods), raw), Assumed: true, Source: ImbalanceRawShare}
}

// imbalanceCurve is the counts learned that the curve reads, ascending, and
// c fitted through f = 1 by least squares: the sum of (f - 1)·g(n) over the
// sum of g(n)^2. Nil with no count above one with a finite f of at least 1.
func imbalanceCurve(byPods map[int]float64) ([]int, float64) {
	var counts []int
	for pods, f := range byPods {
		if pods >= 2 && f >= 1 && !math.IsInf(f, 0) {
			counts = append(counts, pods)
		}
	}
	if len(counts) == 0 {
		return nil, 0
	}
	// Summed in count order, so the same map gives the same c to the bit.
	slices.Sort(counts)
	var num, den float64
	for _, pods := range counts {
		g := prefill.NormalMaxMean(pods)
		num += (byPods[pods] - 1) * g
		den += g * g
	}
	return counts, num / den
}

// fittedSource names the counts a curve was fitted from.
func fittedSource(counts []int) string {
	s := make([]string, len(counts))
	for i, n := range counts {
		s[i] = strconv.Itoa(n)
	}
	return fmt.Sprintf("%s {%s}", imbalanceFitted, strings.Join(s, ", "))
}
