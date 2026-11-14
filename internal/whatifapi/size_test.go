package whatifapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authzv1 "k8s.io/api/authorization/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi/apitest"
)

// sizeOf posts a size and decodes the response strictly.
func sizeOf(t *testing.T, h http.Handler, req SizeRequest) (SizeResponse, []byte) {
	t.Helper()
	code, body := post(t, h, "/v1/size", req)
	require.Equal(t, http.StatusOK, code, string(body))
	var out SizeResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&out))
	assert.Empty(t, apitest.CheckFigures(body), "%s", body)
	return out, body
}

// sizeAt is a size request at rate with the stub's TTFT target (0.2 s, from
// the policy) and a TPOT mean target of tpot seconds.
func sizeAt(id string, rate, tpot float64) SizeRequest {
	return SizeRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(rate)}}},
		SLO: &SLORequest{TPOT: &Objective{Statistic: statisticMean, Target: f64(tpot)}}}
}

// On the stub, the prefill limit is half the lower knee (10 r/s a prefill
// replica, 12 a decode one) at the TTFT target 0.2 s, and decode's is the
// smaller of 0.85 of its knee and (target - 5 ms) x 1000. At 15 r/s and a
// 50 ms TPOT, 2 decode replicas carry decode's own limit (20.4), but the
// fleet's lower knee (24) holds the prefill limit to 12: the search adds a
// decode replica (knee 36), then needs 3 prefill (30 x 0.5 = 15).
var stubSizes = []struct {
	name                     string
	rate, tpot               float64
	fleet                    FleetCounts
	fewerPrefill, fewerDecod *float64
}{
	{"decode raised by the lower knee", 15, 0.05, FleetCounts{Prefill: 3, Decode: 3}, f64(10), f64(12)},
	{"decode's TPOT limit, then the knee", 6.5, 0.012, FleetCounts{Prefill: 2, Decode: 2}, f64(5), f64(6)},
	{"one of each", 4, 0.05, FleetCounts{Prefill: 1, Decode: 1}, nil, nil},
}

// The answer is what POST /v1/plan says at the fleet found: the same plan,
// byte for byte, and its fleet limit, headroom and binding. The fleet is the
// one the stub's arithmetic gives.
func TestSizeAnswerIsThePlan(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	for _, c := range stubSizes {
		t.Run(c.name, func(t *testing.T) {
			req := sizeAt(id, c.rate, c.tpot)
			out, _ := sizeOf(t, h, req)
			require.Nil(t, out.Unmet)
			require.NotNil(t, out.Answer)
			a := out.Answer
			assert.Equal(t, c.fleet, a.Fleet)
			assert.Equal(t, c.rate, out.Rate)
			assert.Equal(t, FleetCounts{Prefill: defaultSizeBound, Decode: defaultSizeBound}, out.MaxReplicas)

			p, body := plan(t, h, req.planAt(a.Fleet))
			want, err := marshal(a.Plan)
			require.NoError(t, err)
			assert.Equal(t, string(body), string(want), "the answer's plan is not POST /v1/plan's at its fleet")
			assert.Equal(t, p.Limits.Fleet.Figure, a.Limit)
			assert.Equal(t, p.Limits.Fleet.Binding, a.Binding)
			require.NotNil(t, a.Limit.Value)
			assert.GreaterOrEqual(t, *a.Limit.Value, c.rate)
			require.NotNil(t, a.Headroom.Value)
			assert.InDelta(t, *a.Limit.Value-c.rate, *a.Headroom.Value, 1e-12)
		})
	}
}

