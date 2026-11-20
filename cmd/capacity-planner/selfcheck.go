package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// Every answer read from a recording also checks the planner on it
// (whatifapi.SelfCheckOf): the replay that reads the answer's state also
// reads the states at the folds' cuts (learnreplay.StatesFrom), so the
// self-check adds no replay, only the folds' pricing. --no-self-check, and
// selfCheck false on the API, turn it off.

// defaultSelfCheck is whether an answer self-checks when not told: true.
// The package's tests turn it off, so that the answers' goldens pin the
// answer alone; the self-check has goldens of its own.
var defaultSelfCheck = true

// selfCheckOn says the answer self-checks: the server's request, or the
// CLI's default less --no-self-check.
func (f *answerFlags) selfCheckOn() bool {
	if f.question == "serve" {
		return f.serveSelfCheck
	}
	return defaultSelfCheck && !f.noSelfCheck
}

// readStates reads the state at the answer's cycle and, when the answer
// self-checks, the states at the folds' cuts, in one replay.
func (f *answerFlags) readStates(ctx context.Context, src learnreplay.Source, q learnreplay.StateQuery) (*learnreplay.State, error) {
	if !f.selfCheckOn() {
		return learnreplay.StateFrom(ctx, src, learnreplay.Options{}, q)
	}
	st, folds, err := learnreplay.StatesFrom(ctx, src, learnreplay.Options{}, q, whatifapi.SelfCheckCycles)
	f.folds = folds
	return st, err
}

// foldFits is the folds' states, priced as the answer's state is.
func (f *answerFlags) foldFits(decodeVariant, prefillVariant string) []whatifapi.FoldFit {
	out := make([]whatifapi.FoldFit, 0, len(f.folds))
	for c, st := range f.folds {
		out = append(out, whatifapi.FoldFit{Cycle: c, At: st.At, Pricer: &servedState{st: st, decodeVariant: decodeVariant,
			prefillVariant: prefillVariant, hint: f.hintSource(), imbalanceAcrossCounts: f.imbalanceAcrossCounts}})
	}
	return out
}

// checkRecording self-checks the recording the CLI answered from, src at
// its state st, into f.checked: what the answer prints with it.
func (f *answerFlags) checkRecording(ctx context.Context, st *learnreplay.State, src *recordingSource) {
	if !f.selfCheckOn() {
		return
	}
	t0 := time.Now()
	ss := &servedSources{warn: io.Discard}
	info := whatifapi.SourceInfo{Kind: whatifapi.SourceRecording, Cycle: st.Cycle, TotalCycles: st.Cycles, FirstAt: st.FirstAt, LastAt: st.At}
	res, err := ss.resolved(f, st, src, info)
	if err != nil {
		f.checked = &whatifapi.SelfCheck{Status: whatifapi.SelfCheckNotRun, Why: err.Error(), Line: "Self-check not run: " + err.Error() + ".",
			Folds: []whatifapi.SelfCheckFold{}, Checks: []whatifapi.SelfCheckRule{}}
		return
	}
	f.checked = whatifapi.SelfCheckOf(ctx, res, whatifapi.SelfCheckInfo{PlannerVersion: selfCheckVersion()})
	f.checked.Seconds = time.Since(t0).Seconds()
}

// selfCheckText is the self-check as the CLI prints it beside an answer:
// the line, and on a result short of ok why, what to trust, and the issue to
// review and file.
func selfCheckText(sc *whatifapi.SelfCheck) string {
	if sc == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s [%s]\n", sc.Line, sc.Status)
	if sc.Status == whatifapi.SelfCheckWeak || sc.Status == whatifapi.SelfCheckInconsistent {
		fmt.Fprintf(&b, "  why: %s\n", sc.Why)
	}
	if sc.Trust != "" {
		fmt.Fprintf(&b, "  %s\n", sc.Trust)
	}
	if sc.IssueURL != "" {
		fmt.Fprintf(&b, "  to report it, review and file this issue (nothing is sent until you do; it holds no name and no recording):\n  %s\n", sc.IssueURL)
	}
	return b.String()
}

// selfCheckVersion is the release an issue is stamped with; the tests fix
// it, so that a golden does not hold the commit it was built at.
var selfCheckVersion = plannerVersion
