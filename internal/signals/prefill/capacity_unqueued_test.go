package prefill

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// unqueuedRun is four minutes at 30k tok/s with no queue, VPServedUnqueued
// on or off (VPFloorServed on, as the planner runs it).
func unqueuedRun(on bool, rates ...float64) Estimate {
	cs := queuedMinutes(rates...)
	for i := range cs {
		cs[i].Rows[0].QueueLength = 0
		cs[i].VPFloorServed = true
		cs[i].VPServedUnqueued = on
	}
	return run(NewObserver(), cs...)
}

// A fleet that never held a queue gets V_P as the served peak, a lower
// bound (NeverQueued), with VPServedUnqueued; off, it stays refused, as
// before the switch.
func TestCapacity_NoQueueTakesTheServedPeakAsALowerBound(t *testing.T) {
	on := unqueuedRun(true, 30000, 30000, 30000, 30000)
	assert.Empty(t, on.VPRefused)
	assert.Equal(t, 30000.0, on.VP)
	assert.Equal(t, SourceServedPeak, on.VPSource)
	assert.True(t, on.VPLowerBound)
	assert.Equal(t, NeverQueued, on.VPLowerBoundReason)
	assert.Equal(t, 30000.0, on.VPServedPeak)
	assert.Equal(t, PeakRun, on.VPSamples)
	assert.False(t, on.VPFlooredAtServed, "the source itself, not a floor raised from another")

	off := unqueuedRun(false, 30000, 30000, 30000, 30000)
	assert.Equal(t, NoPrefillCapacity, off.VPRefused)
	assert.Zero(t, off.VP)
}

// The served peak is sustained over PeakRun minutes: the least minute of the
// best run of three, never one high minute; with fewer than three minutes
// there is no run and V_P stays refused.
func TestCapacity_TheUnqueuedPeakIsSustained(t *testing.T) {
	est := unqueuedRun(true, 10000, 40000, 12000, 15000, 14000)
	assert.Equal(t, 12000.0, est.VP, "runs 10/40/12, 40/12/15, 12/15/14: their least minutes 10, 12, 12")
	short := unqueuedRun(true, 40000, 40000)
	assert.Equal(t, NoPrefillCapacity, short.VPRefused)
}

// A queued source wins over the served peak whatever their rates: the
// switch only fills where every other source is silent.
func TestCapacity_AQueuedSourceWinsOverTheUnqueuedPeak(t *testing.T) {
	cs := queuedMinutes(5000, 5000, 5000, 20000, 20000, 20000)
	for i := range cs {
		if i >= 3 {
			cs[i].Rows[0].QueueLength = 0
		}
		cs[i].VPServedUnqueued = true
	}
	est := run(NewObserver(), cs...)
	assert.Equal(t, SourcePeakUnderQueue, est.VPSource)
	assert.Equal(t, 5000.0, est.VP, "the floor is VPFloorServed's, off here")
	assert.Empty(t, est.VPLowerBoundReason)
}
