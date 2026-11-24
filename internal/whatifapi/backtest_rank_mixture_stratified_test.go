package whatifapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// rankMixtureStratified turns the stratified service moments on for the
// fit; off by default, and the request names it only when on.
func TestBacktest_RankMixtureStratified(t *testing.T) {
	r := &stubResolver{}
	fit := rmFit(t, r, 3120)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)
	req := BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4}
	q, err := backtestQuestionOf(fit, test, req, time.Time{}, "")
	require.NoError(t, err)
	assert.False(t, q.mixture.m.Stratified)
	req.RankMixtureStratified = true
	q, err = backtestQuestionOf(fit, test, req, time.Time{}, "")
	require.NoError(t, err)
	assert.True(t, q.mixture.m.Stratified)
	b, err := json.Marshal(BacktestRequest{TTFTModel: string(whatif.QueueRankMixture)})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "rankMixtureStratified")
}
