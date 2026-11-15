package whatifapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	assert.Less(t, took, budget+25*delay, "the plan ran on past its budget")
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

// A server with no budget configured has the default one.
func TestPlanBudgetDefault(t *testing.T) {
	assert.Equal(t, DefaultPlanBudget, New(Config{}).planBudget)
	assert.Equal(t, time.Second, New(Config{PlanBudget: time.Second}).planBudget)
}
