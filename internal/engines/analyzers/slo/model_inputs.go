package slo

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The slo analyzer's v2 latency models (SwitchV2Inputs, issue #440,
// docs/proposals/predicted-latency-in-controller.md). The analyzer owns the
// latency predictions; the v2 TTFT and TPOT models of the capacity planner
// join it as learners of the shared learner rules
// (docs/proposals/analyzer-learner.md): each observes its cycle (built by
// whatif.MixtureCycleOf or DecodeCycleOf, the planner's own builders),
// admits, fits on a window, predicts, validates and is kept or forgotten.
//
// The models are not frozen, so the learners are registered and learn
// nothing yet. Each cycle the analyzer reads the inputs the collector kept
// for them (collector.PredictionRows), reports the missing ones per
// objective, and publishes where each model stands. Nothing here reaches a
// sizing decision.

// SwitchV2Inputs is the replay switch of the v2 models' inputs: on, the
// engine has the collector issue them for every namespace with a model
// (collector.ReadPredictionInputsIn) and hands them to ObserveModelInputs.
// Off from the constructor, like every shadow addition until its evidence
// flips the default.
const SwitchV2Inputs = "slo-v2-inputs"

// ReadsModelInputs reports whether the analyzer reads its v2 models' inputs
// (SwitchV2Inputs): the engine asks the collector for them only then.
func (a *Analyzer) ReadsModelInputs() bool { return a.v2Inputs }

// modelLearner is one v2 latency learner as it is registered today: its
// name in the learner vocabulary and the objective it predicts. After the
// freeze it gains the learner steps (observe, fit, predict, forget, report).
type modelLearner struct {
	name      string
	objective string
}

// modelLearners is the v2 latency learners, in the order they report.
var modelLearners = []modelLearner{
	{name: learn.TTFTMixture, objective: whatif.ObjectiveTTFT},
	{name: learn.TPOTDistribution, objective: whatif.ObjectiveTPOT},
}

// ModelCoverage is where one v2 model stands for a model after a cycle:
// Reason is one of constants.SLOModelReasons, Missing the inputs not read,
// in whatif.ModelInputs' order.
type ModelCoverage struct {
	Learner string
	Reason  string
	Missing []string
}

// ObserveModelInputs reads one model's cycle for the v2 learners: in is the
// model's analyzer input with the rows the collector kept for them, router
// the router's latency as they fit on it (collector.CollectPredictionLatency),
// nil where the router reported none. It publishes and returns each
// objective's coverage. Every prefill and decode variant of the model is
// pooled; the planner prices one pair at a time, and the learners will key
// by pair once they learn. With SwitchV2Inputs off it reads nothing and
// returns nil.
func (a *Analyzer) ObserveModelInputs(in domain.AnalyzerInput, router *domain.RouterLatency) map[string]ModelCoverage {
	if !a.v2Inputs {
		return nil
	}
	missing := whatif.MissingModelInputs(in, router, "", "")
	out := make(map[string]ModelCoverage, len(modelLearners))
	for _, l := range modelLearners {
		c := ModelCoverage{Learner: l.name, Reason: constants.SLOModelNotBuilt}
		if m := missing[l.objective]; len(m) > 0 {
			c.Reason, c.Missing = constants.SLOModelSignalMissing, m
		}
		out[l.objective] = c
		metrics.RecordSLOModelInputs(in.Namespace, in.ModelID, l.objective, c.Reason, c.Missing)
	}
	return out
}
