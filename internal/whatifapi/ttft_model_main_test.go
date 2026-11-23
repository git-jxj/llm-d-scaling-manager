package whatifapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi/apitest"
)

// mainEntry is a 13 h source with its per-rank series (rmFit's) whose
// prefill V_P the planner refuses (stubResolver.refusePrefill), as a P/D
// fleet whose prefill never queued: the default TTFT is refused at every
// rate. With model, the entry is resolved with the experimental TTFT model.
func mainEntry(t *testing.T, model bool) (*Server, *inputsEntry) {
	t.Helper()
	r := &stubResolver{refusePrefill: true}
	e := rmFit(t, r, 3120)
	e.engine.PrefillSlots = 4
	e.id = "sha256:main-ttft"
	if model {
		e.ttftModel = fitMainMixture(e)
		info := e.ttftModel.info
		e.doc.TTFTModel = &info
		e.id += "-v2"
	}
	s := New(Config{Resolver: r, WhatifSchema: "schema-v3"})
	s.inputs.add(e.id, e)
	return s, e
}

// planBody prices req on s and decodes it.
func planBody(t *testing.T, s *Server, req PlanRequest) (PlanResponse, []byte) {
	t.Helper()
	body, err := s.plan(context.Background(), req)
	require.NoError(t, err)
	var out PlanResponse
	require.NoError(t, json.Unmarshal(body, &out))
	return out, body
}

// On a fleet whose default TTFT model refuses every rate, the experimental
// model, fitted on the source's window, prices the TTFT curve and point A:
// every point up to the planner's own lower knee (decode's, here) is
// answered, flagged RankMixtureTTFT, and the points above the highest step
// rate the window served are marked extrapolated, those under it not. The
// view names the model, its held-out evidence, the label for the dashed
// part, and, the prefill knee refused, that the right end is an estimate.
// Without the model the same fleet's curve is refused throughout.
func TestMainView_ExperimentalModelPricesTheRefusedCurve(t *testing.T) {
	s0, e0 := mainEntry(t, false)
	two := &FleetKnob{Decode: intp(2)}
	base, baseBody := planBody(t, s0, PlanRequest{InputsID: e0.id, Fleet: two})
	for _, p := range base.TTFT.Line.Curve {
		require.NotNil(t, p.Figure.Refused, "the default refuses the TTFT at %g r/s", p.Rate)
	}
	assert.Nil(t, base.TTFT.Model)
	assert.NotContains(t, string(baseBody), "extrapolated")

	s, e := mainEntry(t, true)
	mm := e.ttftModel
	require.Empty(t, mm.info.Abstained)
	require.NotNil(t, mm.info.HeldOut)
	h := mm.info.HeldOut
	assert.Positive(t, h.Steps)
	assert.Equal(t, "mean", h.Statistic)
	assert.LessOrEqual(t, h.Within+h.Low, h.Steps)
	assert.Positive(t, h.MedianRatio)
	assert.Less(t, h.RateFrom, h.RateTo)
	assert.Equal(t, 1, h.Prefill)
	top := *mm.info.MeasuredUpTo
	assert.InDelta(t, 2+0.05*239, top, 0.5, "the highest step rate the window served")

	out, body := planBody(t, s, PlanRequest{InputsID: e.id, Fleet: two})
	assert.Empty(t, apitest.CheckFigures(body), "%s", body)
	knee := *out.TTFT.Markers.DecodeKnee.Value
	require.Less(t, top, knee, "the fixture's knee is above its top rate")
	priced, beyond, refused := 0, 0, false
	for _, p := range out.TTFT.Line.Curve {
		if p.Rate > knee {
			assert.NotNil(t, p.Figure.Refused, "past the planner's knee the curve stops: %g r/s", p.Rate)
			continue
		}
		// Where a rank of the model saturates under the planner's knee, the
		// planner's refusal stands, at that rate and every one above it.
		if p.Figure.Value == nil {
			refused = true
			continue
		}
		require.False(t, refused, "a priced point above a refused one at %g r/s", p.Rate)
		assert.Contains(t, p.Figure.Flags, string(whatif.RankMixtureTTFT))
		assert.Equal(t, p.Rate > top, p.Figure.Extrapolated, "extrapolated at %g r/s (top %g)", p.Rate, top)
		priced++
		if p.Figure.Extrapolated {
			beyond++
		}
	}
	assert.Greater(t, priced, 30)
	assert.Positive(t, beyond)
	assert.Less(t, beyond, priced)
	// A rising curve.
	c := out.TTFT.Line.Curve
	assert.Less(t, *c[0].Figure.Value, *c[priced-1].Figure.Value)

	v := out.TTFT.Model
	require.NotNil(t, v)
	assert.True(t, v.Priced)
	assert.True(t, v.Experimental)
	assert.Equal(t, "Experimental TTFT model", v.Label)
	assert.Equal(t, "beyond the highest load measured", v.Beyond)
	assert.Equal(t, TTFTRightEndEstimate, v.RightEnd)
	assert.Regexp(t, `^held-out steps: \d+ of \d+ within 20%$`, v.Evidence)
	assert.InDelta(t, top, *v.MeasuredUpTo, 1e-9)
	require.NotNil(t, out.TTFT.Line.AtRate.Figure.Value, "point A is priced")
	assert.Contains(t, strings.Join(out.Assumed, "; "), "OUTSIDE VALIDATED REGION")
}

