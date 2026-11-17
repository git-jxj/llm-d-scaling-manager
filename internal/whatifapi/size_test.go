package whatifapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authzv1 "k8s.io/api/authorization/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
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
		// At the API's own bound there is no bound to raise.
		"decode at the API's bound": {req: func(id string) SizeRequest {
			r := sizeAt(id, 8, 0.012)
			r.MaxReplicas = &FleetKnob{Prefill: intp(maxReplicas), Decode: intp(maxReplicas)}
			return r
		}, role: roleDecode, why: ReasonReplicaBound, detail: "the most the API sizes", atBound: FleetCounts{Prefill: 64, Decode: 64}, boundValue: true},
		// A TTFT target of 10 s leaves the prefill limit at 0.99 of the lower
		// knee, past the operating-point cap (0.85 of it): at 2 and 2 decode
		// alone allows 20.4 and prefill alone 19.8, but the fleet only 17.
		"neither role alone": {req: func(id string) SizeRequest {
			r := sizeAt(id, 18, 0.05)
			r.SLO.TTFT = &Objective{Target: f64(10)}
			r.MaxReplicas = &FleetKnob{Prefill: intp(2), Decode: intp(2)}
			return r
		}, role: roleFleet, why: ReasonReplicaBound, detail: "the fleet's limit", atBound: FleetCounts{Prefill: 2, Decode: 2}, boundValue: true},
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
			if c.atBound.Prefill >= maxReplicas {
				assert.NotContains(t, u.Refused.Detail, "raise maxReplicas", "advice to raise a bound already at the API's")
			}
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
	// The search gallops up from one replica: a rate one of each serves
	// prices that one fleet, not the bound.
	one, _ := sizeOf(t, h, sizeAt(id, 4, 0.05))
	require.NotNil(t, one.Answer)
	assert.Equal(t, FleetCounts{Prefill: 1, Decode: 1}, one.Answer.Fleet)
	assert.Equal(t, 1, one.Searched, "fleets searched for an answer of one of each")
	t.Logf("fleets searched: %d for 3 and 3 at 15 r/s, %d for 1 and 1 at 4 r/s", a.Searched, one.Searched)
}

// sizeWith posts a size on a request whose context the caller holds.
func sizeWith(ctx context.Context, t *testing.T, h http.Handler, req SizeRequest) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(req)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/size", bytes.NewReader(b)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.Bytes()
}

// A search finished is never lost to its answer's plan: the plan runs on the
// slot the search took, so one slot sizes (MaxPlans 1), and a size whose plan
// ended with its client is not searched again when asked again.
func TestSizeSearchSurvivesItsPlan(t *testing.T) {
	r := &stubResolver{}
	s := New(Config{Resolver: r, WhatifSchema: "schema-v3", MaxPlans: 1})
	searches := 0
	s.onSearch = func() { searches++ }
	h := s.Handler()
	id := inputsOf(t, h)

	// The client goes away when the answer's plan prices its fleet (3 and 3,
	// priced once by the search and again by the plan).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := 0
	r.onFleet = func(k Knobs) {
		if k.Prefill == 3 && k.Decode == 3 {
			if seen++; seen == 2 {
				cancel()
			}
		}
	}
	code, body := sizeWith(ctx, t, h, sizeAt(id, 15, 0.05))
	require.Equal(t, http.StatusServiceUnavailable, code, "%s", body)
	require.Equal(t, 1, searches)
	n := r.fleets.Load()

	r.onFleet = nil
	out, _ := sizeOf(t, h, sizeAt(id, 15, 0.05))
	require.NotNil(t, out.Answer)
	assert.Equal(t, FleetCounts{Prefill: 3, Decode: 3}, out.Answer.Fleet)
	assert.Equal(t, 1, searches, "the size was searched again")
	assert.Less(t, r.fleets.Load()-n, int64(out.Searched), "the retry priced as many fleets as a search")
}

