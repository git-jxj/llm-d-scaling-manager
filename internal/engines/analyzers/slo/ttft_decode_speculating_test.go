package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The speculating-row switch is off from the constructor: the controller reads
// no speculating decode row for D, as before. It is planner-only.
func TestTTFT_TheDecodeSpeculatingHeldOffFromTheConstructor(t *testing.T) {
	a := New(nil, nil)
	fields := a.switchFields()
	require.Contains(t, fields, SwitchTTFTDecodeSpeculatingHeld)
	assert.False(t, *fields[SwitchTTFTDecodeSpeculatingHeld])
	assert.Contains(t, ReplaySwitchNames(), SwitchTTFTDecodeSpeculatingHeld)
}
