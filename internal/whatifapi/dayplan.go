package whatifapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"time"
)

// The day plan (POST /v1/dayplan): the prefill and decode replicas a
// window's traffic needed, bucket by bucket, against the replicas the fleet
// had. Each bucket is sized as POST /v1/size sizes a rate, by the same
// search (sizer.search) on the same priced fleets: the learned state is read
// once, by the inputs, and every bucket of one shape shares one sizer, whose
// fleets are priced once whatever the rate. Nothing new is modelled.

// SeriesSample is one cycle of the source: its time, the model's arrival
// rate, the serving pods of each role, and the traffic's shape where the
// cycle read one (HasShape).
type SeriesSample struct {
	At                      time.Time
	Rate                    float64
	Prefill, Decode         int
	PrefillGPUs, DecodeGPUs int
	PromptTokens            float64
	OutputTokens            float64
	PrefixHitRate           float64
	HasShape                bool
}

// The day plan's defaults and bounds.
const (
	// DefaultDayPlanStep is a bucket's length when the request names none.
	DefaultDayPlanStep = 5 * time.Minute
	// minDayPlanStep and maxDayPlanStep bound a bucket's length.
	minDayPlanStep = 15 * time.Second
	maxDayPlanStep = 24 * time.Hour
	// maxDayPlanBuckets bounds the buckets one day plan sizes.
	maxDayPlanBuckets = 3000
	// DefaultMinRequests is the fewest requests a bucket must hold to be
	// sized when the request names none.
	DefaultMinRequests = 30
	// minPercentileSamples is the median samples a bucket must hold for its
	// p95 to be the default the plan sizes for.
	minPercentileSamples = 10
	// shapeGrid is the ratio between neighbouring shapes a bucket's
	// observed lengths are rounded to, so that buckets of nearly the same
	// shape share a sizer and its priced fleets.
	shapeGrid = 1.05
)

// What a bucket is sized for (DayPlanRequest.SizeFor).
const (
	SizeForMean = "mean"
	SizeForP95  = "p95"
	SizeForMax  = "max"
)

// A bucket's status.
const (
	BucketSized         = "sized"
	BucketTooLittleData = "tooLittleData"
	BucketNoData        = "noData"
	BucketUnmet         = "unmet"
)

// A bucket's or an hour's verdict: the fleet it had against the one it
// needed.
const (
	VerdictUnder = "under"
	VerdictOver  = "over"
	VerdictMatch = "match"
)

// The flags a day plan adds to a bucket, beside the planner's own.
const (
	// FlagWindowShape is a bucket priced at the inputs' shape: its cycles
	// read no lengths of their own.
	FlagWindowShape = "WindowShape"
	// FlagExtrapolated is a bucket priced at a stated shape outside the
	// lengths the window's buckets observed.
	FlagExtrapolated = "Extrapolated"
	// FlagFewSamples is a bucket whose percentile rests on fewer than
	// minPercentileSamples cycles.
	FlagFewSamples = "FewSamples"
)

// DayPlanRequest is POST /v1/dayplan. Every field but InputsID defaults as a
// size's does; the traffic's classes are refused, since the rates are the
// source's own, bucket by bucket.
type DayPlanRequest struct {
	InputsID    string      `json:"inputsId"`
	Traffic     *Traffic    `json:"traffic,omitempty"`
	SLO         *SLORequest `json:"slo,omitempty"`
	Assume      *Assume     `json:"assume,omitempty"`
	Headroom    *float64    `json:"headroom,omitempty"`
	MaxReplicas *FleetKnob  `json:"maxReplicas,omitempty"`
	// Step is a bucket's length, a Go duration ("5m"); default 5m.
	Step string `json:"step,omitempty"`
	// SizeFor is the rate of a bucket the fleet is sized for: "mean", "p95"
	// or "max" of its cycles' rates. Default p95 when the buckets hold
	// enough cycles for one (a median of 10), else mean.
	SizeFor string `json:"sizeFor,omitempty"`
	// MinRequests is the fewest requests a bucket must hold to be sized;
	// default 30.
	MinRequests *float64 `json:"minRequests,omitempty"`
}

