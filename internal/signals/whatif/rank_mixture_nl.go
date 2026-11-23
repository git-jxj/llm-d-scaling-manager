package whatif

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"
)

// This file is a variant of the experimental rank-mixture TTFT model
// (rank_mixture.go), QueueRankMixtureNL. The queue is the same: limited
// processor sharing per prefill rank, mixed over the ranks' shares, with the
// fitted KV-deferral wait. Two parts differ.
//
// Prefill service, non-linear in the tokens (ServiceAttention). vLLM v1
// schedules a prefill chunk of q new tokens over c tokens already in the KV
// cache (prefix hits and earlier chunks); its attention reads all c + q of
// them under a causal mask, about q (c + q/2) query-key pairs, while the
// GEMMs grow with q alone. Summed over a request's chunks, x computed tokens
// after c cached ones cost
//
//	S(x, c) = S0 + x/V + B x (c + x/2)
//
// the form of DistServe's prefill model (Zhong et al., OSDI 2024, appendix),
// Vidur's attention term (Agrawal et al., MLSys 2024) and the causal pairs of
// BLIS's step model; chunking changes the step count, not the pairs. c is
// the prompt less the computed tokens. The three coefficients are fitted on
// the rank-steps' processor-sharing inversion, as the line is, by
// non-negative least squares on 1, x and x (c + x/2) at the rank-steps' means
// (a mean-of-product approximation). B is identified only where the
// rank-steps' prompts or computed tokens spread; where they do not, B x c is
// a constant per token and the fit reads it into 1/V.
//
// Non-queue part, measured part by part (ZParts). Z is the engine's work
// before its queue (the ranks' TTFT less their prefill and queue time), the
// decode engine's first token and the router's remainder (its mean less both
// engines'), each a line in the prompt with a non-negative intercept and
// slope fitted on that part's own series. Z's constant A0 is the sum of the
// intercepts, so it is a time by construction, and Z is not calibrated:
// Spread is 1. v2's calibration of the spread to the fit window's p90, with
// A0 refitted to hold the mean, read load-dependent misfit as prompt cost: a
// spread that grew moved A0 below zero.
//
// Fit-window hygiene. The KV-deferral lines and Z's parts are fitted on the
// steps whose every rank sits under MaxCleanRho at the fitted service: past
// it a rank's queue time is the overload's admission wait, not a wait for
// KV, and its front end's time grows with the overload, not with the prompt.
//
// Abstention. With no location fitted to the router, the fit window prices
// itself out of sample in A0: its median ratio of the model's mean to the
// router's measured mean and p90 (RankMixtureNLFit.FitMean, FitP90) are
// checks, and the model abstains outside [MinNLFitMean, MaxNLFitMean] and
// [MinNLFitP90, MaxNLFitMean]. Those checks mean something only on a window
// that holds the loads and prompts it will be asked about: on a fit window
// under MinSpreadFitWindow (v2's rule for its calibration) it abstains too.
// On the public warm-ups (about an hour, a ramp through prompt shapes and
// into overload) the checks pass by chance on two and the second half reads
// 2-4x over at light load and 0.2x under in the overload bursts.

// QueueRankMixtureNL is the experimental TTFT model with the non-linear
// prefill service and the decomposed non-queue part (rank_mixture_nl.go).
// Experimental; only a caller that asks prices it.
const QueueRankMixtureNL QueueModel = "rank-mixture-nl"

// QueueRankMixtureNLLine is QueueRankMixtureNL with v2's service line in
// place of the attention law (RankMixtureOptions.LineService): the fit
// changes alone, for a fleet where TTFT matters more than the knee.
const QueueRankMixtureNLLine QueueModel = "rank-mixture-nl-line"

// ServiceLaw is the prefill service law of the rank mixture.
type ServiceLaw string

// The service laws.
const (
	// ServiceLine is S(x) = S0 + x/V, linear in the computed tokens.
	ServiceLine ServiceLaw = ""
	// ServiceAttention is S(x, c) = S0 + x/V + B x (c + x/2), c the cached
	// tokens ahead of the x computed ones.
	ServiceAttention ServiceLaw = "attention"
)

// attentionContext is c + x/2 for x computed tokens on a prompt of l: the
// mean context a computed token attends to.
func attentionContext(x, l float64) float64 {
	return max(l-x, 0) + x/2
}

