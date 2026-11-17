package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// defaultDayWindow is day-plan's Prometheus window when --from and --to are
// left out: the last day, ending now.
const defaultDayWindow = 24 * time.Hour

// seriesOf is every cycle of st's model in the recording data, as the API's
// day plan reads them.
func seriesOf(data []byte, st *learnreplay.State) ([]whatifapi.SeriesSample, error) {
	cycles, err := learnreplay.Series(data, learnreplay.StateQuery{Namespace: st.Namespace, ModelID: st.ModelID})
	if err != nil {
		return nil, err
	}
	out := make([]whatifapi.SeriesSample, len(cycles))
	for i, c := range cycles {
		out[i] = whatifapi.SeriesSample{At: c.At, Rate: c.ArrivalRate, Prefill: c.PrefillPods, Decode: c.DecodePods,
			PrefillGPUs: c.PrefillGPUs, DecodeGPUs: c.DecodeGPUs, PromptTokens: c.PromptTokens, OutputTokens: c.OutputTokens,
			PrefixHitRate: c.PrefixHitRate, HasShape: c.HasShape}
	}
	return out, nil
}

// dayPlanFlags are day-plan's flags.
type dayPlanFlags struct {
	window                                   captureFlags
	recording, model, prefillVariant, output string
	decodeVariant, ttft, tpot, bucket        string
	sizeFor                                  string
	minRequests, headroom                    float64
	maxPrefill, maxDecode                    int
	assumeVP, assumePromptCV                 float64
	assumeA, assumeRouterHop                 time.Duration
	inputTokens, outputTokens                float64
}

// runDayPlan is `capacity-planner day-plan`: the prefill and decode replicas
// each bucket of a window needed, against the replicas the fleet had, priced
// as POST /v1/dayplan prices them.
func runDayPlan(ctx context.Context, args []string, out, warn io.Writer) error {
	fs := newFlags("day-plan", out)
	var f dayPlanFlags
	f.window.registerWindow(fs)
	fs.Lookup("prometheus").Usage = "plan from a window of this Prometheus (base URL), captured and generated in memory, in place of --recording"
	fs.Lookup("from").Usage = "with --prometheus, the window's start (RFC 3339 or Unix seconds); default a day before --to"
	fs.Lookup("to").Usage = "with --prometheus, the window's end (RFC 3339 or Unix seconds); default now"
	fs.Lookup("step").Usage = "with --prometheus, the interval between captured cycles (not the bucket: see --bucket)"
	fs.StringVar(&f.window.namespace, "namespace", "", "the model's namespace (with --prometheus, the namespace to capture: required)")
	fs.IntVar(&f.window.batchCap, "batch-cap", 0, "with --prometheus, the engine's batch cap the window is captured with")
	fs.StringVar(&f.recording, "recording", "", "a learner recording (.jsonl or .jsonl.gz); this or --prometheus is required")
	fs.StringVar(&f.model, "model", "", "the model; empty takes the source's only model")
	fs.StringVar(&f.prefillVariant, "prefill-variant", "", "the prefill variant, when the model has several")
	fs.StringVar(&f.decodeVariant, "variant", "", "the decode variant, when the model has several")
	fs.StringVar(&f.ttft, "ttft", "", "the TTFT target: a mean (500ms) or a percentile (500ms@p95); default the policy's")
	fs.StringVar(&f.tpot, "tpot", "", "the TPOT target: a mean (4ms) or a percentile (4ms@p90), or none; default the policy's")
	fs.Float64Var(&f.headroom, "headroom", 0, "the operating point as a fraction of the knee; default the policy's")
	fs.StringVar(&f.bucket, "bucket", whatifapi.DefaultDayPlanStep.String(), "the length of a bucket, each sized on its own")
	fs.StringVar(&f.sizeFor, "size-for", "", "the rate of a bucket to size for: mean, p95 or max of its cycles; default p95 when the buckets hold 10 cycles or more, else mean")
	fs.Float64Var(&f.minRequests, "min-requests", whatifapi.DefaultMinRequests, "the fewest requests a bucket must hold to be sized")
	fs.IntVar(&f.maxPrefill, "max-prefill", 0, "the most prefill replicas a bucket is sized to; 0 is 32")
	fs.IntVar(&f.maxDecode, "max-decode", 0, "the most decode replicas a bucket is sized to; 0 is 32")
	fs.Float64Var(&f.assumeVP, "assume-vp", 0, "V_P, a prefill pod's prompt tokens per second, in place of the learned one (flagged)")
	fs.DurationVar(&f.assumeA, "assume-a", 0, "a, the fixed latency on a prefill pod, in place of the learned one (flagged)")
	fs.Float64Var(&f.assumePromptCV, "assume-prompt-cv", 0, "the prompt's coefficient of variation, in place of the measured one (flagged)")
	fs.DurationVar(&f.assumeRouterHop, "assume-router-hop", 0, "the router hop, in place of the learned one (flagged)")
	fs.Float64Var(&f.inputTokens, "assume-input-tokens", 0, "price every bucket at this mean prompt, in place of each bucket's own (flagged; EXTRAPOLATED outside the observed)")
	fs.Float64Var(&f.outputTokens, "assume-output-tokens", 0, "price every bucket at this mean output, in place of each bucket's own (flagged; EXTRAPOLATED outside the observed)")
	fs.StringVar(&f.output, "o", outputText, "output: text or json")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	if f.output != outputText && f.output != outputJSON {
		return usagef("-o %q, want text or json", f.output)
	}
	// The analyzers the replay runs log through controller-runtime, which
	// the CLI never shows.
	ctrllog.SetLogger(logr.Discard())
	res, err := f.resolve(ctx, warn)
	if err != nil {
		return err
	}
	req, err := f.request()
	if err != nil {
		return err
	}
	plan, err := whatifapi.DayPlan(ctx, res, req)
	if err != nil {
		if whatifapi.IsInputError(err) {
			return usageError{err}
		}
		return err
	}
	if f.output == outputJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(plan)
	}
	return writeDayPlanText(out, res.Inputs, plan)
}

