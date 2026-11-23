package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SwitchTTFTTierLoad is planner-only: off from the constructor, and a
// replay can turn it on.
func TestTTFT_TheTierLoadOffFromTheConstructor(t *testing.T) {
	assert.False(t, New(nil, nil).ttftTierLoad)
	assert.Contains(t, ReplaySwitchNames(), SwitchTTFTTierLoad)
	a := New(nil, nil)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchTTFTTierLoad: true}))
	assert.True(t, a.ttftTierLoad)
}