// Shares at the priced load. A router that balances by load spreads a busy
// fleet's requests more evenly than a quiet one's (prefix affinity holds a
// quiet fleet's requests on fewer ranks), so the fit window's mean profile
// prices a busy step as more lopsided than it is, and its knee low.
// sharesAt reads the profile of the fit steps nearest the priced rate.

// ShareRow is one fit step's share profile, busiest first, at its rate.
type ShareRow struct {
	Rate   float64
	Shares []float64
}

// nearShareRows is the fit steps sharesAt averages.
const nearShareRows = 5

// shareRowsOf is steps' share profiles, each with every rank of the most
// ranks any step has, at a positive rate.
func shareRowsOf(steps []MixtureStep, n int) []ShareRow {
	var out []ShareRow
	for _, st := range steps {
		if len(st.Ranks) != n || !(st.Rate > 0) {
			continue
		}
		var tot float64
		r := make([]float64, n)
		for i, rk := range st.Ranks {
			r[i] = max(rk.Rate, 0)
			tot += r[i]
		}
		if !(tot > 0) {
			continue
		}
		slices.SortFunc(r, func(a, b float64) int { return cmp.Compare(b, a) })
		for i := range r {
			r[i] /= tot
		}
		out = append(out, ShareRow{Rate: st.Rate, Shares: r})
	}
	return out
}

// sharesAt is the share profile priced at rate: the mean of the
// nearShareRows fit steps nearest it where ShareRows holds that many, else
// Shares.
func (m RankMixture) sharesAt(rate float64) []float64 {
	if len(m.ShareRows) < nearShareRows {
		return m.Shares
	}
	rows := slices.Clone(m.ShareRows)
	slices.SortStableFunc(rows, func(a, b ShareRow) int { return cmp.Compare(math.Abs(a.Rate-rate), math.Abs(b.Rate-rate)) })
	out := make([]float64, len(m.Shares))
	for _, r := range rows[:nearShareRows] {
		for i := range out {
			if i < len(r.Shares) {
				out[i] += r.Shares[i] / nearShareRows
			}
		}
	}
	return out
}

// Pooled service moments. Each rank's mean service and its cs^2 set its
// utilization, and the sojourn near the knee is steep in it. Read from the
// rank's own few hundred draws (v2), a heavy-tailed computed-token law (a few
// requests compute their whole prompt) moves the busiest rank's mean service
// by about 11 % (sd) from draw set to draw set on the client day, and the
// convex sojourn turns that noise into an over-read: it was the whole gap
// between the planner's fit and the offline harness's on the same steps
// (in-sample mean ratio 1.12 against 1.05). The variant reads them once per
// priced rate from momentDraws requests, about 0.6 % (sd).

// momentDraws is the sample serviceMoments reads the moments from.
const momentDraws = 1 << 16

// Stratified draws. The computed-token law's top buckets hold about 1e-4 of
// the requests and a large share of the service's second moment; a rank's
// few hundred independent draws catch them or not by chance, which moved the
// fit window's in-sample mean ratio by 7 % between two draw sets. The
// variant draws a rank's computed tokens one per stratum of the cumulative
// share ((j + u)/n), at least minStratifiedDraws a rank, and its moments on a
// fixed grid of momentDraws strata.

// minStratifiedDraws is the fewest draws a rank takes under the variant.
const minStratifiedDraws = 4096

// momentSeed fixes that sample.
const momentSeed = 0x5eed + 7

// serviceMoments is the service's mean and cs^2 over momentDraws requests
// drawn from the computed-token and prompt histograms.
func (m RankMixture) serviceMoments(hx, hl cumHist) (es, cs2 float64) {
	rng := rand.New(rand.NewPCG(momentSeed, momentDraws))
	var s1, s2 float64
	for i := range momentDraws {
		x := hx.ppf((float64(i) + 0.5) / momentDraws)
		v := m.S0 + x/m.V
		if m.Law == ServiceAttention {
			v = m.service(x, hl.ppf(rng.Float64()))
		}
		s1 += v
		s2 += v * v
	}
	es = s1 / momentDraws
	if !(es > 0) {
		return 0, 0
	}
	return es, max(s2/momentDraws/(es*es)-1, 0)
}

// MaxCleanRho is the rank utilization from which a fit-window step is left
// out of the KV-deferral and Z fits of FitRankMixtureNL.
const MaxCleanRho = 0.9

