package whatifapi

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// hitMixture is a main-view mixture fitted on 2 prefill replicas whose
// pooled window computed about half of each prompt.
func hitMixture(on bool) *mainMixture {
	return &mainMixture{prefill: 2, hitAtCount: on, pooled: whatif.MixtureStep{
		Computed: map[float64]float64{1000: 0, 4000: 40, 8000: 100, math.Inf(1): 100},
		Prompt:   map[float64]float64{8000: 0, 16000: 70, 32000: 100, math.Inf(1): 100},
	}}
}

// With PrefillHitAtCount on, the experimental model draws its computed
// tokens at the prefill count priced: on 1 replica the histogram is scaled
// by (1 - h')/(1 - h), the floor's, so its mean is E[L] (1 - h/2), and its
// figures are flagged PrefixHitAtCount. At the recorded count, at more
// replicas (held, flagged PrefixHitHeld) and with the switch off, the
// pooled histogram is drawn as it is.
func TestMainMixture_TheComputedTokensAtTheCountPriced(t *testing.T) {
	mm := hitMixture(true)
	ec, _ := whatif.CumulativeMean(mm.pooled.Computed)
	el, _ := whatif.CumulativeMean(mm.pooled.Prompt)
	h := 1 - ec/el
	require.Greater(t, h, 0.3)

	c, flag, note := mm.computedAt(1)
	got, ok := whatif.CumulativeMean(c)
	require.True(t, ok)
	t.Logf("E[L] %.0f, E[computed] %.0f on 2 replicas, %.0f priced on 1 (h %.3f)", el, ec, got, h)
	assert.InDelta(t, el*(1-h/2), got, 0.02*el, "the floor's computed tokens, the cap aside")
	assert.LessOrEqual(t, got, el*(1-h/2)*(1+1e-9), "the cap only lowers it")
	assert.Equal(t, whatif.PrefixHitAtCount, flag)
	assert.Contains(t, note, "on 1 prefill replicas, from")

	for name, c := range map[string]struct {
		mm   *mainMixture
		n    int
		flag whatif.Flag
	}{
		"recorded count": {hitMixture(true), 2, ""},
		"more replicas":  {hitMixture(true), 4, whatif.PrefixHitHeld},
		"switch off":     {hitMixture(false), 1, ""},
	} {
		got, flag, note := c.mm.computedAt(c.n)
		assert.Equal(t, c.mm.pooled.Computed, got, name)
		assert.Equal(t, c.flag, flag, name)
		assert.Empty(t, note, name)
	}

	// The flag reaches the model's TTFT figures.
	f := &mixtureFleet{hitFlag: whatif.PrefixHitAtCount}
	assert.Equal(t, []whatif.Flag{whatif.RankMixtureTTFT, whatif.PrefixHitAtCount}, f.flags())
	assert.Equal(t, []whatif.Flag{whatif.RankMixtureTTFT}, (&mixtureFleet{}).flags())
}
