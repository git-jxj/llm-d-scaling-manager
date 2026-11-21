package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