// minCleanSteps is the fewest clean steps the KV and Z fits use; with fewer,
// every step is used.
const minCleanSteps = 4

// MinNLFitMean and MaxNLFitMean bound the fit window's median mean ratio
// (RankMixtureNLFit.FitMean) at which QueueRankMixtureNL prices, and
// MinNLFitP90 and MaxNLFitMean its median p90 ratio (FitP90). The p90's
// floor is tighter: a tail short on the window it was fitted on reads
// shorter where the load is higher.
const (
	MinNLFitMean = 0.8
	MinNLFitP90  = 0.9
	MaxNLFitMean = 1.25
)

// ZPart is one part of Z as a line in the whole prompt: A seconds plus B
// seconds a prompt token, both non-negative. Identified is false where the
// prompts did not spread (B is then 0 and A the part's median). Sigma is
// the part's spread: each request's part is lognormal about the line's mean
// with this sigma (mean-preserving); 0 is the line itself.
type ZPart struct {
	A, B       float64
	Identified bool
	Sigma      float64
}

// ZParts is Z measured part by part.
type ZParts struct {
	Overhead, Decode, Remainder ZPart
}

// RankMixtureNLFit is FitRankMixtureNL's diagnostics.
type RankMixtureNLFit struct {
	// Steps is the fit window's steps; CleanSteps those the KV and Z fits
	// read (every step where under minCleanSteps were clean).
	Steps, CleanSteps int
	// ShareCV is the busiest share's cv (ShareProfile).
	ShareCV float64
	// FitMean and FitP90 are the fit window's median ratios of the model's
	// mean and p90 to the router's measured ones.
	FitMean, FitP90 float64
	// Span is the fit window's length, its steps' span plus one step.
	Span time.Duration
}

// solveSmall is the least-squares solution of rows c = y over the columns in
// cols (normal equations, Gaussian elimination); ok is false where they are
// singular.
func solveSmall(rows [][]float64, y []float64, cols []int) ([]float64, bool) {
	k := len(cols)
	a := make([][]float64, k)
	for i := range a {
		a[i] = make([]float64, k+1)
	}
	for r := range rows {
		for i, ci := range cols {
			for j, cj := range cols {
				a[i][j] += rows[r][ci] * rows[r][cj]
			}
			a[i][k] += rows[r][ci] * y[r]
		}
	}
	for p := range k {
		piv := p
		for i := p + 1; i < k; i++ {
			if math.Abs(a[i][p]) > math.Abs(a[piv][p]) {
				piv = i
			}
		}
		if !(math.Abs(a[piv][p]) > 1e-12) {
			return nil, false
		}
		a[p], a[piv] = a[piv], a[p]
		for i := range k {
			if i == p {
				continue
			}
			f := a[i][p] / a[p][p]
			for j := p; j <= k; j++ {
				a[i][j] -= f * a[p][j]
			}
		}
	}
	out := make([]float64, k)
	for i := range k {
		out[i] = a[i][k] / a[i][i]
	}
	return out, true
}

// nnls is the non-negative least-squares solution of rows c = y for a few
// columns, by trying every subset of free columns (the rest held at 0) and
// keeping the feasible one of least squared error. The columns are scaled to
// their mean magnitude first. ok is false with no row.
func nnls(rows [][]float64, y []float64) ([]float64, bool) {
	if len(rows) == 0 || len(rows) != len(y) {
		return nil, false
	}
	n := len(rows[0])
	scale := make([]float64, n)
	for _, row := range rows {
		for j, v := range row {
			scale[j] += math.Abs(v)
		}
	}
	scaled := make([][]float64, len(rows))
	for i, row := range rows {
		scaled[i] = make([]float64, n)
		for j, v := range row {
			if scale[j] > 0 {
				scaled[i][j] = v / (scale[j] / float64(len(rows)))
			}
		}
	}
	sse := func(c []float64) float64 {
		var e float64
		for i, row := range scaled {
			p := 0.0
			for j, v := range row {
				p += v * c[j]
			}
			e += (y[i] - p) * (y[i] - p)
		}
		return e
	}
	best := make([]float64, n)
	bestE := sse(best)
	for mask := 1; mask < 1<<n; mask++ {
		var cols []int
		for j := range n {
			if mask&(1<<j) != 0 {
				cols = append(cols, j)
			}
		}
		sol, ok := solveSmall(scaled, y, cols)
		if !ok || slices.ContainsFunc(sol, func(v float64) bool { return v < 0 || !Finite(v) }) {
			continue
		}
		c := make([]float64, n)
		for i, j := range cols {
			c[j] = sol[i]
		}
		if e := sse(c); e < bestE {
			best, bestE = c, e
		}
	}
	for j := range best {
		if scale[j] > 0 {
			best[j] /= scale[j] / float64(len(rows))
		}
	}
	return best, true
}

