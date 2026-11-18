package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// EngineRows puts a group's engines in place of its row, each with its
// group's variant, readiness, warm-pool mark and settle, and leaves every
// other row as it is; with no group it is the input itself.
func TestEngineRows(t *testing.T) {
	plain := []ReplicaMetrics{{PodName: "a", VariantName: "v", Ready: true}}
	got := EngineRows(plain)
	assert.Equal(t, plain, got)
	assert.Same(t, &plain[0], &got[0], "no group: the input, not a copy")

	group := ReplicaMetrics{PodName: "g-0", VariantName: "v", Ready: false, FromWarmPool: true, AvgITL: 0.02,
		GroupEngines: []ReplicaMetrics{{PodName: "g-0", Ready: true, AvgITL: 0.01}, {PodName: "g-0-1", Ready: true, AvgITL: 0.03}}}
	rows := []ReplicaMetrics{plain[0], group}
	got = EngineRows(rows)
	require.Len(t, got, 3)
	assert.Equal(t, plain[0], got[0])
	for i, want := range []struct {
		pod string
		itl float64
	}{{"g-0", 0.01}, {"g-0-1", 0.03}} {
		e := got[1+i]
		assert.Equal(t, want.pod, e.PodName)
		assert.InDelta(t, want.itl, e.AvgITL, 0, "the engine's own reading")
		assert.Equal(t, "v", e.VariantName)
		assert.False(t, e.Ready, "a group not Ready gives no engine Ready")
		assert.True(t, e.FromWarmPool)
		assert.Equal(t, "g-0", e.EngineOf, "it settles as its group")
		assert.Nil(t, e.GroupEngines)
	}
	assert.Empty(t, rows[1].EngineOf, "the input is not modified")
	assert.True(t, rows[1].GroupEngines[0].Ready, "the input's engines are not modified")
}
