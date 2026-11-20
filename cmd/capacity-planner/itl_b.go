package main

import (
	"fmt"
	"math"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The decode line's intercept B, stated where the recording measured none
// (issue llm-d-incubation/llm-d-scaling-manager#225). A pinned-B line on the
// default intercept (itl.DefaultBaselineSec) is refused as PinnedDefaultB:
// its slope was fitted against a B no engine was seen to have. On a real P/D
// window that is every cycle, because the tracker learns B only from a full
// fit. --assume-itl-b states B, and the slope is fitted again through it on
// the same readings (itl.Refit), so the line still runs through what the
// replicas read and only its extrapolation moves. Every figure on it is
// flagged AssumedITLB and outside the validated region (flagsOutside).

// assumedITLB is a decode figure priced on a line whose intercept B the
// caller stated (--assume-itl-b, the API's assume.itlB or
// assume.itlBFromFloor), its slope refitted through it.
const assumedITLB whatif.Flag = "AssumedITLB"

// itlBFloor is the --assume-itl-b value that takes B from the floor read
// below the band.
const itlBFloor = "floor"

// maxAssumedITLB is the largest B --assume-itl-b takes: a token every second
// with the cache empty is no engine's floor, and a typo (12s for 12ms).
const maxAssumedITLB = time.Second

// Where an assumed B came from (interceptAssumption.Source).
const (
	// itlBStated is a duration the caller typed.
	itlBStated = "stated"
	// itlBFromFloor is the intercept of the line through the floor below the
	// band and the readings' mean (itl.FloorIntercept).
	itlBFromFloor = "floor"
)

// interceptAssumption is what --assume-itl-b did to one decode variant's
// line.
type interceptAssumption struct {
	// Asked says the flag was set; Source is how (itlBStated, itlBFromFloor).
	Asked  bool
	Source string
	// Applied says the line was refitted through Seconds; Why, when it was
	// not, says why in words.
	Applied bool
	Seconds float64
	Why     string
	// FloorSeconds and FloorFill are the floor below the band and its fill,
	// for a B from the floor (0 with none).
	FloorSeconds, FloorFill float64
	// DefaultA is the slope the line had on the default intercept.
	DefaultA float64
}

// parseAssumeITLB reads --assume-itl-b: "floor", or a duration above 0 and at
// most maxAssumedITLB. Empty is not set.
func parseAssumeITLB(s string) (floor bool, b time.Duration, err error) {
	if s == itlBFloor {
		return true, 0, nil
	}
	d, perr := time.ParseDuration(s)
	if perr != nil || d <= 0 || d > maxAssumedITLB {
		return false, 0, usagef("--assume-itl-b %q, want floor or a duration above 0 and at most %s (12ms)", s, maxAssumedITLB)
	}
	return false, d, nil
}

// assumeIntercept states the decode line's intercept B as --assume-itl-b
// asks, on a line on the default intercept only (whatif.OnDefaultB): the
// slope is fitted again through it on the line's own readings, with the
// tracker's choice of fit (itl.Refit), and pv.state carries the new line,
// its readings and B as the baseline the bands refit through. A line whose B
// was measured is priced as it is; a held or borrowed line has no readings
// here to refit, and stays refused. pv.itlB says which, and why.
func (f *answerFlags) assumeIntercept(pv *pricedVariant) {
	if f.assumeITLB == "" {
		return
	}
	a := &pv.itlB
	a.Asked = true
	floor, stated, err := parseAssumeITLB(f.assumeITLB)
	if err != nil {
		a.Why = err.Error()
		return
	}
	vs := &pv.state
	a.Source, a.Seconds = itlBStated, stated.Seconds()
	if floor {
		a.Source, a.Seconds = itlBFromFloor, 0
	}
	switch {
	case vs.Line.IsZero():
		a.Why = "no ITL line to state B for"
		return
	case !whatif.OnDefaultB(vs.Tier, vs.Line):
		a.Why = fmt.Sprintf("the %s line's B, %s, was measured", vs.Tier, ms(vs.Line.B))
		return
	case vs.Tier != whatif.TierPinnedB || !vs.Refitted || len(vs.LineReadings) == 0:
		a.Why = fmt.Sprintf("a %s line has no readings of its own at this cycle to refit through a stated B; "+
			"ask at a cycle where the line is pinned-B", vs.Tier)
		return
	}
	if floor {
		a.FloorSeconds, a.FloorFill = vs.BelowBandITL, vs.BelowBandFill
		b, why := floorB(*vs)
		if why != "" {
			a.Why = why
			return
		}
		a.Seconds = b
	}
	got := itl.Refit(vs.Readings.All, vs.Readings.Steady, a.Seconds, vs.MinSettled, vs.OneSided)
	if got.Tier != whatif.TierPinnedB || !itl.ValidModel(got.Model.A, got.Model.B) ||
		math.Abs(got.Model.B-a.Seconds) > 1e-12 {
		a.Why = fmt.Sprintf("no slope fits the line's readings through B %s: it is at or above what they read", ms(a.Seconds))
		return
	}
	a.Applied, a.DefaultA = true, vs.Line.A
	vs.Line, vs.LineReadings, vs.Baseline = got.Model, got.Read, a.Seconds
}

// floorB is the intercept of the line through the floor read below the band
// and the mean of the line's readings (itl.FloorIntercept), or why there is
// none.
func floorB(vs learnreplay.VariantState) (float64, string) {
	if !(vs.BelowBandITL > 0) {
		return 0, fmt.Sprintf("no floor below the band: fewer than %d steady readings under the band's bottom", itl.BelowBandMinReadings)
	}
	b, ok := itl.FloorIntercept(vs.LineReadings, itl.Observation{K: vs.BelowBandFill, ITLSec: vs.BelowBandITL})
	if !ok {
		return 0, fmt.Sprintf("the floor %s at fill %.2f gives no intercept with the line's readings "+
			"(their mean fill under twice the floor's, or B outside (0, the floor])", ms(vs.BelowBandITL), vs.BelowBandFill)
	}
	return b, ""
}

// ms is seconds as milliseconds, in words.
func ms(s float64) string { return fmt.Sprintf("%.1f ms", s*1000) }

// itlBWords is the variant's assumed B as the answer's assumed list names
// it, in the words of the question's source; empty when none was asked.
func (pv pricedVariant) itlBWords() string {
	a := pv.itlB
	switch {
	case !a.Asked:
		return ""
	case !a.Applied && pv.api:
		return "ITL B not assumed (assume.itlB): " + a.Why
	case !a.Applied:
		return "ITL B not assumed (--assume-itl-b): " + a.Why
	case a.Source == itlBFromFloor:
		return fmt.Sprintf("ITL B %s, from the floor read below the band (%s at fill %.2f), in place of the 6 ms default; "+
			"the slope refitted through it on the same readings", ms(a.Seconds), ms(a.FloorSeconds), a.FloorFill)
	}
	return fmt.Sprintf("ITL B %s, stated, in place of the 6 ms default; the slope refitted through it on the same readings",
		ms(a.Seconds))
}

// pinnedDefaultBHint is what a PinnedDefaultB refusal adds: how to state B,
// in the words of the question's source, or why the stated B was not used.
func (pv pricedVariant) pinnedDefaultBHint() string {
	if pv.itlB.Asked {
		return "; " + pv.itlBWords()
	}
	floor := ""
	if pv.state.Tier == whatif.TierPinnedB && pv.state.Refitted {
		if b, why := floorB(pv.state); why == "" {
			floor = fmt.Sprintf(" (here %s, through the floor of %s at fill %.2f)", ms(b), ms(pv.state.BelowBandITL), pv.state.BelowBandFill)
		}
	}
	if pv.api {
		return "; state B for a flagged answer: assume.itlBFromFloor true takes it from the floor below the band" + floor +
			", or assume.itlB in seconds (on the page, Assume a term, ITL B)"
	}
	return "; state B for a flagged answer: --assume-itl-b floor takes it from the floor below the band" + floor +
		", or --assume-itl-b 12ms states one"
}