// FitAttention is S0, 1/V and B of ServiceAttention by processor-sharing
// inversion per rank (FitPSInversion's E[S] = E[T]/(1 + lambda E[T])),
// regressed on 1, x and x (c + x/2) at the rank-steps' mean computed and
// whole prompt, each coefficient non-negative. ok is false where the
// computed prompts do not spread (under MinComputedCV) or no coefficient
// of the tokens is positive.
func FitAttention(steps []MixtureStep) (s0, a, b float64, ok bool) {
	var X [][]float64
	var es, xs []float64
	for _, st := range steps {
		for _, r := range st.Ranks {
			if !r.usable() {
				continue
			}
			l := r.Prompt
			if !(l > 0) {
				l = r.Computed
			}
			X = append(X, []float64{1, r.Computed, r.Computed * attentionContext(r.Computed, l)})
			es = append(es, r.PrefillTime/(1+r.Rate*r.PrefillTime))
			xs = append(xs, r.Computed)
		}
	}
	if len(X) < 3 || cvOf(xs) < MinComputedCV {
		return 0, 0, 0, false
	}
	c, ok := nnls(X, es)
	if !ok || !(c[1] > 0 || c[2] > 0) {
		return 0, 0, 0, false
	}
	return c[0], c[1], c[2], true
}

// rankRho is the busiest rank's utilization in st at m's service, each rank
// at its own rate, mean computed and mean prompt.
func (m RankMixture) rankRho(st MixtureStep) float64 {
	var top float64
	for _, r := range st.Ranks {
		if !(r.Rate > 0) || !(r.Computed > 0) {
			continue
		}
		l := r.Prompt
		if !(l > 0) {
			l = r.Computed
		}
		top = max(top, r.Rate*m.service(r.Computed, l))
	}
	return top
}

// cleanSteps is the steps whose busiest rank is under MaxCleanRho at m's
// service; every step where fewer than minCleanSteps are.
func (m RankMixture) cleanSteps(steps []MixtureStep) []MixtureStep {
	var out []MixtureStep
	for _, st := range steps {
		if m.rankRho(st) < MaxCleanRho {
			out = append(out, st)
		}
	}
	if len(out) < minCleanSteps {
		return steps
	}
	return out
}

// fitZPart is one part's line on (x prompt, y seconds): a non-negative
// intercept and slope, the slope only where the prompts spread
// (MinPromptSpreadCV); else the median, not identified.
func fitZPart(x, y []float64) ZPart {
	if len(y) == 0 {
		return ZPart{}
	}
	if len(x) < 2 || cvOf(x) < MinPromptSpreadCV {
		return ZPart{A: max(medianOf(y), 0)}
	}
	X := make([][]float64, len(x))
	for i := range x {
		X[i] = []float64{1, x[i]}
	}
	c, ok := nnls(X, y)
	if !ok {
		return ZPart{A: max(medianOf(y), 0)}
	}
	return ZPart{A: c[0], B: c[1], Identified: true}
}

