package whatifapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The backtest (POST /v1/backtest, capacity-planner backtest): the planner
// fitted on one window predicts the TTFT and TPOT of another it did not see,
// step by step, from that window's inputs alone (its rate, its lengths, the
// replicas each role ran), and each prediction is set beside what the fleet
// measured. Nothing new is modelled: each step is the fleet a report prices
// (Pricer.Fleet) at the step's rate.
//
// The test window's latency (LatencySample) is read only by measuredOf: the
// prediction of a step (predict) is given the step's inputs and the fitted
// Pricer, never the latency, so a measured figure cannot reach a predicted
// one. TestBacktest_MeasuredLatencyDoesNotReachThePrediction holds that.

// LatencySample is one cycle's measured latency, server side: the router's
// histograms where the source carries them, and the engines' own
// (learnreplay.CycleLatency). Resolved.Latency[i] is the cycle of
// Resolved.Series[i].
type LatencySample struct {
	At     time.Time
	Router *domain.RouterLatency
	// EngineTTFT is the engines' TTFT histogram (the prefill pods' on a P/D
	// fleet), EngineTTFTMean their mean; EngineTPOT the decode pods' TPOT
	// histogram and EngineITLMean their mean inter-token latency.
	EngineTTFT     []domain.LatencyBucket
	EngineTTFTMean float64
	EngineTPOT     []domain.LatencyBucket
	EngineITLMean  float64
}

// ConfigField is one setting a run's figures depend on, by name ("model",
// "decode accelerator"), and its value; "" is not recorded.
type ConfigField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// RunConfig is what the source says of the configuration the fleet ran: the
// model, each role's accelerator, parallelism and the engine flags that move
// its latency, and the router's flow-control state.
type RunConfig struct {
	Fields []ConfigField
}

// The measured sources a backtest scores against.
const (
	MeasuredAuto   = "auto"
	MeasuredRouter = "router"
	MeasuredEngine = "engine"
)

// The backtest's defaults and bounds.
const (
	DefaultBacktestStep   = 5 * time.Minute
	minBacktestStep       = time.Minute
	maxBacktestStep       = time.Hour
	DefaultBacktestSettle = 2 * time.Minute
	maxBacktestSettle     = time.Hour
	// DefaultBacktestSplit is the share of a recording fitted on when one
	// recording is split.
	DefaultBacktestSplit = 0.6
	// BacktestTolerance is the "within" share: a prediction within 20% of
	// the measured figure.
	BacktestTolerance = 0.2
	// maxBacktestSteps bounds a backtest's steps.
	maxBacktestSteps = 2000
)

// The reasons a step is not judged at all.
const (
	ExcludedNoTraffic     = "noTraffic"
	ExcludedNoMeasurement = "noMeasurement"
	ExcludedScaling       = "scaling"
	ExcludedWarmUp        = "warmUp"
	ExcludedNotPriced     = "notPriced"
)

// The reasons one figure of a judged step is not scored.
const (
	UnscoredFewRequests    = "fewRequests"
	UnscoredAboveHistogram = "aboveHistogram"
	UnscoredRefused        = "refused"
	UnscoredNotMeasured    = "notMeasured"
)

// The backtest's statistics, in order: the mean, p50, p90 and p95.
var backtestStats = []float64{0, 0.5, 0.9, 0.95}

// BacktestRequest is POST /v1/backtest's body. InputsID is the source fitted
// on. With TestInputsID the test is that source; without it InputsID's
// source is split at Split (a share of its span, "60%") or FitUntil (RFC
// 3339): the fit is the source read at the split, the test every cycle
// after it.
type BacktestRequest struct {
	InputsID     string `json:"inputsId"`
	TestInputsID string `json:"testInputsId,omitempty"`
	Split        string `json:"split,omitempty"`
	FitUntil     string `json:"fitUntil,omitempty"`
	// Step is a step's length, 1m to 1h; absent, 5m. Settle is how long
	// after a change of replicas, and after the test's first cycle, a step
	// is not judged; absent, 2m.
	Step   string `json:"step,omitempty"`
	Settle string `json:"settle,omitempty"`
	// TTFT is a TTFT target to judge each step by, predicted and measured.
	TTFT *Objective `json:"ttft,omitempty"`
	// ArrivalCV2 is the test's Ca^2, when known; absent, Poisson.
	ArrivalCV2 *float64 `json:"arrivalCV2,omitempty"`
	// Measured is the latency scored against: router, engine or auto (the
	// router's where the source carries it).
	Measured string `json:"measured,omitempty"`
	// AllowConfigMismatch prices a test whose configuration differs from
	// the fit's, with the difference as a caveat; without it, refused.
	AllowConfigMismatch bool `json:"allowConfigMismatch,omitempty"`
	// Assume types a term the fit did not learn, as a plan's assume does;
	// every figure it moves is flagged, and the answer lists it.
	Assume *Assume `json:"assume,omitempty"`
}

// BacktestResponse is POST /v1/backtest's answer, and capacity-planner
// backtest -o json.
type BacktestResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	WhatifSchema  string `json:"whatifSchema"`
	BacktestID    string `json:"backtestId"`
	Banner        string `json:"banner"`
	// Verdict is the answer in one plain sentence, and Excluded what was
	// not judged and why, in one more.
	Verdict  string         `json:"verdict"`
	Excluded string         `json:"excluded"`
	Fit      BacktestWindow `json:"fit"`
	Test     BacktestWindow `json:"test"`
	// Split is how one source was split ("60%", or the time), "" with two.
	Split    string         `json:"split,omitempty"`
	Config   BacktestConfig `json:"config"`
	Step     float64        `json:"step"`
	Settle   float64        `json:"settle"`
	Measured string         `json:"measured"`
	// MeasuredNote says what the measured figures are.
	MeasuredNote string `json:"measuredNote"`
	// Assumed is every input of the prediction the test did not give.
	Assumed []string        `json:"assumed"`
	TTFT    *ReportTarget   `json:"ttftTarget,omitempty"`
	Steps   []BacktestStep  `json:"steps"`
	Summary BacktestSummary `json:"summary"`
	Notes   []string        `json:"notes"`
}

