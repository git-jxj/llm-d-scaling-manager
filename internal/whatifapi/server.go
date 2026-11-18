package whatifapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// maxBody is the largest request body the API reads: a scenario is a few
// hundred bytes.
const maxBody = 1 << 20

// Cache sizes: inputs are a replayed state each, so few are kept; plans and
// curve sets are small.
const (
	defaultInputs = 16
	defaultPlans  = 512
	defaultCurves = 256
	defaultSizes  = 128
)

// defaultResponseBytes bounds the plan and the size responses kept, each
// cache on its own: a plan with the per-bucket view and 8 edges measured
// 548 KB on the worked recording, so 512 of them would hold some 280 MB
// with no bound in bytes; 64 MiB keeps about 120 of them.
const defaultResponseBytes = 64 << 20

// bodyBytes is a response's weight in a cache: its length.
func bodyBytes(b []byte) int { return len(b) }

// defaultMaxInputs is how many sources are resolved at once by default: each
// is a replay or a Prometheus capture of seconds to minutes.
const defaultMaxInputs = 2

// retryAfterSeconds is what a busy server asks a client to wait.
const retryAfterSeconds = "1"

// Config is the server's configuration.
type Config struct {
	// Resolver resolves sources (cmd/capacity-planner's).
	Resolver Resolver
	// WhatifSchema names the CLI schema the answer, refusal and flag shapes
	// follow ("schema-v3").
	WhatifSchema string
	// MaxPlans and MaxInputs are how many plans and how many inputs are
	// priced at once; past them a request is 503 with Retry-After. 0 is the
	// default: GOMAXPROCS plans, defaultMaxInputs inputs.
	MaxPlans, MaxInputs int
	// PlanBudget is how long one plan may be priced. Past it the plan stops
	// at its next figure and is refused with PlanBudgetExceeded. 0 or less is
	// DefaultPlanBudget. It must end before the HTTP server's write timeout,
	// or the refusal is written to a connection already closed.
	PlanBudget time.Duration
}

// DefaultPlanBudget is the plan budget a server has when its Config names
// none: a cold plan of the worked recording takes under 20 s, and it ends
// well before capacity-planner serve's 10-minute write timeout (serve refuses
// a --plan-budget past 9 minutes).
const DefaultPlanBudget = 5 * time.Minute

// ReasonPlanBudgetExceeded is the reason an ErrorResponse gives for a plan
// stopped at the server's plan budget.
const ReasonPlanBudgetExceeded = "PlanBudgetExceeded"

// errPlanBudget is the cause a plan's context ends with at its budget.
var errPlanBudget = errors.New("plan budget reached")

// BudgetError is a plan stopped because it ran past the server's plan
// budget. Nothing of it is cached, so the same plan asked again runs again.
//
// It deliberately has no Unwrap to context.DeadlineExceeded: writeErr would
// then read it as a request that ended, and flight.doFor would have every
// caller sharing the plan price it again on its own context.
type BudgetError struct {
	// Budget is the server's plan budget.
	Budget time.Duration
}

// Error says what ran out and what to do.
func (e BudgetError) Error() string {
	return fmt.Sprintf("the plan was not priced within the server's plan budget of %s, and was stopped: "+
		`ask without the per-bucket view (on the page, untick "Show if all traffic were bucket b") or with fewer bucket edges, `+
		"or start the server with a larger --plan-budget", e.Budget)
}

// Server serves the API. Its state is caches only: an inputs entry per
// inputsId, a response per scenarioId and per sizeId, a curve set per fleet,
// and the limits per fleet and targets.
type Server struct {
	resolver     Resolver
	whatifSchema string
	inputs       *lru[*inputsEntry]
	plans        *lru[[]byte]
	sizes        *lru[[]byte]
	// searches is each size's search, by sizeId, kept before its answer's
	// plan is priced: a size whose plan failed is not searched again.
	searches *lru[*SizeResponse]
	curves   *lru[*curveSet]
	limits   *lru[*limitEntry]
	// planSlots and inputSlots bound the work in flight: a token per request
	// priced.
	planSlots, inputSlots chan struct{}
	// planBudget is how long one plan may be priced.
	planBudget time.Duration
	// planFlight, curveFlight and limitFlight price each key once at a time:
	// identical requests in flight share one pricing and one slot.
	planFlight  flight[[]byte]
	sizeFlight  flight[[]byte]
	curveFlight flight[*curveSet]
	limitFlight flight[*limitEntry]
	// onCurves and onLimits, when set, are called each time a curve set or a
	// limit entry is priced: the caching tests count them.
	onCurves, onLimits func()
	// onSearch, when set, is called each time a size searches.
	onSearch func()
}

