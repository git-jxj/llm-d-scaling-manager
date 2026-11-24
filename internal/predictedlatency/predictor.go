// Package predictedlatency is the controller's predicted-latency component
// (--predicted-latency, issue llm-d-incubation/llm-d-scaling-manager#440,
// docs/proposals/predicted-latency-in-controller.md): it would publish the
// TTFT and TPOT the capacity planner's models predict for each model at its
// current load and replicas, from the planner's own model code.
//
// The models are not frozen, so it prices nothing yet. Each cycle it reads
// a model's inputs as the collector gathered them for it, reports the ones
// missing per objective (whatif.MissingModelInputs), and publishes the
// coverage series: signal-missing where one is, not-built where every one
// was read. It is shadow, off by default, and nothing reads what it
// publishes: no scaling decision moves.
package predictedlatency

import (
	"maps"
	"slices"
	"sync"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// Objectives is the objectives the component predicts, in the order it
// publishes them.
var Objectives = []string{whatif.ObjectiveTTFT, whatif.ObjectiveTPOT}

// Coverage is one objective's state after a cycle: Reason is one of
// constants.PredictedLatencyReasons, Missing the inputs not read (in
// whatif.ModelInputs' order), Figures the prediction by statistic, nil
// unless Reason is priced.
type Coverage struct {
	Reason  string
	Missing []string
	Figures map[string]float64
}

// Publisher is where the component's series go: the metrics package's, or a
// test's.
type Publisher interface {
	Publish(namespace, modelID, objective string, c Coverage)
	Forget(namespace, modelID string)
}

// metricsPublisher publishes through the metrics package.
type metricsPublisher struct{}

// Publish records one objective's coverage and figures.
func (metricsPublisher) Publish(namespace, modelID, objective string, c Coverage) {
	metrics.RecordPredictedLatency(namespace, modelID, objective, c.Reason, c.Missing, c.Figures)
}

// Forget deletes every series of the model.
func (metricsPublisher) Forget(namespace, modelID string) {
	metrics.DeletePredictedLatencyForModel(namespace, modelID)
}

// Predictor is the component. Safe for concurrent use.
type Predictor struct {
	publish Publisher
	mu      sync.Mutex
	// published is the models it published for, by utils.GetNamespacedKey,
	// so Retain can forget the ones no longer reconciled.
	published map[string][2]string
}

// New is a Predictor publishing to the controller's metrics.
func New() *Predictor { return NewWith(metricsPublisher{}) }

// NewWith is a Predictor publishing to pub.
func NewWith(pub Publisher) *Predictor {
	return &Predictor{publish: pub, published: map[string][2]string{}}
}

// Observe reads one model's cycle: in is the model's analyzer input with the
// rows the collector kept for the component (collector.PredictionRows), and
// router the router's latency as the models fit on it
// (collector.CollectPredictionLatency), nil where the router reported none.
// It returns, and publishes, each objective's coverage.
//
// Every prefill and decode variant of the model is pooled: the planner
// prices one prefill and one decode variant at a time, and a model with more
// is to be read per pair once the component prices (the proposal's
// "Per-cycle flow").
func (p *Predictor) Observe(in domain.AnalyzerInput, router *domain.RouterLatency) map[string]Coverage {
	missing := whatif.MissingModelInputs(in, router, "", "")
	out := make(map[string]Coverage, len(Objectives))
	for _, objective := range Objectives {
		c := Coverage{Reason: constants.PredictedLatencyNotBuilt}
		if m := missing[objective]; len(m) > 0 {
			c = Coverage{Reason: constants.PredictedLatencySignalMissing, Missing: m}
		}
		out[objective] = c
		p.publish.Publish(in.Namespace, in.ModelID, objective, c)
	}
	p.mu.Lock()
	p.published[utils.GetNamespacedKey(in.Namespace, in.ModelID)] = [2]string{in.Namespace, in.ModelID}
	p.mu.Unlock()
	return out
}

// Retain forgets every model it published for whose key
// (utils.GetNamespacedKey of its namespace and model) active does not hold;
// with active empty, every one.
func (p *Predictor) Retain(active map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, key := range slices.Sorted(maps.Keys(p.published)) {
		if active[key] {
			continue
		}
		m := p.published[key]
		p.publish.Forget(m[0], m[1])
		delete(p.published, key)
	}
}
