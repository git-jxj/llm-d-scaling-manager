package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// floorRun is three queued minutes at a light 5k tok/s, then six minutes at
// 20k tok/s with the queue empty, the floor on or off: the fleet of issue
// llm-d-incubation/llm-d-scaling-manager#280, whose pods queue for capacity
// only at a light load.
func floorRun(on bool) Estimate {
	cs := queuedMinutes(5000, 5000, 5000, 20000, 20000, 20000, 20000, 20000, 20000)
	for i := range cs {
		if i >= 3 {
			cs[i].Rows[0].QueueLength = 0
		}
		cs[i].VPFloorServed = on
	}
	return run(NewObserver(), cs...)
}

// A lower-bound V_P is floored at the highest rate a pod was seen to
// sustain, queued or not: a pod never computes faster than V_P. The source
// stays the peak under a queue, and V_P stays a lower bound.
func TestCapacity_ALowerBoundIsFlooredAtTheServedPeak(t *testing.T) {
	on := floorRun(true)
	assert.Equal(t, 20000.0, on.VP)
	assert.Equal(t, SourcePeakUnderQueue, on.VPSource)
	assert.True(t, on.VPLowerBound)
	assert.True(t, on.VPFlooredAtServed)
	assert.Equal(t, 20000.0, on.VPServedPeak)

	off := floorRun(false)
	assert.Equal(t, 5000.0, off.VP, "off, the peak under a queue, as before the switch")
	assert.False(t, off.VPFlooredAtServed)
	assert.Equal(t, 20000.0, off.VPServedPeak, "the served peak is read beside it either way")
}

// The floor never lowers V_P: a served peak under the peak under a queue
// leaves it.
func TestCapacity_TheFloorNeverLowersAPeak(t *testing.T) {
	cs := queuedMinutes(30000, 30000, 30000, 10000, 10000, 10000)
	for i := range cs {
		if i >= 3 {
			cs[i].Rows[0].QueueLength = 0
		}
		cs[i].VPFloorServed = true
	}
	est := run(NewObserver(), cs...)
	assert.Equal(t, 30000.0, est.VP)
	assert.False(t, est.VPFlooredAtServed)
}

// Busy minutes of a single-rank pod are V_P itself, not a bound, and are
// not floored, even under a higher served minute.
func TestCapacity_BusyMinutesAreNotFloored(t *testing.T) {
	o := NewObserver()
	var est Estimate
	for i := range 3 {
		c := cycle(t0.Add(time.Duration(i)*minute), queued("p-0", 40000, 0))
		c.VPFloorServed = true
		o.Observe(c)
	}
	for i := range 5 {
		c := cycle(t0.Add(10*minute+time.Duration(i)*minute), queued("p-0", 10000, 3))
		c.QueueMin = map[string]float64{"p-0": 2}
		c.VPFloorServed = true
		est = o.Observe(c)
	}
	assert.Equal(t, SourceBusyMinutes, est.VPSource)
	assert.Equal(t, 10000.0, est.VP)
	assert.False(t, est.VPLowerBound)
	assert.False(t, est.VPFlooredAtServed)
}

// A fleet that never queued stays refused: with no source that bounds V_P,
// a peak with no queue cannot tell capacity from the offered load.
func TestCapacity_NoQueueStaysRefused(t *testing.T) {
	cs := queuedMinutes(30000, 30000, 30000, 30000)
	for i := range cs {
		cs[i].Rows[0].QueueLength = 0
		cs[i].VPFloorServed = true
	}
	est := run(NewObserver(), cs...)
	assert.Equal(t, NoPrefillCapacity, est.VPRefused)
	assert.Zero(t, est.VP)
	assert.False(t, est.VPFlooredAtServed)
}

// floorCycles is three queued minutes at 5k tok/s on pods, then one cycle
// per entry of served, each a minute after the last unless at says when (in
// minutes from t0), the queues empty and the floor on. served[i][j] is pod
// j's rate.
func floorCycles(pods []string, at []int, served ...[]float64) []Cycle {
	out := make([]Cycle, 0, 3+len(served))
	for i := range 3 {
		rows := make([]domain.ReplicaMetrics, 0, len(pods))
		for _, p := range pods {
			rows = append(rows, queued(p, 5000, 3))
		}
		out = append(out, cycle(t0.Add(time.Duration(i)*minute), rows...))
	}
	for i, rates := range served {
		rows := make([]domain.ReplicaMetrics, 0, len(pods))
		for j, p := range pods {
			rows = append(rows, queued(p, rates[j], 0))
		}
		m := 3 + i
		if at != nil {
			m = at[i]
		}
		out = append(out, cycle(t0.Add(time.Duration(m)*minute), rows...))
	}
	for i := range out {
		out[i].VPFloorServed = true
	}
	return out
}

// The served peak is a run's least minute: one high minute between two
// lower ones is not the peak.
func TestCapacity_TheServedPeakCutsOneHighMinute(t *testing.T) {
	est := run(NewObserver(), floorCycles([]string{"p-0"}, nil, []float64{20000}, []float64{40000}, []float64{20000})...)
	assert.Equal(t, 20000.0, est.VP)
	assert.True(t, est.VPFlooredAtServed)
	assert.Equal(t, PeakRun, est.VPSamples)
}

// A gap wider than MaxRunGap ends a served run, as it ends a queued one: two
// minutes at 30k, a gap, then 30k, 30k, 10k sustain only 10k.
func TestCapacity_AGapEndsAServedRun(t *testing.T) {
	est := run(NewObserver(), floorCycles([]string{"p-0"}, []int{3, 4, 8, 9, 10},
		[]float64{30000}, []float64{30000}, []float64{30000}, []float64{30000}, []float64{10000})...)
	assert.Equal(t, 10000.0, est.VP)
	assert.True(t, est.VPFlooredAtServed)
}

// A served run is one pod's: two pods taking turns at 30k each sustain only
// 10k, not the 30k some pod read every minute.
func TestCapacity_AServedRunIsOnePods(t *testing.T) {
	est := run(NewObserver(), floorCycles([]string{"p-0", "p-1"}, nil,
		[]float64{30000, 10000}, []float64{10000, 30000}, []float64{30000, 10000},
		[]float64{10000, 30000}, []float64{30000, 10000})...)
	assert.Equal(t, 10000.0, est.VP)
	assert.Equal(t, 10000.0, est.VPServedPeak)
}
