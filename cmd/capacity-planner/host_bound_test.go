package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// hostState is a P/D state with a decode line of intercept b and a floor
// below the band, and the router's TPOT windows.
func hostState(b, floor float64, windows ...whatif.TPOTWindow) *learnreplay.State {
	return &learnreplay.State{TPOTWindows: windows, Variants: []learnreplay.VariantState{
		{Variant: "p", Role: string(domain.RolePrefill), Line: itl.Model{A: 0.03, B: 0.0001}},
		{Variant: "d", Role: string(domain.RoleDecode), Line: itl.Model{A: 0.03, B: b}, BelowBandITL: floor},
	}}
}

// A TPOT percentile read off coarse buckets is refused where the decode
// step is host-bound (#291): the line's intercept and floor under 5 ms, as
// a sub-1B model's 1.5 to 2.5 ms. A fine bucket, a step of 5 ms or more, a
// floor of 5 ms or more, or an assumed ratio prices as before.
func TestTPOTRatio_AHostBoundCoarsePercentileIsRefused(t *testing.T) {
	for name, c := range map[string]struct {
		st      *learnreplay.State
		assumed bool
		refused bool
	}{
		"0.6B: B 1.7 ms, floor 1.75 ms, coarse":  {st: hostState(0.0017, 0.00175, coarseWindow), refused: true},
		"no floor read, coarse":                  {st: hostState(0.0017, 0, coarseWindow), refused: true},
		"fine buckets":                           {st: hostState(0.0017, 0.00175, fineWindow)},
		"32B: B 12 ms, coarse":                   {st: hostState(0.012, 0, coarseWindow)},
		"B 1.7 ms, but the floor 6 ms":           {st: hostState(0.0017, 0.006, coarseWindow)},
		"assumed ratio":                          {st: hostState(0.0017, 0.00175, coarseWindow), assumed: true},
		"a default (zero) intercept is not read": {st: hostState(0, 0, coarseWindow)},
	} {
		t.Run(name, func(t *testing.T) {
			f := answerFlags{set: map[string]bool{"assume-tpot-ratio": c.assumed}, tpotRatio: 1.3}
			r, _, u := f.tpotRatioFor(c.st, 0.9)
			if !c.refused {
				require.Nil(t, u, "%+v", u)
				assert.Positive(t, r.Value)
				return
			}
			require.NotNil(t, u)
			assert.Equal(t, whatif.TPOTBucketTooWide, u.Reason)
			assert.Equal(t, []whatif.Flag{whatif.CoarseTPOTBucket}, u.Flags)
			assert.Contains(t, u.Detail, "host-bound")
			assert.Contains(t, u.Detail, "--assume-tpot-ratio")
			assert.Zero(t, r.Value, "no ratio priced")
		})
	}
}
