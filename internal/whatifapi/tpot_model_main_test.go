package whatifapi

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi/apitest"
)

// tpotEntry is a 4 h source with its decode series (two replicas, a fifth
// apart in token time, two of three drafts accepted a step, output lengths
// from two tokens to a thousand) and router TPOT histograms drawn from the
// distribution (tdEntries' fit), recorded at two decode replicas, whose
// stub refuses every TPOT percentile (no R_q). With model, the entry is
// resolved with the experimental TPOT model, its decode engine read at
// slots max-num-seqs and three speculative tokens.
func tpotEntry(t *testing.T, model bool, slots int) (*Server, *inputsEntry) {
	t.Helper()
	e, _ := tdEntries(t, 960)
	e.doc.Fleet.Decode.Replicas = 2
	e.engine = EngineSlots{DecodeSlots: slots, SpecTokens: 3}
	e.id = "sha256:main-tpot"
	if model {
		e.tpotModel = fitMainTPOT(e)
		info := e.tpotModel.info
		e.doc.TPOTModel = &info
		e.id += "-v2"
	}
	s := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"})
	s.inputs.add(e.id, e)
	return s, e
}

// tpotPlan is the plan of the entry at two prefill and two decode replicas
// with a TPOT target of target at statistic.
func tpotPlan(t *testing.T, s *Server, id, statistic string, target float64, mix *Mix) (PlanResponse, []byte) {
	t.Helper()
	req := PlanRequest{InputsID: id, Fleet: &FleetKnob{Prefill: intp(2), Decode: intp(2)},
		SLO: &SLORequest{TPOT: &Objective{Statistic: statistic, Target: f64(target)}}}
	if mix != nil {
		req.Traffic = &Traffic{Mix: mix}
	}
	return planBody(t, s, req)
}

// On a fleet whose planner refuses every TPOT percentile, the experimental
// TPOT model, fitted on the source's window, prices the p90 curve, point A
// and decode's limit at the target: each point is the planner's own mean
// times the model's ratio at the window's output lengths and decode
// spread, flagged TPOTDistribution, the points above the highest step rate
// the window served (per decode replica) marked extrapolated and those
// under it not. Decode's limit is the rate whose mean meets the target over
// the ratio. Without the model the same fleet's percentile is refused
// throughout, and the plan names no part of the model.
func TestMainViewTPOT_ExperimentalModelPricesThePercentileCurve(t *testing.T) {
	const target = 0.02
	s0, e0 := tpotEntry(t, false, 64)
	base, baseBody := tpotPlan(t, s0, e0.id, "p90", target, nil)
	for _, p := range base.TPOT.Curve {
		require.NotNil(t, p.Figure.Refused, "the default refuses the p90 at %g r/s", p.Rate)
	}
	require.NotNil(t, base.TPOT.AtTarget.Rate.Refused)
	assert.Nil(t, base.TPOT.Model)
	for _, word := range []string{"extrapolated", "TPOTDistribution", "tpotModel", tpotModelSource} {
		assert.NotContains(t, string(baseBody), word)
	}

	s, e := tpotEntry(t, true, 64)
	mt := e.tpotModel
	require.Empty(t, mt.info.Abstained)
	fit := mt.info.Fit
	require.NotNil(t, fit)
	assert.Greater(t, fit.C, fit.Renewal, "C is fitted above its speculative-decoding floor")
	assert.Positive(t, fit.Renewal)
	h := mt.info.HeldOut
	require.NotNil(t, h)
	assert.Positive(t, h.Steps)
	assert.LessOrEqual(t, h.P90Inside, h.Steps)
	assert.LessOrEqual(t, h.P95Inside, h.Steps)
	assert.Equal(t, 2, h.Decode)
	assert.Regexp(t, `^held-out steps, at the measured mean: p90 in the measured bucket on \d+ of \d+, p95 on \d+$`, mt.info.Evidence)
	top := *mt.info.MeasuredUpTo
	assert.InDelta(t, 13.5, top, 0.5, "the highest step rate the window served")

	// The ratio is the lognormal's at the window's spread: above one, and
	// the p95's above the p90's.
	s2 := mt.dist.GroupCV2 + mt.dist.C*mt.inv
	r90 := math.Exp(math.Sqrt(s2)*1.2815515655446004 - s2/2)
	assert.InEpsilon(t, r90, mt.ratio(0.9), 1e-9)
	assert.Greater(t, mt.ratio(0.9), 1.0)
	assert.Greater(t, mt.ratio(0.95), mt.ratio(0.9))

	out, body := tpotPlan(t, s, e.id, "p90", target, nil)
	assert.Empty(t, apitest.CheckFigures(body), "%s", body)
	priced, beyond := 0, 0
	for _, p := range out.TPOT.Curve {
		require.NotNil(t, p.Figure.Value, "the model prices %g r/s", p.Rate)
		mean := 0.005 + p.Rate/1000
		assert.InEpsilon(t, mt.ratio(0.9)*mean, *p.Figure.Value, 1e-9, "the planner's mean times the ratio at %g r/s", p.Rate)
		assert.Contains(t, p.Figure.Flags, string(whatif.TPOTDistribution))
		assert.Equal(t, p.Rate > top, p.Figure.Extrapolated, "extrapolated at %g r/s (top %g)", p.Rate, top)
		priced++
		if p.Figure.Extrapolated {
			beyond++
		}
	}
	assert.Greater(t, priced, 30)
	assert.Positive(t, beyond)
	assert.Less(t, beyond, priced)

	v := out.TPOT.Model
	require.NotNil(t, v)
	assert.True(t, v.Priced)
	assert.True(t, v.Experimental)
	assert.Equal(t, TPOTModelLabel, v.Label)
	assert.Equal(t, TTFTBeyondMeasured, v.Beyond)
	assert.Equal(t, mt.info.Evidence, v.Evidence)
	assert.InDelta(t, top, *v.MeasuredUpTo, 1e-9)
	require.NotNil(t, out.TPOT.Ratio)
	assert.Equal(t, tpotModelSource, out.TPOT.Ratio.Source)
	assert.InEpsilon(t, mt.ratio(0.9), *out.TPOT.Ratio.Value, 1e-12)

	a := out.TPOT.AtRate.Figure
	require.NotNil(t, a.Value, "point A is priced")
	assert.Contains(t, a.Flags, string(whatif.TPOTDistribution))
	limit := out.TPOT.AtTarget.Rate
	require.NotNil(t, limit.Value, "decode's limit at the target is priced")
	assert.InEpsilon(t, (target/mt.ratio(0.9)-0.005)*1000, *limit.Value, 1e-9, "the mean's limit at the target over the ratio")
	assert.Contains(t, limit.Flags, string(whatif.TPOTDistribution))
	assert.False(t, limit.Extrapolated, "a limit under the load served is not extrapolated")
	assert.InEpsilon(t, target, mt.ratio(0.9)*(0.005+*limit.Value/1000), 1e-9, "the p90 at the limit is the target")
	assert.Contains(t, strings.Join(out.Assumed, "; "), "experimental TPOT distribution")
}

