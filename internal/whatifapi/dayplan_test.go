package whatifapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cycles is n cycles every gap from t0, each at rate.
func cycles(t0 time.Time, gap time.Duration, n int, rate float64) []SeriesSample {
	out := make([]SeriesSample, n)
	for i := range out {
		out[i] = SeriesSample{At: t0.Add(time.Duration(i) * gap), Rate: rate, Prefill: 1, Decode: 2}
	}
	return out
}

// A day across a clock change is 24 hours of buckets in UTC: the local
// clock's repeated hour (New York, 2026-11-01, 01:00 to 02:00 twice) is
// neither merged into one bucket nor dropped, and every bucket holds the
// cycles of its own five minutes.
func TestBucketizeAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, ny)
	series := cycles(t0, 30*time.Second, 25*120, 1)
	b := bucketize(series, 5*time.Minute)
	require.Len(t, b, 25*12, "25 hours of cycles is 25 hours of buckets")
	for i, x := range b {
		assert.Len(t, x.samples, 10, "bucket %d (%s)", i, x.start)
		assert.Equal(t, time.UTC, x.start.Location())
		if i > 0 {
			assert.Equal(t, 5*time.Minute, x.start.Sub(b[i-1].start))
		}
	}
	// The hour hours are UTC too: 25 of them, each once.
	hours := hoursOf(series, make([]DayBucket, 0))
	assert.Empty(t, hours)
	db := make([]DayBucket, len(b))
	for i, x := range b {
		db[i] = DayBucket{Start: x.start}
	}
	hours = hoursOf(series, db)
	require.Len(t, hours, 25)
	for i := 1; i < len(hours); i++ {
		assert.Equal(t, time.Hour, hours[i].Start.Sub(hours[i-1].Start))
	}
}

// Buckets start on the step in UTC whatever the first cycle's time, and a
// gap in the cycles is buckets with no data, kept in place, not closed up.
func TestBucketizeGapsAndAlignment(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 10, 2, 13, 0, time.UTC)
	series := append(cycles(t0, 15*time.Second, 40, 2), cycles(t0.Add(30*time.Minute), 15*time.Second, 40, 3)...)
	b := bucketize(series, 5*time.Minute)
	require.Equal(t, time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), b[0].start)
	var empty int
	for _, x := range b {
		if len(x.samples) == 0 {
			empty++
		}
	}
	assert.Equal(t, 3, empty, "10:15 to 10:30 has no cycles")
	assert.Len(t, b, 9, "10:00 to 10:40")
	// Out of order cycles land in their own buckets.
	rev := append([]SeriesSample{}, series...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	assert.Equal(t, b, bucketize(rev, 5*time.Minute))
}

// A bucket's rate: the mean, the nearest-rank p95 and the most.
func TestRateOf(t *testing.T) {
	var s []SeriesSample
	for i := 1; i <= 20; i++ {
		s = append(s, SeriesSample{Rate: float64(i)})
	}
	assert.Equal(t, DayRate{Mean: 10.5, P95: 19, Max: 20}, rateOf(s))
	assert.Equal(t, DayRate{Mean: 4, P95: 4, Max: 4}, rateOf([]SeriesSample{{Rate: 4}}))
}

// Under is either role short; over is neither short and one more.
func TestVerdictOf(t *testing.T) {
	two := 2
	assert.Equal(t, VerdictUnder, verdictOf(DayNeeded{Prefill: &two, Decode: 2}, FleetCounts{3, 1}))
	assert.Equal(t, VerdictOver, verdictOf(DayNeeded{Prefill: &two, Decode: 2}, FleetCounts{2, 3}))
	assert.Equal(t, VerdictMatch, verdictOf(DayNeeded{Prefill: &two, Decode: 2}, FleetCounts{2, 2}))
	// Prefill not priced: decode alone decides, whatever prefill had.
	assert.Equal(t, VerdictMatch, verdictOf(DayNeeded{Decode: 2}, FleetCounts{0, 2}))
	assert.Equal(t, VerdictUnder, verdictOf(DayNeeded{Decode: 3}, FleetCounts{9, 2}))
}

// An hour is under when any bucket is under or beyond the bound, refused
// when every bucket with data is refused, and over only when every sized
// bucket is over and none is unmet.
func TestHourVerdicts(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	a := &FleetCounts{2, 2}
	b := func(i int, status, verdict string) DayBucket {
		x := DayBucket{Start: t0.Add(time.Duration(i) * 5 * time.Minute), Status: status, Verdict: verdict, Actual: a}
		if status == BucketSized {
			x.Needed = &DayNeeded{Decode: 1}
		}
		return x
	}
	for name, c := range map[string]struct {
		buckets []DayBucket
		want    string
	}{
		"over":           {[]DayBucket{b(0, BucketSized, VerdictOver), b(1, BucketSized, VerdictOver)}, VerdictOver},
		"over and unmet": {[]DayBucket{b(0, BucketSized, VerdictOver), b(1, BucketUnmet, "")}, VerdictUnder},
		"all refused":    {[]DayBucket{b(0, BucketRefused, VerdictRefused), b(1, BucketRefused, VerdictRefused)}, VerdictRefused},
		"over and match": {[]DayBucket{b(0, BucketSized, VerdictOver), b(1, BucketSized, VerdictMatch)}, VerdictMatch},
		"too little":     {[]DayBucket{b(0, BucketTooLittleData, "")}, ""},
	} {
		h := hoursOf(nil, c.buckets)
		require.Len(t, h, 1, name)
		assert.Equal(t, c.want, h[0].Verdict, name)
	}
}

// A day plan refuses what it cannot answer before pricing anything.
func TestDayQuestionRefusals(t *testing.T) {
	doc := InputsResponse{Fleet: FleetInfo{Prefill: &RoleInfo{Variant: "p", Replicas: 1}, Decode: RoleInfo{Variant: "d", Replicas: 1}},
		Policy: PolicyInfo{Headroom: 0.85, TTFT: &PolicyObjective{Statistic: "p90", Seconds: 1}}}
	series := cycles(time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), 15*time.Second, 100, 1)
	e := &inputsEntry{id: "x", doc: doc, series: series}
	for name, c := range map[string]struct {
		e    *inputsEntry
		req  DayPlanRequest
		want string
	}{
		"no series":   {&inputsEntry{doc: doc}, DayPlanRequest{}, "no cycles"},
		"no prefill":  {&inputsEntry{series: series}, DayPlanRequest{}, "no prefill variant"},
		"a rate":      {e, DayPlanRequest{Traffic: &Traffic{Classes: &Classes{}}}, "traffic.classes"},
		"a bad step":  {e, DayPlanRequest{Step: "1s"}, "step"},
		"a bad stat":  {e, DayPlanRequest{SizeFor: "p50"}, "sizeFor"},
		"many":        {e, DayPlanRequest{Step: "15s", MaxReplicas: nil}, ""},
		"a bad floor": {e, DayPlanRequest{MinRequests: ptr(-1)}, "minRequests"},
	} {
		_, err := dayQuestionOf(c.e, c.req)
		if c.want == "" {
			assert.NoError(t, err, name)
			continue
		}
		require.Error(t, err, name)
		assert.True(t, IsInputError(err), name)
		assert.Contains(t, err.Error(), c.want, name)
	}
}
