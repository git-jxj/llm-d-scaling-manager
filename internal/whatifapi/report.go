package whatifapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// POST /v1/report answers what one fleet gives: the load the source saw, the
// utilization there, the capacity, the SLO against load and a verdict, for
// each request shape asked and at each load asked (capacity-planner
// at-replicas prints the same report). The body is built by the Pricer an
// inputsId names, which must implement Reporter.

// ReportRequest is POST /v1/report's body. Every field but InputsID
// defaults: the recorded replicas, the measured shape, 50%, 80% and 100% of
// the knee and the load seen, the policy's SLO and headroom, TTFT at p50 and
// p90 and TPOT at the mean and p90.
type ReportRequest struct {
	InputsID string     `json:"inputsId"`
	Fleet    *FleetKnob `json:"fleet,omitempty"`
	// Shapes are the request shapes to answer for: "measured", or
	// "IL/OL[/hit]" with lengths in tokens, a k suffix for 1024 ("8k/200",
	// "1k/4k/0.3"). Absent, the measured shape.
	Shapes []string `json:"shapes,omitempty"`
	// Loads are the table's rates: "5r/s", "80%" of the shape's fleet knee,
	// or "seen". Absent, 50%, 80%, 100% and seen.
	Loads []string `json:"loads,omitempty"`
	// LoadSeen is the load to judge at in place of the source's mean arrival
	// rate, requests per second; LoadSeenX multiplies it (1.5 asks what 50%
	// more traffic does). Both default to the source's rate, times 1.
	LoadSeen  *float64    `json:"loadSeen,omitempty"`
	LoadSeenX *float64    `json:"loadSeenX,omitempty"`
	Headroom  *float64    `json:"headroom,omitempty"`
	SLO       *SLORequest `json:"slo,omitempty"`
	// TTFTStatistics are the TTFT statistics the tables print, "mean" or
	// "pNN"; absent, p50 and p90.
	TTFTStatistics []string `json:"ttftStatistics,omitempty"`
	// TPOTStatistic is the TPOT percentile the tables print beside the mean,
	// "pNN", or "mean" for the mean alone; absent, p90.
	TPOTStatistic string  `json:"tpotStatistic,omitempty"`
	Assume        *Assume `json:"assume,omitempty"`
}

// ReportQuery is a ReportRequest checked and read into numbers: the question
// a Reporter answers. A zero field is its default.
type ReportQuery struct {
	// Prefill and Decode are the replica counts; 0 is the recorded count.
	Prefill, Decode int
	// Shapes are the shapes to answer for; none is the measured one.
	Shapes []ReportShapeAsk
	// Loads are the table's rates; none is 50%, 80%, 100% and seen.
	Loads []ReportLoadAsk
	// LoadSeen is the load to judge at; 0 is the source's. LoadSeenX
	// multiplies it; 0 is 1.
	LoadSeen, LoadSeenX float64
	// Headroom is the operating point's fraction of the fleet knee; 0 is the
	// policy's.
	Headroom float64
	// TTFT and TPOT are the SLO targets; nil is the policy's.
	TTFT, TPOT *ReportTarget
	// TTFTPercentiles are the tables' TTFT percentiles, q in (0, 1), or 0
	// for the mean.
	TTFTPercentiles []float64
	// TPOTPercentile is the tables' TPOT percentile beside the mean; 0 asks
	// for the mean alone.
	TPOTPercentile float64
	Assume         Assume
	// ArrivalCV2 is a stated Ca^2, nil for Poisson (Knobs').
	ArrivalCV2 *float64
}

// ReportShapeAsk is one request shape: Measured, or the mean prompt and
// output lengths in tokens and, when Hit is set, the prefix-cache hit rate.
type ReportShapeAsk struct {
	Measured bool
	IL, OL   float64
	Hit      *float64
	// Label is the shape as asked ("measured", "8k/200").
	Label string
}

// ReportLoadAsk is one rate of the tables: Rate requests per second, Share
// of the shape's fleet knee, or Seen, the load judged at.
type ReportLoadAsk struct {
	Rate, Share float64
	Seen        bool
	// Label is the load as asked ("5r/s", "80%", "seen").
	Label string
}

// ReportTarget is one SLO target: Seconds at Percentile q, or the mean at 0.
type ReportTarget struct {
	Seconds, Percentile float64
}

// Reporter is a Pricer that answers POST /v1/report.
type Reporter interface {
	Report(ctx context.Context, q ReportQuery) (*ReportResponse, error)
}

