package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// backtestFlags are backtest's flags.
type backtestFlags struct {
	fit, test, recording, split, fitUntil string
	model, decodeVariant, prefillVariant  string
	step, settle, ttft, measured, output  string
	arrivalCV2, assumeVP                  float64
	assumeA                               time.Duration
	allowMismatch                         bool
}

// runBacktest is `capacity-planner backtest`: the planner fitted on one
// window predicts another it did not see, step by step from that window's
// inputs alone, beside what the fleet measured there (POST /v1/backtest).
func runBacktest(ctx context.Context, args []string, out, warn io.Writer) error {
	fs := newFlags("backtest", out)
	var f backtestFlags
	fs.StringVar(&f.recording, "recording", "", "one recording to split: fit on its first part, test on the rest (with --split or --fit-until)")
	fs.StringVar(&f.split, "split", "", "with --recording, the share of its span to fit on, as 60%; default 60%")
	fs.StringVar(&f.fitUntil, "fit-until", "", "with --recording, fit on the cycles up to this time (RFC 3339), test on the rest")
	fs.StringVar(&f.fit, "fit", "", "the recording to fit on (with --test)")
	fs.StringVar(&f.test, "test", "", "the recording to test on (with --fit): its inputs are priced, its latency is what it is scored against")
	fs.StringVar(&f.model, "model", "", "the model; empty takes the source's only model")
	fs.StringVar(&f.prefillVariant, "prefill-variant", "", "the prefill variant, when the model has several")
	fs.StringVar(&f.decodeVariant, "variant", "", "the decode variant, when the model has several")
	fs.StringVar(&f.step, "step", whatifapi.DefaultBacktestStep.String(), "the length of a step, 1m to 1h, each judged on its own")
	fs.StringVar(&f.settle, "settle", whatifapi.DefaultBacktestSettle.String(), "how long after a change of replicas, and after the test's start, a step is not judged")
	fs.StringVar(&f.ttft, "ttft-target", "", "a TTFT target to judge each step by, predicted and measured: 500ms or 500ms@p90")
	fs.Float64Var(&f.arrivalCV2, "arrival-cv2", 0, "the test's arrivals' Ca^2, when known; default Poisson (1), flagged")
	fs.StringVar(&f.measured, "measured", whatifapi.MeasuredAuto, "the latency scored against: auto (the router's where recorded), router or engine")
	fs.Float64Var(&f.assumeVP, "assume-vp", 0, "V_P, a prefill pod's prompt tokens per second, where the fit learned none (flagged)")
	fs.DurationVar(&f.assumeA, "assume-a", 0, "a, the fixed latency on a prefill pod, where the fit learned none (flagged)")
	fs.BoolVar(&f.allowMismatch, "allow-config-mismatch", false, "price a test whose configuration differs from the fit's, with the difference as a caveat")
	fs.StringVar(&f.output, "o", outputText, "output: text or json")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	if f.output != outputText && f.output != outputJSON {
		return usagef("-o %q, want text or json", f.output)
	}
	ctrllog.SetLogger(logr.Discard())
	req, err := f.request()
	if err != nil {
		return err
	}
	var bt *whatifapi.BacktestResponse
	switch {
	case f.recording != "" && (f.fit != "" || f.test != ""):
		return usagef("--recording splits one recording; --fit and --test name two: give one way")
	case f.recording != "":
		whole, err := f.resolve(ctx, f.recording, 0, warn)
		if err != nil {
			return err
		}
		cycle, cutoff, words, err := whatifapi.SplitOf(whole, f.split, f.fitUntil)
		if err != nil {
			return usageError{err}
		}
		fit, err := f.resolve(ctx, f.recording, cycle, warn)
		if err != nil {
			return err
		}
		bt, err = whatifapi.Backtest(ctx, fit, whole, req, cutoff, words, "schema-v"+strconv.Itoa(schemaVersion))
		if err != nil {
			return backtestError(err)
		}
	case f.fit != "" && f.test != "":
		if f.split != "" || f.fitUntil != "" {
			return usagef("--split and --fit-until split one --recording; with --fit and --test leave them out")
		}
		fit, err := f.resolve(ctx, f.fit, 0, warn)
		if err != nil {
			return err
		}
		test, err := f.resolve(ctx, f.test, 0, warn)
		if err != nil {
			return err
		}
		bt, err = whatifapi.Backtest(ctx, fit, test, req, time.Time{}, "", "schema-v"+strconv.Itoa(schemaVersion))
		if err != nil {
			return backtestError(err)
		}
	default:
		return usagef("give --recording (split one recording) or --fit and --test (two)")
	}
	if f.output == outputJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		err = enc.Encode(bt)
	} else {
		err = writeBacktestText(out, bt)
	}
	if err != nil {
		return err
	}
	if bt.Summary.Judged == 0 {
		return exitStatus(3)
	}
	return nil
}

