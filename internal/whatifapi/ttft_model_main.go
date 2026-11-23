package whatifapi

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// This file prices the main view's TTFT (POST /v1/plan, /v1/size and
// /v1/report) with the experimental rank-mixture model, where the server
// (Config.TTFTModel) or the inputs request (InputsRequest.TTFTModel) asks for
// it. The model is fitted on the source's own window when the source is
// resolved, as a backtest fits it on its fit window (fitRankMixture), and
// abstains where its identifiability checks fail (identified): the
// planner's own answer then stands at every rate. Where it prices, it prices
// every rate of the TTFT curve up to the planner's own lower knee, and no
// further: the knee the rank mixture would read from its fitted service
// rate is a processor-sharing inversion, which read 1.06 to 1.56 times the
// measured capacity on the lab recordings (issue #406), so the curve never
// runs past the knee the planner itself answers. A rate above the highest
// step rate the fit window served is marked Figure.Extrapolated.

// The main view's words for the experimental model.
const (
	// TTFTModelLabel names the model on the TTFT figure and its curve.
	TTFTModelLabel = "Experimental TTFT model"
	// TTFTBeyondMeasured labels the curve past the highest load measured.
	TTFTBeyondMeasured = "beyond the highest load measured"
	// TTFTRightEndEstimate says the curve's right end, under a knee that is
	// a lower bound or refused, is the model's estimate.
	TTFTRightEndEstimate = "the curve's right end is an estimate from the fitted prefill service rate"
)

// TTFTModelInfo is the experimental TTFT model as fitted on a source
// (InputsResponse.TTFTModel).
type TTFTModelInfo struct {
	// Model is whatif.QueueRankMixture; Experimental is always true; Label
	// is TTFTModelLabel.
	Model        string `json:"model"`
	Experimental bool   `json:"experimental"`
	Label        string `json:"label"`
	// Abstained is why the model prices nothing, "" where it prices.
	Abstained string `json:"abstained,omitempty"`
	// HeldOut is the model's mean TTFT scored on the fit window's
	// self-check folds (whatif.RankMixtureHeldOut), and Evidence that score
	// in words; both absent where nothing was scored.
	HeldOut  *HeldOutScore `json:"heldOut,omitempty"`
	Evidence string        `json:"evidence,omitempty"`
	// MeasuredUpTo is the highest step rate the fit window served,
	// requests a second, at the recorded fleet.
	MeasuredUpTo *float64 `json:"measuredUpTo,omitempty"`
	// Fit is the model's fit, as a backtest's answer carries it.
	Fit *RankMixtureReport `json:"fit,omitempty"`
}

// HeldOutScore is the experimental model scored on the source's held-out
// self-check folds (whatif.RankMixtureHeldOut): at Statistic (the mean; the
// p90 calibrates the model's spread on those same folds, so it is not
// held out), Within of Steps were within BacktestTolerance of the measured
// figure and Low read more than that low; MedianRatio is predicted over
// measured. RateFrom and RateTo are the rates the steps scored ran at, and
// Prefill and Decode the recorded fleet: the range the score covers.
type HeldOutScore struct {
	Statistic   string  `json:"statistic"`
	Within      int     `json:"within"`
	Low         int     `json:"low"`
	Steps       int     `json:"steps"`
	MedianRatio float64 `json:"medianRatio"`
	RateFrom    float64 `json:"rateFrom"`
	RateTo      float64 `json:"rateTo"`
	Prefill     int     `json:"prefill"`
	Decode      int     `json:"decode"`
}

