package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SwitchTTFTComputedHit is planner-only: off from the constructor, and a
// replay can turn it on.
func TestTTFT_TheComputedHitOffFromTheConstructor(t *testing.T) {
	assert.False(t, New(nil, nil).ttftComputedHit)
	assert.Contains(t, ReplaySwitchNames(), SwitchTTFTComputedHit)
	a := New(nil, nil)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchTTFTComputedHit: true}))
	assert.True(t, a.ttftComputedHit)
}