// The p95 is priced at its own ratio; any other percentile, and the mean,
// stay the planner's, the view saying why.
func TestMainViewTPOT_PricesP90AndP95Only(t *testing.T) {
	s, e := tpotEntry(t, true, 64)
	p95, _ := tpotPlan(t, s, e.id, "p95", 0.02, nil)
	require.True(t, p95.TPOT.Model.Priced)
	p := p95.TPOT.Curve[3]
	assert.InEpsilon(t, e.tpotModel.ratio(0.95)*(0.005+p.Rate/1000), *p.Figure.Value, 1e-9)

	s0, e0 := tpotEntry(t, false, 64)
	for _, st := range []string{"p50", "p99", "mean"} {
		base, _ := tpotPlan(t, s0, e0.id, st, 0.02, nil)
		got, _ := tpotPlan(t, s, e.id, st, 0.02, nil)
		require.NotNil(t, got.TPOT.Model, st)
		assert.False(t, got.TPOT.Model.Priced, st)
		assert.Contains(t, got.TPOT.Model.NotPriced, "p90 and p95", st)
		assert.Equal(t, base.TPOT.Curve, got.TPOT.Curve, "%s: the planner's curve stands", st)
		assert.Equal(t, base.TPOT.AtTarget, got.TPOT.AtTarget, st)
	}
}

