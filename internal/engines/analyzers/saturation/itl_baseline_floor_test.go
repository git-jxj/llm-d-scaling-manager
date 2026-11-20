package saturation

import (
	"encoding/json"
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

// floorCycles observes, under "key", cycles of three replicas at fill k on the line
// ITL = 0.05 k + 0.012 under adm, from start, and returns the last reading.
func floorCycles(a *SaturationAnalyzer, k float64, n int, adm itl.Admission, start time.Time) itl.Reading {
	var r itl.Reading
	for c := range n {
		rows := make([]domain.ReplicaMetrics, 0, 3)
		for i := range 3 {
			rows = append(rows, domain.ReplicaMetrics{PodName: fmt.Sprintf("p-%d", i), VariantName: "v", Ready: true,
				ReadySince: start.Add(-time.Hour), KvCacheUsage: k, KvUsageInstant: k, AvgITL: 0.05*k + 0.012})
		}
		r = a.itlTracker.Observe("key", "v", rows, adm, start.Add(time.Duration(c)*15*time.Second))
	}
	return r
}

// A forget of the learned state drops the floors below the band with the
// rest.
func TestITLBaselineFromFloor_AForgetDropsTheFloors(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	adm := itl.Admission{BaselineFromFloor: true}
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	floorCycles(a, 0.05, 20, adm, start)
	if r := floorCycles(a, 0.05, 1, adm, start.Add(20*15*time.Second)); !(r.Floor > 0) {
		t.Fatalf("premise: a floor was read; got %+v", r)
	}
	a.forgetLearnedState()
	r := floorCycles(a, 0.30, 1, adm, start.Add(21*15*time.Second))
	assert.Zero(t, r.Floor, "the floor outlived the forget")
}

// While the model's shape changes, a decode variant's floor is dropped with
// the switch on; off, or for an aggregated variant, or with no change, it is
// kept.
func TestITLBaselineFromFloor_AShapeChangeDropsADecodeFloor(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name, role            string
		on, changing, dropped bool
	}{
		{"decode, changing", domain.RoleDecode, true, true, true},
		{"decode, steady", domain.RoleDecode, true, false, false},
		{"switch off", domain.RoleDecode, false, true, false},
		{"aggregated", domain.RoleBoth, true, true, false},
	} {
		a := NewSaturationAnalyzer(capacity.NewStore())
		adm := itl.Admission{BaselineFromFloor: true}
		floorCycles(a, 0.05, 20, adm, start)
		a.floorOnShapeChange("key", c.role, itl.Admission{BaselineFromFloor: c.on}, c.changing)
		r := floorCycles(a, 0.30, 1, adm, start.Add(20*15*time.Second))
		assert.Equal(t, c.dropped, r.Floor == 0, "%s: floor %g", c.name, r.Floor)
	}
}

// A floor slope's B goes out in the snapshot and comes back paired with its
// A; a default-B slope's document has no floorB, as before the switch.
func TestITLBaselineFromFloor_ASnapshotKeepsTheFloorSlope(t *testing.T) {
	running := mtParams(64, "vllm:v1")
	now := time.Now()
	for _, floorB := range []float64{0.012, 0} {
		a, _, _ := waProcess(running)
		key, _, _ := itlStashKeyAndArgs(a)
		a.RestoreLearnedState(LearnedState{
			Config:        map[string]string{"test-ns/variant-a": running.Fingerprint()},
			Args:          map[string]string{"test-ns/variant-a": running.ArgsFingerprint()},
			VariantSeenAt: map[string]time.Time{key: now},
			ITLSlopes: map[string]SlopeSnapshot{key: {A: 0.05, FittedAt: now.Add(-time.Hour),
				Args: running.ArgsFingerprint(), FloorB: floorB}},
		})
		_, done := a.installRestored("test-ns", "test-model", "variant-a")
		require.True(t, done, "precondition: the variant's entries were pending")
		got, installed := a.itlTracker.Slopes()[key]
		require.True(t, installed)
		assert.InDelta(t, floorB, got.FloorB, 0, "restored paired with its B")
		out := a.exportLearnedState().ITLSlopes[key]
		assert.InDelta(t, floorB, out.FloorB, 0, "exported with its B")
		doc, err := json.Marshal(out)
		require.NoError(t, err)
		assert.Equal(t, floorB > 0, strings.Contains(string(doc), `"floorB"`), "%s", doc)
	}
}
