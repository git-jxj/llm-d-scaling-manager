package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The planner prices the batch term the replay fitted (issue
// llm-d-incubation/llm-d-scaling-manager#309): the decode carries it, the
// figures are flagged, and the knee falls, since every step past the roof is
// slower. A line lifted to its floor drops it: its
// residuals were read against the line the replay fitted.
func TestBatchTerm_DecodeVariantsPricesTheReplaysTerm(t *testing.T) {
	term := whatif.BatchTerm{Slope: 0.034e-3, Roof: 120, Ranks: 1}
	state := func(fit whatif.BatchFit, lift float64) *learnreplay.State {
		return &learnreplay.State{ModelID: "m", Policy: &config.ScalingPolicy{}, Variants: []learnreplay.VariantState{{
			Variant: "dec", Role: domain.RoleDecode, Line: itl.Model{A: 0.03, B: 0.012}, Tier: whatif.TierOLS,
			CapacityTokens: 764_672, BatchCap: 256, Ranks: 1, Replicas: 1, LineLift: lift, BatchFit: fit,
			Shape: learnreplay.MeasuredShape{InputTokens: 1000, OutputTokens: 1000, OutputDispersion: 1},
		}}}
	}
	priced := func(t *testing.T, st *learnreplay.State, set map[string]bool) pricedVariant {
		t.Helper()
		f := answerFlags{model: "m", set: set}
		variants, err := f.decodeVariants(st)
		require.NoError(t, err)
		require.Len(t, variants, 1)
		return variants[0]
	}
	knee := func(t *testing.T, d whatif.Decode) float64 {
		t.Helper()
		a, ok := whatif.KneeRate(d).(whatif.Answer)
		require.True(t, ok, "%v", whatif.KneeRate(d))
		return a.Value
	}

	off := priced(t, state(whatif.BatchFit{}, 0), map[string]bool{})
	on := priced(t, state(whatif.BatchFit{Term: term}, 0), map[string]bool{})
	assert.True(t, off.decode.Batch.IsZero())
	assert.NotContains(t, off.flags, itlBatchTerm)
	assert.Equal(t, term, on.decode.Batch, "the replay's term is priced")
	assert.Contains(t, on.flags, itlBatchTerm)
	assert.Less(t, knee(t, on.decode), knee(t, off.decode), "the term slows the steps past the roof")

	var b strings.Builder
	flagsLine(&b, on, on.qualify(whatif.KneeRate(on.decode)))
	assert.Contains(t, b.String(), string(itlBatchTerm)+" (0.0340 ms a batch token past 120 per rank)")

	refused := priced(t, state(whatif.BatchFit{Rows: 80, Why: whatif.BatchTermSmallGain}, 0), map[string]bool{})
	assert.True(t, refused.decode.Batch.IsZero(), "a refused fit prices nothing")
	assert.NotContains(t, refused.flags, itlBatchTerm)

	lifted := priced(t, state(whatif.BatchFit{Term: term}, 0.001), map[string]bool{})
	assert.True(t, lifted.decode.Batch.IsZero(), "a lifted line is not the one the term was read on")
}
