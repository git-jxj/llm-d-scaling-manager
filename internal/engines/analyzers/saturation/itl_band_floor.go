package saturation

import (
	"math"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The ITL band's floor, adapted to a low-fill fleet (SwitchITLAdaptiveBandFloor).
//
// The ITL line is fitted over readings inside the policy's band, from
// analyzerTuning.itlBand.minObservableK (0.15) to maxObservableK (0.80), and
// a full fit needs its steady readings to spread over itl.DefaultMinKSpread
// (0.30). A fleet whose pods never fill past 0.45 can never spread that far
// inside the band: its line is pinned on the default intercept for good. On
// a P/D fleet whose engines filled well under 0.45 all day, the line was
// pinned on the default B on nearly every cycle; with the floor set to 0.05
// by hand it learned from the first trough on.
//
// So, with the switch on, the floor follows the fill the fleet has, read over
// every engine reading of the last adaptiveBandHorizon:
//
//   - If the highest fill (capped at the band's top) is DefaultMinKSpread or
//     more above the policy's floor, the policy's floor stands.
//   - Otherwise, if at least adaptiveBandMinEngines engines read below the
//     policy's floor, the floor is the readings' adaptiveBandPercentile
//     fill, rounded down to a hundredth and never under AdaptiveBandFloorMin,
//     provided that leaves the spread. A percentile and an engine count, not
//     the least fill: one straggler engine, idle at a fill no other reaches,
//     neither sets the floor nor makes the case for lowering it.
//   - Otherwise the policy's floor stands: a fleet whose range has no spread
//     either way learns nothing from a lower floor.
//
// Every reader of the band inside the saturation analyzer reads the adapted
// floor, as if the policy had set it, and so does the capacity planner's
// state (ITLBandFloor). The slo and throughput analyzers read the policy's:
// the planner prices no line of theirs.
//
// Once adapted, the floor holds, at the lowest it was adapted to: the line
// keeps the readings and the intercept it learned under it, so a floor that
// went back up between troughs would price a line under a band it was not
// read in, and would not say so. It is released only when the fill has
// spread adaptiveBandReleaseSpread above the policy's floor, more than the
// spread that adapts it, for adaptiveBandReleaseCycles cycles in a row: one
// threshold for both made a fleet near the edge flip at the horizon's cadence.

// SwitchITLAdaptiveBandFloor is the replay switch for the adapted floor. On,
// the cycle's policy band floor is lowered to the fleet's low fill where
// that gives a line the spread the policy's floor cannot. Off, the policy's
// floor stands. Planner-only: off from the constructor;
// learnreplay.StateAt turns it on (learnreplay.PlannerSwitches).
const SwitchITLAdaptiveBandFloor = "itl-adaptive-band-floor"

// AdaptiveBandFloorMin is the lowest the adapted floor goes: under it a pod
// holds a request or two and its mean ITL is the noise the band's floor
// keeps out.
const AdaptiveBandFloorMin = 0.05

// adaptiveBandHorizon is how far back the fleet's fill range is read: the
// ITL window's own horizon (itl.DefaultObservationMaxAge).
const adaptiveBandHorizon = itl.DefaultObservationMaxAge

// adaptiveBandPercentile is the percentile of the horizon's engine fills
// the floor is set at.
const adaptiveBandPercentile = 0.10

// adaptiveBandMinEngines is the fewest distinct engines that must read
// below the policy's floor over the horizon for it to be lowered: more than
// a straggler or two, and few enough that a decode fleet of one LWS group
// of four engines can still adapt.
const adaptiveBandMinEngines = 3

// adaptiveBandReleaseSpread is the spread above the policy's floor that
// releases an adapted floor, and adaptiveBandReleaseCycles how many cycles
// in a row it must hold: two minutes at the default 15 s cycle.
const (
	adaptiveBandReleaseSpread = itl.DefaultMinKSpread + 0.05
	adaptiveBandReleaseCycles = 8
)

// engineFill is one engine's fill on one cycle.
type engineFill struct {
	at  time.Time
	pod string
	k   float64
}

// bandFloor is the floor adapted for a model's latest cycle, the policy's it
// replaced, and how many cycles in a row the fill has spread past
// adaptiveBandReleaseSpread.
type bandFloor struct {
	floor, policy float64
	at            time.Time
	released      int
}

// adaptITLBandFloor notes the cycle's decode fills for input's model and
// returns the policy the cycle reads: cfg itself, or a copy with the band's
// floor adapted (SwitchITLAdaptiveBandFloor). Called only with the switch
// on.
func (a *SaturationAnalyzer) adaptITLBandFloor(input domain.AnalyzerInput, cfg *config.ScalingPolicy, now time.Time) *config.ScalingPolicy {
	roles := make(map[string]string, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		roles[vs.VariantName] = vs.Role
	}
	key := input.Namespace + "|" + input.ModelID
	a.mu.Lock()
	defer a.mu.Unlock()
	fills := slices.DeleteFunc(a.itlFills[key], func(f engineFill) bool { return now.Sub(f.at) > adaptiveBandHorizon })
	// The rows the line reads: an LWS group's engines each (domain.EngineRows).
	fills = append(fills, engineFillsOf(domain.EngineRows(input.ReplicaMetrics), roles, now)...)
	a.itlFills[key] = fills
	lo0, hi0 := cfg.ITLBand()
	bf, adapted := adaptedFloor(fills, lo0, hi0, a.itlBandFloors[key])
	if !adapted {
		delete(a.itlBandFloors, key)
		return cfg
	}
	bf.policy, bf.at = lo0, now
	a.itlBandFloors[key] = bf
	var out config.ScalingPolicy
	if cfg != nil {
		out = *cfg
	}
	out.AnalyzerTuning.ITLBand.MinObservableK, out.AnalyzerTuning.ITLBand.MaxObservableK = bf.floor, hi0
	return &out
}

// adaptedFloor is the floor the horizon's engine fills give, and whether it
// is adapted (SwitchITLAdaptiveBandFloor's rule); held is the floor adapted
// before (floor 0 with none).
func adaptedFloor(fills []engineFill, lo0, hi0 float64, held bandFloor) (bandFloor, bool) {
	keep := func(top float64) (bandFloor, bool) {
		if !(held.floor > 0) {
			return bandFloor{}, false
		}
		if top-lo0 >= adaptiveBandReleaseSpread {
			held.released++
		} else {
			held.released = 0
		}
		if held.released >= adaptiveBandReleaseCycles {
			return bandFloor{}, false
		}
		return held, true
	}
	if len(fills) == 0 {
		return keep(0)
	}
	ks := make([]float64, len(fills))
	below := map[string]bool{}
	high := math.Inf(-1)
	for i, f := range fills {
		ks[i], high = f.k, max(high, f.k)
		if f.k < lo0 {
			below[f.pod] = true
		}
	}
	top := min(high, hi0)
	if top-lo0 >= itl.DefaultMinKSpread || len(below) < adaptiveBandMinEngines {
		return keep(top)
	}
	slices.Sort(ks)
	low := ks[int(math.Floor(adaptiveBandPercentile*float64(len(ks)-1)))]
	// The nudge keeps a fill read as 0.07 at 0.07: 0.07 * 100 is
	// 7.000000000000001, but 0.29 * 100 is 28.999999999999996.
	floor := math.Floor(max(low, AdaptiveBandFloorMin)*100+1e-9) / 100
	if !(floor < lo0) || top-floor < itl.DefaultMinKSpread {
		return keep(top)
	}
	out := bandFloor{floor: floor}
	if held.floor > 0 {
		out.floor = min(floor, held.floor)
	}
	return out, true
}

// engineFillsOf is the fill (KvUsageForITL) of each row of a decode or
// aggregated variant that is serving and reads an ITL, an engine's row
// each, as the line reads them, stamped now.
func engineFillsOf(rows []domain.ReplicaMetrics, roles map[string]string, now time.Time) []engineFill {
	var out []engineFill
	for _, rm := range rows {
		if domain.CanonicalRole(roles[rm.VariantName]) == domain.RolePrefill || !rm.Ready || rm.FromWarmPool {
			continue
		}
		k := rm.KvUsageForITL()
		if !(k > 0 && k <= 1) || !(rm.AvgITL > 0) || math.IsInf(rm.AvgITL, 0) {
			continue
		}
		out = append(out, engineFill{at: now, pod: rm.PodName, k: k})
	}
	return out
}

// ITLBandFloor is the band floor adapted for the model's latest cycle, and
// the policy's floor it replaced; false where the policy's stood
// (SwitchITLAdaptiveBandFloor off, or no adaptation). For the capacity
// planner, which states it.
func (a *SaturationAnalyzer) ITLBandFloor(namespace, modelID string) (floor, policy float64, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.itlBandFloors[namespace+"|"+modelID]
	return f.floor, f.policy, ok
}