// BacktestWindow is one side of a backtest: its inputs, and the time it
// spans.
type BacktestWindow struct {
	InputsID string    `json:"inputsId"`
	Source   string    `json:"source"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Cycles   int       `json:"cycles"`
}

// BacktestConfig is the fit's configuration against the test's: Match when
// no recorded field differs; Differences each one that does, NotCompared
// what neither source records. Allowed says a mismatch was priced anyway.
type BacktestConfig struct {
	Match       bool             `json:"match"`
	Differences []ConfigMismatch `json:"differences"`
	NotCompared []string         `json:"notCompared"`
	Allowed     bool             `json:"allowed,omitempty"`
}

// ConfigMismatch is one setting that differs.
type ConfigMismatch struct {
	Field string `json:"field"`
	Fit   string `json:"fit"`
	Test  string `json:"test"`
}

// BacktestStep is one step of the test: its inputs, whether it was judged
// (Excluded says why not), and each statistic predicted and measured.
type BacktestStep struct {
	Start    time.Time `json:"start"`
	Cycles   int       `json:"cycles"`
	Excluded string    `json:"excluded,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Rate     float64   `json:"rate"`
	Prefill  int       `json:"prefill"`
	Decode   int       `json:"decode"`
	// PromptTokens and OutputTokens are the step's mean lengths, absent
	// where no cycle read them (the fit's are priced then).
	PromptTokens *float64 `json:"promptTokens,omitempty"`
	OutputTokens *float64 `json:"outputTokens,omitempty"`
	// PromptCV is the step's prompt spread, read from its prefill pods'
	// computed-token histograms; absent where they read none, and the
	// test's whole-window spread, else the fit's, is priced.
	PromptCV *float64 `json:"promptCV,omitempty"`
	// Requests is the requests the measured histograms hold over the step.
	Requests float64 `json:"requests"`
	// KneeShare is the rate over the fit's lower knee at the step's fleet,
	// and Load its band (LoadBelow80, LoadNearKnee, LoadPastKnee), the band
	// its TTFT is scored in; DecodeKneeShare and DecodeLoad the same over
	// decode's knee, the band its TPOT is scored in. Each is absent where
	// its knee did not price.
	KneeShare       *float64        `json:"kneeShare,omitempty"`
	Load            string          `json:"load,omitempty"`
	DecodeKneeShare *float64        `json:"decodeKneeShare,omitempty"`
	DecodeLoad      string          `json:"decodeLoad,omitempty"`
	TTFT            []BacktestPoint `json:"ttft"`
	TPOT            []BacktestPoint `json:"tpot"`
	// Target is the TTFT target judged, predicted and measured: met,
	// missed or unknown.
	Target *BacktestJudged `json:"target,omitempty"`
}

// BacktestPoint is one statistic of a step: the prediction (with the
// envelope's verdict), its upper bound, the measured figure, and their
// ratio. Unscored says why the pair is not scored.
type BacktestPoint struct {
	Statistic string   `json:"statistic"`
	Predicted Figure   `json:"predicted"`
	Bound     *float64 `json:"bound,omitempty"`
	Measured  *float64 `json:"measured,omitempty"`
	Ratio     *float64 `json:"ratio,omitempty"`
	InRegion  bool     `json:"inRegion"`
	Unscored  string   `json:"unscored,omitempty"`
}

// BacktestJudged is a step judged against a TTFT target.
type BacktestJudged struct {
	Predicted string `json:"predicted"`
	Measured  string `json:"measured"`
}

// BacktestSummary is the test scored: the steps by status, and each
// statistic's score over every judged step and over those inside the
// validated region.
type BacktestSummary struct {
	Steps    int              `json:"steps"`
	Judged   int              `json:"judged"`
	Excluded BacktestExcluded `json:"excluded"`
	TTFT     []BacktestScore  `json:"ttft"`
	TPOT     []BacktestScore  `json:"tpot"`
	// Target is the TTFT target's agreement, with a target.
	Target *BacktestAgreement `json:"target,omitempty"`
	// ByLoad is the same scores over the judged steps of each load band:
	// TTFT by the step's Load, TPOT by its DecodeLoad.
	ByLoad []BacktestLoad `json:"byLoad"`
}

// The load bands a step is scored in: its rate over the fit's lower knee
// at its fleet.
const (
	LoadBelow80  = "below 0.8 of the knee"
	LoadNearKnee = "0.8 to 1.0 of the knee"
	LoadPastKnee = "past the knee"
)

// BacktestLoad is the scores over one load band's judged steps.
type BacktestLoad struct {
	Load string `json:"load"`
	// Steps is the judged steps whose TTFT falls in the band, DecodeSteps
	// those whose TPOT does.
	Steps       int             `json:"steps"`
	DecodeSteps int             `json:"decodeSteps"`
	TTFT        []BacktestScore `json:"ttft"`
	TPOT        []BacktestScore `json:"tpot"`
}

// BacktestExcluded counts the steps not judged, by reason.
type BacktestExcluded struct {
	NoTraffic     int `json:"noTraffic"`
	NoMeasurement int `json:"noMeasurement"`
	Scaling       int `json:"scaling"`
	WarmUp        int `json:"warmUp"`
	NotPriced     int `json:"notPriced"`
}

// BacktestScore is one statistic's score.
type BacktestScore struct {
	Statistic string        `json:"statistic"`
	All       BacktestStats `json:"all"`
	InRegion  BacktestStats `json:"inRegion"`
	// Unscored counts the judged steps whose pair was not scored, by
	// reason; a refused prediction by its reason too ("refused:Saturated").
	Unscored map[string]int `json:"unscored"`
	// Outside counts the scored steps outside the validated region, by the
	// envelope's first reason, its words before the first figure.
	Outside map[string]int `json:"outside"`
}

// BacktestStats is the score over a set of steps. Bias is the median of
// predicted over measured; MedianError and P90Error the median and 90th
// percentile of |ln(predicted/measured)| as a share, e^x - 1; Coverage the
// share of steps measured at or under the prediction's upper bound;
// Within the share within BacktestTolerance. Each is absent over no step.
type BacktestStats struct {
	Steps       int      `json:"steps"`
	Bias        *float64 `json:"bias,omitempty"`
	MedianError *float64 `json:"medianError,omitempty"`
	P90Error    *float64 `json:"p90Error,omitempty"`
	Coverage    *float64 `json:"coverage,omitempty"`
	Within      *float64 `json:"within,omitempty"`
}

// BacktestAgreement is how often the predicted and the measured TTFT agreed
// on the target: MissedMisses is the costly case, a step predicted to meet
// the target that measured a miss.
type BacktestAgreement struct {
	Steps        int `json:"steps"`
	BothMet      int `json:"bothMet"`
	BothMissed   int `json:"bothMissed"`
	MissedMisses int `json:"missedMisses"`
	FalseAlarms  int `json:"falseAlarms"`
	NotJudged    int `json:"notJudged"`
}

// The words a judged target reads.
const (
	judgedMet     = "met"
	judgedMissed  = "missed"
	judgedUnknown = "unknown"
)

// backtestSide is one side's inputs.
type backtestSide struct {
	e      *inputsEntry
	config RunConfig
}

