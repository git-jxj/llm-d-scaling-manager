package slo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// lowLoadStep is one cycle of a decode variant: its engine TTFT and the
// requests it holds waiting on its own capacity.
type lowLoadStep struct {
	ttft    float64
	waiting int
}

// overload is a calm window at 0.12 s, an overload whose queue reads 9 s,
// then the queue drained while the one-minute TTFT mean still reads it, as
// on the staircase recordings.
var overload = []lowLoadStep{
	{0.12, 0}, {0.12, 0}, {0.12, 0}, {0.12, 0}, {0.12, 0},
	{9, 136}, {17, 140}, {6.8, 0}, {3, 0}, {1, 0},
}

// noteSteps folds steps into a fresh analyzer's window, 15 s apart from
// ttftNow, the switch set to on, and returns each cycle's figure.
func noteSteps(t *testing.T, on bool, steps []lowLoadStep) []whatif.DecodeTTFT {
	t.Helper()
	a := New(nil, nil)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchTTFTLowLoadDecodeTTFT: on}))
	out := make([]whatif.DecodeTTFT, 0, len(steps))
	for i, s := range steps {
		cycle := whatif.DecodeTTFT{Seconds: s.ttft, PromptTokens: 2000, Waiting: s.waiting}
		out = append(out, a.noteDecodeTTFT("k", cycle, ttftNow.Add(time.Duration(i)*15*time.Second)))
	}
	return out
}

// After an overload the window's largest is the queue's wait. On, the calm
// figure is held through it; off, as before, the queue's 17 s is priced.
// Neither ever carries Waiting: S1 does not refuse on it.
func TestTTFT_TheLowLoadDecodeTTFTSwitch(t *testing.T) {
	off, on := noteSteps(t, false, overload), noteSteps(t, true, overload)
	last := len(overload) - 1
	assert.InDelta(t, 17, off[last].Seconds, 1e-12, "off: the queue's wait")
	assert.InDelta(t, 0.12, on[last].Seconds, 1e-12, "on: the calm figure, held")
	for i := range overload {
		assert.Zero(t, off[i].Waiting, "cycle %d", i)
		assert.Zero(t, on[i].Waiting, "cycle %d", i)
		if i < 5 {
			assert.Equal(t, off[i], on[i], "cycle %d: a calm window reads the same either way", i)
		}
	}

	// Past prefill.DecodeTTFTLag the cycles are calm again, and the figure
	// is theirs, the largest of the calm ones even with the queue's still in
	// the window.
	steps := append([]lowLoadStep(nil), overload...)
	for range 6 {
		steps = append(steps, lowLoadStep{0.15, 0})
	}
	on = noteSteps(t, true, steps)
	require.Greater(t, time.Duration(len(steps)-1-6)*15*time.Second, prefill.DecodeTTFTLag,
		"the last cycle is past the lag after the last backlog, cycle 6")
	assert.InDelta(t, 0.15, on[len(on)-1].Seconds, 1e-12)
}

// With no calm cycle in the window the held figure stands for
// prefill.HeldDecodeAge, then the window's largest, as with the switch off: a
// fleet that queues that long has no calm figure to speak of.
func TestTTFT_TheLowLoadDecodeTTFTHoldAgesOut(t *testing.T) {
	steps := append([]lowLoadStep(nil), overload[:5]...)
	loaded := int(prefill.HeldDecodeAge/(15*time.Second)) + whatif.TTFTWindowCycles + 1
	for range loaded {
		steps = append(steps, lowLoadStep{9, 136})
	}
	on := noteSteps(t, true, steps)
	// Cycle 4 is the last calm one, and cycle 8 the last whose window holds
	// it: the hold is aged from there.
	lastCalm := 4 + whatif.TTFTWindowCycles - 1
	for i := 5; i < len(steps); i++ {
		age := time.Duration(max(i-lastCalm, 0)) * 15 * time.Second
		if age <= prefill.HeldDecodeAge {
			assert.InDelta(t, 0.12, on[i].Seconds, 1e-12, "cycle %d, %s after the calm one: held", i, age)
			continue
		}
		assert.InDelta(t, 9, on[i].Seconds, 1e-12, "cycle %d, %s after: the window's largest", i, age)
		assert.Zero(t, on[i].Waiting)
	}

	// An idle spell reads nothing, as before: no figure, held or not.
	idle := append(append([]lowLoadStep(nil), overload[:5]...), make([]lowLoadStep, whatif.TTFTWindowCycles)...)
	got := noteSteps(t, true, idle)
	assert.Equal(t, whatif.DecodeTTFT{}, got[len(got)-1])
}

