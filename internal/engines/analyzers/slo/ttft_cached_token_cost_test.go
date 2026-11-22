package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SwitchTTFTCachedTokenCost is planner-only: off from the constructor, and a
// replay can turn it on.
func TestTTFT_TheCachedTokenCostOffFromTheConstructor(t *testing.T) {
	assert.False(t, New(nil, nil).ttftCachedTokenCost)
	assert.Contains(t, ReplaySwitchNames(), SwitchTTFTCachedTokenCost)
	a := New(nil, nil)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchTTFTCachedTokenCost: true}))
	assert.True(t, a.ttftCachedTokenCost)
}