// handleBacktest is POST /v1/backtest.
func (s *Server) handleBacktest(w http.ResponseWriter, r *http.Request) {
	var req BacktestRequest
	if !readJSON(w, r, &req) {
		return
	}
	out, err := s.backtest(r.Context(), req)
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

// backtest answers req on one plan slot, within the plan budget. A split
// resolves the fit at the split, as POST /v1/inputs would, and keeps it.
func (s *Server) backtest(ctx context.Context, req BacktestRequest) (*BacktestResponse, error) {
	test, ok := s.inputs.get(req.InputsID)
	if !ok {
		return nil, errUnknownInputs
	}
	fit := test
	var cutoff time.Time
	split := ""
	if req.TestInputsID != "" {
		if req.Split != "" || req.FitUntil != "" {
			return nil, InputErrorf("split and fitUntil split one source: give them without testInputsId")
		}
		if test, ok = s.inputs.get(req.TestInputsID); !ok {
			return nil, errUnknownInputs
		}
	} else {
		var cycle int
		var err error
		if cycle, cutoff, split, err = splitOf(test, req.Split, req.FitUntil); err != nil {
			return nil, err
		}
		in, err := refitRequest(test, cycle, cutoff)
		if err != nil {
			return nil, err
		}
		if !acquire(s.inputSlots) {
			return nil, errBusy
		}
		res, err := s.resolver.Resolve(ctx, in)
		<-s.inputSlots
		if err != nil {
			return nil, err
		}
		doc := res.Inputs
		doc.SchemaVersion, doc.WhatifSchema = SchemaVersion, s.whatifSchema
		doc.InputsID = contentID("inputs", res.Content)
		fit = &inputsEntry{id: doc.InputsID, doc: doc, pricer: res.Pricer, series: res.Series, latency: res.Latency,
			config: res.Config, bytes: res.Bytes}
		s.inputs.add(doc.InputsID, fit)
	}
	q, err := backtestQuestionOf(fit, test, req, cutoff, split)
	if err != nil {
		return nil, err
	}
	if !acquire(s.planSlots) {
		return nil, errBusy
	}
	defer func() { <-s.planSlots }()
	bctx, cancel := context.WithTimeoutCause(ctx, s.planBudget, errPlanBudget)
	defer cancel()
	out, err := q.run(bctx)
	if err != nil {
		if errors.Is(context.Cause(bctx), errPlanBudget) && ctx.Err() == nil {
			return nil, BudgetError{Budget: s.planBudget}
		}
		return nil, err
	}
	out.SchemaVersion, out.WhatifSchema = SchemaVersion, s.whatifSchema
	return out, nil
}

// Backtest answers req against sources resolved outside a server: the CLI's
// backtest, on the same code path as POST /v1/backtest. test is nil for a
// split: fit is then the source read at the split (cutoff), and wholeTest
// the whole source the test steps are read from.
func Backtest(ctx context.Context, fit, wholeTest *Resolved, req BacktestRequest, cutoff time.Time, split, whatifSchema string) (*BacktestResponse, error) {
	entry := func(res *Resolved) *inputsEntry {
		doc := res.Inputs
		doc.InputsID = contentID("inputs", res.Content)
		return &inputsEntry{id: doc.InputsID, doc: doc, pricer: res.Pricer, series: res.Series, latency: res.Latency, config: res.Config}
	}
	q, err := backtestQuestionOf(entry(fit), entry(wholeTest), req, cutoff, split)
	if err != nil {
		return nil, err
	}
	out, err := q.run(ctx)
	if err != nil {
		return nil, err
	}
	out.SchemaVersion, out.WhatifSchema = SchemaVersion, whatifSchema
	return out, nil
}

// SplitOf is where res splits at split (a share, "60%") or fitUntil (RFC
// 3339): the cycle to read the fit at (1-based), the time it was read, and
// the split in words.
func SplitOf(res *Resolved, split, fitUntil string) (int, time.Time, string, error) {
	return splitOf(&inputsEntry{series: res.Series}, split, fitUntil)
}

// splitOf is SplitOf of an inputs entry.
func splitOf(e *inputsEntry, split, fitUntil string) (int, time.Time, string, error) {
	n := len(e.series)
	if n < 2 {
		return 0, time.Time{}, "", InputErrorf("the source carries %d cycles: a split needs two or more (or give testInputsId)", n)
	}
	if split != "" && fitUntil != "" {
		return 0, time.Time{}, "", InputErrorf("split and fitUntil both name the split; give one")
	}
	first, last := e.series[0].At, e.series[n-1].At
	var cutoff time.Time
	words := ""
	switch {
	case fitUntil != "":
		t, err := time.Parse(time.RFC3339, fitUntil)
		if err != nil {
			return 0, time.Time{}, "", InputErrorf("fitUntil %q, want RFC 3339", fitUntil)
		}
		cutoff, words = t, "fit until "+t.UTC().Format(time.RFC3339)
	default:
		share := DefaultBacktestSplit
		if split != "" {
			v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(split), "%"), 64)
			if err != nil || !(v > 0 && v < 100) {
				return 0, time.Time{}, "", InputErrorf("split %q, want a share of the source between 0%% and 100%%, as 60%%", split)
			}
			share = v / 100
		}
		cutoff = first.Add(time.Duration(share * float64(last.Sub(first))))
		words = strconv.FormatFloat(math.Round(share*1000)/10, 'f', -1, 64) + "%"
	}
	cycle := 0
	for i, x := range e.series {
		if !x.At.After(cutoff) {
			cycle = i + 1
		}
	}
	switch {
	case cycle == 0:
		return 0, time.Time{}, "", InputErrorf("the split %s is before the source's first cycle (%s): nothing to fit on",
			cutoff.UTC().Format(time.RFC3339), first.UTC().Format(time.RFC3339))
	case cycle >= n:
		return 0, time.Time{}, "", InputErrorf("the split %s is at or after the source's last cycle (%s): nothing to test on",
			cutoff.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339))
	}
	return cycle, e.series[cycle-1].At, words, nil
}

// refitRequest is the inputs request that reads e's source at cycle, the
// fit of a split: a recording at that cycle, a Prometheus window cut at the
// cycle's time.
func refitRequest(e *inputsEntry, cycle int, cutoff time.Time) (InputsRequest, error) {
	d := e.doc
	in := InputsRequest{Model: d.Model, DecodeVariant: d.Fleet.Decode.Variant}
	if d.Fleet.Prefill != nil {
		in.PrefillVariant = d.Fleet.Prefill.Variant
	}
	switch d.Source.Kind {
	case SourceRecording:
		in.Source = &SourceRequest{Kind: SourceRecording, Name: d.Source.Name, Cycle: cycle}
	case SourcePrometheus:
		if d.Source.From == nil {
			return in, InputErrorf("the source does not say its window: split it by giving two windows")
		}
		in.Source = &SourceRequest{Kind: SourcePrometheus, Namespace: d.Namespace, From: d.Source.From.UTC().Format(time.RFC3339),
			To: cutoff.UTC().Format(time.RFC3339)}
	default:
		return in, InputErrorf("a %q source cannot be split: give testInputsId", d.Source.Kind)
	}
	return in, nil
}

// backtestQuestion is a backtest's request checked against its sides.
type backtestQuestion struct {
	fit, test   *inputsEntry
	cutoff      time.Time
	split       string
	step        time.Duration
	settle      time.Duration
	measured    string
	target      *ReportTarget
	cv2         *float64
	promptCV    *float64
	config      BacktestConfig
	assumed     []string
	testIndices []int
	assume      Assume
}

