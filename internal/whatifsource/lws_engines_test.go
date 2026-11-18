package whatifsource

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A group whose Pods each run an engine keeps every engine's row beside the
// group's, and the ITL line reads those (domain.EngineRows): an engine that
// read no ITL costs its own reading, not the group's.
func TestCapture_AGroupKeepsItsEnginesForTheLine(t *testing.T) {
	dec := decodeRole(everyPod)
	dec.itl = func(g, w int) bool { return g != 1 || w != 2 }
	d := lwsFleetCapture(t, dec, prefillRole(everyPod))
	require.Len(t, d.cycles, 1)
	rows := d.cycles[0].Input.ReplicaMetrics
	require.Len(t, rows, 3, "one row per group")
	var lead domain.ReplicaMetrics
	for _, r := range rows {
		if r.PodName == "m-abc-decode-1" {
			lead = r
		}
	}
	require.Len(t, lead.GroupEngines, 4, "the group's four engines")
	assert.Equal(t, domain.ITLAbsent, lead.ITLState(), "the group's merged ITL is unread: an engine read none")

	finite := 0
	for _, e := range domain.EngineRows(rows) {
		if e.VariantName != "m-decode" {
			continue
		}
		assert.Len(t, e.GroupEngines, 0, e.PodName)
		assert.True(t, e.Ready, e.PodName)
		if e.ITLState() == domain.ITLFinite {
			finite++
		}
	}
	assert.Equal(t, 7, finite, "seven of the eight engines read an ITL")
}