// DayPlanResponse is a day plan: every bucket, the hours they make, and the
// summary.
type DayPlanResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	WhatifSchema  string `json:"whatifSchema"`
	DayPlanID     string `json:"dayPlanId"`
	InputsID      string `json:"inputsId"`
	Banner        string `json:"banner"`
	// From and To are the first bucket's start and the last bucket's end.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Step is a bucket's length, seconds.
	Step float64 `json:"step"`
	// SizeFor is what each bucket was sized for, and SizeForSource
	// "request" or "default".
	SizeFor       string            `json:"sizeFor"`
	SizeForSource string            `json:"sizeForSource"`
	MinRequests   float64           `json:"minRequests"`
	TTFT          ResolvedObjective `json:"ttft"`
	// TPOT is absent with no TPOT asked.
	TPOT        *ResolvedObjective `json:"tpot,omitempty"`
	Headroom    float64            `json:"headroom"`
	MaxReplicas FleetCounts        `json:"maxReplicas"`
	Buckets     []DayBucket        `json:"buckets"`
	Hours       []DayHour          `json:"hours"`
	Summary     DaySummary         `json:"summary"`
	// Searched is how many fleets the day plan priced.
	Searched int `json:"searched"`
	// Notes are what the plan does not price, in words.
	Notes []string `json:"notes"`
}

// DayRate is a bucket's or an hour's request rate over its cycles, r/s.
type DayRate struct {
	Mean float64 `json:"mean"`
	P95  float64 `json:"p95"`
	Max  float64 `json:"max"`
}

// DayShape is the lengths a bucket was priced at, and Source "bucket" (its
// own cycles'), "window" (the inputs') or "stated" (the request's mix).
type DayShape struct {
	PromptTokens  *float64 `json:"promptTokens,omitempty"`
	OutputTokens  *float64 `json:"outputTokens,omitempty"`
	PrefixHitRate *float64 `json:"prefixHitRate,omitempty"`
	Source        string   `json:"source"`
}

// DayBucket is one bucket: what it saw, what it was sized for, the fleet it
// needed and the fleet it had.
type DayBucket struct {
	Start time.Time `json:"start"`
	// Samples is the cycles in the bucket, and Requests the requests they
	// carried (the mean rate times the time they cover).
	Samples  int      `json:"samples"`
	Requests float64  `json:"requests"`
	Rate     *DayRate `json:"rate,omitempty"`
	// SizedRate is the rate the bucket was sized for.
	SizedRate *float64 `json:"sizedRate,omitempty"`
	Shape     DayShape `json:"shape"`
	Status    string   `json:"status"`
	// Needed is the fewest replicas that serve SizedRate (sized), and Limit
	// the fleet's limit there.
	Needed *FleetCounts `json:"needed,omitempty"`
	Limit  *Figure      `json:"limit,omitempty"`
	// Unmet is why no fleet within the bound serves the rate.
	Unmet *Refusal `json:"unmet,omitempty"`
	// Actual is the median serving pods of each role over the bucket's
	// cycles; absent with no cycles.
	Actual  *FleetCounts `json:"actual,omitempty"`
	Verdict string       `json:"verdict,omitempty"`
	Flags   []string     `json:"flags,omitempty"`
}

// DayHour is one hour of buckets, for a table: its rate over every cycle,
// the most replicas any of its buckets needed, the fewest it had, and its
// verdict: under when any bucket was under, over when every sized bucket
// was over.
type DayHour struct {
	Start   time.Time    `json:"start"`
	Rate    *DayRate     `json:"rate,omitempty"`
	Needed  *FleetCounts `json:"needed,omitempty"`
	Actual  *FleetCounts `json:"actual,omitempty"`
	Verdict string       `json:"verdict,omitempty"`
	// Sized is how many of its buckets were sized.
	Sized int `json:"sized"`
}

// DaySummary is the plan in a few figures. Replica-hours count sized
// buckets only; GPU-hours are present where the source states the GPUs per
// replica.
type DaySummary struct {
	PeakNeeded         *FleetCounts `json:"peakNeeded,omitempty"`
	MinNeeded          *FleetCounts `json:"minNeeded,omitempty"`
	NeededReplicaHours RoleHours    `json:"neededReplicaHours"`
	ActualReplicaHours RoleHours    `json:"actualReplicaHours"`
	GPUsPerReplica     *FleetCounts `json:"gpusPerReplica,omitempty"`
	NeededGPUHours     *float64     `json:"neededGpuHours,omitempty"`
	ActualGPUHours     *float64     `json:"actualGpuHours,omitempty"`
	// UnderHours are the hours with a bucket under (SLO risk), OverHours
	// those whose every sized bucket was over (waste).
	UnderHours []time.Time `json:"underHours"`
	OverHours  []time.Time `json:"overHours"`
	Counts     DayCounts   `json:"counts"`
}

