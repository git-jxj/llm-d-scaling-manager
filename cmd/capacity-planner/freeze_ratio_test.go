package main

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// risingWindows is 8 TPOT windows at each of two fills, the tail wider at the
// higher one.
func risingWindows() []whatif.TPOTWindow {
	out := make([]whatif.TPOTWindow, 0, 16)
	for _, s := range []struct{ k, ratio float64 }{{0.1, 1.2}, {0.5, 1.9}} {
		for range 8 {
			out = append(out, whatif.TPOTWindow{
				Buckets: []whatif.HistogramBucket{{UpperEdge: 0.95 * s.ratio * 0.01, Count: 0}, {UpperEdge: s.ratio * 0.01, Count: 50}, {UpperEdge: math.Inf(1), Count: 50}},
				Sum:     0.5, Count: 50, Fill: s.k, FillKnown: true,
			})
		}
	}
	return out
}

// The frozen file keeps R_q by fill through a write and a read, and a file
// written before it carried R_q by fill reads as the global ratio alone.
func TestFrozenRatio_ByFillRoundTrip(t *testing.T) {
	fr := frozenRatioOf(&learnreplay.State{TPOTWindows: risingWindows()}, 0.9)
	require.Empty(t, fr.Refused)
	require.Len(t, fr.ByFill, 5, "R_q by fill was not frozen")
	assert.InDelta(t, 1.2, fr.ByFill[0].Value, 1e-9)
	assert.InDelta(t, 1.9, fr.ByFill[2].Value, 1e-9)

	body, err := json.Marshal(fr)
	require.NoError(t, err)
	var back frozenRatio
	require.NoError(t, json.Unmarshal(body, &back))
	assert.Equal(t, fr, back)

	var old frozenRatio
	require.NoError(t, json.Unmarshal([]byte(`{"percentile":90,"value":1.8526921989632361,"flags":["CoarseTPOTBucket"]}`), &old))
	assert.InDelta(t, 1.8526921989632361, old.Value, 0)
	assert.Nil(t, old.ByFill)
	r := whatif.TPOTRatio{Value: old.Value, ByFill: old.ByFill}
	for _, k := range []float64{0, 0.3, 0.9} {
		at, global := r.At(k)
		assert.InDelta(t, old.Value, at, 0, "an old file prices its one ratio at fill %g", k)
		assert.False(t, global)
	}
}
