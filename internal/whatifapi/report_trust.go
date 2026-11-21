package whatifapi

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// ExitUntrusted is the report's exit code when it answered, and the
// source's self-check found the figures a verdict rests on inconsistent
// with the recording: the verdict is printed, and is not to be sized from.
// A day plan exits it too, when a bucket's verdict rests on them.
const ExitUntrusted = 4

// The words a verdict's Trust opens with.
const (
	TrustNot        = "not trusted"
	TrustNotCovered = "not covered by the self-check"
)

// restsOn is the figures a verdict rests on, as SelfCheck.Untrusted names
// them: the TPOT figures always (the decode knee and line), and the TTFT
// figures where the prefill side priced (prefillPriced) or a TTFT target
// is judged (ttftJudged).
func restsOn(prefillPriced, ttftJudged bool) []string {
	out := []string{}
	if prefillPriced || ttftJudged {
		out = append(out, "ttft")
	}
	return append(out, "tpot")
}

// judges says sc ran, so that it says something of a verdict.
func (sc *SelfCheck) judges() bool {
	return sc != nil && sc.Status != SelfCheckNotRun
}

// notTrusted is what sc says of a verdict that rests on the figures rests
// names (restsOn) when it found one of them inconsistent: TrustNot, and
// which; empty when it found none of them so.
func (sc *SelfCheck) notTrusted(rests []string) string {
	var bad []string
	for _, f := range rests {
		if slices.Contains(sc.Untrusted, f) {
			bad = append(bad, strings.ToUpper(f))
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return fmt.Sprintf("%s: the self-check found this recording's %s figures inconsistent with it, and this verdict rests on them",
		TrustNot, strings.Join(bad, " and "))
}

// notCovered is what sc says of a verdict at rate on np prefill + nd
// decode replicas when its steps did not test them: TrustNotCovered, and
// what it tested; empty when they did, or it tested none.
func (sc *SelfCheck) notCovered(rate float64, np, nd int) string {
	if sc.Covered == nil || sc.Covered.Covers(rate, np, nd) {
		return ""
	}
	return fmt.Sprintf("%s: it tested %s; this verdict is at %.2f r/s at %d prefill + %d decode", TrustNotCovered,
		sc.Covered.Words(), rate, np, nd)
}

// JudgeTrust sets each shape's verdict's Trust from r.SelfCheck, and the
// exit code ExitUntrusted where the report answered and a verdict rests on
// figures the self-check found inconsistent. A verdict not judged says
// nothing more.
func (r *ReportResponse) JudgeTrust() {
	sc := r.SelfCheck
	if !sc.judges() {
		return
	}
	untrusted := false
	for i := range r.Shapes {
		s := &r.Shapes[i]
		v := &s.Verdict
		if v.Verdict == VerdictNotJudged || v.Verdict == "" {
			continue
		}
		bad := sc.notTrusted(restsOn(v.Scope != ScopeDecodeOnly, r.SLO.TTFT != nil))
		rate := figureValue(r.Load.Rate)
		switch {
		case bad != "":
			v.Trust = bad
			untrusted = true
		case s.Source == shapeSourceAsked:
			v.Trust = TrustNotCovered + ": it tested the recorded traffic, not this shape"
		default:
			v.Trust = sc.notCovered(rate, r.Fleet.Prefill, r.Fleet.Decode)
		}
		// A growth what-if rests on the same figures, at its own rate; it
		// never moves the exit code, which is the verdict at the load's.
		for j := range s.Growth {
			g := &s.Growth[j]
			switch {
			case g.Verdict.Verdict == VerdictNotJudged || g.Verdict.Verdict == "":
			case bad != "", s.Source == shapeSourceAsked:
				g.Verdict.Trust = v.Trust
			case sc.Covered != nil && !sc.Covered.Covers(g.Rate, r.Fleet.Prefill, r.Fleet.Decode):
				g.Verdict.Trust = fmt.Sprintf("%s: it tested %s; this what-if is at %.2f r/s", TrustNotCovered, sc.Covered.Words(), g.Rate)
			}
		}
	}
	if untrusted && r.ExitCode == 0 {
		r.ExitCode = ExitUntrusted
	}
}

// JudgeTrust sets each judged bucket's Trust from p.SelfCheck, as a
// report's verdict's (ReportResponse.JudgeTrust): not trusted where its
// verdict rests on figures the self-check found inconsistent (the TTFT
// figures where the bucket priced prefill, the TPOT figures always); else
// not covered where the bucket's sized rate on the fleet it had is not one
// the self-check tested. Each hour's Trust is its worst bucket's opening
// words, and the summary counts both. A bucket refused, or not sized,
// says nothing more. The day plan exits ExitUntrusted when
// Summary.Counts.Untrusted is above 0.
func (p *DayPlanResponse) JudgeTrust() {
	sc := p.SelfCheck
	if !sc.judges() {
		return
	}
	c := &p.Summary.Counts
	for i := range p.Buckets {
		b := &p.Buckets[i]
		if b.Verdict == "" || b.Verdict == DayVerdictRefused {
			continue
		}
		b.Trust = sc.notTrusted(restsOn(!slices.Contains(b.Flags, FlagPrefillNotPriced), false))
		if b.Trust != "" {
			c.Untrusted++
		} else if b.SizedRate != nil && b.Actual != nil {
			if b.Trust = sc.notCovered(*b.SizedRate, b.Actual.Prefill, b.Actual.Decode); b.Trust != "" {
				c.NotCovered++
			}
		}
		if b.Trust == "" {
			continue
		}
		h := slices.IndexFunc(p.Hours, func(h DayHour) bool { return h.Start.Equal(b.Start.Truncate(time.Hour)) })
		if h >= 0 && p.Hours[h].Trust != TrustNot {
			p.Hours[h].Trust, _, _ = strings.Cut(b.Trust, ":")
		}
	}
}

// shapeSourceAsked is ReportShape.Source for a shape the question typed.
const shapeSourceAsked = "asked"