// RoleHours is replica-hours per role.
type RoleHours struct {
	Prefill float64 `json:"prefill"`
	Decode  float64 `json:"decode"`
}

// DayCounts is how many buckets ended each way.
type DayCounts struct {
	Sized         int `json:"sized"`
	Unmet         int `json:"unmet"`
	TooLittleData int `json:"tooLittleData"`
	NoData        int `json:"noData"`
	Under         int `json:"under"`
	Over          int `json:"over"`
	Match         int `json:"match"`
}

// handleDayPlan is POST /v1/dayplan.
func (s *Server) handleDayPlan(w http.ResponseWriter, r *http.Request) {
	var req DayPlanRequest
	if !readJSON(w, r, &req) {
		return
	}
	body, err := s.dayPlan(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// dayPlan answers req, from the cache when asked before, on one plan slot.
func (s *Server) dayPlan(ctx context.Context, req DayPlanRequest) ([]byte, error) {
	e, ok := s.inputs.get(req.InputsID)
	if !ok {
		return nil, errUnknownInputs
	}
	q, err := dayQuestionOf(e, req)
	if err != nil {
		return nil, err
	}
	id := contentID("dayplan", q.key())
	if b, ok := s.sizes.get(id); ok {
		return b, nil
	}
	return s.sizeFlight.doFor(ctx, id, func() ([]byte, error) {
		if b, ok := s.sizes.get(id); ok {
			return b, nil
		}
		if !acquire(s.planSlots) {
			return nil, errBusy
		}
		defer func() { <-s.planSlots }()
		bctx, cancel := context.WithTimeoutCause(ctx, s.planBudget, errPlanBudget)
		defer cancel()
		out, err := q.plan(bctx)
		if err != nil {
			if context.Cause(bctx) == errPlanBudget && ctx.Err() == nil {
				return nil, BudgetError{Budget: s.planBudget}
			}
			return nil, err
		}
		out.SchemaVersion, out.WhatifSchema, out.DayPlanID, out.InputsID, out.Banner = SchemaVersion, s.whatifSchema, id, e.id, banner
		body, err := marshal(out)
		if err != nil {
			return nil, err
		}
		return s.sizes.add(id, body), nil
	})
}

// DayPlan answers req against a source resolved outside a server: the CLI's
// day-plan, on the same code path as POST /v1/dayplan.
func DayPlan(ctx context.Context, res *Resolved, req DayPlanRequest) (*DayPlanResponse, error) {
	doc := res.Inputs
	doc.InputsID = contentID("inputs", res.Content)
	e := &inputsEntry{id: doc.InputsID, doc: doc, pricer: res.Pricer, series: res.Series}
	req.InputsID = e.id
	q, err := dayQuestionOf(e, req)
	if err != nil {
		return nil, err
	}
	out, err := q.plan(ctx)
	if err != nil {
		return nil, err
	}
	out.SchemaVersion, out.DayPlanID, out.InputsID, out.Banner = SchemaVersion, contentID("dayplan", q.key()), e.id, banner
	return out, nil
}

// dayQuestion is a day plan's request checked against its inputs.
type dayQuestion struct {
	e           *inputsEntry
	sc          Scenario
	k           Knobs
	step        time.Duration
	sizeFor     string
	sizeFromReq bool
	minRequests float64
	bound       FleetCounts
}

// key is what names the day plan's answer.
func (q *dayQuestion) key() any {
	return struct {
		Scenario    Scenario
		Step        time.Duration
		SizeFor     string
		MinRequests float64
		Bound       FleetCounts
	}{q.sc, q.step, q.sizeFor, q.minRequests, q.bound}
}

// dayQuestionOf checks req against e.
func dayQuestionOf(e *inputsEntry, req DayPlanRequest) (*dayQuestion, error) {
	if len(e.series) == 0 {
		return nil, InputErrorf("the source carries no cycles to plan a day from")
	}
	if e.doc.Fleet.Prefill == nil {
		return nil, InputErrorf("the inputs have no prefill variant: POST /v1/dayplan sizes a P/D fleet")
	}
	if t := req.Traffic; t != nil && t.Classes != nil {
		return nil, InputErrorf("traffic.classes: a day plan sizes each bucket for its own observed rate; give the mix and arrivalCV2 only")
	}
	q := &dayQuestion{e: e, step: DefaultDayPlanStep, minRequests: DefaultMinRequests}
	if req.Step != "" {
		d, err := time.ParseDuration(req.Step)
		if err != nil || d < minDayPlanStep || d > maxDayPlanStep {
			return nil, InputErrorf("step %q, want a duration from %s to %s", req.Step, minDayPlanStep, maxDayPlanStep)
		}
		q.step = d
	}
	if m := req.MinRequests; m != nil {
		if !(*m >= 0) || math.IsInf(*m, 1) {
			return nil, InputErrorf("minRequests %g, want 0 or more", *m)
		}
		q.minRequests = *m
	}
	var err error
	if q.bound, err = sizeBound(req.MaxReplicas); err != nil {
		return nil, err
	}
	one := 1
	traffic := &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: ptr(1)}}}
	if req.Traffic != nil {
		traffic.Mix, traffic.ArrivalCV2 = req.Traffic.Mix, req.Traffic.ArrivalCV2
	}
	if q.sc, err = scenarioOf(&e.doc, PlanRequest{InputsID: e.id, Fleet: &FleetKnob{Prefill: &one, Decode: &one},
		Traffic: traffic, SLO: req.SLO, Assume: req.Assume, Headroom: req.Headroom}); err != nil {
		return nil, err
	}
	// The rate is each bucket's, not the scenario's.
	q.sc.Rate = nil
	if q.k, err = q.sc.knobs(); err != nil {
		return nil, err
	}
	first, last := e.series[0].At, e.series[len(e.series)-1].At
	if n := int(last.Sub(first)/q.step) + 2; n > maxDayPlanBuckets {
		return nil, InputErrorf("step %s cuts the window into %d buckets, over the %d a day plan sizes: lengthen the step", q.step, n, maxDayPlanBuckets)
	}
	switch req.SizeFor {
	case "":
		q.sizeFor = SizeForMean
		if medianSamples(bucketize(e.series, q.step)) >= minPercentileSamples {
			q.sizeFor = SizeForP95
		}
	case SizeForMean, SizeForP95, SizeForMax:
		q.sizeFor, q.sizeFromReq = req.SizeFor, true
	default:
		return nil, InputErrorf("sizeFor %q, want %s, %s or %s", req.SizeFor, SizeForMean, SizeForP95, SizeForMax)
	}
	return q, nil
}

