package steadystate

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// The slo analyzer's v2 latency models' wiring (slo.SwitchV2Inputs,
// docs/proposals/predicted-latency-in-controller.md). Off unless the
// slo analyzer reads them (ReadsModelInputs). The collector issues them and
// keeps them apart from the analyzers' rows; the engine hands them to the
// slo analyzer, which owns the latency predictions. Nothing here reaches the
// optimizer or a scaling decision.

// modelInputsObserver is the slo analyzer's export point for the v2 models,
// taken as an optional interface as TakeLearnerReports is: the engine holds
// the analyzer as a sloStateKeeper, whose test doubles need not implement it.
type modelInputsObserver interface {
	ReadsModelInputs() bool
	ObserveModelInputs(in domain.AnalyzerInput, router *domain.RouterLatency) map[string]slo.ModelCoverage
}

var _ modelInputsObserver = (*slo.Analyzer)(nil)

// modelInputs is the slo analyzer as the v2 models' observer while it reads
// their inputs (slo.SwitchV2Inputs), nil otherwise.
func (e *Engine) modelInputs() modelInputsObserver {
	obs, ok := e.sloAnalyzer.(modelInputsObserver)
	if !ok || !obs.ReadsModelInputs() {
		return nil
	}
	return obs
}

// predictionNamespaces is every namespace of modelGroups while the switch is
// on, none while it is off: the collector issues the v2 models' queries
// there (collector.ReadPredictionInputsIn).
func (e *Engine) predictionNamespaces(modelGroups map[string][]llmdvariant.Variant) map[string]bool {
	if e.modelInputs() == nil {
		return nil
	}
	namespaces := map[string]bool{}
	for _, vas := range modelGroups {
		if len(vas) > 0 {
			namespaces[vas[0].Namespace] = true
		}
	}
	return namespaces
}

// observeModelInputs hands the slo analyzer one model's cycle for its v2
// models: the rows the collector kept for them, cut to the replicas the
// analyzers read (routing drops a pod no InferencePool selects), the
// model's variant states, and the router's latency as the models fit on it.
func (e *Engine) observeModelInputs(ctx context.Context, data *modelData) {
	obs := e.modelInputs()
	if obs == nil || data == nil {
		return
	}
	rows, kept := e.ReplicaMetricsCollector.PredictionRows(data.namespace, data.modelID)
	if !kept {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("SLO v2 inputs: the cycle kept no rows for the model",
			"namespace", data.namespace, "modelID", data.modelID)
		rows = data.replicaMetrics
	}
	in := domain.AnalyzerInput{Namespace: data.namespace, ModelID: data.modelID,
		ReplicaMetrics: sameReplicas(rows, data.replicaMetrics), VariantStates: data.variantStates}
	router := e.ReplicaMetricsCollector.CollectPredictionLatency(ctx, data.modelID, data.namespace)
	obs.ObserveModelInputs(in, router)
	if e.sloModelInputs == nil {
		e.sloModelInputs = map[string][2]string{}
	}
	e.sloModelInputs[utils.GetNamespacedKey(data.namespace, data.modelID)] = [2]string{data.namespace, data.modelID}
}

// sameReplicas is the rows of kept whose pod has a row in read, in kept's
// order.
func sameReplicas(kept, read []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	pods := make(map[string]bool, len(read))
	for i := range read {
		pods[read[i].PodName] = true
	}
	out := make([]domain.ReplicaMetrics, 0, len(read))
	for i := range kept {
		if pods[kept[i].PodName] {
			out = append(out, kept[i])
		}
	}
	return out
}

// retainModelInputs deletes the v2 model series of every model activeKeys
// does not hold; nil deletes every one. Published before the model reaches
// recordAnalyzerMetrics, so tracked apart from lastAnalyzerSeries.
func (e *Engine) retainModelInputs(activeKeys map[string]bool) {
	for key, m := range e.sloModelInputs {
		if !activeKeys[key] {
			metrics.DeleteSLOModelInputs(m[0], m[1])
			delete(e.sloModelInputs, key)
		}
	}
}