// Negative controls: a stated output length, or a stated R_q, is the
// planner's alone; and where the model abstains (max-num-seqs not read, a
// slot queue, too short a window) the plan's TPOT is the default's, byte
// for byte, with the view saying why.
func TestMainViewTPOT_DefaultStandsWhereTheModelDoesNotPrice(t *testing.T) {
	s, e := tpotEntry(t, true, 64)
	stated, _ := tpotPlan(t, s, e.id, "p90", 0.02, &Mix{OutputTokens: 500})
	assert.False(t, stated.TPOT.Model.Priced)
	assert.Contains(t, stated.TPOT.Model.NotPriced, "output lengths")
	for _, p := range stated.TPOT.Curve {
		assert.NotNil(t, p.Figure.Refused)
	}
	ratio := 1.5
	req := PlanRequest{InputsID: e.id, Fleet: &FleetKnob{Prefill: intp(2), Decode: intp(2)},
		SLO: &SLORequest{TPOT: &Objective{Statistic: "p90", Target: f64(0.02)}}, Assume: &Assume{TPOTRatio: &ratio}}
	assumed, _ := planBody(t, s, req)
	assert.False(t, assumed.TPOT.Model.Priced)
	assert.Contains(t, assumed.TPOT.Model.NotPriced, "ratio stated")
	assert.Equal(t, "assumed", assumed.TPOT.Ratio.Source)

	s0, e0 := tpotEntry(t, false, 64)
	base, _ := tpotPlan(t, s0, e0.id, "p90", 0.02, nil)
	want, err := json.Marshal(base.TPOT)
	require.NoError(t, err)
	for name, slots := range map[string]int{"max-num-seqs": 0, "would wait for one of its": 4} {
		sa, ea := tpotEntry(t, true, slots)
		require.Contains(t, ea.tpotModel.info.Abstained, name)
		got, _ := tpotPlan(t, sa, ea.id, "p90", 0.02, nil)
		require.NotNil(t, got.TPOT.Model)
		assert.False(t, got.TPOT.Model.Priced)
		assert.Contains(t, got.TPOT.Model.NotPriced, "abstained")
		got.TPOT.Model = nil
		b, err := json.Marshal(got.TPOT)
		require.NoError(t, err)
		assert.JSONEq(t, string(want), string(b), name)
	}
	short, _ := tdEntries(t, 200)
	short.engine = EngineSlots{DecodeSlots: 64, SpecTokens: 3}
	assert.Contains(t, fitMainTPOT(short).info.Abstained, "fit steps")
	nodecode, _ := tdEntries(t, 960)
	nodecode.decode = nil
	assert.Contains(t, fitMainTPOT(nodecode).info.Abstained, "decode series")
}

// An inputs request names the model, and gets an entry of its own; on the
// stub, which gives no decode series, the model abstains. An unknown
// model is the caller's error.
func TestMainViewTPOT_InputsRequest(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	code, body := post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"},
		TPOTModel: whatif.TPOTRenewalLognormal})
	require.Equal(t, http.StatusOK, code, string(body))
	var in InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	require.NotNil(t, in.TPOTModel)
	assert.Nil(t, in.TTFTModel)
	assert.Contains(t, in.TPOTModel.Abstained, "decode series")
	plain := inputsOf(t, h)
	assert.NotEqual(t, plain, in.InputsID, "the model names its own entry")
	code, body = post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"},
		TTFTModel: string(whatif.QueueRankMixture), TPOTModel: whatif.TPOTRenewalLognormal})
	require.Equal(t, http.StatusOK, code, string(body))
	var both InputsResponse
	require.NoError(t, json.Unmarshal(body, &both))
	assert.NotEqual(t, in.InputsID, both.InputsID)
	assert.NotNil(t, both.TTFTModel)
	assert.NotNil(t, both.TPOTModel)

	code, body = post(t, h, "/v1/inputs", InputsRequest{TPOTModel: "ratio"})
	assert.Equal(t, http.StatusBadRequest, code, string(body))
}

// Where the planner refuses every TPOT percentile, so that no fleet is
// sized at a p90 target, the experimental model sizes one: decode's limit
// is the model's inversion, flagged, and the plan the answer carries reads
// the same.
func TestMainViewTPOT_SizesWhereThePlannerRefuses(t *testing.T) {
	size := func(s *Server, id string) SizeResponse {
		body, err := s.size(context.Background(), SizeRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(3)}}},
			SLO: &SLORequest{TPOT: &Objective{Statistic: "p90", Target: f64(0.02)}}})
		require.NoError(t, err)
		var out SizeResponse
		require.NoError(t, json.Unmarshal(body, &out))
		return out
	}
	s0, e0 := tpotEntry(t, false, 64)
	assert.Nil(t, size(s0, e0.id).Answer, "the planner sizes no fleet without R_q")

	s, e := tpotEntry(t, true, 64)
	out := size(s, e.id)
	require.NotNil(t, out.Answer, "%+v", out.Unmet)
	d := out.Answer.Plan.Limits.Decode
	require.NotNil(t, d)
	require.NotNil(t, d.Value)
	assert.Contains(t, d.Flags, string(whatif.TPOTDistribution))
	assert.GreaterOrEqual(t, *d.Value, 3*(1-reachTolerance))
	assert.InEpsilon(t, (0.02/e.tpotModel.ratio(0.9)-0.005)*1000, *d.Value, 1e-9)
}

