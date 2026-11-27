package whatifapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kvCycle is a cycle at rate r/s whose pods decode pods ran at a busiest
// fill of busiest and a mean fill of mean.
func kvCycle(rate float64, pods int, busiest, mean float64) SeriesSample {
	return SeriesSample{Rate: rate, Decode: pods, DecodeKVPods: pods, DecodeKVBusiest: busiest, DecodeKVMean: mean}
}

// One hot decode pod is reported: each cycle's busiest fill over the mean,
// its median, p95 and peak, the median at the load judged, and the fill
// peaks, over the cycles with two pods or more and a mean fill of
// MinKVSpreadFill or more (issue llm-d-incubation/llm-d-scaling-manager#483).
func TestDecodeKVSpreadOfOneHotPod(t *testing.T) {
	series := []SeriesSample{
		kvCycle(2, 4, 0.30, 0.25), // 1.2
		kvCycle(4, 4, 0.66, 0.44), // 1.5
		kvCycle(8, 4, 0.95, 0.50), // 1.9
		kvCycle(9, 4, 0.98, 0.49), // 2.0
		kvCycle(1, 4, 0.04, 0.01), // under MinKVSpreadFill: noise
		kvCycle(9, 1, 0.90, 0.90), // one pod: no spread
		{Rate: 9, Decode: 4},      // no KV read
		kvCycle(0, 4, 0.20, 0.20), // idle, read in the window
	}
	got := DecodeKVSpreadOf(series, LoadP95, 8)
	require.NotNil(t, got)
	assert.Equal(t, 5, got.Cycles)
	assert.InDelta(t, 1.5, got.Median, 1e-9)
	assert.InDelta(t, 2.0, got.P95, 1e-9)
	assert.InDelta(t, 2.0, got.Peak, 1e-9)
	require.NotNil(t, got.AtLoad)
	assert.Equal(t, 2, got.AtLoadCycles, "the cycles at or above 8 r/s")
	assert.InDelta(t, 1.95, *got.AtLoad, 1e-9)
	assert.InDelta(t, 0.98, got.BusiestFillPeak, 1e-9, "past the 0.8 KV threshold")
	assert.InDelta(t, 0.50, got.MeanFillPeak, 1e-9, "which the mean pod never reaches")

	mean := DecodeKVSpreadOf(series, LoadMean, 3)
	require.NotNil(t, mean.AtLoad)
	assert.Equal(t, 4, mean.AtLoadCycles, "every cycle with traffic")

	stated := DecodeKVSpreadOf(series, LoadP95, 0)
	assert.Nil(t, stated.AtLoad, "no observed load")

	assert.Nil(t, DecodeKVSpreadOf([]SeriesSample{kvCycle(5, 1, 0.9, 0.9), kvCycle(5, 3, 0.03, 0.02)}, LoadP95, 5),
		"no cycle with two pods filled")
}

// A report carries the spread on every shape's decode utilization, and no
// figure moves: the KV fill stays the mean pod's.
func TestReportDecodeKVSpreadIsReportOnly(t *testing.T) {
	r := overReport()
	r.Shapes = append(r.Shapes, r.Shapes[0])
	before := r.Shapes[0].Utilization.Decode.KVFill
	r.ReportDecodeKVSpread([]SeriesSample{kvCycle(10, 2, 0.9, 0.6), kvCycle(12, 2, 0.8, 0.5)})
	for _, s := range r.Shapes {
		k := s.Utilization.Decode.KVSpread
		require.NotNil(t, k)
		assert.Equal(t, 2, k.Cycles)
		require.NotNil(t, k.AtLoad)
		assert.InDelta(t, (1.5+1.6)/2, *k.AtLoad, 1e-9)
		assert.Equal(t, before, s.Utilization.Decode.KVFill)
	}
	assert.NotSame(t, r.Shapes[0].Utilization.Decode.KVSpread, r.Shapes[1].Utilization.Decode.KVSpread)

	r = overReport()
	r.ReportDecodeKVSpread([]SeriesSample{kvCycle(10, 1, 0.9, 0.9)})
	assert.Nil(t, r.Shapes[0].Utilization.Decode.KVSpread, "nothing measured, nothing reported")
}