// At twice the recorded prefill replicas each replica's ranks take half the
// rate: the curve at 2r reads as the recorded fleet's at r, and what is
// measured scales with it.
func TestMainView_ExperimentalModelScalesWithPrefillReplicas(t *testing.T) {
	s, e := mainEntry(t, true)
	one, _ := planBody(t, s, PlanRequest{InputsID: e.id})
	two, _ := planBody(t, s, PlanRequest{InputsID: e.id, Fleet: &FleetKnob{Prefill: intp(2), Decode: intp(2)}})
	require.True(t, two.TTFT.Model.Priced)
	assert.InDelta(t, 2**one.TTFT.Model.MeasuredUpTo, *two.TTFT.Model.MeasuredUpTo, 1e-9)
	f1 := e.ttftModel.fleet(stubFleetFor(t, e, 1), Knobs{Prefill: 1, Decode: 1, TTFTPercentile: 0.9}).TTFTAt(3)
	f2 := e.ttftModel.fleet(stubFleetFor(t, e, 2), Knobs{Prefill: 2, Decode: 1, TTFTPercentile: 0.9}).TTFTAt(6)
	a, ok1 := valueOf(f1)
	b, ok2 := valueOf(f2)
	require.True(t, ok1 && ok2)
	assert.InDelta(t, a, b, 1e-12)
}

// stubFleetFor is the entry's pricer's fleet at n prefill replicas.
func stubFleetFor(t *testing.T, e *inputsEntry, n int) Fleet {
	t.Helper()
	fl, err := e.pricer.Fleet(Knobs{Prefill: n, Decode: 1})
	require.NoError(t, err)
	return fl
}

// A stated prompt shape is the planner's alone: the model prices the
// source's own traffic, and the view says why it did not price.
func TestMainView_ExperimentalModelLeavesAStatedShapeToThePlanner(t *testing.T) {
	s, e := mainEntry(t, true)
	out, _ := planBody(t, s, PlanRequest{InputsID: e.id, Traffic: &Traffic{Mix: &Mix{PromptTokens: 4000}}})
	require.NotNil(t, out.TTFT.Model)
	assert.False(t, out.TTFT.Model.Priced)
	assert.Contains(t, out.TTFT.Model.NotPriced, "request shape")
	for _, p := range out.TTFT.Line.Curve {
		assert.NotNil(t, p.Figure.Refused)
	}
}

// Where the model abstains (a source with no per-rank series, as the stub
// gives) the planner's answer stands: the plan's curve, limits and points
// are the default's, byte for byte, the view says why, and the inputs say
// the model abstained. An unknown model is the caller's error.
func TestMainView_ExperimentalModelAbstainsAndTheDefaultStands(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	code, body := post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"},
		TTFTModel: string(whatif.QueueRankMixture)})
	require.Equal(t, http.StatusOK, code, string(body))
	var in InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	require.NotNil(t, in.TTFTModel)
	assert.Contains(t, in.TTFTModel.Abstained, "per-rank series")
	plain := inputsOf(t, h)
	assert.NotEqual(t, plain, in.InputsID, "the model names its own entry")

	base, _ := plan(t, h, PlanRequest{InputsID: plain})
	got, _ := plan(t, h, PlanRequest{InputsID: in.InputsID})
	a, err := json.Marshal(base.TTFT.Line)
	require.NoError(t, err)
	b, err := json.Marshal(got.TTFT.Line)
	require.NoError(t, err)
	assert.JSONEq(t, string(a), string(b))
	require.NotNil(t, got.TTFT.Model)
	assert.False(t, got.TTFT.Model.Priced)
	assert.Contains(t, got.TTFT.Model.NotPriced, "abstained")

	code, body = post(t, h, "/v1/inputs", InputsRequest{TTFTModel: "v9"})
	assert.Equal(t, http.StatusBadRequest, code, string(body))
}

// The curve stops at the lower of the planner's knees; a prefill knee that
// is only a lower bound is raised to the load the fleet served, never
// past decode's knee; a refused prefill knee leaves decode's.
func TestCurveCap(t *testing.T) {
	for _, c := range []struct {
		name       string
		pv         float64
		pok, lower bool
		dv         float64
		dok        bool
		served     float64
		want       float64
		ok         bool
	}{
		{"prefill binds", 10, true, false, 20, true, 15, 10, true},
		{"a lower bound raised to the load served", 10, true, true, 20, true, 15, 15, true},
		{"a lower bound over the load served stays", 10, true, true, 20, true, 8, 10, true},
		{"never past decode's knee", 10, true, true, 12, true, 15, 12, true},
		{"prefill refused: decode's", 0, false, true, 12, true, 15, 12, true},
		{"decode refused: prefill's", 10, true, true, 0, false, 15, 15, true},
		{"none", 0, false, false, 0, false, 15, 0, false},
	} {
		got, ok := curveCap(c.pv, c.pok, c.lower, c.dv, c.dok, c.served)
		assert.Equal(t, c.ok, ok, c.name)
		assert.InDelta(t, c.want, got, 1e-12, c.name)
	}
}
