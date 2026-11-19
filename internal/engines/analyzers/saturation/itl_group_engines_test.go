package saturation

import (
	"context"
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
func itlGroup(pod string) domain.ReplicaMetrics { return itlGroupAt(pod, 0.30, 0.40, 0.50, 0.60) }

// itlGroupAt is itlGroup with its engines at fills.
func itlGroupAt(pod string, fills ...float64) domain.ReplicaMetrics {
	mean := 0.0
	for _, k := range fills {
		mean += k / float64(len(fills))
	}
	g := itlSettleRow(pod, mean)
	g.EngineRanks = len(fills)
	for w, k := range fills {
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
	settling := make([]int64, 0, restartAt+2)
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

// itlReport runs one cycle and returns the ITL line's report of variant v.
func itlReport(t *testing.T, r *restartRig, rows []domain.ReplicaMetrics) learn.Report {
	t.Helper()
	r.cycleCtx(context.Background(), rows, decodeVariants("v"))
	reports := reportsOf(r.a.LearnerReports()["test-ns|test-model"], learn.ITL)
	require.Len(t, reports, 1)
	return reports[0]
}

// Two groups, each its engines spread differently over the fill and on one
// line: the window counts engines, eight a cycle, and the line is fitted
// from them (ols). Their merged rows
// are two readings a cycle.
func TestITLGroupEngines_TwoGroupsFitTheLineFromTheirEngines(t *testing.T) {
	groups := func() []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{itlGroupAt("g0", 0.25, 0.35, 0.45, 0.55), itlGroupAt("g1", 0.30, 0.42, 0.54, 0.66)}
	}
	merged := func() []domain.ReplicaMetrics {
		rows := groups()
		for i := range rows {
			rows[i].GroupEngines = nil
		}
		return rows
	}
	firstSamples := func(rows func() []domain.ReplicaMetrics) (int, learn.Report) {
		r := itlRig(t, nil)
		first := 0
		var rep learn.Report
		for range 4 * itlSettleCycles {
			rep = itlReport(t, r, rows())
			if first == 0 {
				first = rep.Samples
			}
		}
		return first, rep
	}
	first, rep := firstSamples(groups)
	assert.Equal(t, 8, first, "the first admitting cycle: the two groups' eight engines")
	assert.Equal(t, itlSourceOLS, rep.Source, "the line is fitted from the engines' spread")

	first, _ = firstSamples(merged)
	assert.Equal(t, 2, first, "premise: the merged rows are one reading a group")
}

// With no group's engines the rows are read as they are: the same counts as
// before the engines were read.
func TestITLGroupEngines_NoEnginesReadsTheRowsAsTheyAre(t *testing.T) {
	rows := itlSettleFleet()
	assert.Equal(t, rows, domain.EngineRows(rows))
	r := itlRig(t, nil)
	var counts map[learn.Reason]int64
	for range 2 * itlSettleCycles {
		counts = itlCycle(t, r, itlSettleFleet())
	}
	assert.Equal(t, map[learn.Reason]int64{"offered": 3}, counts)
}
