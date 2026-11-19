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
// So, with the switch on, the floor follows the fill the fleet has: over the
// last adaptiveBandHorizon, if the fleet's highest fill (capped at the band's
// top) is less than DefaultMinKSpread above the policy's floor, and its lowest
// fill, held at AdaptiveBandFloorMin or above, leaves that spread, the floor
// is that lowest fill, rounded down to a hundredth. Otherwise the policy's
// floor stands: a fleet whose range has no spread either way learns nothing
// from a lower floor. Every reader of the band in the cycle reads the adapted
// one, as if the policy had set it, and the capacity planner states it
// (ITLBandFloor).
//
// Once adapted, the floor holds, at the lowest it was adapted to, until the
// fleet's fill reaches the spread inside the policy's band: the line keeps
// the readings and the intercept it learned under the lower floor, so a
// floor that went back up between troughs would price a line under a band
// it was not read in, and would not say so.

// SwitchITLAdaptiveBandFloor is the replay switch for the adapted floor. On,
// the cycle's policy band floor is lowered to the fleet's lowest fill where
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

// fillSpan is one cycle's lowest and highest fill over a model's decode
// rows.
type fillSpan struct {
	at     time.Time
	lo, hi float64
}

// bandFloor is the floor adapted for a model's latest cycle, and the
// policy's it replaced.
type bandFloor struct {
	floor, policy float64
	at            time.Time
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
	// The rows the line reads: an LWS group's engines each (domain.EngineRows).
	span, ok := fillSpanOf(domain.EngineRows(input.ReplicaMetrics), roles)
	key := input.Namespace + "|" + input.ModelID
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.itlFills == nil {
		a.itlFills = map[string][]fillSpan{}
		a.itlBandFloors = map[string]bandFloor{}
	}
	spans := slices.DeleteFunc(a.itlFills[key], func(s fillSpan) bool { return now.Sub(s.at) > adaptiveBandHorizon })
	if ok {
		span.at = now
		spans = append(spans, span)
	}
	a.itlFills[key] = spans
	lo0, hi0 := cfg.ITLBand()
	held := a.itlBandFloors[key].floor
	floor, adapted := adaptedFloor(spans, lo0, hi0, held)
	if !adapted {
		delete(a.itlBandFloors, key)
		return cfg
	}
	a.itlBandFloors[key] = bandFloor{floor: floor, policy: lo0, at: now}
	var out config.ScalingPolicy
	if cfg != nil {
		out = *cfg
	}
	out.AnalyzerTuning.ITLBand.MinObservableK, out.AnalyzerTuning.ITLBand.MaxObservableK = floor, hi0
	return &out
}

// adaptedFloor is the floor the fleet's fill spans give, and whether it is
// adapted (SwitchITLAdaptiveBandFloor's rule); held is the floor adapted
// before, 0 with none, which holds unless the spans reach the spread inside
// the policy's band.
func adaptedFloor(spans []fillSpan, lo0, hi0, held float64) (float64, bool) {
	keep := func() (float64, bool) {
		if held > 0 {
			return held, true
		}
		return lo0, false
	}
	if len(spans) == 0 {
		return keep()
	}
	low, high := math.Inf(1), math.Inf(-1)
	for _, s := range spans {
		low, high = min(low, s.lo), max(high, s.hi)
	}
	top := min(high, hi0)
	if top-lo0 >= itl.DefaultMinKSpread {
		return lo0, false
	}
	// The nudge keeps a fill read as 0.07 at 0.07: 0.07 * 100 is
	// 7.000000000000001, but 0.29 * 100 is 28.999999999999996.
	floor := math.Floor(max(low, AdaptiveBandFloorMin)*100+1e-9) / 100
	if !(floor < lo0) || top-floor < itl.DefaultMinKSpread {
		return keep()
	}
	if held > 0 {
		floor = min(floor, held)
	}
	return floor, true
}

// fillSpanOf is the lowest and highest fill (KvUsageForITL) over the rows of
// decode and aggregated variants that are serving and read an ITL, an
// engine's row each, as the line reads them; false with none.
func fillSpanOf(rows []domain.ReplicaMetrics, roles map[string]string) (fillSpan, bool) {
	s := fillSpan{lo: math.Inf(1), hi: math.Inf(-1)}
	n := 0
	for _, rm := range rows {
		if domain.CanonicalRole(roles[rm.VariantName]) == domain.RolePrefill || !rm.Ready || rm.FromWarmPool {
			continue
		}
		k := rm.KvUsageForITL()
		if !(k > 0 && k <= 1) || !(rm.AvgITL > 0) || math.IsInf(rm.AvgITL, 0) {
			continue
		}
		s.lo, s.hi = min(s.lo, k), max(s.hi, k)
		n++
	}
	return s, n > 0
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