// sizeOn "low" sizes on the low end of the fleet's span over V_P and f. On
// the stub the span is 10% either way: at 15 r/s the point answer, 3 and 3,
// serves 15 at its limit but 13.5 at its low end; sized low, 4 and 3 serve
// 18, 16.2 at the low end.
func TestSizeOnTheLowEnd(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	point, _ := sizeOf(t, h, sizeAt(id, 15, 0.05))
	require.NotNil(t, point.Answer)
	assert.Equal(t, SizeOnPoint, point.SizeOn)
	assert.Equal(t, FleetCounts{Prefill: 3, Decode: 3}, point.Answer.Fleet)
	require.NotNil(t, point.Answer.Limit.Sensitivity)
	assert.Less(t, *point.Answer.Limit.Sensitivity.Lo, 15.0, "the point answer's low end, which the page warns of")

	req := sizeAt(id, 15, 0.05)
	req.SizeOn = SizeOnLow
	low, _ := sizeOf(t, h, req)
	require.NotNil(t, low.Answer)
	assert.Equal(t, SizeOnLow, low.SizeOn)
	assert.NotEqual(t, point.SizeID, low.SizeID)
	assert.Equal(t, FleetCounts{Prefill: 4, Decode: 3}, low.Answer.Fleet)
	require.NotNil(t, low.Answer.Limit.Sensitivity)
	assert.GreaterOrEqual(t, *low.Answer.Limit.Sensitivity.Lo, 15.0)
	p, _ := plan(t, h, req.planAt(low.Answer.Fleet))
	assert.Equal(t, p.Limits.Fleet.Figure, low.Answer.Limit)
	require.NotNil(t, low.Answer.FewerPrefill)
	require.NotNil(t, low.Answer.FewerPrefill.Limit.Sensitivity)
	assert.Less(t, *low.Answer.FewerPrefill.Limit.Sensitivity.Lo, 15.0, "one prefill fewer still serves the rate at its low end")
	require.NotNil(t, low.Answer.FewerPrefill.Headroom.Value)
	assert.InDelta(t, *low.Answer.FewerPrefill.Limit.Sensitivity.Lo-15, *low.Answer.FewerPrefill.Headroom.Value, 1e-12,
		"sized low, a step's shortfall is its low end's")

	require.NotNil(t, low.Answer.Headroom.Value)
	assert.InDelta(t, *low.Answer.Limit.Sensitivity.Lo-15, *low.Answer.Headroom.Value, 1e-12,
		"sized low, the answer's headroom is its low end's, as the steps' are")

	req.SizeOn = "worst"
	code, body := post(t, h, "/v1/size", req)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, string(body), "sizeOn")
}

// Sized on the low end where the worst corner of V_P and f refuses the limit
// (a TTFT floor above the target there), the size is that refusal, said of
// the corner, never ReplicaBoundReached: no count lowers a floor. Sized on
// the point, the same question answers.
func TestSizeOnLowRefusedAtTheWorstCorner(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{loRefused: whatif.TargetBelowFloor}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	req := sizeAt(id, 3, 0.05)
	req.MaxReplicas = &FleetKnob{Prefill: intp(4), Decode: intp(4)}
	point, _ := sizeOf(t, h, req)
	require.NotNil(t, point.Answer)

	req.SizeOn = SizeOnLow
	out, _ := sizeOf(t, h, req)
	require.Nil(t, out.Answer)
	require.NotNil(t, out.Unmet)
	u := out.Unmet
	assert.Equal(t, string(whatif.TargetBelowFloor), u.Refused.Reason)
	assert.Contains(t, u.Refused.Detail, "worst corner of V_P and f")
	assert.Contains(t, u.Refused.Detail, "raise the TTFT target above it")
	assert.NotContains(t, u.Refused.Detail, "short of")
	assert.NotContains(t, u.Refused.Detail, "maxReplicas")
	require.NotNil(t, u.AtBound.Headroom.Refused, "the at-bound shortfall is the corner's refusal")
	assert.Equal(t, string(whatif.TargetBelowFloor), u.AtBound.Headroom.Refused.Reason)
}