// inputsEntry is a source resolved: its id, the inputs a plan reads its
// defaults from, and its Pricer.
type inputsEntry struct {
	id     string
	doc    InputsResponse
	pricer Pricer
	// series is the source's cycles, for a day plan; nil where the
	// Resolver gave none.
	series []SeriesSample
}

// New is a server with empty caches.
func New(c Config) *Server {
	plans, inputs := c.MaxPlans, c.MaxInputs
	if plans <= 0 {
		plans = runtime.GOMAXPROCS(0)
	}
	if inputs <= 0 {
		inputs = defaultMaxInputs
	}
	budget := c.PlanBudget
	if budget <= 0 {
		budget = DefaultPlanBudget
	}
	return &Server{resolver: c.Resolver, whatifSchema: c.WhatifSchema, inputs: newLRU[*inputsEntry](defaultInputs),
		plans: newWeighedLRU(defaultPlans, defaultResponseBytes, bodyBytes), sizes: newWeighedLRU(defaultSizes, defaultResponseBytes, bodyBytes),
		searches: newLRU[*SizeResponse](defaultSizes), curves: newLRU[*curveSet](defaultCurves), limits: newLRU[*limitEntry](defaultCurves),
		planSlots: make(chan struct{}, plans), inputSlots: make(chan struct{}, inputs), planBudget: budget}
}

// errBusy is a request past the server's bound on work in flight.
var errBusy = errors.New("the server is pricing as many requests as it takes at once; retry shortly")

// acquire takes a slot, or reports false at once when none is free.
func acquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// Handler is the API's routes. The caller wraps it in authentication where
// the bind needs it (Authenticator.Wrap), or in LoopbackOnly.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/inputs", s.handleInputs)
	mux.HandleFunc("POST /v1/plan", s.handlePlan)
	mux.HandleFunc("POST /v1/size", s.handleSize)
	mux.HandleFunc("POST /v1/report", s.handleReport)
	mux.HandleFunc("POST /v1/dayplan", s.handleDayPlan)
	mux.HandleFunc("GET /v1/sources", s.handleSources)
	mux.HandleFunc("GET /{$}", servePage)
	mux.HandleFunc("GET /page/", servePage)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, fmt.Errorf("%s %s: no such endpoint; the server serves POST /v1/inputs, POST /v1/plan, POST /v1/size, POST /v1/report, POST /v1/dayplan, GET /v1/sources and the page at /",
			r.Method, r.URL.Path))
	})
	return mux
}

// handleSources is GET /v1/sources: the sources the resolver reads, or none
// when it cannot list them.
func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	out := SourcesResponse{Namespaces: []string{}, Recordings: []string{}}
	if l, ok := s.resolver.(SourceLister); ok {
		var err error
		if out, err = l.Sources(r.Context()); err != nil {
			writeErr(w, err)
			return
		}
		if out.Namespaces == nil {
			out.Namespaces = []string{}
		}
		if out.Recordings == nil {
			out.Recordings = []string{}
		}
	}
	out.SchemaVersion = SchemaVersion
	body, err := marshal(out)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// handleInputs is POST /v1/inputs: the source resolved, kept under its id,
