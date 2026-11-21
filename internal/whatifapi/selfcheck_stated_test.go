package whatifapi

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The self-check replays the recording with the answer's stated inputs, and
// its line names them, flagged; what it does not replay (a TPOT ratio, stated
// lengths) it names as run without. Before issue
// llm-d-incubation/llm-d-scaling-manager#277 every fold was priced with no
// stated input, and the line was the same with and without them.
func TestSelfCheck_ReplaysTheStatedInputs(t *testing.T) {
	e, folds := scEntry(t, 1, 1)
	r := folds[0].Pricer.(*stubResolver)
	var mu sync.Mutex
	var seen []Knobs
	r.onFleet = func(k Knobs) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, k)
	}
	stated := &Stated{Assume: Assume{PromptCV: f64(0.06), ITLBFromFloor: true, TPOTRatio: f64(1.3), RouterHop: f64(0.008)},
		ArrivalCV2: f64(2), PromptTokens: 2000, CLI: true}
	sc := selfCheckEntry(context.Background(), e, folds, SelfCheckInfo{PlannerVersion: "vtest", Stated: stated})
	require.NotEmpty(t, sc.Folds, sc.Line)
	require.NotEmpty(t, seen)
	for _, k := range seen {
		require.NotNil(t, k.Assume.PromptCV)
		assert.InDelta(t, 0.06, *k.Assume.PromptCV, 1e-12, "the stated cv, not the step's or the fit's")
		assert.True(t, k.Assume.ITLBFromFloor)
		require.NotNil(t, k.Assume.RouterHop)
		assert.Nil(t, k.Assume.TPOTRatio, "a ratio is one percentile's: not replayed")
		require.NotNil(t, k.ArrivalCV2)
		assert.InDelta(t, 2, *k.ArrivalCV2, 1e-12)
		assert.NotEqual(t, 2000.0, k.PromptTokens, "each step's own lengths, not the stated ones")
	}
	assert.Equal(t, []string{"--assume-prompt-cv 0.06", "--assume-router-hop 8ms", "--assume-itl-b floor", "--arrival-cv2 2"}, sc.Stated)
	assert.Equal(t, []string{"--assume-tpot-ratio 1.3", "--assume-input-tokens 2000"}, sc.WithoutStated)
	assert.True(t, strings.HasPrefix(sc.Line, "Self-check, replayed with the stated --assume-prompt-cv 0.06, --assume-router-hop 8ms, "+
		"--assume-itl-b floor, --arrival-cv2 2 (flagged, outside the validated region): tested on the last "), sc.Line)
	assert.True(t, strings.HasSuffix(sc.Line, " It ran without the stated --assume-tpot-ratio 1.3, --assume-input-tokens 2000: "+
		"what the answer prices on them is not checked."), sc.Line)

	// The API's keys name them on the API.
	stated.CLI = false
	sc = selfCheckEntry(context.Background(), e, folds, SelfCheckInfo{Stated: stated})
	assert.Equal(t, []string{"assume.promptCV 0.06", "assume.routerHop 0.008", "assume.itlBFromFloor", "arrivalCV2 2"}, sc.Stated)
	assert.Equal(t, []string{"assume.tpotRatio 1.3", "traffic.mix.promptTokens 2000"}, sc.WithoutStated)

	// Nothing stated: the line and the fleets priced are as before.
	seen = nil
	plain := selfCheckEntry(context.Background(), e, folds, SelfCheckInfo{})
	assert.True(t, strings.HasPrefix(plain.Line, "Self-check: tested on the last "), plain.Line)
	assert.Empty(t, plain.Stated)
	assert.Empty(t, plain.WithoutStated)
	for _, k := range seen {
		assert.Equal(t, Assume{}, k.Assume)
		assert.Nil(t, k.ArrivalCV2)
	}
}

// A self-check read with the source, before an answer stated anything (the
// server's, beside a report or a day plan), says it ran without them; with
// nothing stated it is the same self-check, and the cached one is not
// changed.
func TestSelfCheck_RanWithoutTheStatedInputs(t *testing.T) {
	sc := &SelfCheck{Status: SelfCheckOK, Line: "Self-check: tested on the last 1 h."}
	assert.Same(t, sc, sc.RanWithout(&Stated{}))
	assert.Same(t, sc, sc.RanWithout(nil))
	var none *SelfCheck
	assert.Nil(t, none.RanWithout(&Stated{Assume: Assume{VP: f64(9000)}}))

	got := sc.RanWithout(&Stated{Assume: Assume{ITLB: f64(0.012), VP: f64(9000)}})
	assert.Equal(t, []string{"assume.vp 9000", "assume.itlB 0.012"}, got.WithoutStated)
	assert.Equal(t, "Self-check: tested on the last 1 h. It ran without the stated assume.vp 9000, assume.itlB 0.012: "+
		"what the answer prices on them is not checked.", got.Line)
	assert.Equal(t, "Self-check: tested on the last 1 h.", sc.Line, "the cached self-check is not changed")
	assert.Empty(t, sc.WithoutStated)
}

// A backtest refuses a stated term out of range, as a plan does, instead of
// refusing every step one by one.
func TestBacktest_RefusesAStatedTermOutOfRange(t *testing.T) {
	e, folds := scEntry(t, 1, 1)
	fit := &inputsEntry{id: e.id + "#fit", doc: e.doc, pricer: folds[0].Pricer, series: e.series, config: e.config}
	for name, a := range map[string]Assume{
		"promptCV above 3":  {PromptCV: f64(5)},
		"negative hop":      {RouterHop: f64(-0.001)},
		"B and floor":       {ITLB: f64(0.01), ITLBFromFloor: true},
		"B above a second":  {ITLB: f64(2)},
		"vp of 0":           {VP: f64(0)},
		"negative promptCV": {PromptCV: f64(-1)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := questionOf(fit, e, BacktestRequest{Assume: &a}, folds[0].At, folds[1].At, "self-check", false)
			require.Error(t, err)
			assert.True(t, IsInputError(err), err)
		})
	}
	ok := Assume{PromptCV: f64(0.06), ITLBFromFloor: true, TPOTRatio: f64(1.2)}
	_, err := questionOf(fit, e, BacktestRequest{Assume: &ok}, folds[0].At, folds[1].At, "self-check", false)
	assert.NoError(t, err)
}
