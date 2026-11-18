package saturation

import (
	"slices"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The ITL line of a LeaderWorkerSet group whose Pods each run an engine
// reads the engines one by one (domain.EngineRows), where a controller
// before it read the group's merged row. The two are different readings of
// the same replicas: a group's row is a mean over its engines, one point a
// cycle, and its engines are a point each around it. A window that held the
// means and goes on to take the engines mixes them until BucketRetention
// ages the means out, and reaches its sample thresholds sooner than either
// would alone. So the first cycle a key's rows carry a group's engines, its
// window starts over (itlReadsEnginesFirst).
//
// Only that first cycle. A cycle whose group arrives as its leader's row
// alone (its workers not reporting) is the leader's own engine, a reading
// of the same kind, and does not clear the window again. A controller
// restart starts the windows empty, so the first cycle after one clears
// nothing.

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
