package whatifapi

import (
	"fmt"
	"slices"
	"strings"
)

// EngineRateTolerance is how far, either way, the request rate the decode
// rows report may be from the router's over a window before the rows are
// taken not to stand for the fleet (EngineRowsShort): 20%, wider than a
// window's edges and a replica starting or draining move it.
const EngineRateTolerance = 0.2

// MinRowsPerReplica is the median engine rows a replica may have, over the
// cycles that state replicas, before the rows are taken not to stand for the
// fleet (EngineRowsShort): a replica is one engine row, and fewer is a
// replica whose engine the recording did not read.
const MinRowsPerReplica = 0.75

// minEngineRateCycles is the fewest cycles with both rates read that
// EngineRowsShort compares: fewer say nothing of a window.
const minEngineRateCycles = 4

// EngineRowsShort is why the engine rows of series do not stand for the
// fleet, or "" where they do (or nothing says otherwise). The planner reads a
// replica as its engine rows: their request rate, their count. On a fleet of
// multi-Pod groups whose rows report one rank (one Pod) of several, the rows
// add up to a fraction of the router's rate and count a fraction of the
// replicas, so every per-replica rate and every knee in request units reads
// low by the ranks left out (issue
// llm-d-incubation/llm-d-scaling-manager#331). Two checks, either refuses:
//
//   - the decode rows' summed request rate against the router's, over the
//     cycles that read both, outside EngineRateTolerance either way;
//   - the serving rows of a role against the replicas its variant states
//     give, the median over the cycles that state them, under
//     MinRowsPerReplica.
func EngineRowsShort(series []SeriesSample) string {
	var why []string
	var engine, router float64
	n := 0
	for _, s := range series {
		if s.RateKnown && s.Rate > 0 && s.EngineRate > 0 {
			engine += s.EngineRate
			router += s.Rate
			n++
		}
	}
	if n >= minEngineRateCycles {
		if r := engine / router; r < 1-EngineRateTolerance || r > 1+EngineRateTolerance {
			why = append(why, fmt.Sprintf("the decode engine rows report %.3g r/s against the router's %.3g r/s (%.0f%%)",
				engine/float64(n), router/float64(n), 100*r))
		}
	}
	for _, role := range []struct {
		name string
		rows func(SeriesSample) (rows, replicas int)
	}{
		{"prefill", func(s SeriesSample) (int, int) { return s.Prefill, s.PrefillReplicas }},
		{"decode", func(s SeriesSample) (int, int) { return s.Decode, s.DecodeReplicas }},
	} {
		var ratios []float64
		rows, replicas := 0, 0
		for _, s := range series {
			if r, m := role.rows(s); m > 0 {
				ratios = append(ratios, float64(r)/float64(m))
				rows, replicas = r, m
			}
		}
		if len(ratios) == 0 {
			continue
		}
		slices.Sort(ratios)
		if med := ratios[len(ratios)/2]; med < MinRowsPerReplica {
			why = append(why, fmt.Sprintf("%d %s engine rows stand for %d replicas (last cycle stated)", rows, role.name, replicas))
		}
	}
	if len(why) == 0 {
		return ""
	}
	return "the engine rows do not stand for the fleet: " + strings.Join(why, "; ") +
		", as a multi-Pod group's rows do when they report one rank of several"
}