// dayBucket is one bucket's cycles.
type dayBucket struct {
	start   time.Time
	samples []SeriesSample
}

// bucketize cuts series into buckets of step, aligned to step in UTC (a
// step that divides a day starts a bucket at midnight UTC), from the first
// cycle's bucket to the last's: a bucket no cycle fell in is kept, empty. A
// clock change is no change in UTC: a day across one has the buckets of 24
// hours.
func bucketize(series []SeriesSample, step time.Duration) []dayBucket {
	if len(series) == 0 {
		return nil
	}
	sorted := slices.Clone(series)
	slices.SortStableFunc(sorted, func(a, b SeriesSample) int { return a.At.Compare(b.At) })
	first := sorted[0].At.UTC().Truncate(step)
	last := sorted[len(sorted)-1].At.UTC().Truncate(step)
	out := make([]dayBucket, int(last.Sub(first)/step)+1)
	for i := range out {
		out[i].start = first.Add(time.Duration(i) * step)
	}
	for _, x := range sorted {
		i := int(x.At.UTC().Truncate(step).Sub(first) / step)
		out[i].samples = append(out[i].samples, x)
	}
	return out
}

// medianSamples is the median cycles of the buckets that hold any.
func medianSamples(buckets []dayBucket) int {
	var n []int
	for _, b := range buckets {
		if len(b.samples) > 0 {
			n = append(n, len(b.samples))
		}
	}
	if len(n) == 0 {
		return 0
	}
	sort.Ints(n)
	return n[len(n)/2]
}

// sampleStep is the series' cycle: the median gap between cycles, the step
// when there is one cycle.
func sampleStep(series []SeriesSample, step time.Duration) time.Duration {
	var gaps []time.Duration
	for i := 1; i < len(series); i++ {
		if d := series[i].At.Sub(series[i-1].At); d > 0 {
			gaps = append(gaps, d)
		}
	}
	if len(gaps) == 0 {
		return step
	}
	slices.Sort(gaps)
	return gaps[len(gaps)/2]
}