// resolve reads the source as serve resolves it: a recording by its file,
// or a Prometheus window, the last day by default.
func (f *dayPlanFlags) resolve(ctx context.Context, warn io.Writer) (*whatifapi.Resolved, error) {
	in := whatifapi.InputsRequest{Model: f.model, PrefillVariant: f.prefillVariant, DecodeVariant: f.decodeVariant}
	src := &servedSources{warn: warn}
	switch {
	case f.recording != "" && f.window.prometheus != "":
		return nil, usagef("--recording and --prometheus both name the source; give one")
	case f.recording != "":
		src.recordings = filepath.Dir(f.recording)
		in.Source = &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(f.recording)}
	case f.window.prometheus != "":
		if f.window.namespace == "" {
			return nil, usagef("--prometheus needs --namespace")
		}
		src.window, src.allowed = f.window, []string{f.window.namespace}
		from, to := f.window.from, f.window.to
		switch {
		case (from == "") != (to == ""):
			return nil, usagef("--from and --to: give both, or neither for the last day")
		case from == "":
			end := time.Now().UTC().Truncate(time.Second)
			from, to = end.Add(-defaultDayWindow).Format(time.RFC3339), end.Format(time.RFC3339)
		}
		in.Source = &whatifapi.SourceRequest{Kind: whatifapi.SourcePrometheus, Namespace: f.window.namespace, From: from, To: to}
	default:
		return nil, usagef("--recording or --prometheus is required")
	}
	res, err := src.Resolve(ctx, in)
	if err != nil && whatifapi.IsInputError(err) {
		return nil, usageError{err}
	}
	return res, err
}

// request is the day plan the flags ask.
func (f *dayPlanFlags) request() (whatifapi.DayPlanRequest, error) {
	req := whatifapi.DayPlanRequest{Step: f.bucket, SizeFor: f.sizeFor, MinRequests: &f.minRequests}
	slo := &whatifapi.SLORequest{}
	var err error
	if slo.TTFT, err = objectiveOf("ttft", f.ttft); err != nil {
		return req, err
	}
	if slo.TPOT, err = objectiveOf("tpot", f.tpot); err != nil {
		return req, err
	}
	if slo.TTFT != nil || slo.TPOT != nil {
		req.SLO = slo
	}
	if f.headroom != 0 {
		req.Headroom = &f.headroom
	}
	if f.maxPrefill != 0 || f.maxDecode != 0 {
		req.MaxReplicas = &whatifapi.FleetKnob{}
		if f.maxPrefill != 0 {
			req.MaxReplicas.Prefill = &f.maxPrefill
		}
		if f.maxDecode != 0 {
			req.MaxReplicas.Decode = &f.maxDecode
		}
	}
	a := whatifapi.Assume{}
	typed := false
	if f.assumeVP > 0 {
		a.VP, typed = &f.assumeVP, true
	}
	if f.assumeA > 0 {
		a.A, typed = ptrTo(f.assumeA.Seconds()), true
	}
	if f.assumePromptCV > 0 {
		a.PromptCV, typed = &f.assumePromptCV, true
	}
	if f.assumeRouterHop > 0 {
		a.RouterHop, typed = ptrTo(f.assumeRouterHop.Seconds()), true
	}
	if typed {
		req.Assume = &a
	}
	if f.inputTokens > 0 || f.outputTokens > 0 {
		req.Traffic = &whatifapi.Traffic{Mix: &whatifapi.Mix{PromptTokens: f.inputTokens, OutputTokens: f.outputTokens}}
	}
	return req, nil
}

// ptrTo is a pointer to x.
func ptrTo(x float64) *float64 { return &x }

// objectiveOf reads --ttft or --tpot: a duration, and optionally "@pNN";
// "none" (TPOT only) asks no target. Empty is the policy's.
func objectiveOf(name, s string) (*whatifapi.Objective, error) {
	if s == "" {
		return nil, nil
	}
	if s == "none" && name == "tpot" {
		return &whatifapi.Objective{Statistic: "none"}, nil
	}
	value, stat, hasStat := strings.Cut(s, "@")
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return nil, usagef("--%s %q: want a duration, as 500ms or 500ms@p95", name, s)
	}
	o := &whatifapi.Objective{Statistic: "mean", Target: ptrTo(d.Seconds())}
	if hasStat {
		o.Statistic = stat
	}
	return o, nil
}

