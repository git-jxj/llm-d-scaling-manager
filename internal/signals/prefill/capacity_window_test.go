package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// windowRun is three queued minutes at 20k tok/s, then minutes queued with
// no rate above 5k for longer than Retention, VPWindowBest on or off: a
// fleet that queued at its busiest hour and not after.
func windowRun(on bool, quiet int) Estimate {
	cs := queuedMinutes(20000, 20000, 20000)
	for range quiet {
		c := cycle(time.Time{}, queued("p-0", 5000, 0))
		cs = append(cs, c)
	}
	for i := range cs {
		cs[i].VPWindowBest = on
	}
	return run(NewObserver(), cs...)
}

// With VPWindowBest the peak under a queue read at the start of the window
// is kept after Retention ages its samples out, flagged with the last minute
// of its run; off, it ages out and V_P is refused, as the controller reads it.
func TestCapacity_TheWindowKeepsAnOldQueuedPeak(t *testing.T) {
	quiet := int(Retention/minute) + 30
	on := windowRun(true, quiet)
	require.Empty(t, on.VPRefused, "%+v", on)
	assert.Equal(t, 20000.0, on.VP)
	assert.Equal(t, SourcePeakUnderQueue, on.VPSource)
	assert.True(t, on.VPLowerBound)
	assert.Equal(t, t0.Add(2*minute), on.VPLearnedAt, "the run's last minute")

	off := windowRun(false, quiet)
	assert.Equal(t, NoPrefillCapacity, off.VPRefused)

	// Inside Retention the samples kept hold the peak: no age is given.
	recent := windowRun(true, 30)
	assert.Equal(t, 20000.0, recent.VP)
	assert.True(t, recent.VPLearnedAt.IsZero())
}

// A peak the samples kept still hold is never taken from the memory, even
// where it is higher than the current reading: only Retention ages a peak
// out. A recording shorter than Retention reads as without the switch.
func TestCapacity_TheMemoryIsOnlyForAgedPeaks(t *testing.T) {
	o := NewObserver()
	var est Estimate
	cs := queuedMinutes(20000, 20000, 20000)
	for i := range cs {
		cs[i].VPWindowBest = true
		cs[i].Now = t0.Add(time.Duration(i) * minute)
		est = o.Observe(cs[i])
	}
	require.Equal(t, 20000.0, est.VP)
	// A later cycle in the last minute replaces its sample with a lower
	// rate: the run in the samples kept now reads 15k.
	c := cycle(t0.Add(2*minute+30*time.Second), queued("p-0", 15000, 3))
	c.VPWindowBest = true
	est = o.Observe(c)
	assert.Equal(t, 15000.0, est.VP, "the samples kept, as without the switch")
	assert.True(t, est.VPLearnedAt.IsZero())
}

// A newer peak under a queue that is higher replaces the one kept; a lower
// one does not.
func TestCapacity_ANewerHigherPeakWins(t *testing.T) {
	quiet := int(Retention/minute) + 30
	build := func(later float64) Estimate {
		cs := queuedMinutes(20000, 20000, 20000)
		for range quiet {
			cs = append(cs, cycle(time.Time{}, queued("p-0", 5000, 0)))
		}
		cs = append(cs, queuedMinutes(later, later, later)...)
		for i := range cs {
			cs[i].VPWindowBest = true
		}
		return run(NewObserver(), cs...)
	}
	higher := build(30000)
	assert.Equal(t, 30000.0, higher.VP)
	assert.True(t, higher.VPLearnedAt.IsZero(), "the current peak, in the samples kept")
	lower := build(10000)
	assert.Equal(t, 20000.0, lower.VP, "the window's best")
	assert.False(t, lower.VPLearnedAt.IsZero())
}