// The verdicts a report gives.
const (
	VerdictUnder     = "UNDER-PROVISIONED"
	VerdictOver      = "OVER-PROVISIONED"
	VerdictRight     = "RIGHT-SIZED"
	VerdictNotJudged = "NOT JUDGED"
)

// The marks a summary cell carries.
const (
	CellMet       = "met"
	CellMissed    = "missed"
	CellSaturated = "saturated"
	CellUnknown   = "unknown"
	CellNoTarget  = "no target"
)

// ReportResponse is POST /v1/report's answer, and capacity-planner
// at-replicas -o json.
type ReportResponse struct {
	SchemaVersion int          `json:"schemaVersion"`
	Question      string       `json:"question"`
	Source        ReportSource `json:"source"`
	Fleet         ReportFleet  `json:"fleet"`
	Load          ReportLoad   `json:"load"`
	SLO           ReportSLO    `json:"slo"`
	// Loads names the tables' rates, in order: a summary cell's column.
	Loads []string `json:"loads"`
	// Shapes is each shape's answer, in the order asked.
	Shapes []ReportShape `json:"shapes"`
	// NotValidated says which figures no run has validated, and by how much
	// they read off where a run measured them.
	NotValidated []string `json:"notValidated"`
	ExitCode     int      `json:"exitCode"`
}

// ReportShape is the fleet's answer at one request shape.
type ReportShape struct {
	Label         string  `json:"label"`
	InputTokens   float64 `json:"inputTokens"`
	OutputTokens  float64 `json:"outputTokens"`
	PrefixHitRate float64 `json:"prefixHitRate"`
	// Source is where the lengths came from: the measured window, or asked.
	Source string `json:"source"`
	// Extrapolated says IL or OL lies outside what the recording saw, and
	// ExtrapolatedWhy by how much.
	Extrapolated    bool          `json:"extrapolated"`
	ExtrapolatedWhy string        `json:"extrapolatedWhy,omitempty"`
	Utilization     ReportUse     `json:"utilization"`
	Capacity        ReportLimits  `json:"capacity"`
	Table           []ReportRow   `json:"table"`
	Verdict         ReportVerdict `json:"verdict"`
	// Flags is every flag a figure of the shape carries, once each.
	Flags []string `json:"flags"`
	// Assumed is every input the shape's answer assumed, in words.
	Assumed []string `json:"assumed"`
}

// ReportSource is what the report was read from.
type ReportSource struct {
	Recording   string    `json:"recording,omitempty"`
	Namespace   string    `json:"namespace"`
	Model       string    `json:"model"`
	Cycle       int       `json:"cycle"`
	TotalCycles int       `json:"totalCycles"`
	FirstAt     time.Time `json:"firstAt"`
	LastAt      time.Time `json:"lastAt"`
}

// ReportFleet is the fleet judged.
type ReportFleet struct {
	Prefill        int    `json:"prefill"`
	Decode         int    `json:"decode"`
	PrefillVariant string `json:"prefillVariant,omitempty"`
	DecodeVariant  string `json:"decodeVariant"`
	// PrefillFrom and DecodeFrom say where each count came from: "recording"
	// or "asked".
	PrefillFrom string `json:"prefillFrom"`
	DecodeFrom  string `json:"decodeFrom"`
}

// ReportLoad is the load the fleet is judged at.
type ReportLoad struct {
	// Rate is the load judged at: Seen times X.
	Rate Figure `json:"rate"`
	// Seen is the load the source saw, or the one asked in its place, and
	// Source which ("observed mean", "asked" or "none").
	Seen   *float64 `json:"seen,omitempty"`
	Source string   `json:"source"`
	X      float64  `json:"x"`
	// Cycles is how many cycles the window holds, and CyclesWithTraffic how
	// many of them read an arrival rate above 0: the mean is over those.
	Cycles            int      `json:"cycles"`
	CyclesWithTraffic int      `json:"cyclesWithTraffic"`
	Peak              *float64 `json:"peak,omitempty"`
}

// ReportUse is the utilization at the load.
type ReportUse struct {
	Decode  ReportDecodeUse  `json:"decode"`
	Prefill ReportPrefillUse `json:"prefill"`
}

