package learnreplay

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// residentRow is a Ready decode row at fill k holding running requests.
func residentRow(variant string, k float64, running int) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{VariantName: variant, Ready: true, KvUsageInstant: k, KvUsageInstantKnown: true,
		RunningRequests: running, RunningRequestsKnown: true, TotalKvCapacityTokens: 1_000_000}
}

// A cycle's reading sums its rows: the tokens in use over the requests
// running. A row below the ITL band, lent by the warm pool, not Ready, of a
// prefill variant, or with no running count, is not read.
func TestNoteResident_SumsTheRowsRead(t *testing.T) {
	in := domain.AnalyzerInput{Config: &config.ScalingPolicy{},
		VariantStates: []domain.VariantReplicaState{{VariantName: "dec", Role: domain.RoleDecode}, {VariantName: "pre", Role: domain.RolePrefill}},
		ReplicaMetrics: []domain.ReplicaMetrics{
			residentRow("dec", 0.4, 10), residentRow("dec", 0.6, 20),
			residentRow("dec", 0.05, 1),
			func() domain.ReplicaMetrics { r := residentRow("dec", 0.5, 5); r.FromWarmPool = true; return r }(),
			func() domain.ReplicaMetrics { r := residentRow("dec", 0.5, 5); r.Ready = false; return r }(),
			func() domain.ReplicaMetrics {
				r := residentRow("dec", 0.5, 5)
				r.RunningRequestsKnown = false
				return r
			}(),
			residentRow("pre", 0.5, 5),
		}}
	got := map[string][]residentSample{}
	noteResident(got, in, 7)
	assert.Equal(t, map[string][]residentSample{"dec": {{cycle: 7, tokens: 1_000_000, running: 30}}}, got)
}

// The reading is the median over the cycles, not bounded by the shape's
// context, and none with fewer than minResidentCycles cycles of enough
// running requests.
func TestResidentOf_TheMedianUnbounded(t *testing.T) {
	m := MeasuredShape{InputTokens: 20000, OutputTokens: 400, PrefixHitRate: 0.92}
	samples := []residentSample{
		{cycle: 1, tokens: 600_000, running: 10},  // 60,000
		{cycle: 2, tokens: 1_000_000, running: 1}, // too few running
		{cycle: 3, tokens: 700_000, running: 10},  // 70,000
		{cycle: 4, tokens: 900_000, running: 10},  // 90,000
	}
	got := residentOf(samples, m)
	assert.True(t, got.Read())
	assert.Equal(t, ResidentKV{PerRequest: 70_000, Running: 10, Cycles: 3, FromCycle: 1, ToCycle: 4}, got)

	assert.False(t, residentOf(samples[:2], m).Read(), "one cycle of enough requests is no reading")
}

// A line-check row is priced at its own reading where it has one, held at its
// shape's prefix-hit discount below that, and at its shape with no hit where
// it has none.
func TestRowResidentKV_ItsThreeBranches(t *testing.T) {
	s := shape.New(20000, 400, 0.9) // discount 2,200; no hit 20,200
	unread := residentRow("dec", 0.5, 10)
	unread.RunningRequestsKnown = false
	cases := []struct {
		name string
		row  domain.ReplicaMetrics
		want float64
	}{
		{"reading used", residentRow("dec", 0.5, 10), 50_000},
		{"floor applied", residentRow("dec", 0.01, 10), 2_200},
		{"no reading: the whole context", unread, 20_200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.InDelta(t, c.want, rowResidentKV(c.row, s), 1e-6)
		})
	}
}
