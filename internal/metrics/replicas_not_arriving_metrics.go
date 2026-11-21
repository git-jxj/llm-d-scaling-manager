package metrics

import (
	"fmt"
	"maps"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

var (
	replicasNotArriving *prometheus.GaugeVec
	// notArrivingPublished is the series set for each variant, keyed
	// namespace/variant, so a changed reason or a cleared shortfall deletes
	// the series it replaces.
	notArrivingPublished = map[string]prometheus.Labels{}
	notArrivingMu        sync.Mutex
)

// registerReplicasNotArrivingMetrics creates and registers
// llmsm_replicas_not_arriving. Called from InitMetrics, after
// controllerInstance is set.
func registerReplicasNotArrivingMetrics(registry prometheus.Registerer) error {
	withInstance := func(labels ...string) []string {
		if controllerInstance != "" {
			labels = append(labels, constants.LabelControllerInstance)
		}
		return labels
	}
	replicasNotArriving = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMReplicasNotArriving,
		Help: "Replicas a variant asked for beyond those Ready, once the shortfall has lasted longer than " +
			"twice its start time (at least 5 minutes), by the reason read from its pods.",
	}, withInstance(constants.LabelNamespace, constants.LabelVariantName, constants.LabelRole, constants.LabelReason))
	if err := registry.Register(replicasNotArriving); err != nil {
		return fmt.Errorf("failed to register %s: %w", constants.LLMSMReplicasNotArriving, err)
	}
	notArrivingMu.Lock()
	notArrivingPublished = map[string]prometheus.Labels{}
	notArrivingMu.Unlock()
	return nil
}

// SetReplicasNotArriving publishes a variant's missing replicas under reason,
// deleting the series it published before under another role or reason, so a
// variant has at most one series.
func SetReplicasNotArriving(namespace, variantName, role, reason string, missing int) {
	notArrivingMu.Lock()
	defer notArrivingMu.Unlock()
	if replicasNotArriving == nil {
		return
	}
	l := prometheus.Labels{
		constants.LabelNamespace:   namespace,
		constants.LabelVariantName: variantName,
		constants.LabelRole:        role,
		constants.LabelReason:      reason,
	}
	if controllerInstance != "" {
		l[constants.LabelControllerInstance] = controllerInstance
	}
	key := namespace + "/" + variantName
	if prev, ok := notArrivingPublished[key]; ok && !maps.Equal(prev, l) {
		replicasNotArriving.Delete(prev)
	}
	replicasNotArriving.With(l).Set(float64(missing))
	notArrivingPublished[key] = l
}

// ClearReplicasNotArriving deletes a variant's series, if it has one.
func ClearReplicasNotArriving(namespace, variantName string) {
	notArrivingMu.Lock()
	defer notArrivingMu.Unlock()
	if replicasNotArriving == nil {
		return
	}
	key := namespace + "/" + variantName
	if prev, ok := notArrivingPublished[key]; ok {
		replicasNotArriving.Delete(prev)
		delete(notArrivingPublished, key)
	}
}
