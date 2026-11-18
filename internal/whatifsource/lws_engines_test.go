package whatifsource

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
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
		assert.True(t, e.Ready, e.PodName)
		if e.ITLState() == domain.ITLFinite {
			finite++
		}
	}
	assert.Equal(t, 7, finite, "seven of the eight engines read an ITL")
}

// An engine of a group whose own data-parallel ranks did not all read an
// ITL is unread: the line learns nothing from it, and the capture says so,
// counting the line's readings, an engine each, over the cycles.
func TestCapture_NotesAnEngineWhoseRankReadNoITL(t *testing.T) {
	srv := promtest.New()
	t.Cleanup(srv.Close)
	dec := decodeRole(everyPod)
	dec.itl, dec.ranks = everyPod, 2
	dec.rankITL = func(g, w, rank int) bool { return g != 1 || w != 2 || rank != 1 }
	dec.add(t, srv)
	prefillRole(everyPod).add(t, srv)
	_, sum := capture(t, Options{Prometheus: apiFor(t, srv), Namespace: testNS, From: t0, To: t0, Step: 15e9,
		KubeStateMetrics: true})
	assert.Equal(t, []string{"variant m-decode: ITL unread on 1 readings over 1 cycles: a data-parallel rank reported no ITL, " +
		"so the decode line learned nothing from them"}, sum.Notes)
}
