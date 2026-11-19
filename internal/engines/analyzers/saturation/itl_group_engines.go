package saturation

import (
	"slices"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The ITL line of a LeaderWorkerSet group whose Pods each run an engine
// reads the engines one by one (domain.EngineRows). A row of several
// Pods' engines merged, which carries no engines, is a different reading:
// a mean over them, one point a cycle, where its engines are a point each
// around it. A window that held such rows and goes on to take the engines
// mixes the two until BucketRetention ages the means out, and reaches its
// sample thresholds sooner than either would alone. So the first cycle a
// key's rows carry a group's engines, its window starts over
// (itlReadsEnginesFirst).
//
// That happens only while a controller runs: a variant whose rows had no
// engines and now do. A controller upgrade does not reach it, since the
// windows are not persisted and start empty, so the first cycle after a
// restart clears nothing. Only that first cycle clears: a cycle whose group
// arrives as its leader's row alone (its workers not reporting) is the
// leader's own engine, a reading of the same kind, and does not clear the
// window again.

// readsEngines reports whether any of variant's rows is a group read engine
// by engine.
func readsEngines(rows []domain.ReplicaMetrics, variant string) bool {
	return slices.ContainsFunc(rows, func(rm domain.ReplicaMetrics) bool {
		return rm.VariantName == variant && len(rm.GroupEngines) > 0
	})
}

// itlReadsEnginesFirst clears key's ITL window the first cycle its rows
// carry a group's engines (engines), and notes the key; it reports whether
// it cleared any reading.
func (a *SaturationAnalyzer) itlReadsEnginesFirst(key, variant string, engines bool, logger logr.Logger) bool {
	if !engines {
		return false
	}
	a.mu.Lock()
	seen := a.itlReadsEngines[key]
	a.itlReadsEngines[key] = true
	a.mu.Unlock()
	if seen {
		return false
	}
	cleared := a.itlTracker.ClearWindow(key)
	if cleared {
		logger.Info("itl-window-cleared", "variant", variant, "key", key,
			"reason", "the variant's rows now carry a group's engines, read one by one")
	}
	return cleared
}