// backtestQuestionOf checks req against fit and test, and refuses a test
// whose configuration differs from the fit's unless the request allows it.
func backtestQuestionOf(fit, test *inputsEntry, req BacktestRequest, cutoff time.Time, split string) (*backtestQuestion, error) {
	q := &backtestQuestion{fit: fit, test: test, cutoff: cutoff, split: split, step: DefaultBacktestStep,
		settle: DefaultBacktestSettle, measured: MeasuredAuto, cv2: req.ArrivalCV2}
	var err error
	if req.Step != "" {
		if q.step, err = time.ParseDuration(req.Step); err != nil || q.step < minBacktestStep || q.step > maxBacktestStep {
			return nil, InputErrorf("step %q, want a duration from %s to %s", req.Step, minBacktestStep, maxBacktestStep)
		}
	}
	if req.Settle != "" {
		if q.settle, err = time.ParseDuration(req.Settle); err != nil || q.settle < 0 || q.settle > maxBacktestSettle {
			return nil, InputErrorf("settle %q, want a duration from 0s to %s", req.Settle, maxBacktestSettle)
		}
	}
	switch req.Measured {
	case "", MeasuredAuto:
	case MeasuredRouter, MeasuredEngine:
		q.measured = req.Measured
	default:
		return nil, InputErrorf("measured %q, want %s, %s or %s", req.Measured, MeasuredAuto, MeasuredRouter, MeasuredEngine)
	}
	if c := req.ArrivalCV2; c != nil && !(*c > 0 && *c <= 100) {
		return nil, InputErrorf("arrivalCV2 %g, want above 0 and at most 100", *c)
	}
	if o := req.TTFT; o != nil {
		if o.Target == nil || !(*o.Target > 0) {
			return nil, InputErrorf("ttft.target: want seconds above 0")
		}
		stat := o.Statistic
		if stat == "" {
			stat = statisticMean
		}
		pq, err := parseStatistic("ttft.statistic", stat)
		if err != nil {
			return nil, err
		}
		q.target = &ReportTarget{Seconds: *o.Target, Percentile: pq}
	}
	if len(test.series) == 0 {
		return nil, InputErrorf("the test source carries no cycles")
	}
	if len(test.latency) != len(test.series) {
		return nil, InputErrorf("the test source carries no measured latency to score against")
	}
	if fit.doc.Model != test.doc.Model {
		return nil, InputErrorf("the fit is model %q and the test model %q: a backtest compares one model", fit.doc.Model, test.doc.Model)
	}
	q.config = compareConfig(fit.config, test.config)
	if !q.config.Match {
		if !req.AllowConfigMismatch {
			return nil, InputErrorf("the test ran another configuration than the fit: %s. A backtest across configurations scores the "+
				"difference, not the planner; give allowConfigMismatch (--allow-config-mismatch) to price it anyway, with the difference as a caveat",
				mismatchWords(q.config.Differences))
		}
		q.config.Allowed = true
	}
	for i, x := range test.series {
		if q.cutoff.IsZero() || x.At.After(q.cutoff) {
			q.testIndices = append(q.testIndices, i)
		}
	}
	if len(q.testIndices) == 0 {
		return nil, InputErrorf("no test cycle after the split")
	}
	slices.SortStableFunc(q.testIndices, func(a, b int) int { return test.series[a].At.Compare(test.series[b].At) })
	span := test.series[q.testIndices[len(q.testIndices)-1]].At.Sub(test.series[q.testIndices[0]].At)
	if int(span/q.step)+1 > maxBacktestSteps {
		return nil, InputErrorf("the test spans %s: at a step of %s that is more than %d steps; ask a longer step", span, q.step, maxBacktestSteps)
	}
	// The test's prompt spread is an input: its lengths, never its latency.
	if f := test.doc.Observed.PromptCV; f.Value != nil && !slices.Contains(f.Flags, string(whatif.AssumedPromptSpread)) {
		v := *f.Value
		q.promptCV = &v
	}
	if a := req.Assume; a != nil {
		q.assume = *a
		q.assumed = append(q.assumed, "terms typed in for the fit (assume): every figure they move is flagged")
	}
	if q.cv2 == nil {
		q.assumed = append(q.assumed, "arrivals Poisson (Ca^2 1): the source does not measure burstiness; give arrivalCV2 (--arrival-cv2) when known")
	}
	return q, nil
}

// mismatchWords is the differences in words.
func mismatchWords(ds []ConfigMismatch) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = fmt.Sprintf("%s is %s in the fit and %s in the test", d.Field, d.Fit, d.Test)
	}
	return strings.Join(parts, "; ")
}

// notRecordedConfig is what no recording carries, compared never.
var notRecordedConfig = []string{"router version (not recorded)"}

// compareConfig is the fit's configuration against the test's: a field
// recorded on both sides that differs is a mismatch; one recorded on one
// side only is not compared.
func compareConfig(fit, test RunConfig) BacktestConfig {
	c := BacktestConfig{Match: true, Differences: []ConfigMismatch{}, NotCompared: slices.Clone(notRecordedConfig)}
	tv := map[string]string{}
	for _, f := range test.Fields {
		tv[f.Name] = f.Value
	}
	seen := map[string]bool{}
	for _, f := range fit.Fields {
		seen[f.Name] = true
		t, ok := tv[f.Name]
		switch {
		case !ok || f.Value == "" || t == "":
			c.NotCompared = append(c.NotCompared, f.Name+" (not recorded on both)")
		case f.Value != t:
			c.Match = false
			c.Differences = append(c.Differences, ConfigMismatch{Field: f.Name, Fit: f.Value, Test: t})
		}
	}
	for _, f := range test.Fields {
		if !seen[f.Name] {
			c.NotCompared = append(c.NotCompared, f.Name+" (not recorded on both)")
		}
	}
	return c
}

// stepOf is one step's cycles, by index into the test.
type stepOf struct {
	start time.Time
	idx   []int
}

// run prices and scores every step.
func (q *backtestQuestion) run(ctx context.Context) (*BacktestResponse, error) {
	test := q.test
	first := test.series[q.testIndices[0]].At
	last := test.series[q.testIndices[len(q.testIndices)-1]].At
	start := first.UTC().Truncate(q.step)
	steps := make([]stepOf, int(last.UTC().Truncate(q.step).Sub(start)/q.step)+1)
	for i := range steps {
		steps[i].start = start.Add(time.Duration(i) * q.step)
	}
	for _, i := range q.testIndices {
		k := int(test.series[i].At.UTC().Truncate(q.step).Sub(start) / q.step)
		steps[k].idx = append(steps[k].idx, i)
	}
	// A change of replicas: the cycle at which either role's serving pods
	// moved.
	var changes []time.Time
	for j := 1; j < len(q.testIndices); j++ {
		a, b := test.series[q.testIndices[j-1]], test.series[q.testIndices[j]]
		if a.Prefill != b.Prefill || a.Decode != b.Decode {
			changes = append(changes, b.At)
		}
	}
	source := q.measuredSource()
	cycle := sampleStep(seriesAt(test.series, q.testIndices), q.step).Seconds()
	out := &BacktestResponse{Banner: banner, Split: q.split, Config: q.config, Step: q.step.Seconds(), Settle: q.settle.Seconds(),
		Measured: source, MeasuredNote: measuredNote(source), Assumed: append([]string{}, q.assumed...), TTFT: q.target,
		Steps: []BacktestStep{}, Notes: []string{}}
	out.Fit = windowOf(q.fit, nil, q.cutoff)
	out.Test = windowOf(test, q.testIndices, time.Time{})
	p := &predictor{pricer: q.fit.pricer, fleets: map[predictKey]Fleet{}, cv2: q.cv2, promptCV: q.promptCV,
		headroom: q.fit.doc.Policy.Headroom, assume: q.assume}
	for _, st := range steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out.Steps = append(out.Steps, q.stepOf(p, st, changes, first, source, cycle))
	}
	out.Summary = summarize(out.Steps, q.target != nil)
	unread := 0
	for _, st := range out.Steps {
		if st.Excluded == "" && st.PromptCV == nil {
			unread++
		}
	}
	if unread > 0 {
		from := "the fit's"
		if q.promptCV != nil {
			from = "the test's whole window's"
		}
		out.Assumed = append(out.Assumed, fmt.Sprintf("the prompt spread of %d judged steps was not read (no computed-token histogram): %s is priced", unread, from))
	}
	out.Verdict, out.Excluded = verdictOf(out, q.step)
	out.Notes = append(out.Notes, backtestNotes(q, out)...)
	out.BacktestID = contentID("backtest", struct {
		Fit, Test    string
		Cutoff       time.Time
		Step, Settle time.Duration
		Measured     string
		Target       *ReportTarget
		CV2          *float64
	}{q.fit.id, q.test.id, q.cutoff, q.step, q.settle, source, q.target, q.cv2})
	return out, nil
}