// FitZParts is Z measured part by part on steps: the ranks' pre-queue
// overhead (TTFT less prefill and queue time) on their mean prompt, and the
// decode engine's TTFT and the router's remainder (router mean less both
// engines' TTFT) on the step's mean prompt. unidentified names the parts
// whose slope the prompts did not identify.
func FitZParts(steps []MixtureStep) (p ZParts, unidentified []string, err error) {
	var lo, ov, ls, dec, rem []float64
	for _, st := range steps {
		for _, r := range st.Ranks {
			if r.usable() && r.TTFT > 0 && r.Prompt > 0 {
				lo = append(lo, r.Prompt)
				ov = append(ov, r.TTFT-r.PrefillTime-r.QueueTime)
			}
		}
		if st.PromptMean > 0 && st.DecodeTTFT > 0 && st.PrefillTTFT > 0 && st.RouterMean > 0 {
			ls = append(ls, st.PromptMean)
			dec = append(dec, st.DecodeTTFT)
			rem = append(rem, st.RouterMean-st.PrefillTTFT-st.DecodeTTFT)
		}
	}
	if len(lo) == 0 || len(ls) == 0 {
		return ZParts{}, nil, errors.New("the non-queue parts need the ranks' TTFT and the steps' decode and router means")
	}
	p = ZParts{Overhead: fitZPart(lo, ov), Decode: fitZPart(ls, dec), Remainder: fitZPart(ls, rem)}
	for _, part := range []struct {
		name string
		z    ZPart
	}{{"overhead", p.Overhead}, {"decode", p.Decode}, {"remainder", p.Remainder}} {
		if !part.z.Identified {
			unidentified = append(unidentified, part.name)
		}
	}
	return p, unidentified, nil
}

// Spreads of Z's parts. Z's parts are not constants per prompt: the front
// end, the decode engine's first token (its own queue and the KV transfer)
// and the router vary request to request. Each part is drawn lognormal about
// its line with a sigma read from that part's own measured series, so the
// spread moves the tail and not the mean. The decode part's sigma is read
// from the decode engines' TTFT histogram (its mean and p90); the overhead's
// is the one at which the model's prefill-engine TTFT (overhead + queue +
// prefill) reads the prefill engines' measured TTFT p90 (median ratio 1 over
// the clean fit steps). The router's remainder has no histogram of its own:
// its sigma is 0. A lognormal's p90 over its mean rises with sigma only up
// to z90 (1.28), so the search runs over [0, MaxPartSigma].

// MaxPartSigma bounds a part's sigma.
const MaxPartSigma = 1.2

// z90 is the standard normal's 0.9 quantile.
const z90 = 1.2815515655446004

// zSeed fixes the parts' draws.
const zSeed = 0x5eed + 99

// draws is each request's Z at its prompt l: every part lognormal about its
// line; nil where no part has a sigma (Z is then the lines' sum, priced as
// a0 + beta L).
func (p *ZParts) draws(l []float64) []float64 {
	if p == nil || !(p.Overhead.Sigma > 0 || p.Decode.Sigma > 0 || p.Remainder.Sigma > 0) {
		return nil
	}
	return p.drawsWith(l, []ZPart{p.Overhead, p.Decode, p.Remainder})
}

// drawsWith is draws over parts.
func (p *ZParts) drawsWith(l []float64, parts []ZPart) []float64 {
	rng := rand.New(rand.NewPCG(zSeed, uint64(len(l))))
	out := make([]float64, len(l))
	for i, li := range l {
		for _, z := range parts {
			mu := max(z.A+z.B*li, 0)
			if z.Sigma > 0 {
				mu *= math.Exp(z.Sigma*rng.NormFloat64() - z.Sigma*z.Sigma/2)
			}
			out[i] += mu
		}
	}
	return out
}

// lnSigma is the sigma of the lognormal with this mean and p90, the smaller
// root, at most z90; ok is false where either is not positive.
func lnSigma(mean, p90 float64) (float64, bool) {
	if !(mean > 0) || !(p90 > 0) {
		return 0, false
	}
	disc := z90*z90 - 2*math.Log(p90/mean)
	if disc <= 0 {
		return z90, true
	}
	return max(z90-math.Sqrt(disc), 0), true
}

// histP90 is the 0.9 quantile of a cumulative histogram (upper bound to
// rate, +Inf the total), linear inside a bucket and the lower edge in the
// +Inf one; ok is false where it holds nothing.
func histP90(h map[float64]float64) (float64, bool) {
	const q = 0.9
	tot := h[math.Inf(1)]
	if !(tot > 0) {
		return 0, false
	}
	les := make([]float64, 0, len(h))
	for le := range h {
		les = append(les, le)
	}
	slices.Sort(les)
	target, prevLe, prevC := q*tot, 0.0, 0.0
	for _, le := range les {
		c := h[le]
		if c >= target {
			if math.IsInf(le, 1) {
				return prevLe, true
			}
			if c == prevC {
				return le, true
			}
			return prevLe + (le-prevLe)*(target-prevC)/(c-prevC), true
		}
		prevLe, prevC = le, c
	}
	return 0, false
}

