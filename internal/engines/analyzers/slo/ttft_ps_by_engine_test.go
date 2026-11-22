package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SwitchTTFTPSByEngine is planner-only: off from the constructor, and a
// replay can turn it on.
func TestTTFT_ThePSByEngineOffFromTheConstructor(t *testing.T) {
	assert.False(t, New(nil, nil).ttftPSByEngine)
	assert.Contains(t, ReplaySwitchNames(), SwitchTTFTPSByEngine)
	a := New(nil, nil)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchTTFTPSByEngine: true}))
	assert.True(t, a.ttftPSByEngine)
}
