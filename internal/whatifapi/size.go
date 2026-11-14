package whatifapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// defaultSizeBound is the most replicas of a role POST /v1/size tries when
// the request sets no bound: a search past it is more likely a rate typed
// wrongly than a fleet anyone runs. A request may raise it to maxReplicas.
const defaultSizeBound = 32

// ReasonReplicaBound is the refusal of a size whose rate no fleet within the
// bound reaches, though every figure is answered: more replicas than the
// bound, or a smaller rate.
const ReasonReplicaBound = "ReplicaBoundReached"

// The roles a size's refusal names: the role no count within the bound
// serves, or the fleet, when each role serves the rate alone but the two
// together do not.
const (
	roleDecode  = "decode"
	rolePrefill = "prefill"
	roleFleet   = "fleet"
)

// handleSize is POST /v1/size: the fewest replicas per role whose fleet
// limit reaches a rate, or why none within the bound does.
func (s *Server) handleSize(w http.ResponseWriter, r *http.Request) {
	var req SizeRequest
	if !readJSON(w, r, &req) {
		return
	}
	body, err := s.size(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// planAt is the plan a size request asks of one fleet.
func (req SizeRequest) planAt(n FleetCounts) PlanRequest {
	return PlanRequest{InputsID: req.InputsID, Fleet: &FleetKnob{Prefill: &n.Prefill, Decode: &n.Decode},
		Traffic: req.Traffic, SLO: req.SLO, Assume: req.Assume, Headroom: req.Headroom}
}

// sizeBound is the request's bound per role, each defaultSizeBound where
// left out, checked against maxReplicas.
func sizeBound(k *FleetKnob) (FleetCounts, error) {
	out := FleetCounts{Prefill: defaultSizeBound, Decode: defaultSizeBound}
	if k == nil {
		return out, nil
	}
	for _, c := range []struct {
		role string
		n    *int
		into *int
	}{{rolePrefill, k.Prefill, &out.Prefill}, {roleDecode, k.Decode, &out.Decode}} {
		if c.n == nil {
			continue
		}
		if *c.n < 1 || *c.n > maxReplicas {
			return out, InputErrorf("maxReplicas.%s %d, want 1 to %d replicas", c.role, *c.n, maxReplicas)
		}
		*c.into = *c.n
	}
	return out, nil
}

// size sizes req and returns the response body: from the cache when the
// same question was sized before. A size not cached is searched once however
// many ask for it at once; the search takes one plan slot (errBusy with none
// free), and the answer's plan is a plan like any other, cached as one.
func (s *Server) size(ctx context.Context, req SizeRequest) ([]byte, error) {
	e, ok := s.inputs.get(req.InputsID)
	if !ok {
		return nil, errUnknownInputs
	}
	if e.doc.Fleet.Prefill == nil {
		return nil, InputErrorf("the inputs have no prefill variant: POST /v1/size sizes a P/D fleet")
	}
	bound, err := sizeBound(req.MaxReplicas)
	if err != nil {
		return nil, err
	}
	sc, err := scenarioOf(&e.doc, req.planAt(FleetCounts{Prefill: 1, Decode: 1}))
	if err != nil {
		return nil, err
	}
	if sc.Rate == nil {
		return nil, InputErrorf("traffic.classes.critical.rate: give the rate to size the fleet for")
	}
	k, err := sc.knobs()
	if err != nil {
		return nil, err
	}
	id := contentID("size", struct {
		Scenario Scenario
		Bound    FleetCounts
	}{sc, bound})
	if b, ok := s.sizes.get(id); ok {
		return b, nil
	}
	return s.sizeFlight.doFor(ctx, id, func() ([]byte, error) {
		if b, ok := s.sizes.get(id); ok {
			return b, nil
		}
		z := &sizer{e: e, sc: sc, k: k, rate: *sc.Rate, bound: bound, seen: map[FleetCounts]sizeEval{}}
		out, err := z.search(ctx, s)
		if err != nil {
			return nil, err
		}
		if out.Answer != nil {
			if err := s.planAnswer(ctx, req, z.rate, out.Answer); err != nil {
				return nil, err
			}
		}
		out.SchemaVersion, out.WhatifSchema, out.SizeID, out.InputsID, out.Banner = SchemaVersion, s.whatifSchema, id, e.id, banner
		out.Rate, out.MaxReplicas, out.Searched = z.rate, bound, len(z.seen)
		body, err := marshal(out)
		if err != nil {
			return nil, err
		}
		return s.sizes.add(id, body), nil
	})
}

// planAnswer prices the plan at the answer's fleet, as POST /v1/plan does,
// and reads the answer's limit, headroom and binding from it, so that the
// answer is what a plan of that fleet says. A plan that does not reach the
// rate the search found it reaches is a bug, not a refusal.
func (s *Server) planAnswer(ctx context.Context, req SizeRequest, rate float64, a *SizeAnswer) error {
	body, err := s.plan(ctx, req.planAt(a.Fleet))
	if err != nil {
		return err
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&a.Plan); err != nil {
		return err
	}
	l := a.Plan.Limits.Fleet
	if l.Value == nil || *l.Value < rate {
		return fmt.Errorf("the plan at %d prefill and %d decode replicas does not reach %g r/s, though the search found it does: %s",
			a.Fleet.Prefill, a.Fleet.Decode, rate, figurePlain(l.Figure))
	}
	a.Limit, a.Binding = l.Figure, l.Binding
	a.Headroom = Figure{Value: ptr(*l.Value - rate), Unit: unitRate}
	return nil
}

// figurePlain is a figure in words, for an error.
func figurePlain(f Figure) string {
	if f.Refused != nil {
		return "refused " + f.Refused.Reason
	}
	if f.Value == nil {
		return "not priced"
	}
	return fmt.Sprintf("%.4g r/s", *f.Value)
}

// sizer searches one size question's replica counts.
type sizer struct {
	e     *inputsEntry
	sc    Scenario
	k     Knobs
	rate  float64
	bound FleetCounts
	// seen is every fleet priced, by its counts.
	seen map[FleetCounts]sizeEval
}

// sizeEval is one fleet's limits at the scenario's targets and headroom, as
// the plan's +1 steps price them: decode's (nil with no TPOT asked), the
// prefill role's, and the fleet's.
type sizeEval struct {
	decode, prefill, fleet whatif.Result
}

// meets reports whether the fleet's limit reaches rate.
func (v sizeEval) meets(rate float64) bool {
	x, ok := valueOf(v.fleet)
	return ok && x >= rate
}

// decodeMeets reports whether decode's own limit, TPOT and the
// operating-point cap on decode's knee, reaches rate; with no TPOT asked
// decode has no limit of its own, and the fleet's says the rest.
func (v sizeEval) decodeMeets(rate float64) bool {
	if v.decode == nil {
		return true
	}
	x, ok := valueOf(v.decode)
	return ok && x >= rate
}

// at prices the fleet of n, once per search, stopping with ctx's error once
// ctx ends.
func (z *sizer) at(ctx context.Context, n FleetCounts) (sizeEval, error) {
	if v, ok := z.seen[n]; ok {
		return v, nil
	}
	if err := ctx.Err(); err != nil {
		return sizeEval{}, err
	}
	k := z.k
	k.Prefill, k.Decode = n.Prefill, n.Decode
	fl, err := z.e.pricer.Fleet(k)
	if err != nil {
		return sizeEval{}, err
	}
	tpot := 0.0
	if z.sc.TPOT != nil {
		tpot = z.sc.TPOT.Target
	}
	var v sizeEval
	v.decode, v.prefill, v.fleet = fl.PlainLimits(z.sc.TTFT.Target, tpot, z.sc.Headroom)
	z.seen[n] = v
	return v, nil
}

// step is the fleet of n as a size's step: its limit, the limit's headroom
// over the rate, and the role that binds.
func (z *sizer) step(ctx context.Context, n FleetCounts) (*SizeStep, error) {
	v, err := z.at(ctx, n)
	if err != nil {
		return nil, err
	}
	st := &SizeStep{Fleet: n, Limit: figureOf(v.fleet, unitRate, false), Binding: binding(v.prefill, v.decode)}
	if x, ok := valueOf(v.fleet); ok {
		st.Headroom = Figure{Value: ptr(x - z.rate), Unit: unitRate}
	} else {
		st.Headroom = refusedFigure(st.Limit.Refused)
	}
	return st, nil
}

// smallest is the smallest n in [lo, hi] for which ok holds, and whether
// one does. It takes ok to be monotone, as a limit is in replicas (one more
// never serves less), and asks ok(hi) first.
func smallest(lo, hi int, ok func(int) (bool, error)) (int, bool, error) {
	if lo > hi {
		return 0, false, nil
	}
	top, err := ok(hi)
	if err != nil || !top {
		return 0, false, err
	}
	for lo < hi {
		mid := lo + (hi-lo)/2
		m, err := ok(mid)
		if err != nil {
			return 0, false, err
		}
		if m {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return hi, true, nil
}

// search is the size: decode first on its own limit (TPOT and the
// operating-point cap), the prefill role at its bound; then decode once more
// on the fleet's limit, since decode admission and the lower knee enter the
// prefill limit; then prefill on the fleet's limit at that decode count.
// Each limit is the one the plan's +1 steps read (PlainLimits): the answer's
// figures come from its plan (planAnswer). It holds a plan slot while it
// searches.
func (z *sizer) search(ctx context.Context, s *Server) (*SizeResponse, error) {
	if !acquire(s.planSlots) {
		return nil, errBusy
	}
	defer func() { <-s.planSlots }()
	maxP, maxD := z.bound.Prefill, z.bound.Decode
	atD := func(test func(sizeEval) bool) func(int) (bool, error) {
		return func(d int) (bool, error) {
			v, err := z.at(ctx, FleetCounts{Prefill: maxP, Decode: d})
			return err == nil && test(v), err
		}
	}
	decodeOwn := func(v sizeEval) bool { return v.decodeMeets(z.rate) }
	fleetMeets := func(v sizeEval) bool { return v.meets(z.rate) }

	d, ok, err := smallest(1, maxD, atD(decodeOwn))
	if err != nil {
		return nil, err
	}
	if !ok {
		return z.unmet(ctx, roleDecode, FleetCounts{Prefill: maxP, Decode: maxD})
	}
	if d, ok, err = smallest(d, maxD, atD(fleetMeets)); err != nil {
		return nil, err
	}
	if !ok {
		return z.unmet(ctx, roleFleet, FleetCounts{Prefill: maxP, Decode: maxD})
	}
	// (maxP, d) meets the rate, as the recheck found, and is priced once:
	// a count is found.
	p, _, err := smallest(1, maxP, func(p int) (bool, error) {
		v, err := z.at(ctx, FleetCounts{Prefill: p, Decode: d})
		return err == nil && v.meets(z.rate), err
	})
	if err != nil {
		return nil, err
	}
	a := &SizeAnswer{Fleet: FleetCounts{Prefill: p, Decode: d}}
	if p > 1 {
		if a.FewerPrefill, err = z.step(ctx, FleetCounts{Prefill: p - 1, Decode: d}); err != nil {
			return nil, err
		}
	}
	if d > 1 {
		if a.FewerDecode, err = z.step(ctx, FleetCounts{Prefill: p, Decode: d - 1}); err != nil {
			return nil, err
		}
	}
	return &SizeResponse{Answer: a}, nil
}

// unmet is the size refused at the bound fleet n: the refusal of the role's
// figure there, when the model refuses it (a term refused, a target under
// the floor), else ReasonReplicaBound with the figure it reached. A fleet
// that falls short where decode alone does not is named by the prefill
// role when its own limit is short.
func (z *sizer) unmet(ctx context.Context, role string, n FleetCounts) (*SizeResponse, error) {
	st, err := z.step(ctx, n)
	if err != nil {
		return nil, err
	}
	v := z.seen[n]
	r := v.fleet
	switch {
	case role == roleDecode:
		r = v.decode
	case !sizeEval{fleet: v.prefill}.meets(z.rate):
		// The prefill role's own limit is short, or refused: name it.
		role, r = rolePrefill, v.prefill
	}
	u := &SizeUnmet{Role: role, AtBound: *st}
	if fig := figureOf(r, unitRate, false); fig.Refused != nil {
		u.Refused = *fig.Refused
		return &SizeResponse{Unmet: u}, nil
	}
	x, _ := valueOf(r)
	what := map[string]string{roleDecode: "decode's own limit (TPOT and the operating-point cap)",
		rolePrefill: "the prefill role's limit (TTFT)", roleFleet: "the fleet's limit"}[role]
	u.Refused = Refusal{Reason: ReasonReplicaBound, Detail: fmt.Sprintf(
		"at %d prefill and %d decode replicas, the bound searched, %s is %.4g r/s, short of %.4g r/s: raise maxReplicas (up to %d) or lower the rate",
		n.Prefill, n.Decode, what, x, z.rate, maxReplicas)}
	return &SizeResponse{Unmet: u}, nil
}
