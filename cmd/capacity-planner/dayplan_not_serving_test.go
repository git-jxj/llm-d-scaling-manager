package main

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// Beside each hour's had, the day plan says the replicas asked for and not
// serving (#317): on the b-on staircase with V_P and a assumed, an hour
// with 1 decode asked and not serving reads "2(+1)" under had D, and a line
// under the table says what (+n) is; with none, the table is as it was.
func TestDayPlanHadSaysWhatWasAskedAndNotServing(t *testing.T) {
	args := slices.Concat([]string{"day-plan", "--recording", stair("ttft-staircase-b-on")}, dayStairAssumed)
	plain, code := runCmd(t, args...)
	require.Equal(t, 0, code, plain)
	require.NotContains(t, plain, "(+")
	require.NotContains(t, plain, "asked for and not serving")

	cached := planDay
	planDay = func(ctx context.Context, res *whatifapi.Resolved, req whatifapi.DayPlanRequest, schema string) (*whatifapi.DayPlanResponse, error) {
		p, err := cached(ctx, res, req, schema)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		var own whatifapi.DayPlanResponse
		if err := json.Unmarshal(b, &own); err != nil {
			return nil, err
		}
		own.Hours[0].NotServing = &whatifapi.FleetCounts{Decode: 1}
		return &own, nil
	}
	t.Cleanup(func() { planDay = cached })
	out, code := runCmd(t, args...)
	require.Equal(t, 0, code, out)
	p, _ := dayJSON(t, args[1:]...)
	require.NotNil(t, p.Hours[0].Actual)
	row := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, p.Hours[0].Start.Format("2006-01-02 15:04")) {
			row = l
		}
	}
	require.NotEmpty(t, row, out)
	assert.Contains(t, row, " "+strconv.Itoa(p.Hours[0].Actual.Decode)+"(+1) ", "had D, with 1 asked and not serving")
	assert.Contains(t, out, "had (+n): n more replicas were asked for and not serving")
	assert.Equal(t, 2, strings.Count(out, "(+"), "the hour and the line under the table; the other hours had none")
}
