package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// Synthetic (issue llm-d-incubation/llm-d-scaling-manager#312): no recording
// holds a data-parallel engine. A DP8 MoE decode whose busiest rank fills 1.25
// times the mean: with its line read at the busiest rank, the band's top is
// the busiest rank's, so the mean fill priced there is Hi/1.25 and the knee
// falls by 1.25. Without the switch the capacity is the variant's own.
func TestBusiestRankCapacity_PricesTheMeanAtTheTopOverTheRatio(t *testing.T) {
	vs := learnreplay.VariantState{CapacityTokens: 1_000_000, BusiestRankFill: true, BusiestRankRatio: 1.25}
	assert.InDelta(t, 800_000, busiestRankCapacity(vs), 1e-6)

	off := vs
	off.BusiestRankFill = false
	assert.InDelta(t, 1_000_000, busiestRankCapacity(off), 1e-6, "switch off: the variant's capacity")
	unread := vs
	unread.BusiestRankRatio = 0
	assert.InDelta(t, 1_000_000, busiestRankCapacity(unread), 1e-6, "no ratio read: the whole capacity")

	knee := func(c float64) float64 {
		d := whatif.Decode{Line: itl.Model{A: 0.02, B: 0.01}, Band: whatif.FillBand{Lo: 0.15, Hi: 0.8},
			CapacityTokens: c, KVPerRequest: 2000, OutputTokens: 500, ReplicaCap: 8 * 1024, Replicas: 1, Tier: whatif.TierOLS}
		a, ok := whatif.KneeRate(d).(whatif.Answer)
		require.True(t, ok, "%v", whatif.KneeRate(d))
		return a.Value
	}
	// 400 sequences at the mean's band top off; 320 on, at the same ITL.
	assert.InDelta(t, knee(1_000_000)/1.25, knee(busiestRankCapacity(vs)), 1e-9)
}

// The flag's words name the ratio, or say none was read.
func TestBusiestRankWords(t *testing.T) {
	assert.Contains(t, busiestRankWords(1.25), "fill / 1.25")
	assert.Contains(t, busiestRankWords(0), "no busiest/mean ratio read")
}
