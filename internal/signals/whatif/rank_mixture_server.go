package whatif

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
)

// Remainder server (RankMixtureOptions.RemainderServer), a third mode of
// QueueRankMixtureNLLine, on top of the remainder spread
// (rank_mixture_modes.go). The router's remainder (its TTFT less both
// engines') is drawn as what the serving pods' API servers and the router
// itself measure it to be, not as one line:
//
//	R = (O + Ren_j) Q(u) + Fp_j(L) + N(L)
//
//	O      the router's request processing less its render call, the fit
//	       window's median of the router's processing mean less its render
//	       mean (llm_d_epp_token_producer_render_duration_seconds); a line
//	       in the prompt where the router renders nothing
//	Ren_j  the render call on prefill pod j's API server (its /render
//	       handler): (R0 + R1 L)/(1 - rho_j)
//	Fp_j   the generation request's frontend on pod j (its HTTP handler
//	       less vLLM's end-to-end latency): (F0 + F1 L)/(1 - rho_j)
//	rho_j  K lam_j (c (R0 + R1 L) + F0 + F1 L): the pod's API-server load,
//	       both request classes, one processor-sharing queue; lam_j the
//	       pod's rate (the priced rate times G times its share), c the
//	       render calls per generation request, K the share of the
//	       handlers' time that holds the server
//	N      the rest (sidecar, network, the decode pod's frontend): a
//	       non-negative line in the prompt, fitted to what the parts above
//	       leave of each fit step's remainder
//	Q      the remainder spread's shape: the router's request-processing
//	       law over its mean, so O + Ren_j is drawn on the router's measured
//	       processing law as the spread draws it
//
// The pod j is drawn by the fit window's busiest-first share profile of the
// prefill pods. K = 0 (the default) is no queue: each pod's API server is a
// delay, and the remainder no longer moves with the pods' rate. On a
// production P/D recording the K = 0 form read the load split's and the
// busiest steps' p90 and p95 closer to measured than the plain spread did,
// with no figure over 1.25 of measured on the load split; a fitted K added
// nothing there, so it is off (RankMixtureOptions.RemainderServerQueue).
//
// The lines are fitted on the fit window's pod-steps, each weighted by the
// square root of its requests, with a soft-L1 loss (robust to the few
// pod-steps a restart or a burst leaves far from the line). It needs the
// remainder spread's shape and MinRemainderServerSteps fit steps with a
// prefill pod's frontend recorded; without either, the remainder is the
// plain spread (or its line, as the spread falls back to it), and the
// assumed line says so.

// MinRemainderServerSteps is the fewest fit steps with a prefill pod's
// API-server frontend the remainder server is fitted on
// (RankMixtureOptions.RemainderServer).
const MinRemainderServerSteps = 12

// remainderServerRhoCap caps a pod's API-server utilization: the
// processor-sharing stretch 1/(1 - rho) is read no closer to 1.
const remainderServerRhoCap = 0.97

// remainderServerLossScale is the soft-L1 loss's scale on a pod-step's
// residual, the residual a share of the pod-steps' median and weighted by
// the square root of its requests.
const remainderServerLossScale = 0.3

// minServerPodRequests is the fewest requests a pod-step is read at.
const minServerPodRequests = 5

// ZServer is the router remainder read as the prefill pods' API servers and
// the router's own work (remainder server, above), seconds and seconds a
// prompt token.
type ZServer struct {
	// R0 and R1 are the render handler's line, F0 and F1 the generation
	// frontend's, and K the API-server load's share; K 0 is no queue.
	R0, R1, F0, F1, K float64
	// Shares is the prefill pods' busiest-first share of their requests,
	// summing to 1; G their requests a second per request the ranks
	// priced read; RenderPerRequest the render calls a pod takes per
	// generation request. HasRender says the pods recorded their /render
	// handler: without it the render part is 0 and O is the router's whole
	// processing, a line in the prompt (Other + Other1 L).
	Shares           []float64
	G                float64
	RenderPerRequest float64
	HasRender        bool
	Other, Other1    float64
	// N0 and N1 are the rest's line.
	N0, N1 float64
	// Steps and PodSteps are the fit steps and pod-steps it was fitted on.
	Steps, PodSteps int
}

// serverRow is one prefill pod-step as the remainder server fits it.
type serverRow struct {
	step              int
	lam, rlam, l, f   float64
	h                 float64 // render mean; NaN where not recorded
	w                 float64 // the square root of its requests
	hasRender, render bool
}