// seriesAt is the samples at idx.
func seriesAt(series []SeriesSample, idx []int) []SeriesSample {
	out := make([]SeriesSample, len(idx))
	for i, j := range idx {
		out[i] = series[j]
	}
	return out
}

// windowOf is one side's window: its cycles at idx (all with nil), or up to
// cutoff when not zero.
func windowOf(e *inputsEntry, idx []int, cutoff time.Time) BacktestWindow {
	w := BacktestWindow{InputsID: e.id, Source: e.doc.Source.Name}
	if w.Source == "" && e.doc.Source.From != nil {
		w.Source = fmt.Sprintf("prometheus %s %s to %s", e.doc.Namespace, e.doc.Source.From.UTC().Format(time.RFC3339),
			e.doc.Source.To.UTC().Format(time.RFC3339))
	}
	var ats []time.Time
	if idx != nil {
		for _, i := range idx {
			ats = append(ats, e.series[i].At)
		}
	} else {
		for _, x := range e.series {
			if cutoff.IsZero() || !x.At.After(cutoff) {
				ats = append(ats, x.At)
			}
		}
	}
	if len(ats) > 0 {
		w.From, w.To, w.Cycles = ats[0], ats[len(ats)-1], len(ats)
	}
	return w
}

// measuredSource is the latency the test is scored against: the asked one,
// or the router's where any cycle carries its TTFT histogram.
func (q *backtestQuestion) measuredSource() string {
	if q.measured != MeasuredAuto {
		return q.measured
	}
	for _, i := range q.testIndices {
		if r := q.test.latency[i].Router; r != nil && len(r.TTFTBuckets) > 0 {
			return MeasuredRouter
		}
	}
	return MeasuredEngine
}

// measuredNote says what a measured source's figures are.
func measuredNote(source string) string {
	if source == MeasuredRouter {
		return "measured by the router (server side, the same scrape as the inputs): TTFT from the request's arrival at the router, " +
			"its queue included; TPOT per streamed request"
	}
	return "measured by the engines (vLLM histograms, the same scrape as the inputs), no router series in the source: on a P/D fleet the TTFT " +
		"is the prefill pods' alone (no router, KV transfer or decode step) and reads low against a prediction of the whole TTFT; " +
		"the TPOT mean is the decode pods' mean inter-token latency, per token, not per request"
}

// stepOf judges one step: its inputs, why it is not judged, or each
// statistic predicted and measured.
func (q *backtestQuestion) stepOf(p *predictor, st stepOf, changes []time.Time, first time.Time, source string, cycle float64) BacktestStep {
	out := BacktestStep{Start: st.start, Cycles: len(st.idx), TTFT: []BacktestPoint{}, TPOT: []BacktestPoint{}}
	if len(st.idx) == 0 {
		out.Excluded, out.Detail = ExcludedNoMeasurement, "no cycle in the step"
		return out
	}
	samples := seriesAt(q.test.series, st.idx)
	in := stepInputsOf(samples)
	out.Rate, out.Prefill, out.Decode = in.rate, in.prefill, in.decode
	if in.hasShape {
		out.PromptTokens, out.OutputTokens = ptr(in.prompt), ptr(in.output)
	}
	if in.promptCV != nil {
		out.PromptCV = ptr(*in.promptCV)
	}
	m := measuredOf(q.test.latency, st.idx, source, cycle)
	out.Requests = m.requests
	end := st.start.Add(q.step)
	switch {
	case !(in.rate > 0):
		out.Excluded, out.Detail = ExcludedNoTraffic, "no requests arrived"
		return out
	case st.start.Before(first.Add(q.settle)):
		out.Excluded, out.Detail = ExcludedWarmUp, fmt.Sprintf("within %s of the test's first cycle", q.settle)
	case slices.ContainsFunc(changes, func(c time.Time) bool { return c.Before(end) && c.After(st.start.Add(-q.settle)) }):
		out.Excluded, out.Detail = ExcludedScaling, fmt.Sprintf("the replicas changed in the step or within %s before it", q.settle)
	case !(m.requests > 0):
		out.Excluded, out.Detail = ExcludedNoMeasurement, "the source measured no request in the step"
	case in.prefill == 0 || in.decode == 0:
		out.Excluded, out.Detail = ExcludedNotPriced, fmt.Sprintf("%d prefill and %d decode pods served: a P/D fleet is priced with both", in.prefill, in.decode)
	}
	pred, err := p.predict(in)
	if err != nil {
		if out.Excluded == "" {
			out.Excluded, out.Detail = ExcludedNotPriced, err.Error()
		}
		return out
	}
	if pred.knee != nil {
		out.KneeShare = ptr(in.rate / *pred.knee)
		out.Load = loadBand(*out.KneeShare)
	}
	if pred.decodeKnee != nil {
		out.DecodeKneeShare = ptr(in.rate / *pred.decodeKnee)
		out.DecodeLoad = loadBand(*out.DecodeKneeShare)
	}
	for i, q := range backtestStats {
		out.TTFT = append(out.TTFT, pointOf(q, pred.ttft[i], pred.ttftBound[i], m.ttft[i], m.requests))
		out.TPOT = append(out.TPOT, pointOf(q, pred.tpot[i], pred.tpotBound[i], m.tpot[i], m.requests))
	}
	if q.target != nil {
		out.Target = q.judge(pred, m)
	}
	return out
}

// stepInputs is what a step gives the prediction: never its latency.
type stepInputs struct {
	rate            float64
	prefill, decode int
	prompt, output  float64
	hasShape        bool
	// promptCV is the step's own prompt spread, read from its prefill
	// pods' computed-token histograms; nil where they read none.
	promptCV *float64
}

// stepInputsOf is the step's inputs: its mean rate, the median serving pods
// of each role, and its lengths weighted by rate.
func stepInputsOf(samples []SeriesSample) stepInputs {
	in := stepInputs{rate: rateOf(samples).Mean}
	a := actualOf(samples)
	in.prefill, in.decode = a.Prefill, a.Decode
	in.prompt, in.output, _, in.hasShape = bucketShape(samples)
	pooled := map[float64]float64{}
	var computed, requests float64
	for _, x := range samples {
		if x.PromptBuckets == nil || !(x.PromptRequestRate > 0) {
			continue
		}
		computed += x.PromptComputedRate
		requests += x.PromptRequestRate
		for b, r := range x.PromptBuckets {
			pooled[b] += r
		}
	}
	if requests > 0 {
		if sp := whatif.ReadPromptSpread(pooled, computed/requests); !sp.Assumed {
			in.promptCV = ptr(math.Round(sp.CV/promptCVGrid) * promptCVGrid)
		}
	}
	return in
}

// promptCVGrid is the grid a step's prompt spread is priced on, so that
// steps of nearly one spread share a fleet.
const promptCVGrid = 0.05

// predictKey is one fleet a backtest prices.
type predictKey struct {
	np, nd         int
	prompt, output float64
	promptCV       float64
	q              float64
}

// predictor prices steps from the fit alone: the Pricer, and the inputs of
// each step. It never sees a latency.
type predictor struct {
	pricer   Pricer
	fleets   map[predictKey]Fleet
	cv2      *float64
	promptCV *float64
	// headroom is the fit's: it moves no TTFT or TPOT priced at a rate.
	headroom float64
	// assume is the request's typed terms.
	assume Assume
}

