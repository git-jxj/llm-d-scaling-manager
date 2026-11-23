package whatif

import (
	"errors"
	"fmt"
	"math"
	"slices"
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
// [MinNLFitP90, MaxNLFitMean].

// QueueRankMixtureNL is the experimental TTFT model with the non-linear
// prefill service and the decomposed non-queue part (rank_mixture_nl.go).
// Experimental; only a caller that asks prices it.
const QueueRankMixtureNL QueueModel = "rank-mixture-nl"

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
// prompts did not spread (B is then 0 and A the part's median).
type ZPart struct {
	A, B       float64
	Identified bool
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
}

// solveSmall is the least-squares solution of X c = y over the columns in
// cols (normal equations, Gaussian elimination); ok is false where they are
// singular.
func solveSmall(X [][]float64, y []float64, cols []int) ([]float64, bool) {
	k := len(cols)
	a := make([][]float64, k)
	for i := range a {
		a[i] = make([]float64, k+1)
	}
	for r := range X {
		for i, ci := range cols {
			for j, cj := range cols {
				a[i][j] += X[r][ci] * X[r][cj]
			}
			a[i][k] += X[r][ci] * y[r]
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

// nnls is the non-negative least-squares solution of X c = y for a few
// columns, by trying every subset of free columns (the rest held at 0) and
// keeping the feasible one of least squared error. The columns are scaled to
// their mean magnitude first. ok is false with no row.
func nnls(X [][]float64, y []float64) ([]float64, bool) {
	if len(X) == 0 || len(X) != len(y) {
		return nil, false
	}
	n := len(X[0])
	scale := make([]float64, n)
	for _, row := range X {
		for j, v := range row {
			scale[j] += math.Abs(v)
		}
	}
	Xs := make([][]float64, len(X))
	for i, row := range X {
		Xs[i] = make([]float64, n)
		for j, v := range row {
			if scale[j] > 0 {
				Xs[i][j] = v / (scale[j] / float64(len(X)))
			}
		}
	}
	sse := func(c []float64) float64 {
		var e float64
		for i, row := range Xs {
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
		sol, ok := solveSmall(Xs, y, cols)
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
			best[j] /= scale[j] / float64(len(X))
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
	if s0, a, b, ok := FitAttention(steps); ok {
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
	clean := m.cleanSteps(steps)
	diag := RankMixtureNLFit{Steps: len(steps), CleanSteps: len(clean), ShareCV: cv}
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
