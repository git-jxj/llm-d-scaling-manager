package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The B-from-floor switch is off from the constructor, so the controller
// keeps the default B; a replay turns it on.
func TestITLBaselineFromFloor_OffFromTheConstructor(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	fields := a.switchFields()
	require.Contains(t, fields, SwitchITLBaselineFromFloor)
	assert.False(t, *fields[SwitchITLBaselineFromFloor], "off from the constructor")
	assert.False(t, a.baselineFromFloorOn())
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchITLBaselineFromFloor: true}))
	assert.True(t, a.baselineFromFloorOn(), "a replay turns it on")
}
