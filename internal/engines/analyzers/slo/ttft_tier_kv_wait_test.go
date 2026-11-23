package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SwitchTTFTTierKVWait is planner-only: off from the constructor, and a
// replay can turn it on.
func TestTTFT_TheTierKVWaitOffFromTheConstructor(t *testing.T) {
	assert.False(t, New(nil, nil).ttftTierKVWait)
	assert.Contains(t, ReplaySwitchNames(), SwitchTTFTTierKVWait)
	a := New(nil, nil)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchTTFTTierKVWait: true}))
	assert.True(t, a.ttftTierKVWait)
}
