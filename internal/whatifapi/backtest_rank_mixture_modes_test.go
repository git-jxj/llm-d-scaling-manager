package whatifapi

import (
	"encoding/json"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The variant's two modes: each is refused without the variant, and the
// request names each only when it is on. The KV-deferral hold reports where
// it holds and says so in the assumed line. The remainder spread on a
// recording without the router's processing series prices the same figures
// as off and says why; on one with it, it reports the measured law.
func TestBacktest_RankMixtureNLLineModes(t *testing.T) {
	r := &stubResolver{}
	fit := rmFit(t, r, 3120)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)

	for name, req := range map[string]BacktestRequest{
		"hold, no model":     {RankMixtureKVHold: true},
		"spread, no model":   {RankMixtureRemainderSpread: true},
		"hold with v2":       {TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4, RankMixtureKVHold: true},
		"spread with v2":     {TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4, RankMixtureRemainderSpread: true},
		"both, unknown name": {TTFTModel: "rank-mixture-nl", RankMixtureKVHold: true, RankMixtureRemainderSpread: true},
	} {
		_, err := backtestQuestionOf(fit, test, req, time.Time{}, "")
		require.Error(t, err, name)
		assert.True(t, IsInputError(err), "%s: %v", name, err)
		assert.Contains(t, err.Error(), string(whatif.QueueRankMixtureNLLine), name)
	}
	b, err := json.Marshal(BacktestRequest{TTFTModel: string(whatif.QueueRankMixtureNLLine)})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "rankMixtureKVHold")
	assert.NotContains(t, string(b), "rankMixtureRemainderSpread")

	base := BacktestRequest{TTFTModel: string(whatif.QueueRankMixtureNLLine), PrefillSlots: 4}
	off := btRun(t, fit, test, base)
	require.NotNil(t, off.RankMixture)
	require.NotNil(t, off.RankMixture.KV, "the fixture fits a KV-deferral wait")
	assert.Zero(t, off.RankMixture.KV.RhoHold)
	offJSON, err := json.Marshal(off)
	require.NoError(t, err)
	for _, k := range []string{`"rhoHold"`, `"remainderShapeMean"`, `"remainderShapeP95"`} {
		assert.NotContains(t, string(offJSON), k, "off, the answer has no trace of the modes")
	}

	req := base
	req.RankMixtureKVHold = true
	held := btRun(t, fit, test, req)
	require.NotNil(t, held.RankMixture.KV)
	assert.Positive(t, held.RankMixture.KV.RhoHold)
	assert.GreaterOrEqual(t, held.RankMixture.KV.RhoHold, held.RankMixture.KV.RhoLo)
	assert.Contains(t, held.RankMixture.Assumed, "KV-deferral lines held past rho")
	assert.Equal(t, off.RankMixture.FitMean, held.RankMixture.FitMean, "the hold does not move the fit window's checks")
	assert.Equal(t, off.RankMixture.FitP90, held.RankMixture.FitP90)
	assert.Equal(t, off.RankMixture.Abstained, held.RankMixture.Abstained)

	req = base
	req.RankMixtureRemainderSpread = true
	line := btRun(t, fit, test, req)
	assert.Contains(t, line.RankMixture.Assumed, "router remainder drawn as its line")
	assert.Zero(t, line.RankMixture.ZParts.RemainderShapeMean)
	assert.Equal(t, predictedOf(t, off), predictedOf(t, line), "no processing recorded: the figures are off's")

	recorded := *fit
	recorded.mixture = slices.Clone(fit.mixture)
	for i := range recorded.mixture {
		recorded.mixture[i].RouterProcessingHist = map[float64]float64{0.01: 1, 0.05: 3, 0.2: 4.5, 1: 4.9, math.Inf(1): 5}
		recorded.mixture[i].RouterProcessingMean = 0.06
	}
	spread := btRun(t, &recorded, test, req)
	zp := spread.RankMixture.ZParts
	require.NotNil(t, zp)
	assert.InDelta(t, 0.06, zp.RemainderShapeMean, 1e-12)
	assert.Greater(t, zp.RemainderShapeP95, 1.0, "the router's own time has a tail")
	assert.Contains(t, spread.RankMixture.Assumed, "router remainder spread as the router's measured request processing")
	assert.Equal(t, off.RankMixture.FitMean, spread.RankMixture.FitMean, "the spread does not move the fit window's checks")
}
