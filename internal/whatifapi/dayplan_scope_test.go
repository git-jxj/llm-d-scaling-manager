package whatifapi

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// routerTTFT is a cycle's router latency with the TTFT histogram bounds
// (seconds) to the requests a second at or under each, over a minute.
func routerTTFT(at time.Time, buckets map[float64]float64, mean float64) LatencySample {
	r := &domain.RouterLatency{WindowSeconds: 60}
	for ub, rate := range buckets {
		r.TTFTBuckets = append(r.TTFTBuckets, domain.LatencyBucket{UpperBound: ub, Rate: rate})
	}
	r.TTFTCountRate = buckets[math.Inf(1)]
	r.TTFTSumRate = mean * r.TTFTCountRate
	return LatencySample{At: at, Router: r}
}

// stubDayMeasured is stubDay with each cycle's router TTFT histogram.
func stubDayMeasured(t *testing.T, r *stubResolver, n int, rate float64, had FleetCounts, buckets map[float64]float64) *DayPlanResponse {
	t.Helper()
	res, err := r.Resolve(t.Context(), InputsRequest{})
	require.NoError(t, err)
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	for i := range n {
		at := t0.Add(time.Duration(i) * 15 * time.Second)
		res.Series = append(res.Series, SeriesSample{At: at, Rate: rate, RateKnown: true, Prefill: had.Prefill, Decode: had.Decode})
		res.Latency = append(res.Latency, routerTTFT(at, buckets, 0.15))
	}
	p, err := DayPlan(t.Context(), res, DayPlanRequest{}, "schema-v3")
	require.NoError(t, err)
	return p
}

// With prefill not priced, decode is judged alone: a decode replica to spare
// is said as decode's (Scope ScopeDecodeOnly), and the hour lands in
// DecodeOverHours, never in OverHours, which the CLI and the page call waste.
func TestDayPlanDecodeAloneIsNoFleetWaste(t *testing.T) {
	p := stubDay(t, &stubResolver{refusePrefill: true}, 20, 6, FleetCounts{Prefill: 1, Decode: 3})
	b := p.Buckets[0]
	require.Equal(t, BucketSized, b.Status)
	assert.Equal(t, DayVerdictOver, b.Verdict, "1 decode serves 6 r/s; the fleet had 3")
	assert.Equal(t, ScopeDecodeOnly, b.Scope)
	require.Len(t, p.Hours, 1)
	assert.Equal(t, ScopeDecodeOnly, p.Hours[0].Scope)
	assert.Empty(t, p.Summary.OverHours, "no fleet is judged over while prefill is not sized")
	assert.Equal(t, []time.Time{p.Hours[0].Start}, p.Summary.DecodeOverHours)

	// A whole fleet's over is waste, and carries no scope.
	r := &stubResolver{dip: func(p, d int) bool { return p < 3 && d < 3 }}
	whole := stubDay(t, r, 20, 6, FleetCounts{Prefill: 4, Decode: 4})
	assert.Equal(t, DayVerdictOver, whole.Buckets[0].Verdict)
	assert.Empty(t, whole.Buckets[0].Scope)
	assert.Len(t, whole.Summary.OverHours, 1)
	assert.Empty(t, whole.Summary.DecodeOverHours)
}

// A bucket the model calls over, whose TTFT the router measured over its
// target, is given no verdict: a fleet that missed its TTFT target has
// nothing to spare. Measured under its target, the over stands.
func TestDayPlanWithholdsOverOnAMeasuredTTFTMiss(t *testing.T) {
	r := &stubResolver{dip: func(p, d int) bool { return p < 3 && d < 3 }}
	inf := math.Inf(1)
	// p90 between 0.5 s and 1 s, over the stub policy's 0.2 s target.
	slow := map[float64]float64{0.1: 1, 0.5: 2, 1: 10, inf: 10}
	missed := stubDayMeasured(t, r, 20, 6, FleetCounts{Prefill: 4, Decode: 4}, slow)
	b := missed.Buckets[0]
	require.NotNil(t, b.MeasuredTTFT)
	assert.Greater(t, *b.MeasuredTTFT, 0.5)
	assert.Empty(t, b.Verdict, "no verdict on a fleet that missed its TTFT target")
	assert.Contains(t, b.Flags, FlagMeasuredTTFTOverTarget)
	assert.Empty(t, missed.Summary.OverHours)

	fast := map[float64]float64{0.1: 10, 0.5: 10, 1: 10, inf: 10}
	held := stubDayMeasured(t, r, 20, 6, FleetCounts{Prefill: 4, Decode: 4}, fast)
	require.NotNil(t, held.Buckets[0].MeasuredTTFT)
	assert.LessOrEqual(t, *held.Buckets[0].MeasuredTTFT, 0.1)
	assert.Equal(t, DayVerdictOver, held.Buckets[0].Verdict)
	assert.NotContains(t, held.Buckets[0].Flags, FlagMeasuredTTFTOverTarget)

	// Too few requests to read a p90 (under minRequestsFor): no figure, and
	// the over stands.
	few := map[float64]float64{0.1: 0.1, 0.5: 0.2, 1: 1, inf: 1}
	thin := stubDayMeasured(t, r, 20, 6, FleetCounts{Prefill: 4, Decode: 4}, few)
	assert.Nil(t, thin.Buckets[0].MeasuredTTFT)
	assert.Equal(t, DayVerdictOver, thin.Buckets[0].Verdict)
}

// refusedLowerBound is the TTFT refusal on a V_P the prefill pods were only
// seen to compute; refusedNoVP the one on no V_P at all.
func refusedLowerBound() whatif.Result {
	return whatif.Unanswerable{Reason: whatif.NoPrefillCapacity, Flags: []whatif.Flag{whatif.VPLowerBound},
		Detail: "V_P 18000 tokens/s is only the rate a prefill pod was seen to compute (it never queued for capacity)"}
}

func refusedNoVP() whatif.Result {
	return whatif.Unanswerable{Reason: whatif.NoPrefillCapacity, Detail: "V_P 0 tokens/s"}
}

// A NoPrefillCapacity refusal on a V_P known only as a lower bound says so,
// in the words the page uses: never that the source learned none.
func TestDayRefusalHintSaysALowerBound(t *testing.T) {
	lb := dayRefusalOf(rolePrefill, termTTFT, refusedLowerBound(), "")
	assert.Equal(t, NoPrefillCapacityLowerBoundHint, lb.Hint)
	assert.NotContains(t, lb.Hint, "learned no")
	none := dayRefusalOf(rolePrefill, termTTFT, refusedNoVP(), "")
	assert.Contains(t, none.Hint, "learned no prefill speed")
}