// prediction is one step's figures at each statistic of backtestStats.
type prediction struct {
	ttft, tpot           []Figure
	ttftBound, tpotBound []*float64
	// knee is the lower knee of the fleet that priced, decodeKnee decode's;
	// nil with none.
	knee, decodeKnee *float64
}

// predict prices one step at each statistic: the TTFT as a report's table
// prices it (TTFTPoint), bounded by the top of its span over V_P and f; the
// TPOT, bounded by the top of its band. Each carries the envelope's verdict.
func (p *predictor) predict(in stepInputs) (prediction, error) {
	var out prediction
	if !(in.rate > 0) || in.prefill == 0 || in.decode == 0 {
		return out, errors.New("no fleet to price")
	}
	for _, q := range backtestStats {
		k := predictKey{np: in.prefill, nd: in.decode, q: q}
		if in.promptCV != nil {
			k.promptCV = *in.promptCV
		}
		if in.hasShape {
			k.prompt, k.output = shapeKnob(in.prompt), shapeKnob(in.output)
		}
		fl, ok := p.fleets[k]
		if !ok {
			kn := Knobs{Prefill: k.np, Decode: k.nd, PromptTokens: k.prompt, OutputTokens: k.output, TTFTPercentile: q, TPOTPercentile: q,
				ArrivalCV2: p.cv2, Headroom: p.headroom}
			kn.Assume = p.assume
			if kn.Assume.PromptCV == nil {
				switch {
				case in.promptCV != nil:
					kn.Assume.PromptCV = ptr(max(*in.promptCV, promptCVGrid))
				case p.promptCV != nil:
					kn.Assume.PromptCV = p.promptCV
				}
			}
			var err error
			if fl, err = p.pricer.Fleet(kn); err != nil {
				return out, err
			}
			p.fleets[k] = fl
		}
		class := fl.Class()
		t, _ := fl.TTFTPoint(in.rate)
		tf := figureOf(t, unitSeconds, true)
		share := 0.0
		if v, ok := t.(whatif.TTFT); ok {
			k := v.PrefillKnee
			if v.DecodeKnee > 0 && (k <= 0 || v.DecodeKnee < k) {
				k = v.DecodeKnee
			}
			share = shareOf(in.rate, k)
		}
		tf = validate(tf, whatif.FigureTTFT, q, class, share)
		var tb *float64
		if tf.Value != nil {
			b := *tf.Value
			if span := figureOf(fl.TTFTAt(in.rate), unitSeconds, true); span.Sensitivity != nil && span.Sensitivity.Hi != nil {
				b = max(b, *span.Sensitivity.Hi)
			}
			tb = &b
		}
		mean, pct := fl.TPOTAt(in.rate)
		r, kind := mean, whatif.FigureMeanTPOT
		if q != 0 {
			r, kind = pct, whatif.FigurePercentileTPOT
		}
		pf := figureOf(r, unitSeconds, true)
		kneeShare := 0.0
		if v, ok := valueOf(fl.DecodeKnee()); ok {
			kneeShare = shareOf(in.rate, v)
		}
		pf = validate(pf, kind, q, class, kneeShare)
		var pb *float64
		if pf.Value != nil {
			b := *pf.Value
			if pf.Band != nil {
				b = max(b, pf.Band.Hi)
			}
			pb = &b
		}
		if out.knee == nil {
			var ks []float64
			for _, r := range []whatif.Result{fl.PrefillKnee(), fl.DecodeKnee()} {
				if v, ok := valueOf(r); ok && v > 0 {
					ks = append(ks, v)
				}
			}
			if len(ks) > 0 {
				out.knee = ptr(slices.Min(ks))
			}
			if v, ok := valueOf(fl.DecodeKnee()); ok && v > 0 {
				out.decodeKnee = ptr(v)
			}
		}
		out.ttft, out.ttftBound = append(out.ttft, tf), append(out.ttftBound, tb)
		out.tpot, out.tpotBound = append(out.tpot, pf), append(out.tpotBound, pb)
	}
	return out, nil
}

// measured is one step's measured figures at each statistic, NaN where the
// histogram cannot say (+Inf: above its top bucket), and the requests it
// holds.
type measured struct {
	ttft, tpot []*float64
	requests   float64
}

// measuredOf is the step's latency from the cycles at idx: each bucket's
// rate averaged over the cycles that carry the histogram, the mean from the
// sum and count rates (the engines' weighted means). requests is the
// TTFT histogram's total rate over the step's cycles.
func measuredOf(lat []LatencySample, idx []int, source string, cycle float64) measured {
	var ttftB, tpotB [][]domain.LatencyBucket
	var ttftSum, ttftCount, tpotSum, tpotCount float64
	var ttftMeans, itlMeans []float64
	for _, i := range idx {
		l := lat[i]
		if source == MeasuredRouter {
			r := l.Router
			if r == nil {
				continue
			}
			if len(r.TTFTBuckets) > 0 {
				ttftB = append(ttftB, r.TTFTBuckets)
			}
			if len(r.TPOTBuckets) > 0 {
				tpotB = append(tpotB, r.TPOTBuckets)
			}
			ttftSum, ttftCount = ttftSum+r.TTFTSumRate, ttftCount+r.TTFTCountRate
			tpotSum, tpotCount = tpotSum+r.TPOTSumRate, tpotCount+r.TPOTCountRate
			continue
		}
		if len(l.EngineTTFT) > 0 {
			ttftB = append(ttftB, l.EngineTTFT)
		}
		if len(l.EngineTPOT) > 0 {
			tpotB = append(tpotB, l.EngineTPOT)
		}
		if l.EngineTTFTMean > 0 {
			ttftMeans = append(ttftMeans, l.EngineTTFTMean)
		}
		if l.EngineITLMean > 0 {
			itlMeans = append(itlMeans, l.EngineITLMean)
		}
	}
	m := measured{}
	th, tok := averaged(ttftB)
	ph, pok := averaged(tpotB)
	if tok {
		m.requests = th.total * cycle * float64(len(idx))
	}
	for _, q := range backtestStats {
		var t, p *float64
		switch {
		case q == 0 && source == MeasuredRouter:
			if ttftCount > 0 {
				t = ptr(ttftSum / ttftCount)
			}
			if tpotCount > 0 {
				p = ptr(tpotSum / tpotCount)
			}
		case q == 0:
			if len(ttftMeans) > 0 {
				t = ptr(meanOf(ttftMeans))
			}
			if len(itlMeans) > 0 {
				p = ptr(meanOf(itlMeans))
			}
		default:
			if tok {
				t = ptr(th.quantile(q))
			}
			if pok {
				p = ptr(ph.quantile(q))
			}
		}
		m.ttft, m.tpot = append(m.ttft, t), append(m.tpot, p)
	}
	return m
}

