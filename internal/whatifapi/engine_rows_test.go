package whatifapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// engineRowsSeries is n cycles of 2 prefill and 2 decode rows at 10 r/s from
// the router, the decode rows reporting engine r/s in all, the variants
// stating replicas of each role.
func engineRowsSeries(n int, engine float64, replicas int) []SeriesSample {
	s := make([]SeriesSample, n)
	for i := range s {
		s[i] = SeriesSample{At: btT0, Rate: 10, RateKnown: true, Prefill: 2, Decode: 2, EngineRate: engine,
			PrefillReplicas: replicas, DecodeReplicas: replicas}
	}
	return s
}

// The rows stand for the fleet when their rate is the router's, within 20%
// either way, and a replica is a row; a quarter of either is refused, naming
// what was read; too few cycles, or nothing stated, say nothing.
func TestEngineRowsShort(t *testing.T) {
	assert.Empty(t, EngineRowsShort(engineRowsSeries(10, 10, 2)))
	assert.Empty(t, EngineRowsShort(engineRowsSeries(10, 8.5, 2)), "within 20% under")
	assert.Empty(t, EngineRowsShort(engineRowsSeries(10, 11.5, 2)), "within 20% over")
	assert.Empty(t, EngineRowsShort(engineRowsSeries(10, 10, 0)), "no replicas stated")
	assert.Empty(t, EngineRowsShort(engineRowsSeries(3, 2.5, 0)), "too few cycles to compare")

	why := EngineRowsShort(engineRowsSeries(10, 2.5, 2))
	assert.Equal(t, "the engine rows do not stand for the fleet: the decode engine rows report 2.5 r/s against the router's 10 r/s (25%), "+
		"as a multi-Pod group's rows do when they report one rank of several", why)
	assert.Contains(t, EngineRowsShort(engineRowsSeries(10, 13, 2)), "(130%)", "over by more than 20%")

	why = EngineRowsShort(engineRowsSeries(10, 10, 8))
	assert.Contains(t, why, "2 prefill engine rows stand for 8 replicas")
	assert.Contains(t, why, "2 decode engine rows stand for 8 replicas")

	// A rate the router did not read (the rows' own sum) is not compared.
	s := engineRowsSeries(10, 2.5, 2)
	for i := range s {
		s[i].RateKnown = false
	}
	assert.Empty(t, EngineRowsShort(s))
}

// The self-check does not score a recording whose engine rows do not stand
// for the fleet: it is not run, with the reason.
func TestSelfCheck_NotRunOnEngineRowsShort(t *testing.T) {
	e, folds := scEntry(t, 1, 1)
	for i := range e.series {
		e.series[i].RateKnown, e.series[i].EngineRate = true, e.series[i].Rate/4
	}
	sc := selfCheckEntry(t.Context(), e, folds, SelfCheckInfo{})
	assert.Equal(t, SelfCheckNotRun, sc.Status)
	assert.Contains(t, sc.Why, "the decode engine rows report")
	assert.Contains(t, sc.Why, "(25%)")
}
