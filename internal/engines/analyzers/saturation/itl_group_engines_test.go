package saturation

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// itlGroup is a LeaderWorkerSet group of variant v whose four Pods each run
// an engine, at fills 0.30 to 0.60: its merged row, at their mean, with the
// engines in it, as the collector gives it.
func itlGroup(pod string) domain.ReplicaMetrics {
	g := itlSettleRow(pod, 0.45)
	g.EngineRanks = 4
	for w, k := range []float64{0.30, 0.40, 0.50, 0.60} {
		name := pod
		if w > 0 {
			name = fmt.Sprintf("%s-%d", pod, w)
		}
		g.GroupEngines = append(g.GroupEngines, domain.EngineReadingOf(itlSettleRow(name, k)))
	}
	return g
}

// itlGroupFleet is one Ready group, one not Ready (a Pod of it is not), and
// one lent from a warm pool.
func itlGroupFleet() []domain.ReplicaMetrics {
	ready, notReady, warm := itlGroup("g0"), itlGroup("g1"), itlGroup("w0")
	notReady.Ready = false
	warm.FromWarmPool = true
	return []domain.ReplicaMetrics{ready, notReady, warm}
}

// The line reads each group's engines in place of its row, never both: the
// Ready group's four engines are readings, the other two groups' eight
// engines are not Ready (a group not Ready, or lent, gives no reading), and
// nothing else is counted. Once settled no engine is.
func TestITLGroupEngines_TheLineReadsEachEngineOnce(t *testing.T) {
	r := itlRig(t, nil)
	var counts map[learn.Reason]int64
	for range 2 * itlSettleCycles {
		counts = itlCycle(t, r, itlGroupFleet())
	}
	assert.Equal(t, map[learn.Reason]int64{"offered": 4, "not-ready": 8}, counts,
		"twelve engines, each once; no group row read beside its engines")
}

// An engine settles as its group, the row the settle tracker observes: a
// group that restarts under its name settles all four of its engines again,
// not only the one whose Pod shares the group row's name.
func TestITLGroupEngines_AnEngineSettlesAsItsGroup(t *testing.T) {
	restartAt := itlSettleCycles + 2
	r := itlRig(t, nil)
	var settling []int64
	for i := range restartAt + 2 {
		f := itlGroupFleet()[:1]
		if i >= restartAt {
			f[0].ReadySince = itlSettleT0.Add(time.Duration(restartAt)*restartCycle - 5*time.Second)
		}
		settling = append(settling, itlCycle(t, r, f)["settling"])
	}
	assert.Equal(t, int64(4), settling[0], "the controller's restart: every engine waits")
	assert.Zero(t, settling[restartAt-1], "settled")
	assert.Equal(t, int64(4), settling[restartAt], "the group restarted: every engine of it settles again")
}

// The first cycle a variant's rows carry a group's engines, the ITL window
// the group's merged rows filled starts over; the cycles after keep what
// the engines add, and a leader-only cycle between does not clear it.
func TestITLGroupEngines_TheWindowStartsOverOnTheEngines(t *testing.T) {
	r := itlRig(t, nil)
	merged := func() []domain.ReplicaMetrics {
		g := itlGroupFleet()[0]
		g.GroupEngines = nil
		return []domain.ReplicaMetrics{g}
	}
	key := r.a.itlWindowKey("test-ns", "test-model", "v", "H200", 1)
	for range 2 * itlSettleCycles {
		itlCycle(t, r, merged())
	}
	before := r.a.itlWindowLen(key)
	require.Greater(t, before, 4, "premise: the merged rows filled the window past one cycle of engines")

	itlCycle(t, r, itlGroupFleet()[:1])
	assert.LessOrEqual(t, r.a.itlWindowLen(key), 4, "cleared, then this cycle's engines at most")
	after := r.a.itlWindowLen(key)
	itlCycle(t, r, merged())
	itlCycle(t, r, itlGroupFleet()[:1])
	assert.Greater(t, r.a.itlWindowLen(key), after, "not cleared again")
}
