package constants

// The slo analyzer's v2 latency models (slo.SwitchV2Inputs,
// docs/proposals/predicted-latency-in-controller.md): the series that say
// whether each model can price a model's latency, and the predicted TPOT the
// analyzer will publish beside its predicted TTFT. Shadow; absent while the
// switch is off.
const (
	// LLMSMSLOPredictedTPOTSeconds is the TPOT the slo analyzer predicts
	// for a decode variant at its current load, by statistic (mean, p90,
	// p95) and learner (slo or tpot-distribution, side by side). Registered;
	// set once the TPOT models are priced in the analyzer
	// (after the models' freeze).
	LLMSMSLOPredictedTPOTSeconds = "llmsm_slo_predicted_tpot_seconds"
	// LLMSMSLOModelCoverage is 1 for why an objective's v2 model stands
	// where it does for a model (SLOModelReasons), 0 for the others.
	LLMSMSLOModelCoverage = "llmsm_slo_model_coverage"
	// LLMSMSLOModelInputMissing is 1 for each input of an objective's v2
	// model the cycle did not read (whatif.ModelInputs); absent for one it
	// read.
	LLMSMSLOModelInputMissing = "llmsm_slo_model_input_missing"

	// LabelSignal names a model input: the whatif.ModelInputs name.
	LabelSignal = "signal"
)

// The reasons of llmsm_slo_model_coverage, one of which is 1 per model and
// objective.
const (
	// SLOModelPriced: the model priced the objective.
	SLOModelPriced = "priced"
	// SLOModelAbstained: the model refused the window, as the capacity
	// planner would on the same window.
	SLOModelAbstained = "abstained"
	// SLOModelSignalMissing: an input the model reads was not read this
	// cycle; llmsm_slo_model_input_missing names it.
	SLOModelSignalMissing = "signal-missing"
	// SLOModelWindowShort: every input was read, the learner's window does
	// not hold enough steps to fit yet.
	SLOModelWindowShort = "window-short"
	// SLOModelNotBuilt: every input was read, and the analyzer does not
	// price with the model yet: the models are not frozen.
	SLOModelNotBuilt = "not-built"
)

// SLOModelReasons is every coverage reason, in the order the series are
// published.
var SLOModelReasons = []string{
	SLOModelPriced, SLOModelAbstained, SLOModelSignalMissing, SLOModelWindowShort, SLOModelNotBuilt,
}
