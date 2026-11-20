package prefill

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lightThenBusy is twenty light minutes of two pods at 3:1, about 150
// requests a pod, then ten busy ones at 1.1:1, about 1500 a pod: the
// router's spread at light load and near capacity, as on the public
// staircase (issue llm-d-incubation/llm-d-scaling-manager#266).
func lightThenBusy(seed uint64, fBusy bool) []Cycle {
	rng := rand.New(rand.NewPCG(seed, 11))
	cs := slices.Concat(sharedMinutes(rng, 20, 150, 3, 1), sharedMinutes(rng, 10, 1500, 1.1, 1))
	for i := range cs {
		cs[i].Now = t0.Add(time.Duration(i) * minute)
		cs[i].FBusyMinutes = fBusy
	}
	return cs
}

// With FBusyMinutes, f is the median of the busy minutes alone, at every
// pod count too; without it (the negative control), the light minutes are
// most of the window and set the median. Every minute is still counted.
func TestImbalance_TheBusiestMinutesSetF(t *testing.T) {
	for seed := range uint64(10) {
		on := run(NewObserver(), lightThenBusy(seed, true)...)
		off := run(NewObserver(), lightThenBusy(seed, false)...)
		require.Empty(t, on.FRefused)
		require.Empty(t, off.FRefused)
		assert.InDelta(t, 1.03, on.F, 0.03, "seed %d: on, the busy minutes' f", seed)
		assert.InDelta(t, 1.5, off.F, 0.12, "seed %d: off, the light minutes' f", seed)
		assert.InDelta(t, on.F, on.FByPods[2], 1e-12, "seed %d: f at two pods is of the busy minutes too", seed)
		assert.InDelta(t, off.F, off.FByPods[2], 1e-12, "seed %d", seed)
		assert.Equal(t, 30, on.FSamples, "seed %d: every minute is counted", seed)
		assert.Equal(t, map[int]int{2: 30}, on.FSamplesByPods, "seed %d", seed)
	}
}

// Short of MinImbalanceMinutes at half the peak, the MinImbalanceMinutes
// busiest are taken, so a window that gives f with the switch off gives one
// on; at or past it, every minute at half the peak, the peak's own included.
func TestImbalance_TooFewBusyMinutesTakeTheBusiest(t *testing.T) {
	loads := []float64{150, 160, 1500, 170, 180, 1400, 190, 200}
	fs := make([]fSample, 0, len(loads)+4)
	for i, load := range loads {
		fs = append(fs, fSample{f: float64(i), load: load, pods: 2})
	}
	got := busiestMinutes(fs)
	require.Len(t, got, MinImbalanceMinutes)
	per := make([]float64, 0, len(got))
	for _, s := range got {
		per = append(per, s.load)
	}
	assert.Equal(t, []float64{1500, 1400, 200, 190, 180}, per)

	for i := range 4 {
		fs = append(fs, fSample{f: 9, load: 750 + float64(i), pods: 2})
	}
	got = busiestMinutes(fs)
	require.Len(t, got, 6, "the two near the peak and four at half of it or more")
	for _, s := range got {
		assert.GreaterOrEqual(t, s.load, BusyMinuteShare*1500)
	}
	assert.Equal(t, []float64{0, 1, 2, 3, 4, 5, 6, 7}, minuteFs(fs[:8], false), "off, every minute in its order")
}

// Busy is prefill work, not requests: minutes of many short prompts and
// minutes of fewer long ones, the long ones the more computed tokens. f is
// the long prompts' minutes', not the short ones', though those carry more
// requests.
func TestImbalance_BusyIsComputedTokensNotRequests(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	short := sharedMinutes(rng, 10, 900, 3, 1) // 1000 tokens a request
	long := sharedMinutes(rng, 10, 300, 1.1, 1)
	for i := range long {
		for j := range long[i].Rows {
			long[i].Rows[j].PrefillComputedTokenRate *= 8 // 8000 tokens a request
		}
	}
	cs := slices.Concat(short, long)
	for i := range cs {
		cs[i].Now = t0.Add(time.Duration(i) * minute)
		cs[i].FBusyMinutes = true
	}
	est := run(NewObserver(), cs...)
	require.Empty(t, est.FRefused)
	assert.Less(t, est.F, 1.15, "the long prompts' minutes, the busier by computed tokens")
}
