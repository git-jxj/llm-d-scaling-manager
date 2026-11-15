package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// kneeRoundingBound is how long the knee point may take. It takes a few
// milliseconds; before the fix it took over 30 minutes.
const kneeRoundingBound = 30 * time.Second

// At 12 prefill and 19 decode replicas, 1024-token prompts and V_P 30000, the
// curve's last grid rate below the knee is the prefill knee itself, 234.375
// r/s. At the imbalance corners f = 1.275 and 1.725 of the span, that rate
// sits one rounding step under the corner's knee: it passes the knee check,
// the busiest pod's utilization reads exactly 1, and the stretch's mean is
// +Inf. The point is refused Saturated, as before, and at once: it used to
// read a gamma tail at +Inf some 10^11 times, and the plan ran past 30
// minutes while 14 prefill and 18 decode replicas priced in 17 s.
func TestServeKneeRoundingPricesAtOnce(t *testing.T) {
	_, src := servedAPI(t)
	res, err := src.Resolve(context.Background(), whatifapi.InputsRequest{Model: "model-02",
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: "ols-sweep.jsonl.gz"}})
	require.NoError(t, err)
	fl, err := res.Pricer.Fleet(whatifapi.Knobs{Prefill: 12, Decode: 19, PromptTokens: 1024, OutputTokens: 1000,
		TTFTPercentile: 0.9, Assume: whatifapi.Assume{VP: fp(30000)}, Headroom: 0.85})
	require.NoError(t, err)
	knee, ok := fl.PrefillKnee().(whatif.Answer)
	require.True(t, ok, "the prefill knee: %+v", fl.PrefillKnee())
	require.Equal(t, 234.375, knee.Value, "the knee this case is built on moved")

	done := make(chan whatif.Result, 1)
	start := time.Now()
	go func() { done <- fl.TTFTAt(knee.Value) }()
	select {
	case r := <-done:
		t.Logf("TTFT at the knee in %s: %+v", time.Since(start), r)
		u, ok := r.(whatif.Unanswerable)
		require.True(t, ok, "the knee is refused: %+v", r)
		assert.Equal(t, whatif.Saturated, u.Reason)
	case <-time.After(kneeRoundingBound):
		t.Fatalf("TTFT at the 12/19 fleet's knee took over %s", kneeRoundingBound)
	}
}