// rateOf is the rate over samples: the mean, the nearest-rank p95 and the
// most.
func rateOf(samples []SeriesSample) DayRate {
	r := make([]float64, len(samples))
	var sum float64
	for i, x := range samples {
		r[i] = x.Rate
		sum += x.Rate
	}
	slices.Sort(r)
	rank := int(math.Ceil(0.95*float64(len(r)))) - 1
	return DayRate{Mean: sum / float64(len(r)), P95: r[max(rank, 0)], Max: r[len(r)-1]}
}

// medianInt is the median of xs, the upper one of an even count.
func medianInt(xs []int) int {
	s := slices.Clone(xs)
	sort.Ints(s)
	return s[len(s)/2]
}

// actualOf is the median serving pods of each role over samples.
func actualOf(samples []SeriesSample) FleetCounts {
	p, d := make([]int, len(samples)), make([]int, len(samples))
	for i, x := range samples {
		p[i], d[i] = x.Prefill, x.Decode
	}
	return FleetCounts{Prefill: medianInt(p), Decode: medianInt(d)}
}

// shapeKnob is x rounded onto the shape grid, a whole token.
func shapeKnob(x float64) float64 {
	g := math.Log(shapeGrid)
	return math.Round(math.Exp(math.Round(math.Log(x)/g) * g))
}

// bucketShape is the lengths samples read, weighted by rate (equally where
// none carried one), and whether any read one.
func bucketShape(samples []SeriesSample) (prompt, output, hit float64, ok bool) {
	var w float64
	for _, x := range samples {
		if !x.HasShape {
			continue
		}
		wx := x.Rate
		if !(wx > 0) {
			wx = 1e-9
		}
		w += wx
		prompt += wx * x.PromptTokens
		output += wx * x.OutputTokens
		hit += wx * x.PrefixHitRate
	}
	if w == 0 {
		return 0, 0, 0, false
	}
	return prompt / w, output / w, hit / w, true
}

// sizedRate is the rate of r the plan sizes for.
func (q *dayQuestion) sizedRate(r DayRate) float64 {
	switch q.sizeFor {
	case SizeForP95:
		return r.P95
	case SizeForMax:
		return r.Max
	}
	return r.Mean
}

