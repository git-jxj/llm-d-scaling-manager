package saturation

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
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

// The itl-fit line is as it was with the switch off, and names the floor
// with it on.
func TestITLBaselineFromFloor_TheFitLineIsUnchangedOff(t *testing.T) {
	fitLine := func(on bool) string {
		a := NewSaturationAnalyzer(capacity.NewStore())
		var lines []string
		logger := funcr.New(func(prefix, args string) { lines = append(lines, prefix+" "+args) }, funcr.Options{Verbosity: 2})
		start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
		for c, k := range []float64{0.30, 0.35, 0.40, 0.35} {
			rows := make([]domain.ReplicaMetrics, 0, 3)
			for i := range 3 {
				rows = append(rows, domain.ReplicaMetrics{PodName: fmt.Sprintf("p-%d", i), VariantName: "v", Ready: true,
					ReadySince: start.Add(-time.Hour), KvCacheUsage: k, KvUsageInstant: k, AvgITL: 0.05*k + 0.012})
			}
			a.noteITL("key", rows, "v", itl.Admission{BaselineFromFloor: on}, start.Add(time.Duration(c)*15*time.Second), logger)
		}
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.Contains(lines[i], `"itl-fit"`) {
				return lines[i]
			}
		}
		t.Fatalf("on %v: no itl-fit line in %v", on, lines)
		return ""
	}
	off, on := fitLine(false), fitLine(true)
	assert.NotContains(t, off, "pinnedAtFloor")
	assert.NotContains(t, off, "floorK")
	assert.Contains(t, on, `"pinnedAtFloor"=false`)
	assert.Contains(t, on, `"floorK"=0`)
}
