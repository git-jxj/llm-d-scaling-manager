package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi/apitest"
)

// On a recording, a plan's own bucket edges price one "if all traffic were
// bucket b" line per edge, each the CLI's fleet with every prompt at the edge;
// the default edges asked are the plan with none, byte for byte.
func TestServeBucketEdges(t *testing.T) {
	h, src := servedAPI(t)
	c := curveFleets()["worked"]
	in, _ := apiInputs(t, h, c.recording, c.model)
	req := c.req
	req.InputsID = in.InputsID

	none, noneBody := apiPlan(t, h, req)
	req.BucketEdges = []int{1024, 4096, 8192, 32768, 131072}
	asked, askedBody := apiPlan(t, h, req)
	assert.Equal(t, none.ScenarioID, asked.ScenarioID)
	assert.Equal(t, string(noneBody), string(askedBody), "the default edges asked moved the plan")

	req.BucketEdges = []int{2000, 16384}
	out, body := apiPlan(t, h, req)
	assert.Empty(t, apitest.CheckFigures(body))
	assert.NotEqual(t, none.ScenarioID, out.ScenarioID)
	assert.Equal(t, none.Limits, out.Limits, "the edges moved a limit: no limit is read per bucket")
	require.Len(t, out.TTFT.PerBucket, 2)
	compared := 0
	for i, name := range []string{"2000", "16k"} {
		l := out.TTFT.PerBucket[i]
		assert.Equal(t, name, l.Bucket)
		assert.Equal(t, whatifapi.SemanticsAllTraffic, l.Semantics)
		sc := out.Scenario
		sc.Mix = whatifapi.Mix{PromptTokens: float64(req.BucketEdges[i]), OutputTokens: out.Scenario.Mix.OutputTokens}
		fl := servedFleetOf(t, src, c.recording, c.model, sc)
		// Every curve point is the CLI's fleet at the edge: answered where it
		// answers, refused where it refuses.
		for j, p := range l.Curve {
			want, ok := fl.TTFTAt(p.Rate).(whatif.TTFT)
			if !ok {
				assert.NotNil(t, p.Figure.Refused, "bucket %s point %d: the CLI refuses, the API answers", name, j)
				continue
			}
			require.NotNil(t, p.Figure.Value, "bucket %s point %d: %+v", name, j, p.Figure.Refused)
			assert.Equal(t, want.Seconds, *p.Figure.Value, "bucket %s point %d", name, j)
			compared++
		}
	}
	assert.Greater(t, compared, 50, "too few bucket points checked against the CLI")
}
