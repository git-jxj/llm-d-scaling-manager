package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// servedAt is cycles 15 s apart from t, each the given sum at pods, every pod
// read.
func servedAt(w *servedWork, t time.Time, pods int, sums ...float64) time.Time {
	for _, s := range sums {
		w.note(servedWorkSample{at: t, pods: pods, sum: s, ok: true})
		t = t.Add(15 * time.Second)
	}
	return t
}

// The peak is the highest mean over a stretch of ServedWorkWindow: nine
// cycles 15 s apart span two minutes. One high cycle does not make it.
func TestServedWork_TheBusiestStretch(t *testing.T) {
	var w servedWork
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := servedAt(&w, t0, 2, 10, 10, 10, 10, 10, 10, 10, 10, 10)
	at = servedAt(&w, at, 2, 100)
	servedAt(&w, at, 2, 10, 10, 10, 10, 10, 10, 10, 10, 10)
	got := w.peaks()
	assert.InDelta(t, (8*10+100)/9.0, got[2], 1e-9, "the stretch holding the spike, averaged over nine cycles")
	assert.Len(t, got, 1)
}

// A stretch shorter than the window, broken by a gap, by a cycle with a pod
// unread, or by a change of pod count, is no reading.
func TestServedWork_BrokenStretchesAreNoReading(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var short servedWork
	servedAt(&short, t0, 2, 50, 50, 50, 50, 50, 50, 50, 50)
	assert.Empty(t, short.peaks(), "eight cycles span 105 s")

	var gap servedWork
	at := servedAt(&gap, t0, 2, 50, 50, 50, 50)
	servedAt(&gap, at.Add(2*time.Minute), 2, 50, 50, 50, 50, 50)
	assert.Empty(t, gap.peaks())

	var unread servedWork
	at = servedAt(&unread, t0, 2, 50, 50, 50, 50)
	unread.note(servedWorkSample{at: at, pods: 2, sum: 50})
	servedAt(&unread, at.Add(15*time.Second), 2, 50, 50, 50, 50)
	assert.Empty(t, unread.peaks())

	var counts servedWork
	at = servedAt(&counts, t0, 2, 50, 50, 50, 50, 50)
	servedAt(&counts, at, 3, 50, 50, 50, 50, 50)
	assert.Empty(t, counts.peaks())
}

// Work older than prefill.Retention is forgotten, as V_P's samples are.
func TestServedWork_KeepsRetention(t *testing.T) {
	var w servedWork
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	servedAt(&w, t0, 2, 90, 90, 90, 90, 90, 90, 90, 90, 90)
	servedAt(&w, t0.Add(prefill.Retention+time.Hour), 2, 20, 20, 20, 20, 20, 20, 20, 20, 20)
	assert.InDelta(t, 20.0, w.peaks()[2], 1e-9)
}
