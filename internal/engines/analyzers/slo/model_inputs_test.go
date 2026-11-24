package slo

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// modelInputsCycle is a P/D model's cycle reading every input but what a
// controller that records nothing collects: the prefill pods' timings and
// histograms, and the router's TPOT histogram.
func modelInputsCycle() (domain.AnalyzerInput, *domain.RouterLatency) {
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

// The v2 learners are registered by name, one per objective, and each name
// is off the forget list until it learns (learn.Learners), so the
// annotation's spec does not hold an analyzer to forgetting it yet.
func TestModelLearners_AreRegisteredOffTheForgetList(t *testing.T) {
	require.Len(t, modelLearners, 2)
	assert.Equal(t, learn.TTFTMixture, modelLearners[0].name)
	assert.Equal(t, learn.TPOTDistribution, modelLearners[1].name)
	for _, l := range modelLearners {
		assert.NotContains(t, learn.Learners(), l.name)
		assert.NotContains(t, learn.Reporters(), l.name)
	}
	// The objectives are the ones the slo series name.
	assert.Equal(t, constants.SLOObjectiveTTFT, whatif.ObjectiveTTFT)
	assert.Equal(t, constants.SLOObjectiveTPOT, whatif.ObjectiveTPOT)
}

// A missing input is reported per objective, by name; an objective whose
// inputs were all read is not-built: the learners learn nothing until the
// models are frozen. Both publish under the slo family, and no predicted
// TPOT is set.
func TestObserveModelInputs_ReportsWhereEachModelStands(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	a := New(nil, nil)
	in, router := modelInputsCycle()
	assert.False(t, a.ReadsModelInputs(), "off from the constructor")
	assert.Nil(t, a.ObserveModelInputs(in, router), "off, it reads nothing")
	n, err := testutil.GatherAndCount(registry, constants.LLMSMSLOModelCoverage)
	require.NoError(t, err)
	require.Zero(t, n, "off, it publishes nothing")
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchV2Inputs: true}))
	assert.True(t, a.ReadsModelInputs())
	got := a.ObserveModelInputs(in, router)
	assert.Equal(t, ModelCoverage{Learner: learn.TTFTMixture, Reason: constants.SLOModelSignalMissing,
		Missing: []string{"prefill-time-mean", "queue-time-mean", "prompt-tokens-histogram", "queue-time-histogram"}},
		got[whatif.ObjectiveTTFT])
	assert.Equal(t, ModelCoverage{Learner: learn.TPOTDistribution, Reason: constants.SLOModelSignalMissing,
		Missing: []string{"router-tpot-histogram"}}, got[whatif.ObjectiveTPOT])
	n, err = testutil.GatherAndCount(registry, constants.LLMSMSLOModelInputMissing)
	require.NoError(t, err)
	assert.Equal(t, 5, n)

	// With the collector's prediction rows, every input is read.
	p := &in.ReplicaMetrics[0]
	p.AvgPrefillTime, p.AvgPrefillTimeKnown, p.AvgQueueTime, p.AvgQueueTimeKnown = 0.2, true, 0.01, true
	p.PromptTokensBuckets = []domain.LengthBucket{{UpperBound: 1000, Rate: 1}}
	p.QueueTimeBuckets = []domain.LatencyBucket{{UpperBound: 0.1, Rate: 1}}
	router.TPOTBuckets = []domain.LatencyBucket{{UpperBound: 0.05, Rate: 2}}
	got = a.ObserveModelInputs(in, router)
	for _, objective := range []string{whatif.ObjectiveTTFT, whatif.ObjectiveTPOT} {
		assert.Equal(t, constants.SLOModelNotBuilt, got[objective].Reason, objective)
		assert.Empty(t, got[objective].Missing, objective)
	}
	n, err = testutil.GatherAndCount(registry, constants.LLMSMSLOModelInputMissing, constants.LLMSMSLOPredictedTPOTSeconds)
	require.NoError(t, err)
	assert.Zero(t, n)
	n, err = testutil.GatherAndCount(registry, constants.LLMSMSLOModelCoverage)
	require.NoError(t, err)
	assert.Equal(t, 2*len(constants.SLOModelReasons), n)
}
