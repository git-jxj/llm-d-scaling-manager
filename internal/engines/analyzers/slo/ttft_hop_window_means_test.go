package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SwitchTTFTHopWindowMeans is planner-only: off from the constructor, so the
// controller reads the hop as before, and a replay can turn it on.
func TestTTFT_TheHopWindowMeansOffFromTheConstructor(t *testing.T) {
	a := New(nil, nil)
	fields := a.switchFields()
	require.Contains(t, fields, SwitchTTFTHopWindowMeans)
	assert.False(t, *fields[SwitchTTFTHopWindowMeans])
	assert.Contains(t, ReplaySwitchNames(), SwitchTTFTHopWindowMeans)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchTTFTHopWindowMeans: true}))
	assert.True(t, a.ttftHopWindowMeans)
}