// meanOf is the mean of xs.
func meanOf(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// latencyHist is a cumulative latency histogram, repaired to be monotone,
// ending in +Inf.
type latencyHist struct {
	buckets []domain.LatencyBucket
	total   float64
}

// averaged is the per-bound mean of the histograms hs, repaired as
// Prometheus's histogram_quantile repairs one; ok false with no +Inf bucket
// or no request.
func averaged(hs [][]domain.LatencyBucket) (latencyHist, bool) {
	if len(hs) == 0 {
		return latencyHist{}, false
	}
	sum := map[float64]float64{}
	for _, h := range hs {
		for _, b := range h {
			if math.IsNaN(b.UpperBound) || !whatif.Finite(b.Rate) || b.Rate < 0 {
				continue
			}
			sum[b.UpperBound] += b.Rate
		}
	}
	bs := make([]domain.LatencyBucket, 0, len(sum))
	for b, r := range sum {
		bs = append(bs, domain.LatencyBucket{UpperBound: b, Rate: r / float64(len(hs))})
	}
	slices.SortFunc(bs, func(x, y domain.LatencyBucket) int { return cmp.Compare(x.UpperBound, y.UpperBound) })
	if len(bs) == 0 || !math.IsInf(bs[len(bs)-1].UpperBound, 1) {
		return latencyHist{}, false
	}
	for i := 1; i < len(bs); i++ {
		bs[i].Rate = max(bs[i].Rate, bs[i-1].Rate)
	}
	total := bs[len(bs)-1].Rate
	if !(total > 0) {
		return latencyHist{}, false
	}
	return latencyHist{buckets: bs, total: total}, true
}

// quantile is the q-quantile by linear interpolation inside its bucket, as
// histogram_quantile, but +Inf in the +Inf bucket: the top bound would read
// the tail low.
func (h latencyHist) quantile(q float64) float64 {
	rank := q * h.total
	for i, b := range h.buckets {
		if b.Rate < rank {
			continue
		}
		if math.IsInf(b.UpperBound, 1) {
			return math.Inf(1)
		}
		lower, below := 0.0, 0.0
		if i > 0 {
			lower, below = h.buckets[i-1].UpperBound, h.buckets[i-1].Rate
		}
		if b.Rate == below {
			return b.UpperBound
		}
		return lower + (b.UpperBound-lower)*(rank-below)/(b.Rate-below)
	}
	return math.Inf(1)
}

// minRequestsFor is the fewest requests a step's measured statistic needs:
// 50 for the mean, else ten beyond the percentile, max(50, 10/(1-q)), as the
// slo analyzer judges a window.
func minRequestsFor(q float64) float64 {
	if q == 0 {
		return 50
	}
	return max(50, math.Round(10/(1-q)))
}

// pointOf is one statistic predicted and measured, scored where both are
// figures.
func pointOf(q float64, pred Figure, bound, meas *float64, requests float64) BacktestPoint {
	p := BacktestPoint{Statistic: statisticName(q), Predicted: pred, Bound: bound,
		InRegion: pred.Validation != nil && pred.Validation.Validated}
	switch {
	case meas == nil:
		p.Unscored = UnscoredNotMeasured
		return p
	case math.IsInf(*meas, 1) && pred.Value != nil:
		p.Unscored = UnscoredAboveHistogram
		return p
	case math.IsInf(*meas, 1):
		p.Unscored = UnscoredRefused
		return p
	}
	p.Measured = meas
	switch {
	case pred.Value == nil:
		p.Unscored = UnscoredRefused
	case requests < minRequestsFor(q):
		p.Unscored = UnscoredFewRequests
	case *meas > 0 && *pred.Value > 0:
		p.Ratio = ptr(*pred.Value / *meas)
	default:
		p.Unscored = UnscoredNotMeasured
	}
	return p
}

// judge is the step against the TTFT target, predicted and measured.
func (q *backtestQuestion) judge(pred prediction, m measured) *BacktestJudged {
	i := slices.Index(backtestStats, q.target.Percentile)
	j := &BacktestJudged{Predicted: judgedUnknown, Measured: judgedUnknown}
	read := func(v *float64) string {
		switch {
		case v == nil:
			return judgedUnknown
		case *v <= q.target.Seconds:
			return judgedMet
		}
		return judgedMissed
	}
	if i < 0 {
		return j
	}
	j.Predicted = read(pred.ttft[i].Value)
	if m.requests >= minRequestsFor(q.target.Percentile) {
		j.Measured = read(m.ttft[i])
	}
	return j
}

// summarize scores the steps.
func summarize(steps []BacktestStep, target bool) BacktestSummary {
	s := BacktestSummary{Steps: len(steps), TTFT: []BacktestScore{}, TPOT: []BacktestScore{}}
	for _, st := range steps {
		switch st.Excluded {
		case "":
			s.Judged++
		case ExcludedNoTraffic:
			s.Excluded.NoTraffic++
		case ExcludedNoMeasurement:
			s.Excluded.NoMeasurement++
		case ExcludedScaling:
			s.Excluded.Scaling++
		case ExcludedWarmUp:
			s.Excluded.WarmUp++
		case ExcludedNotPriced:
			s.Excluded.NotPriced++
		}
	}
	s.TTFT, s.TPOT = scoresOf(steps)
	s.ByLoad = []BacktestLoad{}
	for _, band := range []string{LoadBelow80, LoadNearKnee, LoadPastKnee} {
		var in, dec []BacktestStep
		for _, st := range steps {
			if st.Excluded != "" {
				continue
			}
			if st.Load == band {
				in = append(in, st)
			}
			if st.DecodeLoad == band {
				dec = append(dec, st)
			}
		}
		l := BacktestLoad{Load: band, Steps: len(in), DecodeSteps: len(dec)}
		l.TTFT, _ = scoresOf(in)
		_, l.TPOT = scoresOf(dec)
		s.ByLoad = append(s.ByLoad, l)
	}
	if target {
		a := &BacktestAgreement{}
		for _, st := range steps {
			if st.Excluded != "" || st.Target == nil {
				continue
			}
			a.Steps++
			switch {
			case st.Target.Predicted == judgedUnknown || st.Target.Measured == judgedUnknown:
				a.NotJudged++
			case st.Target.Predicted == judgedMet && st.Target.Measured == judgedMet:
				a.BothMet++
			case st.Target.Predicted == judgedMissed && st.Target.Measured == judgedMissed:
				a.BothMissed++
			case st.Target.Predicted == judgedMet:
				a.MissedMisses++
			default:
				a.FalseAlarms++
			}
		}
		s.Target = a
	}
	return s
}

// loadBand is the band of a rate at share of the knee.
func loadBand(share float64) string {
	switch {
	case share < 0.8:
		return LoadBelow80
	case share < 1:
		return LoadNearKnee
	}
	return LoadPastKnee
}

// scoresOf is each statistic's TTFT and TPOT score over the judged steps.
func scoresOf(steps []BacktestStep) (ttft, tpot []BacktestScore) {
	ttft, tpot = []BacktestScore{}, []BacktestScore{}
	for i, q := range backtestStats {
		ttft = append(ttft, scoreOf(q, steps, func(st BacktestStep) []BacktestPoint { return st.TTFT }, i))
		tpot = append(tpot, scoreOf(q, steps, func(st BacktestStep) []BacktestPoint { return st.TPOT }, i))
	}
	return ttft, tpot
}

// scoreOf is statistic i's score over the judged steps.
func scoreOf(q float64, steps []BacktestStep, points func(BacktestStep) []BacktestPoint, i int) BacktestScore {
	sc := BacktestScore{Statistic: statisticName(q), Unscored: map[string]int{}, Outside: map[string]int{}}
	var all, in []BacktestPoint
	for _, st := range steps {
		ps := points(st)
		if st.Excluded != "" || i >= len(ps) {
			continue
		}
		p := ps[i]
		if p.Ratio == nil {
			sc.Unscored[p.Unscored]++
			if r := p.Predicted.Refused; r != nil {
				sc.Unscored[UnscoredRefused+":"+r.Reason]++
			}
			continue
		}
		all = append(all, p)
		switch {
		case p.InRegion:
			in = append(in, p)
		case p.Predicted.Validation != nil:
			sc.Outside[outsideKey(p.Predicted.Validation.Outside)]++
		}
	}
	sc.All, sc.InRegion = statsOf(all), statsOf(in)
	return sc
}

// outsideKey is an envelope's reason for a figure outside it, cut to its
// words before the first figure and the first clause.
func outsideKey(why string) string {
	why, _, _ = strings.Cut(why, ",")
	if i := strings.IndexAny(why, "0123456789"); i > 0 {
		why = why[:i]
	}
	return strings.TrimSpace(why)
}

// statsOf is the score over ps, each with a ratio.
func statsOf(ps []BacktestPoint) BacktestStats {
	s := BacktestStats{Steps: len(ps)}
	if len(ps) == 0 {
		return s
	}
	ratios := make([]float64, len(ps))
	errs := make([]float64, len(ps))
	covered, within := 0, 0
	for i, p := range ps {
		ratios[i] = *p.Ratio
		errs[i] = math.Abs(math.Log(*p.Ratio))
		if p.Bound != nil && *p.Measured <= *p.Bound {
			covered++
		}
		if math.Abs(*p.Ratio-1) <= BacktestTolerance {
			within++
		}
	}
	n := float64(len(ps))
	s.Bias = ptr(median(ratios))
	s.MedianError = ptr(math.Expm1(median(errs)))
	s.P90Error = ptr(math.Expm1(nearestRank(errs, 0.9)))
	s.Coverage, s.Within = ptr(float64(covered)/n), ptr(float64(within)/n)
	return s
}

// median is the median of xs, the mean of the middle two of an even count.
func median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// nearestRank is the nearest-rank q-quantile of xs.
func nearestRank(xs []float64, q float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[max(int(math.Ceil(q*float64(len(s))))-1, 0)]
}

// biasWords is a median ratio in words.
func biasWords(r float64) string {
	switch {
	case r > 1.2:
		return "it reads high, on the safe side"
	case r > 1.05:
		return "it reads slightly high"
	case r >= 0.95:
		return "on target"
	case r >= 1/1.2:
		return "it reads slightly low"
	}
	return "it reads low, the unsafe side"
}

// durationWords is d in hours or minutes.
func durationWords(d time.Duration) string {
	if d >= 90*time.Minute {
		return strconv.FormatFloat(math.Round(d.Hours()*10)/10, 'f', -1, 64) + " h"
	}
	return strconv.Itoa(int(math.Round(d.Minutes()))) + " min"
}

// stepWords is the step in words, "minutes" or "5-minute steps".
func stepWords(step time.Duration) string {
	if step == time.Minute {
		return "minutes"
	}
	return strconv.Itoa(int(step.Minutes())) + "-minute steps"
}

// verdictOf is the answer in one sentence, and what was not judged in one
// more. It says only what the judged steps say, and names the region.
func verdictOf(out *BacktestResponse, step time.Duration) (string, string) {
	s := out.Summary
	span := out.Test.To.Sub(out.Test.From) + step/time.Duration(max(out.Test.Cycles, 1))
	unit := stepWords(step)
	part := func(name string, scores []BacktestScore) string {
		var sc *BacktestScore
		for _, stat := range []string{"p90", "p50", statisticMean} {
			i := slices.IndexFunc(scores, func(x BacktestScore) bool { return x.Statistic == stat })
			if i >= 0 && scores[i].All.Steps > 0 {
				sc = &scores[i]
				break
			}
		}
		if sc == nil {
			var why []string
			if i := slices.IndexFunc(scores, func(x BacktestScore) bool { return x.Statistic == "p90" }); i >= 0 {
				for k, n := range scores[i].Unscored {
					if r, ok := strings.CutPrefix(k, UnscoredRefused+":"); ok {
						why = append(why, fmt.Sprintf("%s in %d", r, n))
					}
				}
			}
			slices.Sort(why)
			if len(why) == 0 {
				return fmt.Sprintf("%s: not scored in any step", name)
			}
			return fmt.Sprintf("%s: no figure in any judged step (refused: %s)", name, strings.Join(why, ", "))
		}
		a := sc.All
		w := fmt.Sprintf("%s %s was within %.0f%% of what the fleet measured in %.0f%% of the %d %s it priced (median ratio %.2f, %s)",
			sc.Statistic, name, BacktestTolerance*100, *a.Within*100, a.Steps, unit, *a.Bias, biasWords(*a.Bias))
		if n := sc.Unscored[UnscoredRefused+":"+string(whatif.Saturated)]; n > 0 {
			w += fmt.Sprintf(", and gave no figure for %d it judged past the knee", n)
		}
		return w
	}
	region := 0
	for _, sc := range s.TTFT {
		if sc.Statistic == "p90" {
			region = sc.InRegion.Steps
		}
	}
	verdict := fmt.Sprintf("Over the %s it did not see, the planner's %s; %s. %d of the judged %s were inside the planner's validated region "+
		"(the envelope of measured runs).", durationWords(span), part("TTFT", s.TTFT), part("TPOT", s.TPOT), region, unit)
	if s.Judged == 0 {
		verdict = fmt.Sprintf("Over the %s it did not see, no step could be judged: see what was not judged.", durationWords(span))
	}
	e := s.Excluded
	var parts []string
	for _, x := range []struct {
		n    int
		what string
	}{{e.WarmUp, "warm-up"}, {e.Scaling, "replicas changing"}, {e.NoTraffic, "no traffic"}, {e.NoMeasurement, "no measured requests"},
		{e.NotPriced, "not priced"}} {
		if x.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", x.n, x.what))
		}
	}
	excluded := fmt.Sprintf("Judged %d of %d %s", s.Judged, s.Steps, unit)
	if len(parts) > 0 {
		excluded += "; not judged: " + strings.Join(parts, ", ")
	}
	excluded += "."
	return verdict, excluded
}