// weightedQuantile is the q quantile of y weighted by w.
func weightedQuantile(y, w []float64, q float64) float64 {
	idx := make([]int, len(y))
	var tw float64
	for i := range idx {
		idx[i] = i
		tw += w[i]
	}
	slices.SortFunc(idx, func(a, b int) int { return cmp.Compare(y[a], y[b]) })
	acc := 0.0
	for _, i := range idx {
		acc += w[i]
		if acc >= q*tw {
			return y[i]
		}
	}
	return y[idx[len(idx)-1]]
}

// poolHist is out with h added at weight 1/n; nil while neither holds any.
func poolHist(out, h map[float64]float64, n float64) map[float64]float64 {
	for b, v := range h {
		if out == nil {
			out = map[float64]float64{}
		}
		out[b] += v / n
	}
	return out
}

// fitSigmas sets the parts' sigmas on m (spreads of Z's parts, above) from
// the clean steps.
func (m *RankMixture) fitSigmas(clean []MixtureStep) {
	p := m.Parts
	var ds []float64
	for _, st := range clean {
		if q, ok := histP90(st.DecodeTTFTHist); ok {
			if s, ok := lnSigma(st.DecodeTTFT, q); ok {
				ds = append(ds, s)
			}
		}
	}
	if len(ds) > 0 {
		p.Decode.Sigma = min(medianOf(ds), MaxPartSigma)
	}
	type obs struct {
		d    RankMixtureDraws
		meas float64
	}
	var os []obs
	for _, st := range clean {
		q, ok := histP90(st.PrefillTTFTHist)
		if !ok || !(q > 0) {
			continue
		}
		d, u := m.Draws(st.Rate, st.Computed, st.Prompt, DefaultMixtureDraws)
		if u != nil {
			continue
		}
		os = append(os, obs{d, q})
	}
	if len(os) == 0 {
		return
	}
	ratio := func(sigma float64) float64 {
		part := p.Overhead
		part.Sigma = sigma
		r := make([]float64, 0, len(os))
		for _, o := range os {
			z := p.drawsWith(o.d.L, []ZPart{part})
			y := make([]float64, len(z))
			for i := range z {
				y[i] = o.d.QP[i] + z[i]
			}
			r = append(r, weightedQuantile(y, o.d.W, 0.9)/o.meas)
		}
		return medianOf(r)
	}
	lo, hi := 0.0, MaxPartSigma
	if ratio(lo) >= 1 {
		return
	}
	if ratio(hi) < 1 {
		p.Overhead.Sigma = hi
		return
	}
	for range spreadIterations {
		mid := (lo + hi) / 2
		if ratio(mid) < 1 {
			lo = mid
		} else {
			hi = mid
		}
	}
	p.Overhead.Sigma = (lo + hi) / 2
}

