package whatif

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// residenceDecode is the 0.6B staircase's decode at 8k prompts and 26
// output tokens: line A 31.25 ms, B 1.72 ms, 1.16M tokens a replica.
func residenceDecode(held float64) Decode {
	return Decode{Line: itl.Model{A: 0.03125, B: 0.00172}, Band: FillBand{Lo: 0.15, Hi: 0.9}, CapacityTokens: 1.16e6,
		KVPerRequest: 8200, OutputTokens: 26, ReplicaCap: 256, Replicas: 2, Tier: TierOLS, HeldBefore: held}
}

// A P/D decode holds a request's KV from the pull, not from its first
// token (#301): residence is HeldBefore + OL * ITL(k). Little's law then
// reads a higher fill at a rate, and the knee a lower rate, by the share
// HeldBefore adds; with 0 every figure is the old one.
func TestDecode_ResidenceHoldsTheKVFromThePull(t *testing.T) {
	const rate, held = 20.0, 0.030 // 30 ms of pull and first step
	off, on := residenceDecode(0), residenceDecode(held)
	k0, ok := off.fillAtReplicaRate(rate / 2)
	require.True(t, ok)
	k1, ok := on.fillAtReplicaRate(rate / 2)
	require.True(t, ok)
	// k = lam (H + OL B) / (C/KVreq - lam OL A), each solved by hand.
	den := 1.16e6/8200 - 10*26*0.03125
	assert.InDelta(t, 10*26*0.00172/den, k0, 1e-12)
	assert.InDelta(t, 10*(held+26*0.00172)/den, k1, 1e-12)
	assert.Greater(t, k1/k0, 1.6, "at 26 output tokens the pull is most of the residence")

	// Little's law on the result: requests resident = rate x residence.
	seqs := k1 * 1.16e6 / 8200
	assert.InDelta(t, 10*(held+26*on.Line.ITLAt(k1)), seqs, 1e-9)

	// The knee is N c / (H + OL ITL(k_full)).
	full := on.full()
	assert.InDelta(t, 2*full.slots/(held+26*on.Line.ITLAt(full.fill)), on.kneeRate(), 1e-9)
	assert.Less(t, on.kneeRate(), off.kneeRate())

	// The admission chain completes at the same rate.
	assert.InDelta(t, 5/(held+26*on.Line.ITLAt(math.Max(5*8200/1.16e6, on.lowestFill()))), on.chainMu(5), 1e-9)
}

// A HeldBefore that is not a finite non-negative number is refused.
func TestDecode_ResidenceRefusesABadHeldBefore(t *testing.T) {
	for _, h := range []float64{-0.001, math.NaN(), math.Inf(1)} {
		u := residenceDecode(h).check()
		require.NotNil(t, u, "%g", h)
		assert.Equal(t, InvalidInput, u.Reason)
	}
	assert.Nil(t, residenceDecode(0).check())
}
