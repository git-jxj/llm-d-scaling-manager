package metrics

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

var (
	predictedTTFT          *prometheus.GaugeVec
	predictedTPOT          *prometheus.GaugeVec
	predictedCoverage      *prometheus.GaugeVec
	predictedMissingSignal *prometheus.GaugeVec

	// predictedMissing is the signals last published missing per
	// namespace|model|objective, so a signal read again has its series
	// deleted rather than left at a stale 1.
	predictedMu      sync.Mutex
	predictedMissing = map[string][]string{}
)

// registerPredictedLatencyMetrics creates and registers the predicted-latency
// component's gauges. Called from InitMetrics, after controllerInstance is
// set.
func registerPredictedLatencyMetrics(registry prometheus.Registerer) error {
	withInstance := func(labels ...string) []string {
		if controllerInstance != "" {
			labels = append(labels, constants.LabelControllerInstance)
		}
		return labels
	}
	gauge := func(name, help string, labels []string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	}
	figureLabels := withInstance(constants.LabelNamespace, constants.LabelModelName, constants.LabelStatistic)
	predictedTTFT = gauge(constants.LLMSMPredictedTTFTSeconds,
		"Predicted latency, shadow only: the TTFT the capacity planner's TTFT model predicts for a model at its "+
			"current load and replicas, in seconds, by statistic (mean, p90, p95). Absent unless "+
			"llmsm_predicted_latency_coverage is priced for ttft. Nothing acts on it.",
		figureLabels)
	predictedTPOT = gauge(constants.LLMSMPredictedTPOTSeconds,
		"Predicted latency, shadow only: the TPOT the capacity planner's TPOT model predicts for a model at its "+
			"current load and replicas, in seconds, by statistic (mean, p90, p95). Absent unless "+
			"llmsm_predicted_latency_coverage is priced for tpot. Nothing acts on it.",
		figureLabels)
	predictedCoverage = gauge(constants.LLMSMPredictedLatencyCoverage,
		"Predicted latency: 1 for why a model's prediction of an objective is what it is, 0 for the others: "+
			"priced, abstained, signal-missing, window-short, not-built.",
		withInstance(constants.LabelNamespace, constants.LabelModelName, constants.LabelObjective, constants.LabelReason))
	predictedMissingSignal = gauge(constants.LLMSMPredictedLatencyMissingSignal,
		"Predicted latency: 1 for each input of an objective's model the controller did not read this cycle; "+
			"absent for an input it read.",
		withInstance(constants.LabelNamespace, constants.LabelModelName, constants.LabelObjective, constants.LabelSignal))
	for name, c := range map[string]prometheus.Collector{
		constants.LLMSMPredictedTTFTSeconds:          predictedTTFT,
		constants.LLMSMPredictedTPOTSeconds:          predictedTPOT,
		constants.LLMSMPredictedLatencyCoverage:      predictedCoverage,
		constants.LLMSMPredictedLatencyMissingSignal: predictedMissingSignal,
	} {
		if err := registry.Register(c); err != nil {
			return fmt.Errorf("failed to register %s: %w", name, err)
		}
	}
	predictedMu.Lock()
	predictedMissing = map[string][]string{}
	predictedMu.Unlock()
	return nil
}

// predictedLabels is one model's label set, with the instance where set.
func predictedLabels(namespace, modelID string, more prometheus.Labels) prometheus.Labels {
	labels := prometheus.Labels{constants.LabelNamespace: namespace, constants.LabelModelName: modelID}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	for k, v := range more {
		labels[k] = v
	}
	return labels
}

// predictedFigures is the gauge an objective's figures are published on;
// nil for an objective it does not know.
func predictedFigures(objective string) *prometheus.GaugeVec {
	switch objective {
	case constants.SLOObjectiveTTFT:
		return predictedTTFT
	case constants.SLOObjectiveTPOT:
		return predictedTPOT
	}
	return nil
}

// RecordPredictedLatency publishes one model's prediction of an objective
// (constants.SLOObjectiveTTFT or SLOObjectiveTPOT): reason, one of
// constants.PredictedLatencyReasons, as the one coverage series at 1;
// missing, the inputs not read this cycle; and figures, by statistic
// (constants.PredictedLatencyStatistics), which are published only where
// reason is priced. A series a previous cycle published and this one does
// not hold is deleted. Nil-guarded so tests that never call InitMetrics are
// a no-op.
func RecordPredictedLatency(namespace, modelID, objective, reason string, missing []string, figures map[string]float64) {
	fig := predictedFigures(objective)
	if predictedCoverage == nil || fig == nil {
		return
	}
	for _, r := range constants.PredictedLatencyReasons {
		v := 0.0
		if r == reason {
			v = 1
		}
		predictedCoverage.With(predictedLabels(namespace, modelID,
			prometheus.Labels{constants.LabelObjective: objective, constants.LabelReason: r})).Set(v)
	}
	for _, st := range constants.PredictedLatencyStatistics {
		l := predictedLabels(namespace, modelID, prometheus.Labels{constants.LabelStatistic: st})
		if v, ok := figures[st]; ok && reason == constants.PredictedLatencyPriced {
			fig.With(l).Set(v)
		} else {
			fig.Delete(l)
		}
	}
	for _, s := range missing {
		predictedMissingSignal.With(predictedLabels(namespace, modelID,
			prometheus.Labels{constants.LabelObjective: objective, constants.LabelSignal: s})).Set(1)
	}
	key := namespace + "\x00" + modelID + "\x00" + objective
	predictedMu.Lock()
	defer predictedMu.Unlock()
	for _, s := range predictedMissing[key] {
		if !slices.Contains(missing, s) {
			predictedMissingSignal.Delete(predictedLabels(namespace, modelID,
				prometheus.Labels{constants.LabelObjective: objective, constants.LabelSignal: s}))
		}
	}
	if len(missing) == 0 {
		delete(predictedMissing, key)
		return
	}
	predictedMissing[key] = slices.Clone(missing)
}

// DeletePredictedLatencyForModel removes every predicted-latency series of a
// model: it stopped being reconciled, or the component stopped predicting
// for it.
func DeletePredictedLatencyForModel(namespace, modelID string) {
	if predictedCoverage == nil {
		return
	}
	match := prometheus.Labels{constants.LabelNamespace: namespace, constants.LabelModelName: modelID}
	for _, g := range []*prometheus.GaugeVec{predictedTTFT, predictedTPOT, predictedCoverage, predictedMissingSignal} {
		g.DeletePartialMatch(match)
	}
	prefix := namespace + "\x00" + modelID + "\x00"
	predictedMu.Lock()
	defer predictedMu.Unlock()
	for key := range predictedMissing {
		if strings.HasPrefix(key, prefix) {
			delete(predictedMissing, key)
		}
	}
}