// The report's measured shape has its TPOT percentile repriced around each
// row's own mean, flagged and marked past the load served; another
// shape, and a stated R_q, are the planner's.
func TestMainViewTPOT_Report(t *testing.T) {
	_, e := tpotEntry(t, true, 64)
	mt := e.tpotModel
	top := *mt.info.MeasuredUpTo
	row := func(rate float64) ReportRow {
		return ReportRow{Rate: rate, TPOTMean: Figure{Value: f64(0.01), Unit: unitSeconds},
			TPOTPercentile: &ReportStat{Statistic: "p90", Figure: Figure{Refused: &Refusal{Reason: string(whatif.NoTPOTHistogram)}}}}
	}
	rep := func() *ReportResponse {
		return &ReportResponse{Fleet: ReportFleet{Decode: 2}, Shapes: []ReportShape{
			{Label: "measured", Table: []ReportRow{row(top / 2), row(top * 1.2)}},
			{Label: "long", Table: []ReportRow{row(top / 2)}}}}
	}
	out := rep()
	mt.applyToReport(out, ReportQuery{})
	m := out.Shapes[0]
	for i, want := range []bool{false, true} {
		st := m.Table[i].TPOTPercentile
		require.NotNil(t, st.Value)
		assert.InEpsilon(t, mt.ratio(0.9)*0.01, *st.Value, 1e-12)
		assert.Contains(t, st.Flags, string(whatif.TPOTDistribution))
		assert.Equal(t, want, st.Extrapolated, "row %d", i)
	}
	assert.Contains(t, m.Flags, string(whatif.TPOTDistribution))
	assert.Contains(t, strings.Join(m.Assumed, "; "), "summary cells are the planner's own")
	assert.NotNil(t, out.Shapes[1].Table[0].TPOTPercentile.Refused, "another shape is the planner's")

	stated := rep()
	r := 1.5
	mt.applyToReport(stated, ReportQuery{Assume: Assume{TPOTRatio: &r}})
	assert.Equal(t, rep(), stated, "a stated R_q is the planner's")

	_, ea := tpotEntry(t, true, 0)
	abst := rep()
	ea.tpotModel.applyToReport(abst, ReportQuery{})
	assert.Equal(t, rep(), abst, "an abstained model prices nothing")
}

// At twice the recorded decode replicas each replica takes half the rate:
// the load measured doubles, and a point is extrapolated only past it.
func TestMainViewTPOT_MeasuredScalesWithDecodeReplicas(t *testing.T) {
	s, e := tpotEntry(t, true, 64)
	top := *e.tpotModel.info.MeasuredUpTo
	req := PlanRequest{InputsID: e.id, Fleet: &FleetKnob{Prefill: intp(4), Decode: intp(4)},
		SLO: &SLORequest{TPOT: &Objective{Statistic: "p90", Target: f64(0.02)}}}
	out, _ := planBody(t, s, req)
	require.True(t, out.TPOT.Model.Priced)
	assert.InDelta(t, 2*top, *out.TPOT.Model.MeasuredUpTo, 1e-9)
	beyond := 0
	for _, p := range out.TPOT.Curve {
		require.NotNil(t, p.Figure.Value)
		assert.Equal(t, p.Rate > 2*top, p.Figure.Extrapolated, "extrapolated at %g r/s (measured %g at 4 decode)", p.Rate, 2*top)
		if p.Figure.Extrapolated {
			beyond++
		}
	}
	assert.Positive(t, beyond)
	assert.Contains(t, strings.Join(out.Assumed, "; "), "read at 2 decode replicas and taken at 4")
}

// With the model on, the self-check prices its TPOT percentiles as the
// answer does: each fold's backtest fits the model on the fold's own window
// and prices the p90 with it, so a p90 the stub refuses (no R_q) is scored.
// Without it the self-check scores no p90 step, as before.
func TestMainViewTPOT_SelfCheckPricesWithTheModel(t *testing.T) {
	e, _ := tdEntries(t, 1920)
	e.engine = EngineSlots{DecodeSlots: 64, SpecTokens: 3}
	e.doc.Source.Cycle = len(e.series)
	var folds []FoldFit
	for _, c := range SelfCheckCycles(len(e.series)) {
		folds = append(folds, FoldFit{Cycle: c, At: e.series[c-1].At, Pricer: e.pricer})
	}
	off := selfCheckEntry(t.Context(), e, folds, SelfCheckInfo{PlannerVersion: "vtest"})
	on := selfCheckEntry(t.Context(), e, folds, SelfCheckInfo{PlannerVersion: "vtest", TPOTModel: whatif.TPOTRenewalLognormal})
	require.NotEmpty(t, off.Folds, off.Line)
	require.NotEmpty(t, on.Folds, on.Line)
	require.NotNil(t, off.TPOT)
	require.NotNil(t, on.TPOT)
	assert.Equal(t, off.TPOT.Statistic, on.TPOT.Statistic, "the figure judged, the mean, is the planner's either way")
	assert.Equal(t, off.TPOT.Bias, on.TPOT.Bias)
	assert.Zero(t, off.TPOT.p90Steps, "without the model the stub's p90 is refused")
	assert.Positive(t, on.TPOT.p90Steps, "with it the p90 is the model's, scored")
}
