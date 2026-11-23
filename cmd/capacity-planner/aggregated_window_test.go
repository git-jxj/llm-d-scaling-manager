package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// aggregatedReporter is contradictsReporter as one aggregated pool: no
// prefill variant, the decode variant serving both roles on dec replicas.
func aggregatedReporter(hot []int, rate float64, dec int) *reporter {
	r := contradictsReporter(hot, rate, 0, dec)
	r.st.Variants = r.st.Variants[1:]
	r.st.Variants[0].Role = ""
	delete(r.st.ServingReplicas, "p")
	r.pfVariant = ""
	return r
}

// An aggregated fleet (no prefill pods) is judged on its own knee in the
// window check (#357): the decode knee its line prices, which was fitted on
// pods that prefill too. NOT JUDGED where the pool served past it for 3
// minutes, judged otherwise. A cycle lighter in prompt, within 20%, has a
// higher pool knee, as a cycle lighter in output does: 5.2 r/s at 820
// prompt tokens is 4.3 r/s at the shape priced, under 1.25x a knee of 4. On
// a P/D fleet the prompt is prefill's work on other pods, and does not move
// the decode knee: the same cycles contradict it.
func TestReport_AnAggregatedPoolIsJudgedOnItsOwnKnee(t *testing.T) {
	base := fleetEval{kneeOK: true, knee: 4, binds: roleDecodeWord, decodeKnee: whatif.Answer{Value: 4}, under: true, pastOperating: true}

	got := aggregatedReporter(cycles(10, 22), 12, 2).contradicted(0, 2, base)
	assert.Contains(t, got, "2 decode or fewer served above 1.25x the fleet knee of 4.0 r/s at the shape priced for 3 min")
	var v whatifapi.ReportVerdict
	require.True(t, aggregatedReporter(cycles(10, 22), 12, 2).notJudgedUnder(0, 2, base, &v), "served past its knee")
	assert.Equal(t, whatifapi.VerdictNotJudged, v.Verdict)
	assert.Contains(t, v.Summary, contradictsWords)

	assert.False(t, aggregatedReporter(cycles(10, 22), 4.5, 2).notJudgedUnder(0, 2, base, &whatifapi.ReportVerdict{}), "served under 1.25x its knee")
	assert.False(t, aggregatedReporter(cycles(10, 22), 12, 3).notJudgedUnder(0, 2, base, &whatifapi.ReportVerdict{}), "a larger pool served it")

	lighter := func(r *reporter) *reporter {
		for _, i := range cycles(10, 22) {
			r.st.Variants[len(r.st.Variants)-1].ShapeSeries[i].InputTokens = 820
		}
		return r
	}
	assert.Empty(t, lighter(aggregatedReporter(cycles(10, 22), 5.2, 2)).contradicted(0, 2, base), "lighter prompt, one pool")
	assert.False(t, lighter(aggregatedReporter(cycles(10, 22), 5.2, 2)).notJudgedUnder(0, 2, base, &whatifapi.ReportVerdict{}),
		"judged: the pool did not serve past its own knee")
	assert.NotEmpty(t, lighter(contradictsReporter(cycles(10, 22), 5.2, 2, 2)).contradicted(2, 2, base), "lighter prompt, P/D")
}