// plan sizes every bucket.
func (q *dayQuestion) plan(ctx context.Context) (*DayPlanResponse, error) {
	series := q.e.series
	buckets := bucketize(series, q.step)
	cycle := sampleStep(series, q.step).Seconds()
	out := &DayPlanResponse{Step: q.step.Seconds(), SizeFor: q.sizeFor, SizeForSource: "default", MinRequests: q.minRequests,
		TTFT: q.sc.TTFT, TPOT: q.sc.TPOT, Headroom: q.sc.Headroom, MaxReplicas: q.bound,
		From: buckets[0].start, To: buckets[len(buckets)-1].start.Add(q.step)}
	if q.sizeFromReq {
		out.SizeForSource = "request"
	}
	stated := !q.sc.Mix.Observed
	var lo, hi [2]float64
	sizers := map[[2]float64]*sizer{}
	sizerFor := func(k Knobs) *sizer {
		key := [2]float64{k.PromptTokens, k.OutputTokens}
		z := sizers[key]
		if z == nil {
			z = &sizer{e: q.e, sc: q.sc, k: k, bound: q.bound, seen: map[FleetCounts]sizeEval{}, plain: map[FleetCounts]sizeEval{}}
			sizers[key] = z
		}
		return z
	}
	gpus := FleetCounts{}
	for _, x := range series {
		gpus.Prefill, gpus.Decode = max(gpus.Prefill, x.PrefillGPUs), max(gpus.Decode, x.DecodeGPUs)
	}
	for _, b := range buckets {
		db := DayBucket{Start: b.start, Samples: len(b.samples), Shape: DayShape{Source: "window"}}
		if len(b.samples) == 0 {
			db.Status = BucketNoData
			out.Buckets = append(out.Buckets, db)
			continue
		}
		r := rateOf(b.samples)
		db.Rate = &r
		db.Requests = r.Mean * cycle * float64(len(b.samples))
		a := actualOf(b.samples)
		db.Actual = &a
		k := q.k
		if p, o, h, ok := bucketShape(b.samples); ok {
			for i, v := range []float64{p, o} {
				if lo[i] == 0 || v < lo[i] {
					lo[i] = v
				}
				hi[i] = max(hi[i], v)
			}
			db.Shape.PrefixHitRate = ptr(h)
			if !stated {
				k.PromptTokens, k.OutputTokens = shapeKnob(p), shapeKnob(o)
				db.Shape.Source = "bucket"
				db.Shape.PromptTokens, db.Shape.OutputTokens = ptr(p), ptr(o)
			}
		} else if !stated {
			db.Flags = append(db.Flags, FlagWindowShape)
		}
		if stated {
			db.Shape.Source = "stated"
			if k.PromptTokens > 0 {
				db.Shape.PromptTokens = ptr(k.PromptTokens)
			}
			if k.OutputTokens > 0 {
				db.Shape.OutputTokens = ptr(k.OutputTokens)
			}
		}
		if q.sizeFor != SizeForMean && len(b.samples) < minPercentileSamples {
			db.Flags = append(db.Flags, FlagFewSamples)
		}
		if db.Requests < q.minRequests || !(r.Mean > 0) {
			db.Status = BucketTooLittleData
			out.Buckets = append(out.Buckets, db)
			continue
		}
		rate := q.sizedRate(r)
		db.SizedRate = ptr(rate)
		z := sizerFor(k)
		z.rate = rate
		res, err := z.search(ctx)
		if err != nil {
			return nil, err
		}
		if u := res.Unmet; u != nil {
			db.Status = BucketUnmet
			db.Unmet = &u.Refused
			db.Flags = appendNew(db.Flags, u.AtBound.Limit.Flags...)
			out.Buckets = append(out.Buckets, db)
			continue
		}
		n := res.Answer.Fleet
		db.Status, db.Needed = BucketSized, &n
		lim := figureOf(z.seen[n].fleet, unitRate, false)
		db.Limit = &lim
		db.Flags = appendNew(db.Flags, lim.Flags...)
		db.Verdict = verdictOf(n, a)
		out.Buckets = append(out.Buckets, db)
	}
	for _, z := range sizers {
		out.Searched += z.searched()
	}
	if stated {
		k := q.k
		outside := func(v float64, i int) bool { return v > 0 && hi[i] > 0 && (v < lo[i] || v > hi[i]) }
		if outside(k.PromptTokens, 0) || outside(k.OutputTokens, 1) {
			for i := range out.Buckets {
				out.Buckets[i].Flags = appendNew(out.Buckets[i].Flags, FlagExtrapolated)
			}
			out.Notes = append(out.Notes, fmt.Sprintf("EXTRAPOLATED: the stated lengths lie outside what the window's buckets observed "+
				"(prompt %.0f to %.0f tokens, output %.0f to %.0f): every bucket is priced at a shape the source never served.", lo[0], hi[0], lo[1], hi[1]))
		}
	}
	out.Hours = hoursOf(series, out.Buckets)
	out.Summary = summaryOf(out.Buckets, out.Hours, q.step, gpus)
	out.Notes = append(out.Notes,
		"Steady state: each bucket is sized as if its rate held, with no scale-up lag. A fleet that follows the plan must start replicas one start time before the rate arrives.",
		fmt.Sprintf("Each bucket is sized for the %s of its cycles' rates, on the fleet's limit at the SLO and headroom %.2f (POST /v1/size's search).", q.sizeFor, q.sc.Headroom),
		fmt.Sprintf("A bucket with fewer than %.0f requests is not sized (too little data); a bucket no cycle fell in has no data.", q.minRequests),
		"Actual is the median serving pods of each role over the bucket's cycles. The prefix hit rate is reported, not priced: the learned state's stands.")
	return out, nil
}

// appendNew appends each of xs not already in s.
func appendNew(s []string, xs ...string) []string {
	for _, x := range xs {
		if !slices.Contains(s, x) {
			s = append(s, x)
		}
	}
	return s
}

// verdictOf is a bucket's verdict: under when either role had fewer than it
// needed, over when neither had fewer and one had more.
func verdictOf(needed, actual FleetCounts) string {
	switch {
	case actual.Prefill < needed.Prefill || actual.Decode < needed.Decode:
		return VerdictUnder
	case actual.Prefill > needed.Prefill || actual.Decode > needed.Decode:
		return VerdictOver
	}
	return VerdictMatch
}