// ReportDecodeUse is decode's utilization: the KV fill each replica runs at,
// the batch slots it occupies, and the share of requests that wait for one.
type ReportDecodeUse struct {
	KVFill  Figure  `json:"kvFill"`
	BandTop float64 `json:"bandTop"`
	// SlotsOccupied is the sequences resident over the fleet, Slots the
	// fleet's batch slots.
	SlotsOccupied   Figure `json:"slotsOccupied"`
	Slots           int64  `json:"slots"`
	WaitProbability Figure `json:"waitProbability"`
}

// ReportPrefillUse is prefill's utilization: the load over the prefill knee
// on the busiest pod, and over the pods pooled.
type ReportPrefillUse struct {
	Pooled    Figure  `json:"pooled"`
	WorstPod  Figure  `json:"worstPod"`
	Imbalance float64 `json:"imbalance,omitempty"`
}

// ReportLimits is the fleet's capacity.
type ReportLimits struct {
	PrefillKnee ReportRate `json:"prefillKnee"`
	DecodeKnee  ReportRate `json:"decodeKnee"`
	FleetKnee   ReportRate `json:"fleetKnee"`
	Operating   ReportRate `json:"operating"`
	// MaxAtSLO is the highest rate that holds every SLO target, with no
	// headroom: slo-to-rate's fleet rate at headroom 1.
	MaxAtSLO       ReportRate `json:"maxAtSLO"`
	Headroom       float64    `json:"headroom"`
	HeadroomSource string     `json:"headroomSource"`
}

// ReportRate is a rate, the output tokens a second it carries, and what binds
// it, in words.
type ReportRate struct {
	Figure
	OutputTokensPerSecond *float64 `json:"outputTokensPerSecond,omitempty"`
	Binds                 string   `json:"binds,omitempty"`
}

// ReportSLO is the targets the report judges by.
type ReportSLO struct {
	TTFT *ReportObjective `json:"ttft,omitempty"`
	TPOT *ReportObjective `json:"tpot,omitempty"`
}

// ReportObjective is one target: its statistic, seconds and source ("policy"
// or "asked").
type ReportObjective struct {
	Statistic string  `json:"statistic"`
	Target    float64 `json:"target"`
	Source    string  `json:"source"`
}

// ReportRow is one rate of a shape's SLO against load. E2EMeanSum is a sum of
// means, TTFT mean + (OL - 1) x TPOT mean, not a percentile.
type ReportRow struct {
	// Load is the rate as asked ("80%"), Label the row's name.
	Load           string       `json:"load"`
	Label          string       `json:"label"`
	ShareOfKnee    *float64     `json:"shareOfKnee,omitempty"`
	Rate           float64      `json:"rate"`
	TTFT           []ReportStat `json:"ttft"`
	TPOTMean       Figure       `json:"tpotMean"`
	TPOTPercentile *ReportStat  `json:"tpotPercentile,omitempty"`
	E2EMeanSum     Figure       `json:"e2eMeanSum"`
	// Cell is the row in the summary matrix.
	Cell ReportCell `json:"cell"`
}

// ReportCell is one cell of the shape-by-load summary: TTFT p90 and the
// TPOT percentile, and Mark, whether the SLO targets hold at the rate
// (CellMet, CellMissed, CellSaturated, CellUnknown, CellNoTarget).
type ReportCell struct {
	TTFT ReportStat `json:"ttft"`
	TPOT ReportStat `json:"tpot"`
	Mark string     `json:"mark"`
}

// ReportStat is a figure at a statistic.
type ReportStat struct {
	Statistic string `json:"statistic"`
	Figure
}

// ReportVerdict is the fleet judged at the load: Verdict, why in numbers,
// the roles that can lose a replica, and each fleet checked.
type ReportVerdict struct {
	Verdict string        `json:"verdict"`
	Reason  string        `json:"reason"`
	Shrink  []string      `json:"shrink"`
	Checks  []ReportCheck `json:"checks"`
}

// ReportCheck is one fleet checked at the load: whether it holds every SLO
// target with headroom, and why.
type ReportCheck struct {
	Prefill int    `json:"prefill"`
	Decode  int    `json:"decode"`
	Holds   bool   `json:"holds"`
	Known   bool   `json:"known"`
	Reason  string `json:"reason"`
}

// FigureOf is r on the wire, in unit, as every other figure the API returns
// (worseIsHigh: a refused corner of a span takes the higher side).
func FigureOf(r whatif.Result, unit string, worseIsHigh bool) Figure {
	return figureOf(r, unit, worseIsHigh)
}