// serverStep is one fit step's router figures.
type serverStep struct {
	l, rate, r, proc, rend float64
}

// FitZServer fits the remainder server on steps of stepSeconds each:
// kFree fits K in [0, 1], else K is 0. off is why it was not fitted, ""
// where it was.
func FitZServer(steps []MixtureStep, stepSeconds float64, kFree bool) (z *ZServer, off string) {
	var rows []serverRow
	var sts []serverStep
	for _, st := range steps {
		if !(st.PromptMean > 0) || !(st.DecodeTTFT > 0) || !(st.PrefillTTFT > 0) || !(st.RouterMean > 0) ||
			!(st.RouterProcessingMean > 0) || !(st.Rate > 0) {
			continue
		}
		n := len(rows)
		for _, r := range st.Ranks {
			if r.Frontend == 0 || !Finite(r.Frontend) || !(r.Prompt > 0) || !(r.Rate*stepSeconds >= minServerPodRequests) {
				continue
			}
			row := serverRow{step: len(sts), lam: r.Rate, rlam: r.RenderRate, l: r.Prompt, f: r.Frontend, h: math.NaN(),
				w: math.Sqrt(r.Rate * stepSeconds)}
			if r.RenderRate > 0 && r.Render > 0 {
				row.h, row.hasRender = r.Render, true
			}
			rows = append(rows, row)
		}
		if len(rows) == n {
			continue
		}
		sts = append(sts, serverStep{l: st.PromptMean, rate: st.Rate, r: st.RouterMean - st.PrefillTTFT - st.DecodeTTFT,
			proc: st.RouterProcessingMean, rend: st.RouterRenderMean})
	}
	if len(sts) < MinRemainderServerSteps {
		return nil, fmt.Sprintf("fewer than %d fit steps hold a prefill pod's API-server frontend and the router's processing "+
			"(not recorded, or not exported)", MinRemainderServerSteps)
	}
	z = &ZServer{Steps: len(sts), PodSteps: len(rows)}
	withRender := 0
	for _, r := range rows {
		if r.hasRender {
			withRender++
		}
	}
	z.HasRender = withRender > len(rows)/2
	for i := range rows {
		rows[i].render = z.HasRender && rows[i].hasRender
	}
	z.fitLines(rows, kFree)
	// The pods' rate per priced request, their share profile and the
	// render calls per generation request.
	sum := make([]float64, len(sts))
	var lams [][]float64
	byStep := map[int][]float64{}
	var rr []float64
	for _, r := range rows {
		sum[r.step] += r.lam
		byStep[r.step] = append(byStep[r.step], r.lam)
		if z.HasRender {
			rr = append(rr, r.rlam/max(r.lam, 1e-9))
		}
	}
	var g []float64
	for i, s := range sts {
		if sum[i] > 0 {
			g = append(g, sum[i]/s.rate)
			lams = append(lams, byStep[i])
		}
	}
	z.G = medianOf(g)
	z.Shares = shareProfile(lams)
	if z.HasRender {
		z.RenderPerRequest = medianOf(rr)
	}
	// The router's own work outside the render call.
	if z.HasRender {
		o := make([]float64, len(sts))
		for i, s := range sts {
			o[i] = s.proc - s.rend
		}
		z.Other = medianOf(o)
	} else {
		x, y := make([]float64, len(sts)), make([]float64, len(sts))
		for i, s := range sts {
			x[i], y[i] = s.l, s.proc
		}
		z.Other, z.Other1 = nnLine(x, y, nil)
	}
	// The rest: what the parts leave of each step's remainder.
	x, y := make([]float64, len(sts)), make([]float64, len(sts))
	for i, s := range sts {
		mean, _ := z.meanParts(s.l, s.rate)
		x[i], y[i] = s.l, s.r-mean
	}
	z.N0, z.N1 = nnLine(x, y, nil)
	return z, ""
}

