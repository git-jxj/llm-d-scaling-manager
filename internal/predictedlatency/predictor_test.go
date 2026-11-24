package predictedlatency

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// recorder is a Publisher that keeps what it was given.
type recorder struct {
	published map[string]Coverage
	forgot    []string
}

func (r *recorder) Publish(namespace, modelID, objective string, c Coverage) {
	if r.published == nil {
		r.published = map[string]Coverage{}
	}
	r.published[namespace+"/"+modelID+"/"+objective] = c
}

func (r *recorder) Forget(namespace, modelID string) {
	r.forgot = append(r.forgot, namespace+"/"+modelID)
}

// pd is a P/D model's cycle reading every input but what a controller that
// records nothing collects: the prefill pods' timings and the router's TPOT
// histogram.
func pd() (domain.AnalyzerInput, *domain.RouterLatency) {
	in := domain.AnalyzerInput{Namespace: "ns", ModelID: "m",
		VariantStates: []domain.VariantReplicaState{{VariantName: "p1", Role: domain.RolePrefill},
			{VariantName: "d1", Role: domain.RoleDecode}},
		ReplicaMetrics: []domain.ReplicaMetrics{
			{PodName: "p", VariantName: "p1", Ready: true, RequestRate: 2, RequestRateKnown: true, AvgTTFT: 0.4, AvgTTFTKnown: true,
				PrefillComputedTokenRate: 4000, PrefillComputedTokenRateKnown: true,
				ComputedTokensBuckets: []domain.LengthBucket{{UpperBound: 1000, Rate: 1}}},
			{PodName: "d", VariantName: "d1", Ready: true, RequestRate: 2, RequestRateKnown: true, AvgTTFT: 0.05, AvgTTFTKnown: true,
				AvgITL: 0.02, AvgITLKnown: true, RunningRequests: 8, RunningRequestsKnown: true,
				OutputTokensBuckets: []domain.LengthBucket{{UpperBound: 256, Rate: 2}}},
		}}
	router := &domain.RouterLatency{TTFTBuckets: []domain.LatencyBucket{{UpperBound: 1, Rate: 2}}, TTFTSumRate: 1, TTFTCountRate: 2,
		TPOTSumRate: 0.04, TPOTCountRate: 2}
	return in, router
}

// A missing input is reported per objective, by name; an objective whose
// inputs were all read is not-built, since the component prices nothing
// until the models are frozen.
func TestObserve_ReportsMissingSignalsPerObjective(t *testing.T) {
	rec := &recorder{}
	in, router := pd()
	got := NewWith(rec).Observe(in, router)
	assert.Equal(t, Coverage{Reason: constants.PredictedLatencySignalMissing,
		Missing: []string{"prefill-time-mean", "queue-time-mean", "prompt-tokens-histogram", "queue-time-histogram"}},
		got[whatif.ObjectiveTTFT])
	assert.Equal(t, Coverage{Reason: constants.PredictedLatencySignalMissing, Missing: []string{"router-tpot-histogram"}},
		got[whatif.ObjectiveTPOT])
	assert.Equal(t, got[whatif.ObjectiveTTFT], rec.published["ns/m/ttft"])

	// With the collector's prediction rows, every input is read.
	p := &in.ReplicaMetrics[0]
	p.AvgPrefillTime, p.AvgPrefillTimeKnown, p.AvgQueueTime, p.AvgQueueTimeKnown = 0.2, true, 0.01, true
	p.PromptTokensBuckets = []domain.LengthBucket{{UpperBound: 1000, Rate: 1}}
	p.QueueTimeBuckets = []domain.LatencyBucket{{UpperBound: 0.1, Rate: 1}}
	router.TPOTBuckets = []domain.LatencyBucket{{UpperBound: 0.05, Rate: 2}}
	got = NewWith(rec).Observe(in, router)
	for _, o := range Objectives {
		assert.Equal(t, Coverage{Reason: constants.PredictedLatencyNotBuilt}, got[o], o)
		assert.Nil(t, got[o].Figures, "the stub prices nothing")
	}
}

// A model no longer reconciled is forgotten once, the others kept.
func TestRetain_ForgetsTheModelsNotActive(t *testing.T) {
	rec := &recorder{}
	p := NewWith(rec)
	in, router := pd()
	p.Observe(in, router)
	in.ModelID = "other"
	p.Observe(in, router)
	p.Retain(map[string]bool{"ns/other": true})
	assert.Equal(t, []string{"ns/m"}, rec.forgot)
	p.Retain(map[string]bool{"ns/other": true})
	assert.Equal(t, []string{"ns/m"}, rec.forgot, "forgotten once")
	p.Retain(nil)
	assert.Equal(t, []string{"ns/m", "ns/other"}, rec.forgot)
}

// Through the metrics package: the coverage series and the missing-signal
// series a cycle publishes, and none of the figures.
func TestNew_PublishesTheCoverageSeries(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	in, router := pd()
	p := New()
	p.Observe(in, router)
	n, err := testutil.GatherAndCount(registry, constants.LLMSMPredictedLatencyCoverage)
	require.NoError(t, err)
	assert.Equal(t, 2*len(constants.PredictedLatencyReasons), n)
	n, err = testutil.GatherAndCount(registry, constants.LLMSMPredictedLatencyMissingSignal)
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	n, err = testutil.GatherAndCount(registry, constants.LLMSMPredictedTTFTSeconds, constants.LLMSMPredictedTPOTSeconds)
	require.NoError(t, err)
	assert.Zero(t, n)
	p.Retain(nil)
	n, err = testutil.GatherAndCount(registry, constants.LLMSMPredictedLatencyCoverage, constants.LLMSMPredictedLatencyMissingSignal)
	require.NoError(t, err)
	assert.Zero(t, n)
}
