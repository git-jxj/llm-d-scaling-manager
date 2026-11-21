package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The fleet needed at each f, in words: the same at every f in one clause,
// else each f's fleet, and an f no fleet holds at named.
func TestFSensitivityWords(t *testing.T) {
	n := whatifapi.FleetCounts{Prefill: 3, Decode: 3}
	for _, c := range []struct {
		name string
		rows []whatifapi.ReportNeededAtF
		want string
	}{
		{"none", nil, ""},
		{"the same", []whatifapi.ReportNeededAtF{{F: 1, Prefill: 3, Decode: 3}, {F: 1.5, Prefill: 3, Decode: 3}},
			"; the same at f 1 to 1.5"},
		{"moves", []whatifapi.ReportNeededAtF{{F: 1, Prefill: 3, Decode: 3}, {F: 1.5, Prefill: 4, Decode: 3}},
			"; at f 1 needs 3 + 3, at f 1.5 needs 4 + 3"},
		{"unmet", []whatifapi.ReportNeededAtF{{F: 1, Prefill: 2, Decode: 3}, {F: 1.5, Unmet: "fleet"}},
			"; at f 1 needs 2 + 3, at f 1.5 no fleet holds"},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, fSensitivityWords(n, c.rows))
		})
	}
}

// Issue llm-d-incubation/llm-d-scaling-manager#300: the staircase learned f
// at two prefill pods only. At its p95 load with 16k prompts the fleet needed
// is read at the curve's f, about 1, and the verdict gives the fleet needed
// at f 1 and at f 1.5 beside it: at 1.5 the pods past two are worth less, and
// more of them are needed.
func TestAtReplicas_TheFleetNeededAtEachF(t *testing.T) {
	st := stairReplay.state(t)
	q := stairQuery(2, 2, 41.7)
	sh, err := whatifapi.ParseReportShape("16k/200/0.3")
	require.NoError(t, err)
	q.Shapes = []whatifapi.ReportShapeAsk{sh}
	ctx, cancel := context.WithTimeout(t.Context(), reportDeadline)
	defer cancel()
	rep, err := buildReport(ctx, st, &servedState{st: st, decodeVariant: "variant-01", imbalanceAcrossCounts: true}, q)
	require.NoError(t, err)
	v := rep.Shapes[0].Verdict
	require.Equal(t, whatifapi.VerdictUnder, v.Verdict, v.Summary)
	require.NotNil(t, v.Needed, v.Summary)
	n := *v.Needed
	require.GreaterOrEqual(t, n.Prefill, 2, "precondition: the answer rests on f, %s", v.Summary)
	require.Len(t, n.FSensitivity, 2, v.Summary)
	lo, hi := n.FSensitivity[0], n.FSensitivity[1]
	assert.Equal(t, 1.0, lo.F)
	assert.Equal(t, whatif.ImbalanceFloor, hi.F)
	assert.Empty(t, lo.Unmet)
	assert.Equal(t, n.Prefill, lo.Prefill, "the curve reads f about 1 past two pods: f 1's answer")
	assert.Greater(t, hi.Prefill, lo.Prefill, "at f 1.5 more prefill pods are needed")
	assert.Contains(t, v.Summary, fSensitivityWords(whatifapi.FleetCounts{Prefill: n.Prefill, Decode: n.Decode}, n.FSensitivity))
}