// One replica fewer of either role misses the rate: each step's limit is
// short of it, and so is POST /v1/plan's at that fleet. At one replica there
// is no step.
func TestSizeOneFewerFails(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	for _, c := range stubSizes {
		t.Run(c.name, func(t *testing.T) {
			req := sizeAt(id, c.rate, c.tpot)
			out, _ := sizeOf(t, h, req)
			require.NotNil(t, out.Answer)
			for _, s := range []struct {
				role string
				step *SizeStep
				want *float64
				less FleetCounts
			}{
				{"prefill", out.Answer.FewerPrefill, c.fewerPrefill, FleetCounts{Prefill: c.fleet.Prefill - 1, Decode: c.fleet.Decode}},
				{"decode", out.Answer.FewerDecode, c.fewerDecod, FleetCounts{Prefill: c.fleet.Prefill, Decode: c.fleet.Decode - 1}},
			} {
				if s.want == nil {
					assert.Nil(t, s.step, "%s: a step below one replica", s.role)
					continue
				}
				require.NotNil(t, s.step, "%s: no step", s.role)
				assert.Equal(t, s.less, s.step.Fleet, s.role)
				require.NotNil(t, s.step.Limit.Value, s.role)
				assert.InDelta(t, *s.want, *s.step.Limit.Value, 1e-12, s.role)
				assert.Less(t, *s.step.Limit.Value, c.rate, "%s: one fewer still reaches the rate", s.role)
				require.NotNil(t, s.step.Headroom.Value)
				assert.Less(t, *s.step.Headroom.Value, 0.0, s.role)

				p, _ := plan(t, h, req.planAt(s.step.Fleet))
				require.NotNil(t, p.Limits.Fleet.Value)
				assert.Less(t, *p.Limits.Fleet.Value, c.rate, "%s: the plan at one fewer reaches the rate", s.role)
				assert.InDelta(t, *p.Limits.Fleet.Value, *s.step.Limit.Value, 1e-12, "%s: the step is not the plan's figure", s.role)
				assert.Equal(t, p.Limits.Fleet.Binding, s.step.Binding, s.role)
			}
		})
	}
}

// A size no fleet within the bound serves is a refusal with its reason and
// the figure at the bound, never an answer: the bound reached by the prefill
// role or by decode's own limit, and a term the model refuses.
func TestSizeRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		refuse     bool
		req        func(id string) SizeRequest
		role, why  string
		detail     string
		atBound    FleetCounts
		boundValue bool
	}{
		"prefill at the bound": {req: func(id string) SizeRequest {
			r := sizeAt(id, 15, 0.05)
			r.MaxReplicas = &FleetKnob{Prefill: intp(2), Decode: intp(2)}
			return r
		}, role: rolePrefill, why: ReasonReplicaBound, detail: "raise maxReplicas", atBound: FleetCounts{Prefill: 2, Decode: 2}, boundValue: true},
		"decode's TPOT at the bound": {req: func(id string) SizeRequest { return sizeAt(id, 8, 0.012) },
			role: roleDecode, why: ReasonReplicaBound, detail: "decode's own limit", atBound: FleetCounts{Prefill: 32, Decode: 32}, boundValue: true},
		"a refused term": {refuse: true, req: func(id string) SizeRequest { return sizeAt(id, 3, 0.05) },
			role: rolePrefill, why: "NoPrefillCapacity", detail: "no P1 estimate", atBound: FleetCounts{Prefill: 32, Decode: 32}},
		"a percentile with no ratio": {req: func(id string) SizeRequest {
			r := sizeAt(id, 3, 0.05)
			r.SLO.TPOT.Statistic = "p90"
			return r
		}, role: roleDecode, why: "NoTPOTHistogram", detail: "no TPOT histogram", atBound: FleetCounts{Prefill: 32, Decode: 32}},
	} {
		t.Run(name, func(t *testing.T) {
			h := New(Config{Resolver: &stubResolver{refusePrefill: c.refuse}, WhatifSchema: "schema-v3"}).Handler()
			out, _ := sizeOf(t, h, c.req(inputsOf(t, h)))
			assert.Nil(t, out.Answer, "an answer beside a refusal")
			require.NotNil(t, out.Unmet)
			u := out.Unmet
			assert.Equal(t, c.role, u.Role)
			assert.Equal(t, c.why, u.Refused.Reason)
			assert.Contains(t, u.Refused.Detail, c.detail)
			assert.Equal(t, c.atBound, u.AtBound.Fleet)
			if c.boundValue {
				require.NotNil(t, u.AtBound.Limit.Value, "the figure the bound reached")
				assert.Less(t, *u.AtBound.Limit.Value, out.Rate)
			} else {
				assert.NotNil(t, u.AtBound.Limit.Refused, "the bound's limit is the refusal")
			}
		})
	}
}

