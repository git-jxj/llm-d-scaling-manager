package metrics

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// One coverage reason is 1 per model and objective, the others 0; a signal
// read again loses its missing series; the figures exist only while priced;
// deleting the model removes every series of it and none of another's.
func TestRecordPredictedLatency(t *testing.T) {
	_, registry := engineConfigEmitter(t)
	coverage := func(objective, reason string) float64 {
		t.Helper()
		v, ok := gaugeAt(t, registry, constants.LLMSMPredictedLatencyCoverage, map[string]string{
			constants.LabelModelName: "m", constants.LabelObjective: objective, constants.LabelReason: reason})
		if !ok {
			t.Fatalf("no coverage series for %s %s", objective, reason)
		}
		return v
	}

	RecordPredictedLatency("ns", "m", constants.SLOObjectiveTTFT, constants.PredictedLatencySignalMissing,
		[]string{"prefill-time-mean", "queue-time-mean"}, nil)
	if got := countSeries(t, registry, constants.LLMSMPredictedLatencyCoverage); got != len(constants.PredictedLatencyReasons) {
		t.Fatalf("%d coverage series, want one per reason", got)
	}
	if coverage(constants.SLOObjectiveTTFT, constants.PredictedLatencySignalMissing) != 1 ||
		coverage(constants.SLOObjectiveTTFT, constants.PredictedLatencyNotBuilt) != 0 {
		t.Fatal("signal-missing is not the one reason at 1")
	}
	if got := countSeries(t, registry, constants.LLMSMPredictedLatencyMissingSignal); got != 2 {
		t.Fatalf("%d missing-signal series, want 2", got)
	}

	// One read again: its series goes, the other stays.
	RecordPredictedLatency("ns", "m", constants.SLOObjectiveTTFT, constants.PredictedLatencySignalMissing,
		[]string{"queue-time-mean"}, nil)
	if _, ok := gaugeAt(t, registry, constants.LLMSMPredictedLatencyMissingSignal, map[string]string{
		constants.LabelSignal: "prefill-time-mean"}); ok {
		t.Fatal("a signal read again kept its missing series")
	}
	if got := countSeries(t, registry, constants.LLMSMPredictedLatencyMissingSignal); got != 1 {
		t.Fatalf("%d missing-signal series, want 1", got)
	}

	// Figures only while priced.
	figures := map[string]float64{"mean": 0.2, "p90": 0.4, "p95": 0.5}
	RecordPredictedLatency("ns", "m", constants.SLOObjectiveTTFT, constants.PredictedLatencyNotBuilt, nil, figures)
	if got := countSeries(t, registry, constants.LLMSMPredictedTTFTSeconds); got != 0 {
		t.Fatalf("%d TTFT figures while not priced, want 0", got)
	}
	if got := countSeries(t, registry, constants.LLMSMPredictedLatencyMissingSignal); got != 0 {
		t.Fatalf("%d missing-signal series with none missing, want 0", got)
	}
	RecordPredictedLatency("ns", "m", constants.SLOObjectiveTTFT, constants.PredictedLatencyPriced, nil, figures)
	if v, ok := gaugeAt(t, registry, constants.LLMSMPredictedTTFTSeconds, map[string]string{constants.LabelStatistic: "p90"}); !ok || v != 0.4 {
		t.Fatalf("p90 = %v, %v; want 0.4, true", v, ok)
	}
	RecordPredictedLatency("ns", "m", constants.SLOObjectiveTTFT, constants.PredictedLatencyAbstained, nil, nil)
	if got := countSeries(t, registry, constants.LLMSMPredictedTTFTSeconds); got != 0 {
		t.Fatalf("%d TTFT figures after the model abstained, want 0", got)
	}

	// An unknown objective publishes nothing.
	RecordPredictedLatency("ns", "m", "itl", constants.PredictedLatencyPriced, nil, figures)
	if got := countSeries(t, registry, constants.LLMSMPredictedLatencyCoverage); got != len(constants.PredictedLatencyReasons) {
		t.Fatalf("%d coverage series after an unknown objective, want %d", got, len(constants.PredictedLatencyReasons))
	}

	RecordPredictedLatency("ns", "m", constants.SLOObjectiveTPOT, constants.PredictedLatencySignalMissing, []string{"router-tpot-histogram"}, nil)
	RecordPredictedLatency("ns", "other", constants.SLOObjectiveTPOT, constants.PredictedLatencySignalMissing, []string{"router-tpot-histogram"}, nil)
	DeletePredictedLatencyForModel("ns", "m")
	if got := countSeries(t, registry, constants.LLMSMPredictedLatencyCoverage); got != len(constants.PredictedLatencyReasons) {
		t.Fatalf("%d coverage series after deleting m, want model other's %d", got, len(constants.PredictedLatencyReasons))
	}
	if got := countSeries(t, registry, constants.LLMSMPredictedLatencyMissingSignal); got != 1 {
		t.Fatalf("%d missing-signal series after deleting m, want model other's 1", got)
	}
}

// Before InitMetrics nothing is published and nothing panics.
func TestRecordPredictedLatency_NilGuarded(t *testing.T) {
	saved := predictedCoverage
	predictedCoverage = nil
	defer func() { predictedCoverage = saved }()
	RecordPredictedLatency("ns", "m", constants.SLOObjectiveTTFT, constants.PredictedLatencyNotBuilt, []string{"x"}, nil)
	DeletePredictedLatencyForModel("ns", "m")
}
