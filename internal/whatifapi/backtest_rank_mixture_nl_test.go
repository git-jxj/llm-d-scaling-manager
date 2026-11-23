package whatifapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The non-linear variant's Z is measured part by part at spread 1, A0 is the
// sum of the parts' non-negative intercepts, and the report names the law
// and the parts; v2's report gains none of it. On a fit window under 12 h
// it abstains, as v2 does, for its own reason.
func TestBacktest_RankMixtureNLFits(t *testing.T) {
	r := &stubResolver{}
	short := rmFit(t, r, 480)
	ts0, tl0 := btSeries(240, 0, 1)
	test0 := btEntry(t, r, "test0", ts0, tl0, btConfig("H100"))
	test0.mixture = btMixture(ts0)
	sh := btRun(t, short, test0, BacktestRequest{TTFTModel: string(whatif.QueueRankMixtureNL), PrefillSlots: 4})
	require.NotNil(t, sh.RankMixture)
	assert.Contains(t, sh.RankMixture.Abstained, "too short for its own checks")
	fit := rmFit(t, r, 3120)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)

	v2 := btRun(t, fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4})
	require.NotNil(t, v2.RankMixture)

	out := btRun(t, fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixtureNL), PrefillSlots: 4})
	require.NotNil(t, out.RankMixture)
	rm := out.RankMixture
	assert.Equal(t, string(whatif.QueueRankMixtureNL), rm.Model)
	assert.Equal(t, string(whatif.ServiceAttention), rm.Law)
	require.NotNil(t, rm.ZParts)
	assert.GreaterOrEqual(t, rm.A0, 0.0)
	assert.InDelta(t, rm.ZParts.OverheadA+rm.ZParts.DecodeA+rm.ZParts.RemainderA, rm.A0, 1e-12)
	assert.Equal(t, 1.0, rm.Spread)
	assert.NotContains(t, rm.Abstained, "too short")
	assert.Positive(t, rm.FitMean)
	assert.Contains(t, strings.Join(out.Assumed, "; "), string(whatif.QueueRankMixtureNL))
	b, err := json.Marshal(v2.RankMixture)
	require.NoError(t, err)
	for _, k := range []string{`"law"`, `"zParts"`, `"fitMean"`, `"cleanSteps"`} {
		assert.NotContains(t, string(b), k, "v2's report is unchanged")
	}
	if rm.Abstained == "" {
		priced := 0
		for _, st := range out.Steps {
			for _, p := range st.TTFT {
				if p.Predicted.Value != nil && st.RankMixture != nil && st.RankMixture.Refused == "" {
					assert.Contains(t, p.Predicted.Flags, string(whatif.RankMixtureTTFT))
					priced++
				}
			}
		}
		assert.Positive(t, priced)
	} else {
		assert.Contains(t, rm.Abstained, "fit window's own")
	}
}

// The variant's own checks: v2's spread and A0 rules do not apply; the fit
// window must price its own mean and p90.
func TestRankMixtureNL_Identified(t *testing.T) {
	ok := func() *rankMixtureQ {
		return &rankMixtureQ{report: RankMixtureReport{Model: string(whatif.QueueRankMixtureNL), Spread: 1, Slots: 4, A0: 0.4,
			FitMean: 1.02, FitP90: 0.97}}
	}
	assert.Empty(t, ok().identifiedNL().report.Abstained)
	low := ok()
	low.report.FitMean = 0.7
	assert.Contains(t, low.identifiedNL().report.Abstained, "own mean not priced")
	high := ok()
	high.report.FitMean = 1.3
	assert.Contains(t, high.identifiedNL().report.Abstained, "own mean not priced")
	short := ok()
	short.report.FitP90 = 0.85
	assert.Contains(t, short.identifiedNL().report.Abstained, "own p90 not priced")
	nan := ok()
	nan.report.FitP90 = 0
	assert.Contains(t, nan.identifiedNL().report.Abstained, "own p90 not priced")
	brief := ok()
	brief.report.SpreadDefaulted = true
	assert.Contains(t, brief.identifiedNL().report.Abstained, "too short")
	k := ok()
	k.report.Slots = 0
	assert.Contains(t, k.identifiedNL().report.Abstained, "max-num-seqs not read")
	prop := ok()
	prop.report.ServiceProportional = true
	assert.Contains(t, prop.identifiedNL().report.Abstained, "one prompt length")
}

// An unknown model names both.
func TestBacktest_UnknownTTFTModelNamesBoth(t *testing.T) {
	r := &stubResolver{}
	fit := rmFit(t, r, 480)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)
	_, err := backtestQuestionOf(fit, test, BacktestRequest{TTFTModel: "nope"}, time.Time{}, "")
	require.Error(t, err)
	assert.True(t, IsInputError(err))
	assert.Contains(t, err.Error(), string(whatif.QueueRankMixtureNL))
	// A source without the per-rank series: the variant abstains, named.
	test.mixture = nil
	q, err := backtestQuestionOf(fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixtureNL)}, time.Time{}, "")
	require.NoError(t, err)
	assert.Equal(t, string(whatif.QueueRankMixtureNL), q.mixture.report.Model)
	assert.Contains(t, q.mixture.report.Abstained, "per-rank series")
}

// rank-mixture-nl-line is the variant with the service line: it names
// itself and its law.
func TestBacktest_RankMixtureNLLine(t *testing.T) {
	r := &stubResolver{}
	fit := rmFit(t, r, 3120)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)
	out := btRun(t, fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixtureNLLine), PrefillSlots: 4})
	require.NotNil(t, out.RankMixture)
	assert.Equal(t, string(whatif.QueueRankMixtureNLLine), out.RankMixture.Model)
	assert.Equal(t, "line", out.RankMixture.Law)
	assert.Zero(t, out.RankMixture.B)
	require.NotNil(t, out.RankMixture.ZParts)
	test.mixture = nil
	q, err := backtestQuestionOf(fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixtureNLLine)}, time.Time{}, "")
	require.NoError(t, err)
	assert.Equal(t, string(whatif.QueueRankMixtureNLLine), q.mixture.report.Model)
}