// Identical sizes asked at once search once: the second waits on the first.
func TestSizeConcurrentSearchOnce(t *testing.T) {
	r := &stubResolver{}
	s := New(Config{Resolver: r, WhatifSchema: "schema-v3"})
	searches := atomic.Int64{}
	s.onSearch = func() { searches.Add(1) }
	h := s.Handler()
	id := inputsOf(t, h)
	r.gate = make(chan struct{})
	type result struct {
		code int
		body []byte
	}
	done := make(chan result, 2)
	for range 2 {
		go func() {
			code, body := post(t, h, "/v1/size", sizeAt(id, 15, 0.05))
			done <- result{code, body}
		}()
	}
	require.Eventually(t, func() bool {
		s.sizeFlight.mu.Lock()
		defer s.sizeFlight.mu.Unlock()
		for _, c := range s.sizeFlight.calls {
			if c.waiting == 1 {
				return true
			}
		}
		return false
	}, 5*time.Second, time.Millisecond, "the second size never joined the first")
	close(r.gate)
	a, b := <-done, <-done
	require.Equal(t, http.StatusOK, a.code, "%s", a.body)
	require.Equal(t, http.StatusOK, b.code, "%s", b.body)
	assert.Equal(t, a.body, b.body)
	assert.Equal(t, int64(1), searches.Load(), "two searches for one size")
}

// With every plan slot taken when a size starts, it is a 503 with
// Retry-After, before any search; a source with no prefill variant is a 400.
func TestSizeBusyAndDecodeOnly(t *testing.T) {
	s := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3", MaxPlans: 1})
	searches := 0
	s.onSearch = func() { searches++ }
	h := s.Handler()
	id := inputsOf(t, h)
	s.planSlots <- struct{}{}
	rec := httptest.NewRecorder()
	b, err := json.Marshal(sizeAt(id, 15, 0.05))
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/size", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	<-s.planSlots
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Equal(t, retryAfterSeconds, rec.Header().Get("Retry-After"))
	assert.Equal(t, 0, searches, "a size searched with no slot")
	out, _ := sizeOf(t, h, sizeAt(id, 15, 0.05))
	assert.NotNil(t, out.Answer, "the slot freed, the same size answers")

	code, body := post(t, h, "/v1/inputs", InputsRequest{Model: "decode-only", Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"}})
	require.Equal(t, http.StatusOK, code, "%s", body)
	var in InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	code, body = post(t, h, "/v1/size", sizeAt(in.InputsID, 15, 0.05))
	assert.Equal(t, http.StatusBadRequest, code, "%s", body)
	assert.Contains(t, string(body), "no prefill variant")
}

// A limit reaches the rate within a billionth of it, as the worked
// recording's plateau wanders by up to 8e-13; a millionth short does not.
func TestReachesTolerance(t *testing.T) {
	const rate = 0.71899389578821249
	assert.True(t, reaches(rate, rate))
	assert.True(t, reaches(0.71899389578765072, rate), "a plateau's wander is no shortfall")
	assert.False(t, reaches(rate*(1-1e-6), rate))
	assert.True(t, figureReaches(Figure{Value: ptr(rate * (1 - 1e-12))}, rate, false))
	assert.True(t, resultReaches(whatif.Answer{Value: rate * (1 - 1e-12)}, rate, false))
}

// A fleet with one replica fewer that serves the rate is the search's own
// failure, a 500, never an answer. On this stub the prefill limit dips at
// one decode replica on every count the search gallops through (1, 2, 4,
// ... 32) but not at 3, and under 3 prefill replicas from two decode up: the
// search lands on 3 prefill and 2 decode, though 3 and 1 serve the rate.
func TestSizeOneFewerThatServesIsAnError(t *testing.T) {
	galloped := func(p int) bool { return p&(p-1) == 0 }
	r := &stubResolver{dip: func(p, d int) bool { return (d == 1 && galloped(p)) || (d >= 2 && p < 3) }}
	h := New(Config{Resolver: r, WhatifSchema: "schema-v3"}).Handler()
	code, body := post(t, h, "/v1/size", sizeAt(inputsOf(t, h), 6, 0.05))
	assert.Equal(t, http.StatusInternalServerError, code, "%s", body)
	assert.Contains(t, string(body), "does not hold")
	assert.Contains(t, string(body), "3 prefill and 1 decode")
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