// backtestError is err as the CLI exits with it: a request the inputs
// refuse (a configuration mismatch among them) is a usage error.
func backtestError(err error) error {
	if whatifapi.IsInputError(err) {
		return usageError{err}
	}
	return err
}

// request is the backtest the flags ask.
func (f *backtestFlags) request() (whatifapi.BacktestRequest, error) {
	req := whatifapi.BacktestRequest{Step: f.step, Settle: f.settle, Measured: f.measured, AllowConfigMismatch: f.allowMismatch}
	if f.arrivalCV2 != 0 {
		req.ArrivalCV2 = &f.arrivalCV2
	}
	if f.assumeVP > 0 || f.assumeA > 0 {
		req.Assume = &whatifapi.Assume{}
		if f.assumeVP > 0 {
			req.Assume.VP = &f.assumeVP
		}
		if f.assumeA > 0 {
			req.Assume.A = ptrTo(f.assumeA.Seconds())
		}
	}
	o, err := objectiveOf("ttft-target", f.ttft)
	if err != nil {
		return req, err
	}
	req.TTFT = o
	return req, nil
}

// resolveBacktest resolves one recording at cycle (0 the last): the
// served path, which tests replace with one that replays each once.
var resolveBacktest = func(ctx context.Context, f *backtestFlags, path string, cycle int, warn io.Writer) (*whatifapi.Resolved, error) {
	src := &servedSources{warn: warn, recordings: filepath.Dir(path)}
	in := whatifapi.InputsRequest{Model: f.model, PrefillVariant: f.prefillVariant, DecodeVariant: f.decodeVariant,
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(path), Cycle: cycle}}
	res, err := src.Resolve(ctx, in)
	if err != nil && whatifapi.IsInputError(err) {
		return nil, usageError{err}
	}
	return res, err
}

// resolve is resolveBacktest.
func (f *backtestFlags) resolve(ctx context.Context, path string, cycle int, warn io.Writer) (*whatifapi.Resolved, error) {
	return resolveBacktest(ctx, f, path, cycle, warn)
}

// seriesAndLatencyOf is seriesOf, and each cycle's measured latency beside
// it, read in the same pass.
func seriesAndLatencyOf(src learnreplay.Source, st *learnreplay.State, v learnreplay.SeriesVariants) ([]whatifapi.SeriesSample, []whatifapi.LatencySample, error) {
	cycles, lat, err := learnreplay.SeriesWithLatencyFrom(src, learnreplay.StateQuery{Namespace: st.Namespace, ModelID: st.ModelID}, v)
	if err != nil {
		return nil, nil, err
	}
	out := make([]whatifapi.SeriesSample, len(cycles))
	for i, c := range cycles {
		out[i] = whatifapi.SeriesSample{At: c.At, Rate: c.ArrivalRate, RateKnown: c.RateKnown, Prefill: c.PrefillPods, Decode: c.DecodePods,
			PrefillGPUs: c.PrefillGPUs, DecodeGPUs: c.DecodeGPUs, PromptTokens: c.PromptTokens, OutputTokens: c.OutputTokens,
			PrefixHitRate: c.PrefixHitRate, HasShape: c.HasShape, OtherPods: c.OtherPods,
			PromptComputedRate: c.PromptComputedRate, PromptRequestRate: c.PromptRequestRate, PromptBuckets: c.PromptBuckets}
	}
	ls := make([]whatifapi.LatencySample, len(lat))
	for i, l := range lat {
		ls[i] = whatifapi.LatencySample{At: l.At, Router: l.Router, EngineTTFT: l.EngineTTFT, EngineTTFTMean: l.EngineTTFTMean,
			EngineTPOT: l.EngineTPOT, EngineITLMean: l.EngineITLMean}
	}
	return out, ls, nil
}

