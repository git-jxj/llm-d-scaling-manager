package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// --experimental-ttft=v2 prices a backtest's TTFT with the experimental
// rank-mixture model, where --experimental-ttft (or =v1) turns on the TTFT
// model's switches (experimental_ttft.go). It is backtest's only: the
// rank-mixture model is fitted on a backtest's fit window. The model (whatif.QueueRankMixture; whatifapi.BacktestRequest.TTFTModel):
// processor sharing per prefill data-parallel rank, mixed over the ranks'
// measured traffic shares, a fitted KV-deferral wait and a per-prompt-token
// non-queue part, fitted on the backtest's fit window. Every TTFT it prices
// is flagged whatif.RankMixtureTTFT and reads OUTSIDE VALIDATED REGION.

// experimentalTTFTV2 is the flag's value that asks for the rank mixture.
const experimentalTTFTV2 = "v2"

// experimentalTTFTV2NL is the flag's value that asks for the rank mixture's
// non-linear variant (whatif.QueueRankMixtureNL): the attention service law
// and the non-queue part measured part by part. Backtest only.
const experimentalTTFTV2NL = "v2nl"

// experimentalTTFTV2NLLine is =v2nl with v2's service line, no attention
// term (whatif.QueueRankMixtureNLLine). Backtest only.
const experimentalTTFTV2NLLine = "v2nl-line"

// experimentalTTFTV2Usage is backtest's and predict's help for the flag: the
// switches' (experimentalTTFTUsage), and =v2.
const experimentalTTFTV2Usage = experimentalTTFTUsage + ". On backtest, =v2 instead prices TTFT with an experimental model: limited " +
	"processor sharing per prefill rank, mixed over the ranks' measured traffic shares, with a fitted KV-deferral wait and a cost per " +
	"prompt token, fitted on the fit window; every TTFT it prices is flagged RankMixtureTTFT and reads OUTSIDE VALIDATED REGION"

// experimentalTTFTValue is --experimental-ttft where it also takes =v2: alone,
// true or v1 it sets *switches (the TTFT model's switches), v2 sets *v2,
// false neither.
type experimentalTTFTValue struct {
	switches, v2 *bool
	// nl, where set, takes =v2nl: v2 and nl both true; line, where set,
	// takes =v2nl-line: v2, nl and line true.
	nl, line *bool
}

// String is the value.
func (v experimentalTTFTValue) String() string {
	switch {
	case v.v2 != nil && *v.v2 && v.nl != nil && *v.nl && v.line != nil && *v.line:
		return experimentalTTFTV2NLLine
	case v.v2 != nil && *v.v2 && v.nl != nil && *v.nl:
		return experimentalTTFTV2NL
	case v.v2 != nil && *v.v2:
		return experimentalTTFTV2
	case v.switches != nil && *v.switches:
		return "true"
	}
	return "false"
}

// Set reads one value.
func (v experimentalTTFTValue) Set(s string) error {
	if v.nl != nil {
		*v.nl = s == experimentalTTFTV2NL || (s == experimentalTTFTV2NLLine && v.line != nil)
	}
	if v.line != nil {
		*v.line = s == experimentalTTFTV2NLLine
	}
	switch s {
	case "true", "v1":
		*v.switches, *v.v2 = true, false
	case "false":
		*v.switches, *v.v2 = false, false
	case experimentalTTFTV2:
		*v.switches, *v.v2 = false, true
	case experimentalTTFTV2NL, experimentalTTFTV2NLLine:
		if v.nl == nil || (s == experimentalTTFTV2NLLine && v.line == nil) {
			return fmt.Errorf("--%s=%s is backtest's only", experimentalTTFTFlag, s)
		}
		*v.switches, *v.v2 = false, true
	default:
		return fmt.Errorf("want --%s, --%s=false or --%s=%s", experimentalTTFTFlag, experimentalTTFTFlag, experimentalTTFTFlag, experimentalTTFTV2)
	}
	return nil
}

// IsBoolFlag lets the flag stand without a value: alone it is true.
func (v experimentalTTFTValue) IsBoolFlag() bool { return true }

// withRankMixture is res with the recording's cycles as the rank mixture reads
// them (learnreplay.MixtureSeriesFrom), read from the file at path in a pass
// of their own.
func withRankMixture(path string, res *whatifapi.Resolved) error {
	src, err := openRecording(path, false)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	return rankMixtureFrom(src.open, res)
}

// rankMixtureFrom is withRankMixture on a source already open: a recording
// file or a captured window, as serve reads it.
func rankMixtureFrom(open learnreplay.Source, res *whatifapi.Resolved) error {
	v := learnreplay.SeriesVariants{Decode: res.Inputs.Fleet.Decode.Variant}
	if p := res.Inputs.Fleet.Prefill; p != nil {
		v.Prefill = p.Variant
	}
	cycles, err := learnreplay.MixtureSeriesFrom(open, learnreplay.StateQuery{Namespace: res.Inputs.Namespace, ModelID: res.Inputs.Model}, v)
	if err != nil {
		return err
	}
	if len(cycles) != len(res.Series) {
		return fmt.Errorf("the rank mixture read %d cycles of the recording and the series %d", len(cycles), len(res.Series))
	}
	res.Mixture = cycles
	return nil
}

// prefillSlotsOf is K, the prompts a prefill rank runs at once, as the
// recording's state gives the engine's flags: --max-num-seqs with chunked
// prefill, 1 without (a rank then serves first come); 0 where
// --max-num-seqs is not read.
func prefillSlotsOf(res *whatifapi.Resolved) int {
	s, ok := res.Pricer.(*servedState)
	if !ok || res.Inputs.Fleet.Prefill == nil {
		return 0
	}
	params := variantParams(s.st, res.Inputs.Fleet.Prefill.Variant)
	if params == nil {
		return 0
	}
	k := int(params.ExplicitMaxNumSeqs())
	if k > 0 && !params.ChunkedPrefillEnabled {
		return 1
	}
	return k
}

// rankMixtureReport prices rep's TTFT tables with the experimental
// rank-mixture model (--experimental-ttft=v2), fitted on the recording read
// again with its per-rank series, as serve's POST /v1/report does.
func (f *answerFlags) rankMixtureReport(ctx context.Context, rep *whatifapi.ReportResponse, q whatifapi.ReportQuery) error {
	cycle, err := f.cycleNumber()
	if err != nil {
		return err
	}
	src := &servedSources{warn: f.warn, recordings: filepath.Dir(f.recording)}
	res, err := src.Resolve(ctx, whatifapi.InputsRequest{Model: f.model, PrefillVariant: f.prefillVariant, DecodeVariant: f.variant,
		PrefillVPPlateau: f.vpPlateau, TTFTModel: string(whatif.QueueRankMixture),
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(f.recording), Cycle: cycle}})
	if err != nil {
		if whatifapi.IsInputError(err) {
			return usageError{err}
		}
		return err
	}
	rep.TTFTModel = whatifapi.ApplyTTFTModel(res, rep, q)
	return nil
}
