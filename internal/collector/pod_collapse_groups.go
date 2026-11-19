package collector

import (
	"cmp"
	"context"
	"slices"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// placedRow is a replica row and the place of its Pod in a LeaderWorkerSet,
// as attributeInstance read it.
type placedRow struct {
	row   domain.ReplicaMetrics
	place lwsPlace
}

// lwsGroup is the rows of one LeaderWorkerSet group of one model, the
// leader's first (then by worker index).
type lwsGroup struct {
	key     string
	members []placedRow
}

// enginePerPod reports whether the group's Pods run engines of their own: a
// worker reported a row. A group of the leader's row alone is the Ray (or
// vLLM headless) layout, where the leader reports for the group.
func (g lwsGroup) enginePerPod() bool {
	return slices.ContainsFunc(g.members, func(m placedRow) bool { return m.place.worker != 0 })
}

// groupRows splits rows into groups: a LeaderWorkerSet group's rows of one
// model together, every other row alone (key ""). Groups are in order of
// first appearance and their members leader first.
func groupRows(rows []placedRow) []lwsGroup {
	type key struct{ group, model, pod string }
	order := make([]key, 0, len(rows))
	by := make(map[key][]placedRow, len(rows))
	for _, r := range rows {
		k := key{group: r.place.group, model: r.row.ModelID}
		if k.group == "" {
			k.pod = r.row.PodName
		}
		if _, seen := by[k]; !seen {
			order = append(order, k)
		}
		by[k] = append(by[k], r)
	}
	out := make([]lwsGroup, 0, len(order))
	for _, k := range order {
		members := by[k]
		slices.SortStableFunc(members, func(a, b placedRow) int { return cmp.Compare(a.place.worker, b.place.worker) })
		out = append(out, lwsGroup{key: k.group, members: members})
	}
	return out
}

// collapseToGroups merges the rows of one LeaderWorkerSet group into one row,
// so a group whose Pods each run an engine is one replica, as the scale
// target counts it.
//
// Two layouts put an LWS group on several Pods. Under Ray (or vLLM's
// headless multi-node mode) the leader's engine spans the group and only the
// leader reports, so the group arrives as the leader's one row and is passed
// through unchanged. Where every Pod runs an engine of its own -- data
// parallelism across nodes, each Pod with its own API server -- every Pod
// reports its own KV cache, batch and traffic, and the leader's row alone is
// one Pod's share of the replica: a 4-Pod group read as a quarter of its
// capacity. Those rows are merged by mergePodInstances, the rules that merge
// one Pod's ranks, since each Pod's engine is one rank of the group's.
//
// It assumes each Pod reports its own engines and only those, as an API
// server in vLLM's external or hybrid load balancing does. A leader whose
// API server also reported its workers' engines (internal load balancing
// across nodes) while the workers reported too would be counted twice;
// warnLWSRanks says so when the rank counts show it.
//
// The merged row is the leader's identity (the lowest worker index present
// when the leader did not report). For an engine-per-Pod group it is Ready
// only when every reporting Pod is Ready and impaired (lwsGroupImpaired) does
// not say the group lacks a serving Pod: a Pod still starting, or missing,
// holds part of the group's queue and traffic, and a reading without it is
// not one the analyzers trust from a serving replica. A leader-only group
// keeps the leader's own readiness. ReadySince is already the group's
// (replicaReadySince).
//
// Rows outside any group pass through. Input order is preserved by first
// appearance; the input slice is not modified.
func collapseToGroups(rows []placedRow, impaired func(pod string) bool) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, len(rows))
	for _, g := range groupRows(rows) {
		if g.key == "" || !g.enginePerPod() {
			for _, m := range g.members {
				out = append(out, m.row)
			}
			continue
		}
		group := make([]domain.ReplicaMetrics, len(g.members))
		ready := impaired == nil || !impaired(g.members[0].row.PodName)
		for i, m := range g.members {
			group[i] = m.row
			ready = ready && m.row.Ready
		}
		merged := mergePodInstances(group)
		merged.Ready = ready
		out = append(out, merged)
	}
	return out
}

// lwsRankMismatch says why an engine-per-Pod group's ranks suggest a double
// count, "" when they do not: a leader of several ranks beside workers that
// report too (an API server that reports engines across the group), or, with
// every Pod of the group reporting, ranks summed over the group other than
// size times the leader's own. size is 0 when unknown; a group missing a Pod
// is not judged on its sum, which the missing Pod explains.
func lwsRankMismatch(g lwsGroup, size int) string {
	if !g.enginePerPod() {
		return ""
	}
	lead := g.members[0]
	if lead.place.worker == 0 && lead.row.Ranks() > 1 {
		return "the leader reports several engine ranks and its workers report their own"
	}
	if size <= 0 || len(g.members) != size {
		return ""
	}
	sum := 0
	for _, m := range g.members {
		sum += m.row.Ranks()
	}
	if sum != size*lead.row.Ranks() {
		return "the group's ranks are not its size times one Pod's"
	}
	return ""
}

// warnLWSRanks warns, once per group for the collector's life, about an
// engine-per-Pod group whose rank counts suggest a double count
// (lwsRankMismatch): collapseToGroups would sum engines a leader already
// reported. The merge goes ahead; the warning is for the operator.
func (c *ReplicaMetricsCollector) warnLWSRanks(ctx context.Context, namespace string, rows []placedRow) {
	for _, g := range groupRows(rows) {
		if g.key == "" {
			continue
		}
		why := lwsRankMismatch(g, c.lwsGroupSize(ctx, namespace, g.members[0].row.PodName))
		if why == "" {
			continue
		}
		key := namespace + "/" + g.key
		c.cycleMu.Lock()
		warned := c.warnedLWSGroups[key]
		if !warned {
			if c.warnedLWSGroups == nil {
				c.warnedLWSGroups = map[string]bool{}
			}
			c.warnedLWSGroups[key] = true
		}
		c.cycleMu.Unlock()
		if warned {
			continue
		}
		ranks := 0
		for _, m := range g.members {
			ranks += m.row.Ranks()
		}
		ctrl.LoggerFrom(ctx).Info("a LeaderWorkerSet group's engine ranks suggest a double count; "+
			"its Pods' rows are summed as if each reported only its own engines",
			"namespace", namespace, "group", g.key, "reason", why,
			"pods", len(g.members), "ranks", ranks, "leaderRanks", g.members[0].row.Ranks())
	}
}