// hoursOf is the buckets by UTC hour.
func hoursOf(series []SeriesSample, buckets []DayBucket) []DayHour {
	var out []DayHour
	byHour := map[time.Time][]SeriesSample{}
	for _, x := range series {
		h := x.At.UTC().Truncate(time.Hour)
		byHour[h] = append(byHour[h], x)
	}
	for _, b := range buckets {
		h := b.Start.Truncate(time.Hour)
		if len(out) == 0 || !out[len(out)-1].Start.Equal(h) {
			out = append(out, DayHour{Start: h})
			if s := byHour[h]; len(s) > 0 {
				r := rateOf(s)
				out[len(out)-1].Rate = &r
			}
		}
		hr := &out[len(out)-1]
		if b.Actual != nil {
			if hr.Actual == nil {
				a := *b.Actual
				hr.Actual = &a
			} else {
				hr.Actual.Prefill, hr.Actual.Decode = min(hr.Actual.Prefill, b.Actual.Prefill), min(hr.Actual.Decode, b.Actual.Decode)
			}
		}
		if b.Needed == nil {
			continue
		}
		hr.Sized++
		if hr.Needed == nil {
			n := *b.Needed
			hr.Needed = &n
		} else {
			hr.Needed.Prefill, hr.Needed.Decode = max(hr.Needed.Prefill, b.Needed.Prefill), max(hr.Needed.Decode, b.Needed.Decode)
		}
		switch {
		case b.Verdict == VerdictUnder || hr.Verdict == VerdictUnder:
			hr.Verdict = VerdictUnder
		case b.Verdict == VerdictOver && (hr.Verdict == "" || hr.Verdict == VerdictOver):
			hr.Verdict = VerdictOver
		default:
			hr.Verdict = VerdictMatch
		}
	}
	return out
}

// summaryOf is the plan's summary.
func summaryOf(buckets []DayBucket, hours []DayHour, step time.Duration, gpus FleetCounts) DaySummary {
	s := DaySummary{UnderHours: []time.Time{}, OverHours: []time.Time{}}
	h := step.Hours()
	for _, b := range buckets {
		switch b.Status {
		case BucketNoData:
			s.Counts.NoData++
		case BucketTooLittleData:
			s.Counts.TooLittleData++
		case BucketUnmet:
			s.Counts.Unmet++
		}
		if b.Needed == nil {
			continue
		}
		s.Counts.Sized++
		switch b.Verdict {
		case VerdictUnder:
			s.Counts.Under++
		case VerdictOver:
			s.Counts.Over++
		case VerdictMatch:
			s.Counts.Match++
		}
		n := *b.Needed
		if s.PeakNeeded == nil {
			p, m := n, n
			s.PeakNeeded, s.MinNeeded = &p, &m
		} else {
			s.PeakNeeded.Prefill, s.PeakNeeded.Decode = max(s.PeakNeeded.Prefill, n.Prefill), max(s.PeakNeeded.Decode, n.Decode)
			s.MinNeeded.Prefill, s.MinNeeded.Decode = min(s.MinNeeded.Prefill, n.Prefill), min(s.MinNeeded.Decode, n.Decode)
		}
		s.NeededReplicaHours.Prefill += float64(n.Prefill) * h
		s.NeededReplicaHours.Decode += float64(n.Decode) * h
		s.ActualReplicaHours.Prefill += float64(b.Actual.Prefill) * h
		s.ActualReplicaHours.Decode += float64(b.Actual.Decode) * h
	}
	if gpus.Prefill > 0 && gpus.Decode > 0 {
		g := gpus
		s.GPUsPerReplica = &g
		s.NeededGPUHours = ptr(s.NeededReplicaHours.Prefill*float64(g.Prefill) + s.NeededReplicaHours.Decode*float64(g.Decode))
		s.ActualGPUHours = ptr(s.ActualReplicaHours.Prefill*float64(g.Prefill) + s.ActualReplicaHours.Decode*float64(g.Decode))
	}
	for _, hr := range hours {
		switch hr.Verdict {
		case VerdictUnder:
			s.UnderHours = append(s.UnderHours, hr.Start)
		case VerdictOver:
			s.OverHours = append(s.OverHours, hr.Start)
		}
	}
	return s
}