// backtestNotes is what the answer rests on, in words.
func backtestNotes(q *backtestQuestion, out *BacktestResponse) []string {
	notes := []string{
		"Fitted on the fit window alone; each step of the test is priced from its own inputs (rate, prompt and output lengths, " +
			"the replicas each role served) and never from its measured latency.",
		fmt.Sprintf("Each step's rate is the mean of its cycles' rates; its measured figures average the cycles' histograms, each over the "+
			"source's rate window, so a step's measurement reaches back that window before the step. A percentile is scored with %0.f requests "+
			"or more at p50, %0.f at p90 and %0.f at p95 (50 for the mean).", minRequestsFor(0.5), minRequestsFor(0.9), minRequestsFor(0.95)),
		fmt.Sprintf("Bias is the median of predicted over measured; error the median and p90 of |ln ratio| as a share; coverage the share of "+
			"steps measured at or under the prediction's upper bound (the top of its span over V_P and f for TTFT, of its band for TPOT); "+
			"within, the share inside %.0f%%.", BacktestTolerance*100),
		"In region: the prediction is inside the validated envelope (docs/guides/capacity-planner/accuracy.md); all: every judged step.",
	}
	if q.config.Allowed {
		notes = append(notes, "CONFIGURATION DIFFERS, priced anyway: "+mismatchWords(q.config.Differences)+". The score includes that difference.")
	}
	return notes
}
