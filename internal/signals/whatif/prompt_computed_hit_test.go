package whatif

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// computedRow is a serving prefill row that computed the given tokens a
// second over rate requests a second of whole-token prompts, read over the
// same minute (AvgInputTokensRecent).
func computedRow(computed, rate, whole float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{Ready: true, PrefillComputedTokenRate: computed, PrefillComputedTokenRateKnown: true,
		RequestRate: rate, RequestRateKnown: true, AvgInputTokensRecent: whole}
}

// The computed-token hit is 1 - computed / whole over the window's rows
// that read both: a quarter of a 32000-token prompt computed is a 0.75 hit,
// whatever the prefix-cache counter says. A row with no one-minute prompt
// counts for the prompt but not for the hit; with none, it is unread.
func TestPromptWindow_ComputedHit(t *testing.T) {
	w := &PromptWindow{}
	for i := range 3 {
		w.Note([]domain.ReplicaMetrics{computedRow(16000, 2, 32000), computedRow(4000, 1, 0)}, i, time.Unix(int64(15*i), 0))
	}
	hit, ok := w.ComputedHit()
	assert.True(t, ok)
	assert.InDelta(t, 0.75, hit, 1e-12, "16000 of 64000 tokens a second computed; the row with no prompt left out")
	tokens, _, _, _ := w.Prompt()
	assert.InDelta(t, 20000.0/3, tokens, 1e-9, "the prompt still counts every row")

	none := &PromptWindow{}
	none.Note([]domain.ReplicaMetrics{computedRow(4000, 1, 0)}, 0, time.Unix(0, 0))
	_, ok = none.ComputedHit()
	assert.False(t, ok, "no one-minute prompt read: unread")
	var nilWindow *PromptWindow
	_, ok = nilWindow.ComputedHit()
	assert.False(t, ok)

	// A minute that computed more than its prompts (the rates' edges) reads
	// no negative hit.
	over := &PromptWindow{}
	over.Note([]domain.ReplicaMetrics{computedRow(1100, 1, 1000)}, 0, time.Unix(0, 0))
	hit, ok = over.ComputedHit()
	assert.True(t, ok)
	assert.Zero(t, hit)

	// Clone carries the hit.
	hit, ok = w.Clone().ComputedHit()
	assert.True(t, ok)
	assert.InDelta(t, 0.75, hit, 1e-12)
}
