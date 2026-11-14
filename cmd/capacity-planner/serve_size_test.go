package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi/apitest"
)

// apiSize sizes a fleet and decodes the response strictly.
func apiSize(t *testing.T, h http.Handler, req whatifapi.SizeRequest) whatifapi.SizeResponse {
	t.Helper()
	code, body := apiPost(t, h, "/v1/size", req)
	require.Equal(t, http.StatusOK, code, "%s", body)
	assert.Empty(t, apitest.CheckFigures(body), "%s", body)
	var out whatifapi.SizeResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&out))
	return out
}

// sizeOfPlan is a plan request's question as a size at rate, bounded at
// bound replicas a role.
func sizeOfPlan(req whatifapi.PlanRequest, rate float64, bound int) whatifapi.SizeRequest {
	traffic := whatifapi.Traffic{Classes: &whatifapi.Classes{Critical: &whatifapi.ClassRate{Rate: fp(rate)}}}
	if req.Traffic != nil {
		traffic.Mix, traffic.ArrivalCV2 = req.Traffic.Mix, req.Traffic.ArrivalCV2
	}
	return whatifapi.SizeRequest{InputsID: req.InputsID, Traffic: &traffic, SLO: req.SLO, Assume: req.Assume,
		Headroom: req.Headroom, MaxReplicas: &whatifapi.FleetKnob{Prefill: ip(bound), Decode: ip(bound)}}
}

// planOfSize is the plan a size asks of one fleet.
func planOfSize(req whatifapi.SizeRequest, n whatifapi.FleetCounts) whatifapi.PlanRequest {
	return whatifapi.PlanRequest{InputsID: req.InputsID, Fleet: &whatifapi.FleetKnob{Prefill: ip(n.Prefill), Decode: ip(n.Decode)},
		Traffic: req.Traffic, SLO: req.SLO, Assume: req.Assume, Headroom: req.Headroom}
}

// On the engine itself, at the worked fleet's question and two and a half
// times the rate one replica of each role carries: the size's answer is what
// POST /v1/plan says at its fleet, and one replica fewer of either role
// misses the rate in the plan too.
func TestServeSizeIsThePlan(t *testing.T) {
	h, _ := servedAPI(t)
	in, _ := apiInputs(t, h, "ols-sweep.jsonl.gz", "model-02")
	base := workedRate()
	base.InputsID = in.InputsID
	one, _ := apiPlan(t, h, base)
	require.NotNil(t, one.Limits.Fleet.Value, "the worked fleet's limit: %+v", one.Limits.Fleet)
	rate := 2.5 * *one.Limits.Fleet.Value

	req := sizeOfPlan(base, rate, 8)
	out := apiSize(t, h, req)
	require.Nil(t, out.Unmet, "%+v", out.Unmet)
	require.NotNil(t, out.Answer)
	a := out.Answer
	t.Logf("%.4g r/s: %d prefill, %d decode, limit %.4g bound by %v; %d fleets searched",
		rate, a.Fleet.Prefill, a.Fleet.Decode, *a.Limit.Value, a.Limit.BoundBy, out.Searched)
	assert.Greater(t, a.Fleet.Prefill+a.Fleet.Decode, 2, "a rate past one of each needs more")

	p, body := apiPlan(t, h, planOfSize(req, a.Fleet))
	want, err := json.MarshalIndent(a.Plan, "", "  ")
	require.NoError(t, err)
	assert.JSONEq(t, string(body), string(want), "the answer's plan is not POST /v1/plan's at its fleet")
	assert.Equal(t, p.Limits.Fleet.Figure, a.Limit)
	assert.Equal(t, p.Limits.Fleet.Binding, a.Binding)
	require.NotNil(t, a.Limit.Value)
	assert.GreaterOrEqual(t, *a.Limit.Value, rate)

	for _, s := range []struct {
		role string
		step *whatifapi.SizeStep
	}{{"prefill", a.FewerPrefill}, {"decode", a.FewerDecode}} {
		if s.step == nil {
			continue
		}
		require.NotNil(t, s.step.Limit.Value, "%s: %+v", s.role, s.step.Limit)
		assert.Less(t, *s.step.Limit.Value, rate, "%s: one fewer reaches the rate", s.role)
		less, _ := apiPlan(t, h, planOfSize(req, s.step.Fleet))
		require.NotNil(t, less.Limits.Fleet.Value)
		assert.InDelta(t, *less.Limits.Fleet.Value, *s.step.Limit.Value, 1e-9*rate, "%s: the step is not the plan's figure", s.role)
		assert.Equal(t, less.Limits.Fleet.BoundBy, s.step.Limit.BoundBy, s.role)
	}
	assert.True(t, a.FewerPrefill != nil || a.FewerDecode != nil, "no step at all: the test reads nothing")
}

// A recording that learned no V_P sizes nothing: the refusal is the
// engine's, with the prefill role named.
func TestServeSizeRefusesATermRefused(t *testing.T) {
	h, _ := servedAPI(t)
	in, _ := apiInputs(t, h, "short.jsonl.gz", "")
	out := apiSize(t, h, whatifapi.SizeRequest{InputsID: in.InputsID,
		Traffic:     &whatifapi.Traffic{Classes: &whatifapi.Classes{Critical: &whatifapi.ClassRate{Rate: fp(1)}}},
		SLO:         &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: statMean, Target: fp(1)}, TPOT: &whatifapi.Objective{Statistic: "none"}},
		MaxReplicas: &whatifapi.FleetKnob{Prefill: ip(4), Decode: ip(4)}})
	assert.Nil(t, out.Answer)
	require.NotNil(t, out.Unmet)
	assert.Equal(t, "prefill", out.Unmet.Role)
	assert.NotEqual(t, whatifapi.ReasonReplicaBound, out.Unmet.Refused.Reason)
	assert.NotEmpty(t, out.Unmet.Refused.Detail)
	assert.NotNil(t, out.Unmet.AtBound.Limit.Refused)
	t.Logf("refused: %s: %s", out.Unmet.Refused.Reason, out.Unmet.Refused.Detail)
}
