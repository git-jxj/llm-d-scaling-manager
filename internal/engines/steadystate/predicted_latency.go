package steadystate

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// The predicted-latency component's wiring (--predicted-latency,
// docs/proposals/predicted-latency-in-controller.md). Off unless
// Engine.PredictedLatency is set. It reads what the collector kept for it
// and publishes its own series; nothing it does reaches an analyzer, the
// optimizer or a scaling decision.

// predictionNamespaces is every namespace of modelGroups while the component
// is on, none while it is off: the collector issues its queries there
// (collector.ReadPredictionInputsIn).
func (e *Engine) predictionNamespaces(modelGroups map[string][]llmdvariant.Variant) map[string]bool {
	if e.PredictedLatency == nil {
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

// observePredictedLatency hands the component one model's cycle: the rows
// the collector kept for it, cut to the replicas the analyzers read
// (routing drops a pod no InferencePool selects), the model's variant
// states, and the router's latency as the models fit on it.
func (e *Engine) observePredictedLatency(ctx context.Context, data *modelData) {
	if e.PredictedLatency == nil || data == nil {
		return
	}
	rows, ok := e.ReplicaMetricsCollector.PredictionRows(data.namespace, data.modelID)
	if !ok {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Predicted latency: the cycle kept no rows for the model",
			"namespace", data.namespace, "modelID", data.modelID)
		rows = data.replicaMetrics
	}
	in := domain.AnalyzerInput{Namespace: data.namespace, ModelID: data.modelID,
		ReplicaMetrics: sameReplicas(rows, data.replicaMetrics), VariantStates: data.variantStates}
	router := e.ReplicaMetricsCollector.CollectPredictionLatency(ctx, data.modelID, data.namespace)
	e.PredictedLatency.Observe(in, router)
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

// retainPredictedLatency forgets the component's series of every model
// activeKeys does not hold; nil forgets every one.
func (e *Engine) retainPredictedLatency(activeKeys map[string]bool) {
	if e.PredictedLatency == nil {
		return
	}
	e.PredictedLatency.Retain(activeKeys)
}