// TTFTModelView is the experimental model at a plan's fleet
// (TTFTView.Model).
type TTFTModelView struct {
	Model        string `json:"model"`
	Experimental bool   `json:"experimental"`
	Label        string `json:"label"`
	// Priced says the model priced this fleet's TTFT; NotPriced is why not
	// where it did not (the model abstained, or the fleet is one it does
	// not price), and the planner's own TTFT stands.
	Priced    bool   `json:"priced"`
	NotPriced string `json:"notPriced,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
	// MeasuredUpTo is the highest load the fit window served, at this
	// fleet's prefill count; a curve point above it is Extrapolated and
	// drawn as Beyond says.
	MeasuredUpTo *float64 `json:"measuredUpTo,omitempty"`
	Beyond       string   `json:"beyond,omitempty"`
	// RightEnd says the curve's right end is the model's estimate, where
	// the prefill knee it stops at is a lower bound or refused.
	RightEnd string `json:"rightEnd,omitempty"`
}

// mainMixture is the experimental model fitted on a source for the main
// view.
type mainMixture struct {
	rq *rankMixtureQ
	// pooled is the source window's cycles pooled into one step: the
	// computed-token and prompt histograms every rate is priced at.
	pooled whatif.MixtureStep
	// top is the highest step rate the fit served; prefill the recorded
	// prefill replicas.
	top     float64
	prefill int
	info    TTFTModelInfo
}

// ttftModelOf is the model a source is resolved with: the request's, else
// the server's. An unknown model is the caller's error.
func ttftModelOf(req, server string) (string, error) {
	m := req
	if m == "" {
		m = server
	}
	if m != "" && m != string(whatif.QueueRankMixture) {
		return "", InputErrorf("ttftModel %q, want %q or none", m, whatif.QueueRankMixture)
	}
	return m, nil
}

// fitMainMixture fits the model on e's cycles up to the cycle answered, cut
// into DefaultBacktestStep steps, with the prefill engine's max-num-seqs
// read from the source.
func fitMainMixture(e *inputsEntry) *mainMixture {
	mm := &mainMixture{info: TTFTModelInfo{Model: string(whatif.QueueRankMixture), Experimental: true, Label: TTFTModelLabel}}
	if p := e.doc.Fleet.Prefill; p != nil {
		mm.prefill = p.Replicas
	}
	abstain := func(why string) *mainMixture {
		mm.rq = abstainedMixture(why)
		mm.info.Abstained = why
		return mm
	}
	switch {
	case e.doc.Fleet.Prefill == nil:
		return abstain("the source has no prefill role")
	case len(e.mixture) != len(e.series) || len(e.series) == 0:
		return abstain("the source did not give the per-rank series")
	}
	answered := e.series[len(e.series)-1].At
	if c := e.doc.Source.Cycle; c > 0 && c <= len(e.series) {
		answered = e.series[c-1].At
	}
	var idx []int
	for i, s := range e.series {
		if !s.At.After(answered) {
			idx = append(idx, i)
		}
	}
	rq, steps, err := fitRankMixtureSteps(e, idx, DefaultBacktestStep, e.engine.PrefillSlots)
	if err != nil {
		return abstain(err.Error())
	}
	mm.rq = rq.identified()
	r := mm.rq.report
	mm.info.Fit = &r
	if r.Abstained != "" {
		mm.info.Abstained = r.Abstained
		return mm
	}
	for _, st := range steps {
		mm.top = max(mm.top, st.Rate)
	}
	if mm.top > 0 {
		mm.info.MeasuredUpTo = ptr(mm.top)
	}
	first := e.series[idx[0]].At
	mm.pooled = whatif.MixtureStepOf(mixtureWindow(e, idx, first.Add(-time.Nanosecond), answered), answered)
	o := whatif.RankMixtureOptions{Slots: rq.m.Slots, MinSpreadSpan: whatif.MinSpreadFitWindow, SpreadFloor: whatif.MinSpread}
	if h := whatif.RankMixtureHeldOut(steps, o, rq.m.Spread, BacktestTolerance); h.Steps > 0 {
		mm.info.HeldOut = &HeldOutScore{Statistic: statisticMean, Within: h.Within, Low: h.Low, Steps: h.Steps, MedianRatio: h.MedianRatio,
			RateFrom: h.RateLo, RateTo: h.RateHi, Prefill: mm.prefill, Decode: e.doc.Fleet.Decode.Replicas}
		mm.info.Evidence = heldOutWords(h.Within, h.Steps)
	}
	return mm
}

// heldOutWords is a held-out score in the page's words.
func heldOutWords(within, n int) string {
	return fmt.Sprintf("held-out steps: %d of %d within %.0f%%", within, n, 100*BacktestTolerance)
}

// priced says the model prices.
func (mm *mainMixture) priced() bool {
	return mm != nil && mm.rq != nil && mm.rq.report.Abstained == ""
}

// notPricedAt is why the model does not price the fleet at k, "" where it
// does: it prices the source's own traffic, so a stated shape or term, a
// stated arrival burstiness, or a fleet with no prefill replica is the
// planner's alone.
func (mm *mainMixture) notPricedAt(k Knobs) string {
	switch {
	case !mm.priced():
		return "the model abstained: " + mm.info.Abstained
	case mm.prefill <= 0 || k.Prefill <= 0:
		return "the fleet has no prefill replica"
	case k.PromptTokens != 0 || k.OutputTokens != 0 || (k.PrefixHit != nil && !k.PrefixHitRead) || k.CachedTokenCost != nil:
		return "the model prices the source's own request shape, not a stated one"
	case statesTTFTTerm(k.Assume) || k.ArrivalCV2 != nil:
		return "the model prices the source's own terms and random arrivals, not stated ones"
	}
	return ""
}

// statesTTFTTerm says a states a term the planner's TTFT is priced from:
// V_P, a, the prompt spread or the router hop. The decode terms and the
// router's flow control are not the model's.
func statesTTFTTerm(a Assume) bool {
	return a.VP != nil || a.A != nil || a.PromptCV != nil || a.RouterHop != nil
}

// mixtureFleet is a Fleet whose TTFT the experimental model prices, up to
// the planner's own lower knee (curveCap); every other figure is the
// planner's.
type mixtureFleet struct {
	Fleet
	mm *mainMixture
	// q is the TTFT statistic, 0 the mean; scale is the recorded prefill
	// replicas over the fleet's: the rate one recorded prefill replica's
	// worth of ranks takes.
	q, scale float64
	// knee is the highest rate priced (curveCap), kneeOK false with no
	// knee answered; lowerBound says the prefill knee is a lower bound or
	// refused.
	knee       float64
	kneeOK     bool
	lowerBound bool
	floor      float64
}

// fleet is inner with the model's TTFT, or inner where the model does not
// price at k.
func (mm *mainMixture) fleet(inner Fleet, k Knobs) Fleet {
	if mm == nil || mm.notPricedAt(k) != "" {
		return inner
	}
	f := &mixtureFleet{Fleet: inner, mm: mm, q: k.TTFTPercentile, scale: float64(mm.prefill) / float64(k.Prefill)}
	pk, dk := inner.PrefillKnee(), inner.DecodeKnee()
	f.lowerBound = isRefusal(pk) || slices.Contains(figureOf(pk, unitRate, false).Flags, string(whatif.VPLowerBound))
	pv, pok := valueOf(pk)
	dv, dok := valueOf(dk)
	f.knee, f.kneeOK = curveCap(pv, pok, f.lowerBound, dv, dok, mm.top/f.scale)
	// The floor: the TTFT with no queue, at a rate whose utilization is
	// negligible.
	if v, ok := f.price(1e-6); ok {
		f.floor = v
	}
	return f
}

// price is the model's TTFT at the fleet's rate; ok false where it refused.
func (f *mixtureFleet) price(rate float64) (float64, bool) {
	v, _, u := f.mm.rq.m.Price(rate*f.scale, f.mm.pooled.Computed, f.mm.pooled.Prompt, f.q)
	if u != nil || !whatif.Finite(v) || math.IsNaN(v) {
		return 0, false
	}
	return v, true
}

// curveCap is the highest rate the model prices: the lower of the
// planner's prefill knee (pv, pok) and decode knee (dv, dok), where a
// prefill knee that is only a lower bound (lowerBound) is raised to served,
// the highest load the source served at this fleet: the fleet served it, so
// the knee is at least there. ok is false with neither knee answered. A
// refused prefill knee leaves decode's alone.
func curveCap(pv float64, pok, lowerBound bool, dv float64, dok bool, served float64) (float64, bool) {
	if pok && lowerBound && served > pv {
		pv = served
	}
	switch {
	case pok && dok:
		return min(pv, dv), true
	case pok:
		return pv, true
	case dok:
		return dv, true
	}
	return 0, false
}

// curveEnd is the highest rate the model prices: the curve is drawn up to
// it, past the planner's lower knee where the fleet served past a knee that
// is only a lower bound.
func (f *mixtureFleet) curveEnd() float64 { return f.knee }

// within says the model prices rate: up to curveCap.
func (f *mixtureFleet) within(rate float64) bool {
	return f.kneeOK && rate > 0 && rate <= f.knee*(1+1e-9)
}

// TTFTAt is the model's TTFT at rate, up to the planner's lower knee; past
// it, or where the model refuses, the planner's.
func (f *mixtureFleet) TTFTAt(rate float64) whatif.Result {
	if !f.within(rate) {
		return f.Fleet.TTFTAt(rate)
	}
	v, ok := f.price(rate)
	if !ok {
		return f.Fleet.TTFTAt(rate)
	}
	return whatif.TTFT{Seconds: v, Independent: v, Pooled: v, Floor: f.floor, Flags: []whatif.Flag{whatif.RankMixtureTTFT}}
}

// TTFTPoint is TTFTAt with the planner's notes.
func (f *mixtureFleet) TTFTPoint(rate float64) (whatif.Result, []string) {
	r, notes := f.Fleet.TTFTPoint(rate)
	if !f.within(rate) {
		return r, notes
	}
	if v, ok := f.price(rate); ok {
		return whatif.TTFT{Seconds: v, Independent: v, Pooled: v, Floor: f.floor, Flags: []whatif.Flag{whatif.RankMixtureTTFT}}, notes
	}
	return r, notes
}

// Assumed is the planner's assumed inputs and the model's.
func (f *mixtureFleet) Assumed() []string {
	out := slices.Clone(f.Fleet.Assumed())
	out = append(out, f.mm.rq.report.Assumed+"; TTFT priced at the source window's pooled prompt mix, up to the planner's own lower knee")
	if f.scale != 1 {
		out = append(out, fmt.Sprintf("experimental TTFT model at %d prefill replicas, %d recorded: each replica's ranks take the recorded "+
			"share profile, at the rate per replica", int(math.Round(float64(f.mm.prefill)/f.scale)), f.mm.prefill))
	}
	return out
}

// extrapolated says the model's TTFT at rate is above the highest load the
// fit window served, at this fleet.
func (f *mixtureFleet) extrapolated(rate float64) bool {
	return f.within(rate) && f.mm.top > 0 && rate*f.scale > f.mm.top*(1+1e-9)
}

// extrapolating is a Fleet that marks the rates it prices beyond what was
// measured.
type extrapolating interface {
	extrapolated(rate float64) bool
}

// markExtrapolated sets f.Extrapolated where fl prices rate beyond what was
// measured and f is answered.
func markExtrapolated(fl Fleet, rate float64, f Figure) Figure {
	if x, ok := fl.(extrapolating); ok && f.Value != nil && x.extrapolated(rate) {
		f.Extrapolated = true
	}
	return f
}

// view is the model at the fleet at k, for a plan's TTFT view.
func (mm *mainMixture) view(fl Fleet, k Knobs) *TTFTModelView {
	if mm == nil {
		return nil
	}
	v := &TTFTModelView{Model: mm.info.Model, Experimental: true, Label: TTFTModelLabel, Evidence: mm.info.Evidence}
	f, ok := fl.(*mixtureFleet)
	if !ok {
		v.NotPriced = mm.notPricedAt(k)
		if v.NotPriced == "" {
			v.NotPriced = "no knee answered"
		}
		return v
	}
	v.Priced = f.kneeOK
	if !f.kneeOK {
		v.NotPriced = "no knee answered to stop the curve at"
	}
	if f.mm.top > 0 {
		v.MeasuredUpTo = ptr(f.mm.top / f.scale)
		v.Beyond = TTFTBeyondMeasured
	}
	if f.lowerBound {
		v.RightEnd = TTFTRightEndEstimate
	}
	return v
}

// applyToReport prices the report's TTFT tables with the model, where it
// prices: the measured shape's rows at the recorded or asked fleet, each
// rate up to the shape's lower knee. The verdict and the summary cells are
// the planner's own, and the shape's assumed lines say so.
func (mm *mainMixture) applyToReport(out *ReportResponse, q ReportQuery) {
	if !mm.priced() || out.Fleet.Prefill <= 0 || statesTTFTTerm(q.Assume) || q.ArrivalCV2 != nil {
		return
	}
	scale := float64(mm.prefill) / float64(out.Fleet.Prefill)
	for i := range out.Shapes {
		sh := &out.Shapes[i]
		if sh.Label != "measured" {
			continue
		}
		pk, dk := sh.Capacity.PrefillKnee, sh.Capacity.DecodeKnee
		pv, dv := 0.0, 0.0
		if pk.Value != nil {
			pv = *pk.Value
		}
		if dk.Value != nil {
			dv = *dk.Value
		}
		knee, ok := curveCap(pv, pk.Value != nil, pk.LowerBound || pk.Refused != nil, dv, dk.Value != nil, mm.top/scale)
		if !ok {
			continue
		}
		moved := false
		for j := range sh.Table {
			row := &sh.Table[j]
			if !(row.Rate > 0) || row.Rate > knee*(1+1e-9) {
				continue
			}
			for s := range row.TTFT {
				st := &row.TTFT[s]
				p, err := parseStatistic("ttft", st.Statistic)
				if err != nil {
					continue
				}
				v, _, u := mm.rq.m.Price(row.Rate*scale, mm.pooled.Computed, mm.pooled.Prompt, p)
				if u != nil || !whatif.Finite(v) {
					continue
				}
				st.Figure = Figure{Value: ptr(v), Unit: unitSeconds, Flags: []string{string(whatif.RankMixtureTTFT)},
					Extrapolated: mm.top > 0 && row.Rate*scale > mm.top*(1+1e-9)}
				moved = true
			}
		}
		if moved {
			if !slices.Contains(sh.Flags, string(whatif.RankMixtureTTFT)) {
				sh.Flags = append(sh.Flags, string(whatif.RankMixtureTTFT))
			}
			line := TTFTModelLabel + " (" + string(whatif.QueueRankMixture) + ") prices the table's TTFT up to the lower knee"
			if mm.info.Evidence != "" {
				line += " (" + mm.info.Evidence + ")"
			}
			sh.Assumed = append(sh.Assumed, line+"; the verdict and the summary cells are the planner's own")
		}
	}
}

// ApplyTTFTModel fits the experimental TTFT model on res, a source resolved
// with InputsRequest.TTFTModel, and prices out's TTFT tables with it as
// POST /v1/report does: at-replicas --experimental-ttft=v2. It returns the
// model as fitted, abstained or not.
func ApplyTTFTModel(res *Resolved, out *ReportResponse, q ReportQuery) *TTFTModelInfo {
	mm := fitMainMixture(&inputsEntry{doc: res.Inputs, series: res.Series, latency: res.Latency, mixture: res.Mixture, engine: res.Engine})
	mm.applyToReport(out, q)
	info := mm.info
	return &info
}
