package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// A variant has at most one llmsm_replicas_not_arriving series: a new reason
// replaces the old one, and clearing removes it.
func TestReplicasNotArrivingKeepsOneSeries(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		n, err := testutil.GatherAndCount(registry, constants.LLMSMReplicasNotArriving)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	SetReplicasNotArriving("ns", "decode", "decode", "no-pods", 1)
	SetReplicasNotArriving("ns", "decode", "decode", "admission-rejected", 1)
	SetReplicasNotArriving("ns", "prefill", "prefill", "unschedulable", 2)
	if n := count(); n != 2 {
		t.Fatalf("%d series, want 2: one per variant", n)
	}
	if v := testutil.ToFloat64(replicasNotArriving.With(prometheus.Labels{
		constants.LabelNamespace: "ns", constants.LabelVariantName: "prefill",
		constants.LabelRole: "prefill", constants.LabelReason: "unschedulable",
	})); v != 2 {
		t.Errorf("prefill value = %v, want 2", v)
	}

	ClearReplicasNotArriving("ns", "decode")
	ClearReplicasNotArriving("ns", "never-published")
	if n := count(); n != 1 {
		t.Fatalf("%d series after clearing decode, want 1", n)
	}
}
