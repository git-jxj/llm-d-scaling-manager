package whatifapi

import (
	"fmt"
	"slices"
	"strings"
)

// ExitUntrusted is the report's exit code when it answered, and the
// source's self-check found the figures a verdict rests on inconsistent
// with the recording: the verdict is printed, and is not to be sized from.
const ExitUntrusted = 4

// The words a verdict's Trust opens with.
const (
	TrustNot        = "not trusted"
	TrustNotCovered = "not covered by the self-check"
)

// restsOn is the figures a verdict rests on, as SelfCheck.Untrusted names
// them: the TPOT figures always (the decode knee and line), and the TTFT
// figures where the prefill side priced or a TTFT target is judged.
func restsOn(v ReportVerdict, slo ReportSLO) []string {
	out := []string{}
	if v.Scope != ScopeDecodeOnly || slo.TTFT != nil {
		out = append(out, "ttft")
	}
	return append(out, "tpot")
}

// JudgeTrust sets each shape's verdict's Trust from r.SelfCheck, and the
// exit code ExitUntrusted where the report answered and a verdict rests on
// figures the self-check found inconsistent. A verdict not judged says
// nothing more.
func (r *ReportResponse) JudgeTrust() {
	sc := r.SelfCheck
	if sc == nil || sc.Status == SelfCheckNotRun {
		return
	}
	untrusted := false
	for i := range r.Shapes {
		s := &r.Shapes[i]
		v := &s.Verdict
		if v.Verdict == VerdictNotJudged || v.Verdict == "" {
			continue
		}
		var bad []string
		for _, f := range restsOn(*v, r.SLO) {
			if slices.Contains(sc.Untrusted, f) {
				bad = append(bad, strings.ToUpper(f))
			}
		}
		rate := figureValue(r.Load.Rate)
		switch {
		case len(bad) > 0:
			v.Trust = fmt.Sprintf("%s: the self-check found this recording's %s figures inconsistent with it, and this verdict rests on them",
				TrustNot, strings.Join(bad, " and "))
			untrusted = true
		case s.Source == shapeSourceAsked:
			v.Trust = TrustNotCovered + ": it tested the recorded traffic, not this shape"
		case sc.Covered != nil && !sc.Covered.Covers(rate, r.Fleet.Prefill, r.Fleet.Decode):
			v.Trust = fmt.Sprintf("%s: it tested %s; this verdict is at %.2f r/s at %d prefill + %d decode", TrustNotCovered,
				sc.Covered.Words(), rate, r.Fleet.Prefill, r.Fleet.Decode)
		}
	}
	if untrusted && r.ExitCode == 0 {
		r.ExitCode = ExitUntrusted
	}
}

// shapeSourceAsked is ReportShape.Source for a shape the question typed.
const shapeSourceAsked = "asked"