// The switch is off from the constructor, and off a backlog passed in is not
// read: the figure is the window's largest, Waiting 0.
func TestTTFT_TheLowLoadDecodeTTFTOffFromTheConstructor(t *testing.T) {
	a := New(nil, nil)
	assert.False(t, a.ttftLowLoadDecodeTTFT)
	got := a.noteDecodeTTFT("k", whatif.DecodeTTFT{Seconds: 9, PromptTokens: 2000, Waiting: 136}, ttftNow)
	assert.Equal(t, whatif.DecodeTTFT{Seconds: 9, PromptTokens: 2000}, got)
}

// Through Analyze: the backlog is read off the decode rows' capacity queue,
// so with the switch on the fleet is priced at the calm figure after an
// overload; off, at the queue's, which puts the 3 s target below the floor.
func TestTTFT_TheLowLoadDecodeTTFTThroughAnalyze(t *testing.T) {
	priced := func(on bool) (whatif.DecodeTTFT, string, float64) {
		var last ttftInputs
		onTTFTPriced = func(in ttftInputs, _ float64, _ whatif.TTFTTarget) { last = in }
		t.Cleanup(func() { onTTFTPriced = nil })
		r := newTTFTRig(ttftEstimate(), nil)
		r.in.VariantStates = append(r.in.VariantStates, decState)
		require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchTTFTLowLoadDecodeTTFT: on}))
		var reading metrics.SLOTTFTReading
		for _, s := range overload {
			dec := decodeRows("dec", 2)
			for i := range dec {
				dec[i].AvgTTFT, dec[i].AvgTTFTKnown = s.ttft, true
				dec[i].CapacityQueueLength, dec[i].CapacityQueueKnown = s.waiting, true
			}
			r.in.ReplicaMetrics = append(prefillRows("pre", 4000), dec...)
			reading = r.one(t)
			r.now = r.now.Add(15 * time.Second)
		}
		return last.decodeTTFT, reading.Reason, reading.Predicted
	}
	offTTFT, offReason, _ := priced(false)
	onTTFT, onReason, onPredicted := priced(true)
	assert.InDelta(t, 17, offTTFT.Seconds, 1e-12)
	assert.Equal(t, constants.SLOTTFTReasonTargetBelowFloor, offReason, "off, the queue's wait is the floor")
	assert.InDelta(t, 0.12, onTTFT.Seconds, 1e-12)
	assert.Equal(t, constants.SLOTTFTReasonPriced, onReason)
	assert.Less(t, onPredicted, 3.0, "on, the target is met")
}

// Rows that do not say why their requests wait read no backlog: the switch
// on reads such a fleet as before.
func TestTTFT_TheLowLoadDecodeTTFTUnreadRows(t *testing.T) {
	r := newTTFTRig(ttftEstimate(), nil)
	r.in.VariantStates = append(r.in.VariantStates, decState)
	require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchTTFTLowLoadDecodeTTFT: true}))
	var last ttftInputs
	onTTFTPriced = func(in ttftInputs, _ float64, _ whatif.TTFTTarget) { last = in }
	t.Cleanup(func() { onTTFTPriced = nil })
	for _, s := range overload {
		dec := decodeRows("dec", 2)
		for i := range dec {
			dec[i].AvgTTFT, dec[i].AvgTTFTKnown, dec[i].QueueLength = s.ttft, true, s.waiting
		}
		r.in.ReplicaMetrics = append(prefillRows("pre", 4000), dec...)
		r.one(t)
		r.now = r.now.Add(15 * time.Second)
	}
	assert.InDelta(t, 17, last.decodeTTFT.Seconds, 1e-12)
}