// handleReport is POST /v1/report.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	var req ReportRequest
	if !readJSON(w, r, &req) {
		return
	}
	out, err := s.report(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	body, err := marshal(out)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// errNoReporter is inputs whose Pricer cannot report.
var errNoReporter = errors.New("the inputs this inputsId names cannot be reported on")

// report answers req on one of the server's plan slots, within its plan
// budget.
func (s *Server) report(ctx context.Context, req ReportRequest) (*ReportResponse, error) {
	e, ok := s.inputs.get(req.InputsID)
	if !ok {
		return nil, errUnknownInputs
	}
	rep, ok := e.pricer.(Reporter)
	if !ok {
		return nil, errNoReporter
	}
	q, err := ReportQueryOf(req)
	if err != nil {
		return nil, err
	}
	if !acquire(s.planSlots) {
		return nil, errBusy
	}
	defer func() { <-s.planSlots }()
	bctx, cancel := context.WithTimeoutCause(ctx, s.planBudget, errPlanBudget)
	defer cancel()
	out, err := rep.Report(bctx, q)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil && errors.Is(context.Cause(bctx), errPlanBudget) {
		return nil, BudgetError{Budget: s.planBudget}
	}
	if err != nil {
		return nil, err
	}
	out.SchemaVersion = SchemaVersion
	return out, nil
}

// The most shapes and loads one report prices: each shape prices its fleets
// at every load, and a fleet one fewer of each role for its verdict.
const (
	maxReportShapes     = 6
	maxReportLoads      = 8
	maxReportStatistics = 4
)

// ReportQueryOf checks req and reads it into numbers. A field asked wrongly
// is an InputError.
func ReportQueryOf(req ReportRequest) (ReportQuery, error) {
	q := ReportQuery{TTFTPercentiles: []float64{0.5, 0.9}, TPOTPercentile: 0.9}
	if k := req.Fleet; k != nil {
		for _, c := range []struct {
			name string
			n    *int
			to   *int
		}{{"fleet.prefill", k.Prefill, &q.Prefill}, {"fleet.decode", k.Decode, &q.Decode}} {
			if c.n == nil {
				continue
			}
			if *c.n < 1 {
				return q, InputErrorf("%s %d, want 1 or more", c.name, *c.n)
			}
			*c.to = *c.n
		}
	}
	if len(req.Shapes) > maxReportShapes {
		return q, InputErrorf("shapes: %d, want at most %d", len(req.Shapes), maxReportShapes)
	}
	for _, s := range req.Shapes {
		sh, err := ParseReportShape(s)
		if err != nil {
			return q, InputError{Err: fmt.Errorf("shapes: %w", err)}
		}
		q.Shapes = append(q.Shapes, sh)
	}
	if len(req.Loads) > maxReportLoads {
		return q, InputErrorf("loads: %d, want at most %d", len(req.Loads), maxReportLoads)
	}
	for _, s := range req.Loads {
		l, err := ParseReportLoad(s)
		if err != nil {
			return q, InputError{Err: fmt.Errorf("loads: %w", err)}
		}
		q.Loads = append(q.Loads, l)
	}
	for _, c := range []struct {
		name string
		x    *float64
		to   *float64
	}{{"loadSeen", req.LoadSeen, &q.LoadSeen}, {"loadSeenX", req.LoadSeenX, &q.LoadSeenX}} {
		if c.x == nil {
			continue
		}
		if !usable(*c.x) || math.IsInf(*c.x, 0) {
			return q, InputErrorf("%s %g, want a finite value above 0", c.name, *c.x)
		}
		*c.to = *c.x
	}
	if req.Headroom != nil {
		if h := *req.Headroom; !(h > 0 && h < 1) {
			return q, InputErrorf("headroom %g, want a fraction of the knee in (0, 1)", h)
		}
		q.Headroom = *req.Headroom
	}
	if o := req.SLO; o != nil {
		var err error
		if q.TTFT, err = reportTarget("slo.ttft", o.TTFT); err != nil {
			return q, err
		}
		if q.TPOT, err = reportTarget("slo.tpot", o.TPOT); err != nil {
			return q, err
		}
	}
	if len(req.TTFTStatistics) > 0 {
		if len(req.TTFTStatistics) > maxReportStatistics {
			return q, InputErrorf("ttftStatistics: %d, want at most %d", len(req.TTFTStatistics), maxReportStatistics)
		}
		q.TTFTPercentiles = nil
		for _, s := range req.TTFTStatistics {
			p, err := parseStatistic("ttftStatistics", s)
			if err != nil {
				return q, err
			}
			q.TTFTPercentiles = append(q.TTFTPercentiles, p)
		}
	}
	if req.TPOTStatistic != "" {
		p, err := parseStatistic("tpotStatistic", req.TPOTStatistic)
		if err != nil {
			return q, err
		}
		q.TPOTPercentile = p
	}
	if a := req.Assume; a != nil {
		sc := Scenario{Assume: *a, TPOT: &ResolvedObjective{Statistic: statisticName(q.TPOTPercentile)}}
		if err := sc.checkAssume(); err != nil {
			return q, err
		}
		q.Assume = *a
	}
	return q, nil
}

// ParseReportShape reads a shape: "measured", or "IL/OL[/hit]", lengths in
// tokens with an optional k (1024 tokens), and hit a rate in [0, 1).
func ParseReportShape(s string) (ReportShapeAsk, error) {
	if s == "measured" {
		return ReportShapeAsk{Measured: true, Label: s}, nil
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return ReportShapeAsk{}, fmt.Errorf("shape %q, want measured or IL/OL[/hit] (8k/200, 1k/4k/0.3)", s)
	}
	sh := ReportShapeAsk{Label: s}
	var err error
	if sh.IL, err = tokens(parts[0]); err != nil {
		return sh, fmt.Errorf("shape %q: IL: %w", s, err)
	}
	if sh.OL, err = tokens(parts[1]); err != nil {
		return sh, fmt.Errorf("shape %q: OL: %w", s, err)
	}
	if len(parts) == 3 {
		h, err := strconv.ParseFloat(parts[2], 64)
		if err != nil || !(h >= 0 && h < 1) {
			return sh, fmt.Errorf("shape %q: hit %q, want a rate from 0 to below 1", s, parts[2])
		}
		sh.Hit = &h
	}
	return sh, nil
}

// maxShapeTokens bounds a stated length: 1024k tokens.
const maxShapeTokens = 1 << 20

// tokens reads a length in tokens: a number, or one with a k suffix.
func tokens(s string) (float64, error) {
	scale := 1.0
	if strings.HasSuffix(s, "k") {
		s, scale = strings.TrimSuffix(s, "k"), 1024
	}
	x, err := strconv.ParseFloat(s, 64)
	if err != nil || !(x*scale >= 1 && x*scale <= maxShapeTokens) {
		return 0, fmt.Errorf("%q, want a length from 1 to 1024k tokens", s)
	}
	return x * scale, nil
}

// ParseReportLoad reads a load: "seen", "80%" of the knee, or "5r/s".
func ParseReportLoad(s string) (ReportLoadAsk, error) {
	switch {
	case s == "seen":
		return ReportLoadAsk{Seen: true, Label: s}, nil
	case strings.HasSuffix(s, "%"):
		x, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		if err != nil || !(x > 0 && x <= 100) {
			return ReportLoadAsk{}, fmt.Errorf("load %q, want a share of the knee above 0%% and at most 100%%", s)
		}
		return ReportLoadAsk{Share: x / 100, Label: s}, nil
	case strings.HasSuffix(s, "r/s"):
		x, err := strconv.ParseFloat(strings.TrimSuffix(s, "r/s"), 64)
		if err != nil || !usable(x) || math.IsInf(x, 0) {
			return ReportLoadAsk{}, fmt.Errorf("load %q, want a finite rate above 0", s)
		}
		return ReportLoadAsk{Rate: x, Label: s}, nil
	}
	return ReportLoadAsk{}, fmt.Errorf("load %q, want seen, a share of the knee (80%%) or a rate (5r/s)", s)
}

// reportTarget reads one SLO target; nil with none asked.
func reportTarget(field string, o *Objective) (*ReportTarget, error) {
	if o == nil {
		return nil, nil
	}
	if len(o.BucketTargets) > 0 {
		return nil, InputErrorf("%s.bucketTargets: a report judges one target per objective", field)
	}
	if o.Target == nil || !usable(*o.Target) || math.IsInf(*o.Target, 0) {
		return nil, InputErrorf("%s.target: want a finite target above 0, in seconds", field)
	}
	stat := o.Statistic
	if stat == "" {
		stat = statisticMean
	}
	p, err := parseStatistic(field+".statistic", stat)
	if err != nil {
		return nil, err
	}
	return &ReportTarget{Seconds: *o.Target, Percentile: p}, nil
}

// ReportStatisticName is q as a statistic: "mean", or "p" and the percentile.
func ReportStatisticName(q float64) string { return statisticName(q) }