// FitRankMixtureNL fits QueueRankMixtureNL on the fit window's steps: the
// attention service (FitAttention; FitPSInversion's line where it is not
// identified), the shares, the KV-deferral lines and Z's parts on the clean
// steps, and A0 and Beta as Z's sums at Spread 1. o's Slots and NoMixture,
// NoKV and NoPrompt apply; the spread options do not.
func FitRankMixtureNL(steps []MixtureStep, o RankMixtureOptions) (RankMixture, RankMixtureNLFit, error) {
	if len(steps) < 8 {
		return RankMixture{}, RankMixtureNLFit{}, fmt.Errorf("the rank mixture needs at least 8 steps to fit, got %d", len(steps))
	}
	m := RankMixture{Law: ServiceAttention, Spread: 1, Slots: o.Slots}
	if s0, a, b, ok := FitAttention(steps); ok && !o.LineService {
		m.S0, m.B = s0, b
		m.V = math.Inf(1)
		if a > 0 {
			m.V = 1 / a
		}
	} else {
		s0, v, proportional, err := FitPSInversion(steps)
		if err != nil {
			return RankMixture{}, RankMixtureNLFit{}, err
		}
		m.Law, m.S0, m.V, m.ServiceProportional = ServiceLine, s0, v, proportional
	}
	shares, cv, err := ShareProfile(steps)
	if err != nil {
		return RankMixture{}, RankMixtureNLFit{}, err
	}
	if o.NoMixture {
		for i := range shares {
			shares[i] = 1 / float64(len(shares))
		}
	}
	m.Shares = shares
	if !o.NoMixture {
		m.ShareRows = shareRowsOf(steps, len(shares))
	}
	clean := m.cleanSteps(steps)
	diag := RankMixtureNLFit{Steps: len(steps), CleanSteps: len(clean), ShareCV: cv}
	if sorted := slices.SortedFunc(slices.Values(steps), func(a, b MixtureStep) int { return a.At.Compare(b.At) }); len(sorted) > 1 {
		diag.Span = sorted[len(sorted)-1].At.Sub(sorted[0].At) + stepLength(sorted)
	}
	if !o.NoKV {
		if kv, err := fitKVDeferralAt(clean, func(st MixtureStep) float64 {
			return st.Rate / float64(len(shares)) * m.service(st.xbar(), st.promptMean())
		}); err == nil {
			m.KV = kv
		}
	}
	parts := ZParts{}
	if !o.NoPrompt {
		if parts, m.BetaUnidentified, err = FitZParts(clean); err != nil {
			return RankMixture{}, RankMixtureNLFit{}, err
		}
	}
	m.Parts = &parts
	m.A0 = parts.Overhead.A + parts.Decode.A + parts.Remainder.A
	m.BetaOverhead, m.BetaDecode, m.BetaRemainder = parts.Overhead.B, parts.Decode.B, parts.Remainder.B
	m.Beta = m.BetaOverhead + m.BetaDecode + m.BetaRemainder
	if !o.NoPrompt {
		m.fitSigmas(clean)
	}
	all := drawSteps(m, steps)
	var means []float64
	for _, p := range all {
		if p.priceError || !(p.mean > 0) {
			continue
		}
		mean, _ := p.d.Stats(m.A0, m.Beta)
		means = append(means, mean/p.mean)
	}
	if len(means) == 0 {
		return RankMixture{}, RankMixtureNLFit{}, errors.New("the fit window prices no step with a measured mean")
	}
	diag.FitMean = medianOf(means)
	diag.FitP90, _ = medianP90(all, m.A0, 1)
	return m, diag, nil
}

// promptMean is the step's mean whole prompt, its rank-weighted mean where
// PromptMean is not set, else its mean computed prompt.
func (s MixtureStep) promptMean() float64 {
	if s.PromptMean > 0 {
		return s.PromptMean
	}
	return s.xbar()
}

// assumedNL is Assumed for QueueRankMixtureNL.
func (m RankMixture) assumedNL() string {
	busiest := 0.0
	if len(m.Shares) > 0 {
		busiest = m.Shares[0] * float64(len(m.Shares))
	}
	kv := "no KV-deferral wait (not fitted)"
	if m.KV.Known {
		kv = fmt.Sprintf("KV-deferral wait p_d = %.3g + %.3g rho, m_d = %.3g + %.3g rho s (fitted over rho %.3g-%.3g)",
			m.KV.P0, m.KV.P1, m.KV.M0, m.KV.M1, m.KV.RhoLo, m.KV.RhoHi)
	}
	queue := fmt.Sprintf("%s (K = %d)", QueueLimitedProcessorSharing, m.Slots)
	if m.Slots <= 0 {
		queue = string(QueueProcessorSharing) + " (max-num-seqs not read)"
	}
	svc := fmt.Sprintf("S(x) = %.3g s + x / %.4g tok/s", m.S0, m.V)
	if m.Law == ServiceAttention {
		svc = fmt.Sprintf("S(x, c) = %.3g s + x / %.4g tok/s + %.3g ns x (c + x/2)", m.S0, m.V, m.B*1e9)
	}
	p := m.Parts
	return fmt.Sprintf("experimental TTFT model %s (OUTSIDE VALIDATED REGION): %s per rank on %d ranks "+
		"(busiest share %.3g of the mean), %s; %s; Z measured by part, not calibrated: overhead %.3g s + %.3g us x L, "+
		"decode %.3g s + %.3g us x L, router remainder %.3g s + %.3g us x L",
		QueueRankMixtureNL, queue, len(m.Shares), busiest, svc, kv, p.Overhead.A, p.Overhead.B*1e6,
		p.Decode.A, p.Decode.B*1e6, p.Remainder.A, p.Remainder.B*1e6)
}