// writeDayPlanText is the day plan's human output: the hours, then the
// summary and the notes.
func writeDayPlanText(out io.Writer, in whatifapi.InputsResponse, p *whatifapi.DayPlanResponse) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Day plan for %s/%s, %s to %s UTC, buckets of %s sized for the %s rate (%s)\n",
		in.Namespace, in.Model, p.From.Format("2006-01-02 15:04"), p.To.Format("2006-01-02 15:04"),
		time.Duration(p.Step*float64(time.Second)), p.SizeFor, p.SizeForSource)
	fmt.Fprintf(&b, "SLO: TTFT %s %s", p.TTFT.Statistic, fmtSeconds(p.TTFT.Target))
	if p.TPOT != nil {
		fmt.Fprintf(&b, ", TPOT %s %s", p.TPOT.Statistic, fmtSeconds(p.TPOT.Target))
	}
	fmt.Fprintf(&b, "; headroom %.2f. %s\n\n", p.Headroom, p.Banner)
	const layout = "%-17s %9s %9s %6s %6s %6s %6s  %s\n"
	fmt.Fprintf(&b, layout, "hour (UTC)", "rate mean", "rate p95", "need P", "need D", "had P", "had D", "")
	for _, h := range p.Hours {
		mean, p95 := "-", "-"
		if h.Rate != nil {
			mean, p95 = fmt.Sprintf("%.2f", h.Rate.Mean), fmt.Sprintf("%.2f", h.Rate.P95)
		}
		np, nd, ap, ad := "-", "-", "-", "-"
		if h.Needed != nil {
			np, nd = strconv.Itoa(h.Needed.Prefill), strconv.Itoa(h.Needed.Decode)
		}
		if h.Actual != nil {
			ap, ad = strconv.Itoa(h.Actual.Prefill), strconv.Itoa(h.Actual.Decode)
		}
		verdict := map[string]string{whatifapi.VerdictUnder: "UNDER (SLO risk)", whatifapi.VerdictOver: "over (waste)",
			whatifapi.VerdictMatch: "ok", "": "not sized"}[h.Verdict]
		fmt.Fprintf(&b, layout, h.Start.Format("2006-01-02 15:04"), mean, p95, np, nd, ap, ad, verdict)
	}
	s := p.Summary
	c := s.Counts
	fmt.Fprintf(&b, "\nbuckets: %d sized (%d under, %d over, %d match), %d unmet, %d too little data, %d no data\n",
		c.Sized, c.Under, c.Over, c.Match, c.Unmet, c.TooLittleData, c.NoData)
	if s.PeakNeeded != nil {
		fmt.Fprintf(&b, "needed: peak %d prefill + %d decode, min %d prefill + %d decode\n",
			s.PeakNeeded.Prefill, s.PeakNeeded.Decode, s.MinNeeded.Prefill, s.MinNeeded.Decode)
	}
	fmt.Fprintf(&b, "replica-hours (sized buckets): needed %.2f prefill + %.2f decode, had %.2f prefill + %.2f decode\n",
		s.NeededReplicaHours.Prefill, s.NeededReplicaHours.Decode, s.ActualReplicaHours.Prefill, s.ActualReplicaHours.Decode)
	if s.NeededGPUHours != nil {
		fmt.Fprintf(&b, "GPU-hours: needed %.2f, used %.2f (GPUs per replica: %d prefill, %d decode)\n",
			*s.NeededGPUHours, *s.ActualGPUHours, s.GPUsPerReplica.Prefill, s.GPUsPerReplica.Decode)
	}
	fmt.Fprintf(&b, "hours under (SLO risk): %s\n", hourList(s.UnderHours))
	fmt.Fprintf(&b, "hours over (waste): %s\n", hourList(s.OverHours))
	if flags := dayFlags(p); len(flags) > 0 {
		fmt.Fprintf(&b, "flags: %s\n", strings.Join(flags, ", "))
	}
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// fmtSeconds is a target in ms.
func fmtSeconds(x float64) string { return fmt.Sprintf("%gms", x*1000) }

// hourList is hours as HH:MM, or none.
func hourList(hs []time.Time) string {
	if len(hs) == 0 {
		return "none"
	}
	s := make([]string, len(hs))
	for i, h := range hs {
		s[i] = h.Format("15:04")
	}
	return strings.Join(s, ", ")
}

// dayFlags is every flag of the plan's buckets once, in order met, with
// Extrapolated written EXTRAPOLATED.
func dayFlags(p *whatifapi.DayPlanResponse) []string {
	var out []string
	seen := map[string]bool{}
	for _, b := range p.Buckets {
		for _, fl := range b.Flags {
			if seen[fl] {
				continue
			}
			seen[fl] = true
			if fl == whatifapi.FlagExtrapolated {
				fl = "EXTRAPOLATED"
			}
			out = append(out, fl)
		}
	}
	return out
}
