package whatifapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A plan that runs past the server's plan budget stops at its next figure and
// is a 503 with reason PlanBudgetExceeded, well before its grid would end. It
// caches nothing and frees its slot, and the same plan with time enough is
// priced.
func TestPlanPastItsBudgetIsRefused(t *testing.T) {
	const delay, budget = 20 * time.Millisecond, 100 * time.Millisecond
	r := &stubResolver{}
	s := New(Config{Resolver: r, WhatifSchema: "schema-v3", MaxPlans: 1, PlanBudget: budget})
	h := s.Handler()
	id := inputsOf(t, h)
	req := PlanRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(4)}}}}

	r.ttftDelay = delay
	b, err := json.Marshal(req)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	hr := httptest.NewRequest(http.MethodPost, "/v1/plan", bytes.NewReader(b))
	hr.Header.Set("Content-Type", "application/json")
	start := time.Now()
	h.ServeHTTP(rec, hr)
	took := time.Since(start)
	body := rec.Body.Bytes()
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "%s", body)
	// Asked again, the same plan runs again: no Retry-After invites it.
	assert.Empty(t, rec.Header().Get("Retry-After"))
	var e ErrorResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&e), "%s", body)
	assert.Equal(t, ReasonPlanBudgetExceeded, e.Reason)
	assert.Contains(t, e.Error, "plan budget of 100ms")
	// The grid alone is gridPoints+1 figures of delay each, 1.3 s; the plan
	// stops at the first figure past its budget.
	t.Logf("refused in %s (budget %s, a figure %s)", took, budget, delay)
	assert.Less(t, took, gridPoints*delay, "the plan ran on past its budget, to its last figure")
	assert.Zero(t, s.plans.len(), "a stopped plan was cached")
	assert.Zero(t, s.curves.len(), "a stopped curve set was cached")
	assert.Empty(t, s.planSlots, "a slot was kept")

	r.ttftDelay = 0
	plan(t, h, req)
}

// A request that ends inside the budget is its own end, not the budget's:
// the client went away, and no reason is given.
func TestPlanEndedByItsClientIsNotTheBudget(t *testing.T) {
	r := &stubResolver{ttftDelay: 5 * time.Millisecond}
	s := New(Config{Resolver: r, WhatifSchema: "schema-v3", MaxPlans: 1, PlanBudget: time.Hour})
	id := inputsOf(t, s.Handler())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.plan(ctx, PlanRequest{InputsID: id})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotErrorAs(t, err, new(BudgetError))
	assert.Empty(t, s.planSlots, "a slot was kept")
}

// An error the plan returns for its own reason while the budget runs out is
// that error, not the budget's: a knob refused as the budget ends is a 400
// naming the knob.
func TestPlanErrorAsTheBudgetEndsIsItsOwn(t *testing.T) {
	const budget = 50 * time.Millisecond
	r := &stubResolver{}
	s := New(Config{Resolver: r, WhatifSchema: "schema-v3", MaxPlans: 1, PlanBudget: budget})
	h := s.Handler()
	id := inputsOf(t, h)
	// The +1 prefill step's fleet is refused once the budget has run out.
	r.fleetErr = func(k Knobs) error {
		if k.Prefill != 2 {
			return nil
		}
		time.Sleep(3 * budget)
		return InputErrorf("stub: the +1 fleet's knob is refused")
	}
	code, body := post(t, h, "/v1/plan", PlanRequest{InputsID: id,
		Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(4)}}}})
	require.Equal(t, http.StatusBadRequest, code, "%s", body)
	var e ErrorResponse
	require.NoError(t, json.Unmarshal(body, &e), "%s", body)
	assert.Contains(t, e.Error, "the +1 fleet's knob is refused")
	assert.Empty(t, e.Reason)
	assert.Empty(t, s.planSlots, "a slot was kept")
}

// A server with no budget configured, or a negative one, has the default.
func TestPlanBudgetDefault(t *testing.T) {
	assert.Equal(t, DefaultPlanBudget, New(Config{}).planBudget)
	assert.Equal(t, time.Second, New(Config{PlanBudget: time.Second}).planBudget)
	assert.Equal(t, DefaultPlanBudget, New(Config{PlanBudget: -1}).planBudget, "a negative budget is the default")
}

// errorBody decodes an error body strictly.
func errorBody(t *testing.T, body []byte) ErrorResponse {
	t.Helper()
	var e ErrorResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&e), "%s", body)
	return e
}

