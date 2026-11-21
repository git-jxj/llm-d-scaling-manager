package whatifapi

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Beside the pods it had, a bucket says the replicas asked for and not
// serving: the median over its cycles, as had's. Here 10:00 had 2 prefill
// and 2 decode serving with 1 decode asked and not serving in 12 of its 20
// cycles (a quota held it), and 1 prefill in 5 (under the median); 10:05
// had none. The hour takes each role's most; a note says what it is, and a
// plan with none says nothing of it.
func TestDayPlanSaysWhatWasAskedAndNotServing(t *testing.T) {
	res, err := (&stubResolver{}).Resolve(t.Context(), InputsRequest{})
	require.NoError(t, err)
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	first := shaped(t0, 2, 1000)
	for i := range first {
		if i < 12 {
			first[i].DecodeNotServing = 1
		}
		if i < 5 {
			first[i].PrefillNotServing = 1
		}
	}
	res.Series = slices.Concat(first, shaped(t0.Add(5*time.Minute), 2, 1000))
	p, err := DayPlan(t.Context(), res, DayPlanRequest{}, "schema-v3")
	require.NoError(t, err)
	require.Len(t, p.Buckets, 2)
	assert.Equal(t, &FleetCounts{Prefill: 2, Decode: 2}, p.Buckets[0].Actual, "had counts those serving only")
	assert.Equal(t, &FleetCounts{Prefill: 0, Decode: 1}, p.Buckets[0].NotServing)
	assert.Nil(t, p.Buckets[1].NotServing, "none asked and not serving")
	require.Len(t, p.Hours, 1)
	assert.Equal(t, &FleetCounts{Prefill: 0, Decode: 1}, p.Hours[0].NotServing)
	assert.True(t, hasNote(p.Notes, "In 1 bucket(s) more were asked for and not serving"), "%q", p.Notes)

	res.Series = append(shaped(t0, 2, 1000), shaped(t0.Add(5*time.Minute), 2, 1000)...)
	p, err = DayPlan(t.Context(), res, DayPlanRequest{}, "schema-v3")
	require.NoError(t, err)
	assert.Nil(t, p.Hours[0].NotServing)
	assert.False(t, hasNote(p.Notes, "asked for and not serving"), "%q", p.Notes)
}

// hasNote says a note holds s.
func hasNote(notes []string, s string) bool {
	for _, n := range notes {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}
