package constants

// The predicted-latency component's series (--predicted-latency,
// docs/proposals/predicted-latency-in-controller.md, "Metrics"). Shadow:
// nothing acts on them. Absent while the switch is off.
const (
	// LLMSMPredictedTTFTSeconds is the TTFT the capacity planner's
	// experimental TTFT model predicts for a model at its current load and
	// replicas, by statistic (mean, p90, p95). Absent unless
	// llmsm_predicted_latency_coverage reads priced for ttft.
	LLMSMPredictedTTFTSeconds = "llmsm_predicted_ttft_seconds"
	// LLMSMPredictedTPOTSeconds is the TPOT the planner's TPOT model
	// predicts, by statistic, absent as LLMSMPredictedTTFTSeconds is.
	LLMSMPredictedTPOTSeconds = "llmsm_predicted_tpot_seconds"
	// LLMSMPredictedLatencyCoverage is 1 for why a model's prediction of an
	// objective is what it is (PredictedLatencyReasons), 0 for the others.
	LLMSMPredictedLatencyCoverage = "llmsm_predicted_latency_coverage"
	// LLMSMPredictedLatencyMissingSignal is 1 for each input of an
	// objective's model the cycle did not read, by signal
	// (whatif.ModelInputs); absent for a signal that was read.
	LLMSMPredictedLatencyMissingSignal = "llmsm_predicted_latency_missing_signal"

	// LabelSignal names a model input: the whatif.ModelInputs name.
	LabelSignal = "signal"
)

// The reasons of llmsm_predicted_latency_coverage, one of which is 1 per
// model and objective.
const (
	// PredictedLatencyPriced: the model priced the objective; its series
	// carry the figures.
	PredictedLatencyPriced = "priced"
	// PredictedLatencyAbstained: the model refused, as the capacity planner
	// would on the same window (an unstationary window, a fit that did not
	// converge, a rate beyond what was measured).
	PredictedLatencyAbstained = "abstained"
	// PredictedLatencySignalMissing: an input the model reads was not read
	// this cycle; llmsm_predicted_latency_missing_signal names it.
	PredictedLatencySignalMissing = "signal-missing"
	// PredictedLatencyWindowShort: every input was read, but the fit window
	// does not hold enough steps yet.
	PredictedLatencyWindowShort = "window-short"
	// PredictedLatencyNotBuilt: every input was read, and the controller
	// does not price with the model yet: the models are not frozen.
	PredictedLatencyNotBuilt = "not-built"
)

// PredictedLatencyReasons is every coverage reason, in the order the series
// are published.
var PredictedLatencyReasons = []string{
	PredictedLatencyPriced, PredictedLatencyAbstained, PredictedLatencySignalMissing,
	PredictedLatencyWindowShort, PredictedLatencyNotBuilt,
}

// PredictedLatencyStatistics is the statistics the predicted series carry.
var PredictedLatencyStatistics = []string{"mean", "p90", "p95"}