// planWithin's three conditions, each alone. The plan's error must be a
// deadline, the request's own context must not have ended, and the budget
// must be what ended the plan's context; failing any one, the error is the
// plan's own, never PlanBudgetExceeded. The plan's error paths return their
// errors as they got them (ctx.Err(), the Pricer's), with no wrapping.
func TestPlanWithinNeedsAllThreeConditions(t *testing.T) {
	rate := &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(4)}}}
	t.Run("the request's deadline ends first, the budget soon after", func(t *testing.T) {
		r := &stubResolver{ttftDelay: 5 * time.Millisecond}
		s := New(Config{Resolver: r, WhatifSchema: "schema-v3", MaxPlans: 1, PlanBudget: 60 * time.Millisecond})
		id := inputsOf(t, s.Handler())
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := s.plan(ctx, PlanRequest{InputsID: id, Traffic: rate})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorAs(t, err, new(BudgetError))
	})
	t.Run("the budget ends first, then the request, then the plan returns", func(t *testing.T) {
		r := &stubResolver{fleetErr: func(k Knobs) error {
			if k.Prefill != 2 {
				return nil
			}
			time.Sleep(150 * time.Millisecond)
			return fmt.Errorf("stub: %w", context.DeadlineExceeded)
		}}
		s := New(Config{Resolver: r, WhatifSchema: "schema-v3", MaxPlans: 1, PlanBudget: 20 * time.Millisecond})
		id := inputsOf(t, s.Handler())
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		defer cancel()
		_, err := s.plan(ctx, PlanRequest{InputsID: id, Traffic: rate})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorAs(t, err, new(BudgetError), "the client is gone: no refusal reaches it")
	})
	t.Run("a deadline error the budget did not cause", func(t *testing.T) {
		r := &stubResolver{fleetErr: func(k Knobs) error {
			if k.Prefill != 2 {
				return nil
			}
			return fmt.Errorf("stub: a downstream call timed out: %w", context.DeadlineExceeded)
		}}
		s := New(Config{Resolver: r, WhatifSchema: "schema-v3", MaxPlans: 1, PlanBudget: time.Hour})
		h := s.Handler()
		code, body := post(t, h, "/v1/plan", PlanRequest{InputsID: inputsOf(t, h), Traffic: rate})
		assert.Equal(t, http.StatusServiceUnavailable, code, "%s", body)
		e := errorBody(t, body)
		assert.Empty(t, e.Reason, "%s", body)
		assert.Contains(t, e.Error, "a downstream call timed out")
	})
	t.Run("the budget ends while the plan fails for its own reason", func(t *testing.T) {
		// The error names the deadline in words but does not wrap it: it is
		// the plan's own failure, a 500, not the budget's.
		const budget = 50 * time.Millisecond
		r := &stubResolver{fleetErr: func(k Knobs) error {
			if k.Prefill != 2 {
				return nil
			}
			time.Sleep(3 * budget)
			return fmt.Errorf("stub: %v", context.DeadlineExceeded)
		}}
		s := New(Config{Resolver: r, WhatifSchema: "schema-v3", MaxPlans: 1, PlanBudget: budget})
		h := s.Handler()
		code, body := post(t, h, "/v1/plan", PlanRequest{InputsID: inputsOf(t, h), Traffic: rate})
		assert.Equal(t, http.StatusInternalServerError, code, "%s", body)
		assert.Empty(t, errorBody(t, body).Reason, "%s", body)
	})
}

// A caller that ran a flight's call itself takes its deadline error as the
// call's answer: asked again with its own context live, a Pricer's own
// timeout would run the call forever.
func TestFlightCallerKeepsItsOwnDeadlineError(t *testing.T) {
	var f flight[int]
	calls := 0
	done := make(chan error, 1)
	go func() {
		_, err := f.doFor(context.Background(), "k", func() (int, error) {
			calls++
			return 0, fmt.Errorf("stub: timed out: %w", context.DeadlineExceeded)
		})
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, 1, calls)
	case <-time.After(5 * time.Second):
		t.Fatal("doFor ran its own call again and again on a deadline error with a live context")
	}
}

// A request that joins a plan already being priced shares that plan's budget:
// both are refused PlanBudgetExceeded, and a later request prices the plan
// afresh.
func TestPlanSharedPastItsBudget(t *testing.T) {
	r := &stubResolver{ttftDelay: 20 * time.Millisecond}
	s := New(Config{Resolver: r, WhatifSchema: "schema-v3", MaxPlans: 1, PlanBudget: 100 * time.Millisecond})
	h := s.Handler()
	id := inputsOf(t, h)
	req := PlanRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(4)}}}}
	e, _ := s.inputs.get(id)
	sc, err := scenarioOf(&e.doc, req)
	require.NoError(t, err)
	key := contentID("scenario", sc)

	r.gate = make(chan struct{})
	codes, bodies := make([]int, 2), make([][]byte, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() { codes[i], bodies[i] = post(t, h, "/v1/plan", req) })
	}
	require.Eventually(t, func() bool { return s.planFlight.waiters(key) == 1 }, 5*time.Second, time.Millisecond,
		"the second request never joined the first")
	close(r.gate)
	wg.Wait()
	for i := range codes {
		assert.Equal(t, http.StatusServiceUnavailable, codes[i], "%s", bodies[i])
		assert.Equal(t, ReasonPlanBudgetExceeded, errorBody(t, bodies[i]).Reason, "%s", bodies[i])
	}
	r.gate, r.ttftDelay = nil, 0
	plan(t, h, req)
}
