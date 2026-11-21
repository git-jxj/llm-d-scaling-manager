package itl

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// TestObserve_BusiestRankFill: with Admission.BusiestRankFill a row's k is its
// busiest rank's (domain.ReplicaMetrics.BusiestRankKvUsageForITL), for the
// band as for the reading; without it, the mean over the ranks, as before
// (issue llm-d-incubation/llm-d-scaling-manager#312).
func TestObserve_BusiestRankFill(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	// Eight ranks, the busiest 1.25 times as full as their mean.
	rows := []domain.ReplicaMetrics{
		{PodName: "in", VariantName: "v", Ready: true, EngineRanks: 8, KvCacheUsage: 0.4, MaxRankKvCacheUsage: 0.5,
			KvUsageMean: 0.4, KvUsageMeanKnown: true, AvgITL: 0.02},
		// Mean 0.72 is in the band; its busiest rank, 0.9, is above it.
		{PodName: "hot", VariantName: "v", Ready: true, EngineRanks: 8, KvCacheUsage: 0.72, MaxRankKvCacheUsage: 0.9,
			KvUsageMean: 0.72, KvUsageMeanKnown: true, AvgITL: 0.03},
	}
	for _, c := range []struct {
		busiest   bool
		wantK     []float64
		wantAbove int
	}{
		{false, []float64{0.4, 0.72}, 0},
		{true, []float64{0.5}, 1},
	} {
		tr := NewTracker()
		r := tr.Observe("key", "v", rows, Admission{BusiestRankFill: c.busiest}, now)
		all := tr.Readings("key").All
		ks := make([]float64, 0, len(all))
		for _, o := range all {
			ks = append(ks, o.K)
		}
		if len(ks) != len(c.wantK) || r.AboveBand != c.wantAbove {
			t.Fatalf("busiest %v: readings at k %v, %d above the band; want %v, %d", c.busiest, ks, r.AboveBand, c.wantK, c.wantAbove)
		}
		for i := range ks {
			if d := ks[i] - c.wantK[i]; d > 1e-12 || d < -1e-12 {
				t.Errorf("busiest %v: reading %d at k %v, want %v", c.busiest, i, ks[i], c.wantK[i])
			}
		}
	}
}