// fitLines fits R0, R1, F0, F1 and K on the pod-steps: with K fixed, the
// lines by iteratively reweighted least squares (the soft-L1 loss), the
// stretch 1/(1 - rho) read from the lines of the round before; with kFree,
// the K of least loss over a grid in [0, 1].
func (z *ZServer) fitLines(rows []serverRow, kFree bool) {
	fs, hs := make([]float64, 0, len(rows)), make([]float64, 0, len(rows))
	for _, r := range rows {
		fs = append(fs, r.f)
		if r.render {
			hs = append(hs, r.h)
		}
	}
	fm, hm := math.Abs(medianOf(fs)), 1.0
	if len(hs) > 0 {
		hm = medianOf(hs)
	}
	fm, hm = max(fm, 1e-9), max(hm, 1e-9)
	ks := []float64{0}
	if kFree {
		ks = ks[:0]
		for i := range 41 {
			ks = append(ks, float64(i)/40)
		}
	}
	best := math.Inf(1)
	for _, k := range ks {
		c := ZServer{K: k, HasRender: z.HasRender}
		loss := c.fitAt(rows, fm, hm)
		if loss < best {
			best = loss
			z.R0, z.R1, z.F0, z.F1, z.K = c.R0, c.R1, c.F0, c.F1, k
		}
	}
}

// fitAt fits the lines at z.K and is their soft-L1 loss.
func (z *ZServer) fitAt(rows []serverRow, fm, hm float64) float64 {
	n := len(rows)
	rho := make([]float64, n)
	lf, yf, wf := make([]float64, 0, n), make([]float64, 0, n), make([]float64, 0, n)
	lh, yh, wh := make([]float64, 0, n), make([]float64, 0, n), make([]float64, 0, n)
	robust := func(res float64) float64 {
		return 1 / math.Sqrt(1+(res/remainderServerLossScale)*(res/remainderServerLossScale))
	}
	rounds := 40
	if z.K == 0 {
		rounds = 30
	}
	for round := range rounds {
		lf, yf, wf, lh, yh, wh = lf[:0], yf[:0], wf[:0], lh[:0], yh[:0], wh[:0]
		for i, r := range rows {
			sr, sg := z.service(r.l)
			rho[i] = 0
			if z.K > 0 && round > 0 {
				rho[i] = min(z.K*(r.rlam*sr+r.lam*sg), remainderServerRhoCap)
			}
			s := 1 - rho[i]
			// The residual (line/s - y)/m w is (line - y s) w/(m s): a
			// line fitted to y s at weight (w/(m s))^2.
			wg := r.w / (fm * s)
			rw := 1.0
			if round > 0 {
				rw = robust((sg/s - r.f) * r.w / fm)
			}
			lf, yf, wf = append(lf, r.l), append(yf, r.f*s), append(wf, wg*wg*rw)
			if r.render {
				wr := r.w / (hm * s)
				rw := 1.0
				if round > 0 {
					rw = robust((sr/s - r.h) * r.w / hm)
				}
				lh, yh, wh = append(lh, r.l), append(yh, r.h*s), append(wh, wr*wr*rw)
			}
		}
		z.F0, z.F1 = nnLine(lf, yf, wf)
		if z.HasRender && len(lh) > 0 {
			z.R0, z.R1 = nnLine(lh, yh, wh)
		}
	}
	var loss float64
	soft := func(res float64) float64 {
		u := res / remainderServerLossScale
		return 2 * (math.Sqrt(1+u*u) - 1)
	}
	for _, r := range rows {
		sr, sg := z.service(r.l)
		s := 1.0
		if z.K > 0 {
			s = 1 - min(z.K*(r.rlam*sr+r.lam*sg), remainderServerRhoCap)
		}
		loss += soft((sg/s - r.f) * r.w / fm)
		if r.render {
			loss += soft((sr/s - r.h) * r.w / hm)
		}
	}
	return loss
}

// service is the render and generation service at prompt l: R0 + R1 l (0
// without render) and F0 + F1 l.
func (z *ZServer) service(l float64) (sr, sg float64) {
	if z.HasRender {
		sr = z.R0 + z.R1*l
	}
	return sr, z.F0 + z.F1*l
}

// pods is each pod's render and frontend time and its utilization at
// prompt l and priced rate.
func (z *ZServer) pods(l, rate float64) (ren, fp, rho []float64) {
	sr, sg := z.service(l)
	ren, fp, rho = make([]float64, len(z.Shares)), make([]float64, len(z.Shares)), make([]float64, len(z.Shares))
	for j, sh := range z.Shares {
		lam := rate * z.G * sh
		if z.K > 0 {
			rho[j] = min(z.K*(lam*z.RenderPerRequest*sr+lam*sg), remainderServerRhoCap)
		}
		ren[j], fp[j] = sr/(1-rho[j]), sg/(1-rho[j])
	}
	return ren, fp, rho
}

