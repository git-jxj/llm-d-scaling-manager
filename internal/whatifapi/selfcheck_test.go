package whatifapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scEntry is the synthetic window as one source, its measured latency
// scaled by latencyX and every rate times rateX, with its folds: the stub
// fitted at half and three quarters of its cycles.
func scEntry(t *testing.T, latencyX, rateX float64) (*inputsEntry, []FoldFit) {
	t.Helper()
	r := &stubResolver{}
	s, l := btSeries(480, 0, latencyX)
	for i := range s {
		s[i].Rate *= rateX
		if l[i].Router != nil {
			for j := range l[i].Router.TTFTBuckets {
				l[i].Router.TTFTBuckets[j].Rate *= rateX
			}
			for j := range l[i].Router.TPOTBuckets {
				l[i].Router.TPOTBuckets[j].Rate *= rateX
			}
			l[i].Router.TTFTSumRate *= rateX
			l[i].Router.TTFTCountRate *= rateX
			l[i].Router.TPOTSumRate *= rateX
			l[i].Router.TPOTCountRate *= rateX
		}
	}
	e := btEntry(t, r, "src", s, l, btConfig("H100"))
	e.doc.Source.Cycle = len(s) // answered at its last cycle
	folds := make([]FoldFit, 0, len(SelfCheckFractions))
	for _, c := range SelfCheckCycles(len(s)) {
		folds = append(folds, FoldFit{Cycle: c, At: s[c-1].At, Pricer: r})
	}
	return e, folds
}

// The self-check is not run, and says why, on a source it cannot judge.
func TestSelfCheck_SaysWhyItDidNotRun(t *testing.T) {
	e, folds := scEntry(t, 1, 1)
	short := *e
	short.series, short.latency = e.series[:200], e.latency[:200]
	assert.Contains(t, selfCheckEntry(context.Background(), &short, folds, SelfCheckInfo{}).Why, "a self-check needs 1 h")

	blind := *e
	blind.latency = make([]LatencySample, len(e.latency))
	assert.Contains(t, selfCheckEntry(context.Background(), &blind, folds, SelfCheckInfo{}).Why, "no measured latency")

	none := selfCheckEntry(context.Background(), e, nil, SelfCheckInfo{})
	assert.Equal(t, SelfCheckNotRun, none.Status)
	assert.Contains(t, none.Line, "Self-check not run")

	aggregated := *e
	aggregated.doc.Fleet.Prefill = nil
	assert.Contains(t, selfCheckEntry(context.Background(), &aggregated, folds, SelfCheckInfo{}).Why, "no prefill variant")
}

// A planner that reads far from what the fleet measured is inconsistent:
// the rules that failed say by how much, the answer says what not to trust,
// and an issue is offered; one past its knee where the fleet served is
// inconsistent too.
func TestSelfCheck_FindsAnInconsistency(t *testing.T) {
	for name, c := range map[string]struct {
		latencyX, rateX float64
		rule            string
	}{
		"reads high": {0.2, 1, "ttft-bias"},
		"reads low":  {8, 1, "ttft-bias"},
		"overloaded": {1, 2.2, "ttft-overloaded"},
	} {
		t.Run(name, func(t *testing.T) {
			e, folds := scEntry(t, c.latencyX, c.rateX)
			sc := selfCheckEntry(context.Background(), e, folds, SelfCheckInfo{PlannerVersion: "vtest"})
			require.Equal(t, SelfCheckInconsistent, sc.Status, "%s: %s", sc.Line, sc.Why)
			failed := []string{}
			for _, r := range sc.Checks {
				if !r.Passed {
					failed = append(failed, r.Name)
				}
			}
			assert.Contains(t, failed, c.rule)
			assert.Contains(t, sc.Trust, "Do not trust prefill sizing")
			require.NotEmpty(t, sc.IssueURL)
			assert.True(t, strings.HasPrefix(sc.IssueURL, selfCheckIssueRepo+"?"), sc.IssueURL)
			assert.Len(t, sc.Folds, 2)
			assert.True(t, strings.HasPrefix(sc.Line, "Self-check: on the last "), sc.Line)
		})
	}
	// The same fleet, measured as it predicts, is not inconsistent: no issue.
	e, folds := scEntry(t, 1, 1)
	sc := selfCheckEntry(context.Background(), e, folds, SelfCheckInfo{})
	assert.NotEqual(t, SelfCheckNotRun, sc.Status, sc.Why)
	if sc.Status != SelfCheckInconsistent {
		assert.Empty(t, sc.IssueURL)
	}
}

// The issue a self-check offers holds no name from the recording: not the
// model, the namespace, a variant or the file; and no request rate.
func TestSelfCheck_IssueHoldsNoName(t *testing.T) {
	e, folds := scEntry(t, 8, 1)
	secrets := []string{"secret-model-xyz", "secret-ns", "secret-prefill-variant", "secret-decode-variant", "secret-recording.jsonl.gz"}
	e.doc.Model, e.doc.Namespace, e.doc.Source.Name = secrets[0], secrets[1], secrets[4]
	e.doc.Fleet.Prefill.Variant, e.doc.Fleet.Decode.Variant = secrets[2], secrets[3]
	sc := selfCheckEntry(context.Background(), e, folds, SelfCheckInfo{PlannerVersion: "vtest"})
	require.NotEmpty(t, sc.IssueURL)
	u, err := url.Parse(sc.IssueURL)
	require.NoError(t, err)
	body := u.Query().Get("diagnostics")
	require.NotEmpty(t, body)
	assert.Equal(t, selfCheckIssueTemplate, u.Query().Get("template"))
	for _, s := range append(secrets, "secret") {
		assert.NotContains(t, strings.ToLower(sc.IssueURL), s)
		assert.NotContains(t, strings.ToLower(body), s)
	}
	assert.NotContains(t, body, "r/s", "no request rate")
	for _, want := range []string{"planner: vtest", "failed: ttft-bias", "fleet: prefill 1, decode 1", "shape: prompt"} {
		assert.Contains(t, body, want)
	}
}

// The server self-checks a source beside its inputs by default when it was
// started to, and not when the request says selfCheck false.
func TestSelfCheck_OverHTTP(t *testing.T) {
	r := &btResolver{folds: true, n: 480}
	h := New(Config{Resolver: r, WhatifSchema: "schema-v3", SelfCheck: true}).Handler()
	code, body := post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"}})
	require.Equal(t, http.StatusOK, code, string(body))
	var in InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	require.NotNil(t, in.SelfCheck)
	assert.NotEqual(t, SelfCheckNotRun, in.SelfCheck.Status, in.SelfCheck.Why)

	off := false
	code, body = post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"}, SelfCheck: &off})
	require.Equal(t, http.StatusOK, code, string(body))
	var none InputsResponse
	require.NoError(t, json.Unmarshal(body, &none))
	assert.Nil(t, none.SelfCheck)
	assert.False(t, *r.lastSelfCheck, "the resolver was told not to read folds")
}