// runConfigOf is what the state says of the configuration the fleet ran:
// the model, the flow-control state the fleet was priced at, and each
// priced role's accelerator, parallelism, GPUs and the engine flags that
// move its latency or its capacity. A value the state does not carry is "".
func runConfigOf(st *learnreplay.State, decodeVariant, prefillVariant string) whatifapi.RunConfig {
	c := whatifapi.RunConfig{}
	add := func(name, value string) { c.Fields = append(c.Fields, whatifapi.ConfigField{Name: name, Value: value}) }
	add("model", st.ModelID)
	// The router's flow-control queue series, the one rule the planner
	// infers the gate from (domain.AnalyzerInput.FlowControlSeries).
	fc := "absent (off)"
	if st.FlowControlSeries {
		fc = "present (on)"
	}
	add("flow-control series", fc)
	role := func(name, variant string) {
		for _, v := range st.Variants {
			if v.Variant != variant {
				continue
			}
			add(name+" accelerator", v.Accelerator)
			add(name+" GPUs per replica", intWords(v.GPUsPerReplica))
			add(name+" tensor parallel", intWords(v.TensorParallel))
			p := v.Params
			if p == nil {
				return
			}
			add(name+" engine", string(p.Engine))
			if w := engineOf(p); w != nil {
				add(name+" vLLM release", w.VLLMVersion)
			}
			add(name+" weights dtype", p.WeightDtype)
			add(name+" quantization", orNone(p.Quantization))
			add(name+" KV cache dtype", p.KvCacheDtype)
			add(name+" max-num-seqs", strconv.FormatInt(p.MaxNumSeqs, 10))
			add(name+" max-num-batched-tokens", strconv.FormatInt(p.EffectiveMaxBatchedTokens, 10))
			add(name+" max-model-len", strconv.FormatInt(p.MaxModelLen, 10))
			add(name+" block size", strconv.FormatInt(p.BlockSize, 10))
			add(name+" gpu-memory-utilization", strconv.FormatFloat(p.GpuMemoryUtilization, 'f', -1, 64))
			add(name+" enforce-eager", strconv.FormatBool(p.EnforceEager))
			return
		}
	}
	role("decode", decodeVariant)
	if prefillVariant != "" {
		role("prefill", prefillVariant)
	}
	return c
}

// intWords is n, or "" for 0 (not stated).
func intWords(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// orNone is s, or "none" for "".
func orNone(s string) string {
	if s == "" {
		return statNone
	}
	return s
}

// btMs is seconds as ms, or "-".
func btMs(x *float64) string {
	if x == nil {
		return "-"
	}
	if math.IsInf(*x, 1) {
		return ">top"
	}
	if *x >= 10 {
		return fmt.Sprintf("%.1fs", *x)
	}
	return fmt.Sprintf("%.0fms", *x*1000)
}

// pointText is a step's statistic, predicted/measured, with * where the
// prediction is inside the validated region.
func pointText(ps []whatifapi.BacktestPoint, stat string) string {
	for _, p := range ps {
		if p.Statistic != stat {
			continue
		}
		mark := ""
		if p.InRegion {
			mark = "*"
		}
		return btMs(p.Predicted.Value) + "/" + btMs(p.Measured) + mark
	}
	return "-"
}

// shareText is a share as a percentage, or "-".
func shareText(x *float64) string {
	if x == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *x*100)
}

// ratioText is a ratio, or "-".
func ratioText(x *float64) string {
	if x == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *x)
}

