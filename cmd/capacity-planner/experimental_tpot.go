package main

import (
	"fmt"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// --experimental-tpot=v2 prices a backtest's TPOT percentiles with the
// experimental per-request TPOT distribution (whatif.TPOTRenewalLognormal;
// whatifapi.BacktestRequest.TPOTModel): lognormal around the planner's mean
// TPOT, its log-variance the decode groups' spread plus a renewal term over
// the step's output lengths, fitted on the backtest's fit window by
// interval-censored likelihood over the router's TPOT buckets. It is
// backtest's only. Every percentile it prices is flagged
// whatif.TPOTDistribution and reads OUTSIDE VALIDATED REGION.

// experimentalTPOTFlag is the flag's name.
const experimentalTPOTFlag = "experimental-tpot"

// experimentalTPOTV2 is the flag's one value.
const experimentalTPOTV2 = "v2"

// experimentalTPOTUsage is backtest's help for the flag.
const experimentalTPOTUsage = "=v2 prices the TPOT percentiles with an experimental per-request TPOT distribution: lognormal around the " +
	"planner's mean TPOT, its spread the decode groups' plus the renewal of speculative decode steps over the step's output lengths, " +
	"fitted on the fit window by interval-censored likelihood over the router's TPOT buckets and scored at the histogram's resolution; " +
	"every percentile it prices is flagged TPOTDistribution and reads OUTSIDE VALIDATED REGION"

// tpotDistribution asks req for the experimental TPOT distribution where
// --experimental-tpot=v2 is given, and reads each side's decode cycles for
// it: test from testPath, fit from fitPath.
func (f *backtestFlags) tpotDistribution(req *whatifapi.BacktestRequest, testPath string, test *whatifapi.Resolved, fitPath string, fit *whatifapi.Resolved) error {
	switch f.experimentalTPOT {
	case "":
		return nil
	case experimentalTPOTV2:
	default:
		return usagef("--%s=%q, want --%s=%s", experimentalTPOTFlag, f.experimentalTPOT, experimentalTPOTFlag, experimentalTPOTV2)
	}
	req.TPOTModel = whatif.TPOTRenewalLognormal
	req.DecodeSlots, req.SpecTokens = decodeEngineOf(fit)
	if err := withDecodeSeries(testPath, test); err != nil {
		return err
	}
	return withDecodeSeries(fitPath, fit)
}

// withDecodeSeries is res with the recording's cycles as the TPOT
// distribution reads them (learnreplay.DecodeSeriesFrom), read from the file
// at path in a pass of their own.
func withDecodeSeries(path string, res *whatifapi.Resolved) error {
	src, err := openRecording(path, false)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	v := learnreplay.SeriesVariants{Decode: res.Inputs.Fleet.Decode.Variant}
	if p := res.Inputs.Fleet.Prefill; p != nil {
		v.Prefill = p.Variant
	}
	cycles, err := learnreplay.DecodeSeriesFrom(src.open, learnreplay.StateQuery{Namespace: res.Inputs.Namespace, ModelID: res.Inputs.Model}, v)
	if err != nil {
		return err
	}
	if len(cycles) != len(res.Series) {
		return fmt.Errorf("the decode series read %d cycles of the recording and the series %d", len(cycles), len(res.Series))
	}
	res.Decode = cycles
	return nil
}

// decodeEngineOf is the decode engine's max-num-seqs (0 not read) and
// num_speculative_tokens (0 none or not read), as the recording's state
// gives its flags.
func decodeEngineOf(res *whatifapi.Resolved) (slots, spec int) {
	s, ok := res.Pricer.(*servedState)
	if !ok {
		return 0, 0
	}
	params := variantParams(s.st, res.Inputs.Fleet.Decode.Variant)
	if params == nil {
		return 0, 0
	}
	return int(params.ExplicitMaxNumSeqs()), params.SpeculativeTokens()
}
