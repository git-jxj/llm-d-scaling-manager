package whatifapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cyclesAt is n cycles every 15 s from t0, the cycles in skip left out.
func cyclesAt(t0 time.Time, n int, skip func(i int) bool) []time.Time {
	var out []time.Time
	for i := range n {
		if skip == nil || !skip(i) {
			out = append(out, t0.Add(time.Duration(i)*15*time.Second))
		}
	}
	return out
}

func TestWindowGapsOf(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 2, 18, 0, 0, time.UTC)

	whole := WindowGapsOf(cyclesAt(t0, 289, nil))
	require.NotNil(t, whole)
	assert.Equal(t, 289, whole.ExpectedCycles)
	assert.Equal(t, 289, whole.PresentCycles)
	assert.Zero(t, whole.LargestGapSeconds)
	assert.False(t, whole.Flagged)
	assert.Empty(t, whole.Text(), "no hole, no words")

	// 60 cycles (15 minutes) out of 289: 21% missing, flagged.
	holed := WindowGapsOf(cyclesAt(t0, 289, func(i int) bool { return i >= 100 && i < 160 }))
	require.NotNil(t, holed)
	assert.Equal(t, 289, holed.ExpectedCycles)
	assert.Equal(t, 229, holed.PresentCycles)
	assert.InDelta(t, 60.0/289, holed.MissingShare, 1e-9)
	assert.InDelta(t, 15*60, holed.LargestGapSeconds, 1e-9)
	assert.True(t, holed.Flagged)
	assert.Equal(t, "289 cycles expected, 229 present (21% missing), largest gap 15m0s from 02:42 to 02:58Z WindowGaps", holed.Text())

	// One cycle in 100 missing: 1%, reported, not flagged.
	one := WindowGapsOf(cyclesAt(t0, 100, func(i int) bool { return i == 50 }))
	require.NotNil(t, one)
	assert.Equal(t, 100, one.ExpectedCycles)
	assert.False(t, one.Flagged)
	assert.NotContains(t, one.Text(), FlagWindowGaps)

	// Jitter under half a cycle is not a gap.
	jit := cyclesAt(t0, 50, nil)
	jit[10] = jit[10].Add(5 * time.Second)
	assert.Equal(t, 50, WindowGapsOf(jit).ExpectedCycles)

	assert.Nil(t, WindowGapsOf(cyclesAt(t0, 2, nil)), "too few cycles for a cycle time")
	var none *WindowGaps
	assert.Empty(t, none.Text())
}