// and answered with the caller's own source.
func (s *Server) handleInputs(w http.ResponseWriter, r *http.Request) {
	var req InputsRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !acquire(s.inputSlots) {
		writeErr(w, errBusy)
		return
	}
	defer func() { <-s.inputSlots }()
	res, err := s.resolver.Resolve(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	doc := res.Inputs
	doc.SchemaVersion, doc.WhatifSchema = SchemaVersion, s.whatifSchema
	doc.InputsID = contentID("inputs", res.Content)
	// The entry kept is the first caller's, but each caller is answered with
	// its own source: two recordings of the same bytes share an id, not a
	// name.
	s.inputs.add(doc.InputsID, &inputsEntry{id: doc.InputsID, doc: doc, pricer: res.Pricer, series: res.Series})
	body, err := marshal(doc)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// handlePlan is POST /v1/plan: the scenario priced, or its cached bytes.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req PlanRequest
	if !readJSON(w, r, &req) {
		return
	}
	body, err := s.plan(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// errUnknownInputs is an inputsId the server does not hold.
var errUnknownInputs = errors.New("unknown inputsId: resolve the source with POST /v1/inputs (ids are kept while in use, the least used dropped first)")

// plan prices req and returns the response body: from the cache when the same
// scenario was priced before, so the same scenario has the same bytes. A
// plan not cached is priced once however many ask for it at once, takes one
// of the server's plan slots (errBusy with none free), and stops between
// figures once ctx ends.
func (s *Server) plan(ctx context.Context, req PlanRequest) ([]byte, error) {
	return s.planHolding(ctx, req, false)
}

// planHolding is plan for a caller that may already hold a plan slot (held):
// a size prices its answer's plan on the slot its search took. A held caller
// that joined a plan already in flight shares that plan's result; when its
// leader, holding no slot, was turned away busy, the held caller asks once
// more and, with no plan in flight then, prices it on its own slot. So a
// finished search is lost to a busy server only when another request for the
// same plan is turned away busy twice in a row.
func (s *Server) planHolding(ctx context.Context, req PlanRequest, held bool) ([]byte, error) {
	body, err := s.planOnce(ctx, req, held)
	if held && errors.Is(err, errBusy) {
		body, err = s.planOnce(ctx, req, held)
	}
	return body, err
}

// planOnce is one ask of planHolding.
func (s *Server) planOnce(ctx context.Context, req PlanRequest, held bool) ([]byte, error) {
	e, ok := s.inputs.get(req.InputsID)
	if !ok {
		return nil, errUnknownInputs
	}
	sc, err := scenarioOf(&e.doc, req)
	if err != nil {
		return nil, err
	}
	id := contentID("scenario", sc)
	if b, ok := s.plans.get(id); ok {
		return b, nil
	}
	return s.planFlight.doFor(ctx, id, func() ([]byte, error) {
		if b, ok := s.plans.get(id); ok {
			return b, nil
		}
		if !held {
			if !acquire(s.planSlots) {
				return nil, errBusy
			}
			defer func() { <-s.planSlots }()
		}
		out, err := s.planWithin(ctx, &planner{s: s, e: e, sc: sc})
		if err != nil {
			return nil, err
		}
		out.ScenarioID = id
		body, err := marshal(out)
		if err != nil {
			return nil, err
		}
		return s.plans.add(id, body), nil
	})
}

// planWithin prices p within the server's plan budget: past it, the plan
// stops at its next figure and is a BudgetError. A request that ended first
// is its context's error, as before.
func (s *Server) planWithin(ctx context.Context, p *planner) (*PlanResponse, error) {
	bctx, cancel := context.WithTimeoutCause(ctx, s.planBudget, errPlanBudget)
	defer cancel()
	out, err := p.plan(bctx)
	// Only the budget's own end is a BudgetError: an error the plan returned
	// for another reason as the budget ran out (a knob refused, a fleet that
	// failed) is that error.
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil && errors.Is(context.Cause(bctx), errPlanBudget) {
		return nil, BudgetError{Budget: s.planBudget}
	}
	return out, err
}

// LoopbackOnly answers only requests addressed to a loopback name, by the
// rule CheckListen binds by (isLoopback: localhost, 127.0.0.0/8, ::1, with
// any port): a server with no authentication must not answer a page that
// rebinds its own name to 127.0.0.1. Others are 403.
func LoopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !isLoopback(strings.Trim(host, "[]")) {
			writeError(w, http.StatusForbidden, fmt.Errorf("host %q: this server answers loopback names only (localhost, 127.0.0.0/8, [::1])", r.Host))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readJSON reads the request body into v strictly: a media type other than
// application/json (none included, so that no form a page posts across sites
// is read) is 415; an unknown field, a second value, or a body past maxBody
// is a 400.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, fmt.Errorf("content type %q, want application/json", r.Header.Get("Content-Type")))
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("request body: %w", err))
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, errors.New("request body: more than one JSON value"))
		return false
	}
	return true
}

// marshal is v as indented JSON with a final newline, HTML left unescaped.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// writeBody writes a JSON body with 200.
func writeBody(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// writeErr writes err with its status: 400 for the caller's input, 404 for
// an unknown inputsId, 503 for a busy server (with Retry-After), a plan past
// the plan budget (reason PlanBudgetExceeded) or a request that ended, 500
// for anything else.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case IsInputError(err):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, errUnknownInputs):
		writeError(w, http.StatusNotFound, err)
	case errors.As(err, new(ForbiddenError)):
		writeError(w, http.StatusForbidden, err)
	case errors.Is(err, errBusy):
		w.Header().Set("Retry-After", retryAfterSeconds)
		writeError(w, http.StatusServiceUnavailable, err)
	case errors.As(err, new(BudgetError)):
		writeErrorReason(w, http.StatusServiceUnavailable, err, ReasonPlanBudgetExceeded)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the request ended before it was priced: %w", err))
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

// writeError writes an ErrorResponse.
func writeError(w http.ResponseWriter, status int, err error) {
	writeErrorReason(w, status, err, "")
}

// writeErrorReason writes an ErrorResponse with its reason, "" for none.
func writeErrorReason(w http.ResponseWriter, status int, err error, reason string) {
	body, merr := marshal(ErrorResponse{SchemaVersion: SchemaVersion, Error: err.Error(), Reason: reason})
	if merr != nil {
		body = []byte(`{"error": "internal"}` + "\n")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