// writeBacktestText is the backtest's human output: the verdict first, what
// was not judged, the windows and the configuration, the steps, the scores
// and the notes.
func writeBacktestText(out io.Writer, bt *whatifapi.BacktestResponse) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n\n", bt.Verdict, bt.Excluded)
	at := func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") }
	fmt.Fprintf(&b, "fit:  %s, %s to %s UTC, %d cycles\n", bt.Fit.Source, at(bt.Fit.From), at(bt.Fit.To), bt.Fit.Cycles)
	fmt.Fprintf(&b, "test: %s, %s to %s UTC, %d cycles", bt.Test.Source, at(bt.Test.From), at(bt.Test.To), bt.Test.Cycles)
	if bt.Split != "" {
		fmt.Fprintf(&b, " (one recording split: %s)", bt.Split)
	}
	b.WriteString("\n")
	switch {
	case bt.Config.Match:
		fmt.Fprintf(&b, "configuration: the same in every field both record (not compared: %s)\n", strings.Join(bt.Config.NotCompared, ", "))
	default:
		b.WriteString("CONFIGURATION DIFFERS (priced anyway, --allow-config-mismatch):\n")
		for _, d := range bt.Config.Differences {
			fmt.Fprintf(&b, "  %s: fit %s, test %s\n", d.Field, d.Fit, d.Test)
		}
	}
	fmt.Fprintf(&b, "measured: %s\n", bt.MeasuredNote)
	for _, a := range bt.Assumed {
		fmt.Fprintf(&b, "assumed: %s\n", a)
	}
	if t := bt.TTFT; t != nil {
		fmt.Fprintf(&b, "TTFT target: %s %s\n", whatifapi.ReportStatisticName(t.Percentile), fmtSeconds(t.Seconds))
	}
	b.WriteString("\neach step: predicted/measured (* inside the validated region)\n")
	const layout = "%-16s %7s %5s  %-17s %-17s %-17s %-17s %s\n"
	fmt.Fprintf(&b, layout, "step (UTC)", "rate", "P+D", "TTFT p50", "TTFT p90", "TPOT p50", "TPOT p90", "")
	for _, s := range bt.Steps {
		status := "judged"
		if s.Excluded != "" {
			status = "not judged: " + s.Excluded
			if s.Excluded == whatifapi.ExcludedNotPriced {
				status += " (" + s.Detail + ")"
			}
		}
		if s.Target != nil && s.Excluded == "" {
			status += fmt.Sprintf(" (target: predicted %s, measured %s)", s.Target.Predicted, s.Target.Measured)
		}
		fmt.Fprintf(&b, layout, at(s.Start), fmt.Sprintf("%.2f", s.Rate), fmt.Sprintf("%d+%d", s.Prefill, s.Decode),
			pointText(s.TTFT, "p50"), pointText(s.TTFT, "p90"), pointText(s.TPOT, "p50"), pointText(s.TPOT, "p90"), status)
	}
	b.WriteString("\nscore over the judged steps: all | inside the validated region\n")
	const sl = "%-10s %6s %6s %8s %8s %9s %7s | %6s %6s %7s\n"
	fmt.Fprintf(&b, sl, "", "steps", "bias", "med err", "p90 err", "coverage", "within", "steps", "bias", "within")
	for _, part := range []struct {
		name   string
		scores []whatifapi.BacktestScore
	}{{"TTFT", bt.Summary.TTFT}, {"TPOT", bt.Summary.TPOT}} {
		for _, sc := range part.scores {
			a, r := sc.All, sc.InRegion
			fmt.Fprintf(&b, sl, part.name+" "+sc.Statistic, strconv.Itoa(a.Steps), ratioText(a.Bias), shareText(a.MedianError),
				shareText(a.P90Error), shareText(a.Coverage), shareText(a.Within), strconv.Itoa(r.Steps), ratioText(r.Bias), shareText(r.Within))
		}
	}
	b.WriteString("\nby load (the step's rate over the fit's knee at its fleet: TTFT by the lower knee, TPOT by decode's), judged steps: steps, bias, within\n")
	for _, l := range bt.Summary.ByLoad {
		fmt.Fprintf(&b, "%-24s %3d+%-3d steps", l.Load, l.Steps, l.DecodeSteps)
		for _, part := range []struct {
			name   string
			scores []whatifapi.BacktestScore
		}{{"TTFT", l.TTFT}, {"TPOT", l.TPOT}} {
			for _, sc := range part.scores {
				if sc.Statistic == "mean" || sc.Statistic == "p90" {
					fmt.Fprintf(&b, "  %s %s %d %s %s", part.name, sc.Statistic, sc.All.Steps, ratioText(sc.All.Bias), shareText(sc.All.Within))
				}
			}
		}
		b.WriteString("\n")
	}
	if t := bt.Summary.Target; t != nil {
		fmt.Fprintf(&b, "\nTTFT target over %d judged steps: both met %d, both missed %d, predicted met but measured missed %d, "+
			"predicted missed but measured met %d, not judged %d\n", t.Steps, t.BothMet, t.BothMissed, t.MissedMisses, t.FalseAlarms, t.NotJudged)
	}
	b.WriteString("\n")
	for _, n := range bt.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	fmt.Fprintf(&b, "%s\n", bt.Banner)
	_, err := io.WriteString(out, b.String())
	return err
}