// other is the router's work outside its render call at prompt l.
func (z *ZServer) other(l float64) float64 {
	if z.HasRender {
		return z.Other
	}
	return z.Other + z.Other1*l
}

// meanParts is the server's mean at prompt l and priced rate, N left
// out: its total, and the router's part (O and the render).
func (z *ZServer) meanParts(l, rate float64) (total, router float64) {
	ren, fp, _ := z.pods(l, rate)
	router = z.other(l)
	var frontend float64
	for j, sh := range z.Shares {
		router += sh * ren[j]
		frontend += sh * fp[j]
	}
	return router + frontend, router
}

// Mean is the server's mean remainder at prompt l and priced rate, seconds.
func (z *ZServer) Mean(l, rate float64) float64 {
	total, _ := z.meanParts(l, rate)
	return total + z.N0 + z.N1*l
}

// zServerSeed fixes the server's draws.
const zServerSeed = zSeed + 7

// draws is each request's remainder at its prompt l and the priced rate,
// O and the render drawn on shape: the render at the requests' mean
// prompt, the frontend and the rest at each request's own.
func (z *ZServer) draws(l []float64, rate float64, shape []float64) []float64 {
	out := make([]float64, len(l))
	if len(l) == 0 {
		return out
	}
	var lb float64
	for _, li := range l {
		lb += li
	}
	lb /= float64(len(l))
	ren, _, rho := z.pods(lb, rate)
	cum := make([]float64, len(z.Shares))
	var acc float64
	for j, sh := range z.Shares {
		acc += sh
		cum[j] = acc
	}
	rng := rand.New(rand.NewPCG(zServerSeed, uint64(len(l))))
	o := z.other(lb)
	for i, li := range l {
		j, _ := slices.BinarySearch(cum, rng.Float64()*acc)
		j = min(j, len(cum)-1)
		_, sg := z.service(li)
		out[i] = (o+ren[j])*shapeAt(shape, rng.Float64()) + sg/(1-rho[j]) + z.N0 + z.N1*li
	}
	return out
}

// shareProfile is the busiest-first share profile of each step's pod rates,
// averaged over the steps that hold the modal pod count; nil with none.
func shareProfile(byStep [][]float64) []float64 {
	count := map[int]int{}
	for _, v := range byStep {
		count[len(v)]++
	}
	modal, most := 0, 0
	for n, c := range count {
		if c > most || (c == most && n > modal) {
			modal, most = n, c
		}
	}
	if modal == 0 {
		return nil
	}
	out := make([]float64, modal)
	k := 0
	for _, v := range byStep {
		if len(v) != modal {
			continue
		}
		s := slices.Clone(v)
		slices.Sort(s)
		slices.Reverse(s)
		var tot float64
		for _, x := range s {
			tot += x
		}
		if !(tot > 0) {
			continue
		}
		for i, x := range s {
			out[i] += x / tot
		}
		k++
	}
	if k == 0 {
		return nil
	}
	for i := range out {
		out[i] /= float64(k)
	}
	return out
}

// nnLine is the weighted least-squares line y = a + b x with a and b held
// at 0 or above (nil w weighs each point 1): the free line where it is
// feasible, else the best of the line through the origin, the mean and 0.
func nnLine(x, y, w []float64) (a, b float64) {
	weight := func(i int) float64 {
		if w == nil {
			return 1
		}
		return w[i]
	}
	var sw, sx, sy, sxx, sxy float64
	for i := range x {
		wi := weight(i)
		sw += wi
		sx += wi * x[i]
		sy += wi * y[i]
		sxx += wi * x[i] * x[i]
		sxy += wi * x[i] * y[i]
	}
	if !(sw > 0) {
		return 0, 0
	}
	if d := sw*sxx - sx*sx; d > 1e-12*sw*sxx {
		b = (sw*sxy - sx*sy) / d
		a = (sy - b*sx) / sw
		if a >= 0 && b >= 0 {
			return a, b
		}
	}
	sse := func(a, b float64) float64 {
		var e float64
		for i := range x {
			r := y[i] - a - b*x[i]
			e += weight(i) * r * r
		}
		return e
	}
	a, b = 0, 0
	best := sse(0, 0)
	if m := sy / sw; m > 0 && sse(m, 0) < best {
		a, b, best = m, 0, sse(m, 0)
	}
	if sxx > 0 {
		if s := sxy / sxx; s > 0 && sse(0, s) < best {
			a, b = 0, s
		}
	}
	return a, b
}