// A size asked wrongly is a 400 naming the field; an unknown id a 404.
func TestSizeRequestsRefused(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	for name, c := range map[string]struct {
		body any
		code int
		want string
	}{
		"no rate":       {SizeRequest{InputsID: id}, 400, "traffic.classes.critical.rate"},
		"bound too big": {SizeRequest{InputsID: id, Traffic: sizeAt(id, 1, 0.05).Traffic, MaxReplicas: &FleetKnob{Decode: intp(65)}}, 400, "maxReplicas.decode"},
		"bound zero":    {SizeRequest{InputsID: id, Traffic: sizeAt(id, 1, 0.05).Traffic, MaxReplicas: &FleetKnob{Prefill: intp(0)}}, 400, "maxReplicas.prefill"},
		"a plan's own":  {SizeRequest{InputsID: id, Traffic: sizeAt(id, 1, 0.05).Traffic, Headroom: f64(2)}, 400, "headroom"},
		"unknown id":    {SizeRequest{InputsID: "sha256:none"}, 404, "unknown inputsId"},
		"unknown field": {map[string]any{"inputsId": id, "fleet": map[string]any{"prefill": 2}}, 400, "unknown field"},
	} {
		t.Run(name, func(t *testing.T) {
			code, body := post(t, h, "/v1/size", c.body)
			assert.Equal(t, c.code, code, "%s", body)
			assert.Contains(t, string(body), c.want)
		})
	}
}

// The same size is the same id and bytes, searched once; the search prices
// a handful of fleets, not every count up to the bound.
func TestSizeCachingAndCost(t *testing.T) {
	r := &stubResolver{}
	h := New(Config{Resolver: r, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	a, aBody := sizeOf(t, h, sizeAt(id, 15, 0.05))
	n := r.plainLimits.Load()
	b, bBody := sizeOf(t, h, sizeAt(id, 15, 0.05))
	assert.Equal(t, a.SizeID, b.SizeID)
	assert.Equal(t, aBody, bBody)
	assert.Equal(t, n, r.plainLimits.Load(), "the same size searched again")
	// Three bisections of a bound of 32, each six fleets at most, and the two
	// steps: every count tried would be over a thousand.
	assert.LessOrEqual(t, a.Searched, 20, "a search of %d fleets for a bound of %d per role", a.Searched, defaultSizeBound)
	c, _ := sizeOf(t, h, sizeAt(id, 14, 0.05))
	assert.NotEqual(t, a.SizeID, c.SizeID, "a rate moved is a new size")
}

// /v1/size sits behind the same authentication as the other endpoints: no
// token is 401, a user allowed only /whatif/v1/plan is 403, one allowed
// /whatif/v1/size reaches it, authorized as get on that path; with none, a
// foreign Host is 403.
func TestSizeBehindAuth(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}}).Handler()
	planOnly := NewAuthenticator(newCluster("/whatif/v1/plan", ""), nil, 0, nil).Wrap(h)
	assert.Equal(t, http.StatusUnauthorized, call(planOnly, "/v1/size", "").Code)
	assert.Equal(t, http.StatusUnauthorized, call(planOnly, "/v1/size", "Bearer bad").Code)
	assert.Equal(t, http.StatusForbidden, call(planOnly, "/v1/size", "Bearer good").Code)

	c := newCluster("/whatif/v1/size", "")
	allowed := NewAuthenticator(c, nil, 0, nil).Wrap(h)
	// Allowed: the API answers (a 400, the empty body), past the filter.
	assert.Equal(t, http.StatusBadRequest, call(allowed, "/v1/size", "Bearer good").Code)
	require.NotNil(t, c.lastSAR.NonResourceAttributes)
	assert.Equal(t, authzv1.NonResourceAttributes{Path: "/whatif/v1/size", Verb: "get"}, *c.lastSAR.NonResourceAttributes)

	loop := LoopbackOnly(h)
	for host, want := range map[string]int{"127.0.0.1:8080": http.StatusBadRequest, "rebound.example:8080": http.StatusForbidden} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://"+host+"/v1/size", nil)
		req.Header.Set("Content-Type", "application/json")
		loop.ServeHTTP(rec, req)
		assert.Equal(t, want, rec.Code, host)
	}
}
