package main

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// With saturation.SwitchDecodeBurstFill a stated --arrival-cv2 above 1
// prices the decode's TPOT at the fill a request sees beside the clustered
// arrivals (#291 (b)), and the figures say the Ca^2 was stated
// (AssumedArrivalCV2). Off, unstated, or at Poisson the decode is priced
// as before.
func TestDecodeBurst_TheStatedCa2OnTheSwitch(t *testing.T) {
	for name, c := range map[string]struct {
		on     bool
		cv2    float64
		stated bool
		want   float64
	}{
		"on, Ca2 4":    {on: true, cv2: 4, stated: true, want: 4},
		"off, Ca2 4":   {cv2: 4, stated: true},
		"on, unstated": {on: true},
		// A Ca^2 the flag's default or a stray value set, not stated:
		// nothing is priced from it.
		"on, Ca2 4 unstated": {on: true, cv2: 4},
		"on, Poisson":        {on: true, cv2: 1, stated: true},
		"on, Ca2 0.5":        {on: true, cv2: 0.5, stated: true},
		"on, Ca2 2":          {on: true, cv2: 2, stated: true, want: 2},
		"off, unstated":      {},
	} {
		t.Run(name, func(t *testing.T) {
			st := residenceState(true, false, 0)
			st.DecodeBurstFill = c.on
			f := answerFlags{set: map[string]bool{}, arrivalCV2: c.cv2}
			if c.stated {
				f.set["arrival-cv2"] = true
			}
			vs, err := f.decodeVariants(st)
			require.NoError(t, err)
			require.Len(t, vs, 1)
			assert.InDelta(t, c.want, vs[0].decode.ArrivalCV2, 0)
			assert.Equal(t, c.want > 0, slices.Contains(vs[0].flags, assumedArrivalCV2), "flags %v", vs[0].flags)
		})
	}

	// On, the mean TPOT at a rate rises over Poisson's.
	st := residenceState(true, false, 0)
	st.DecodeBurstFill = true
	price := func(cv2 float64) float64 {
		f := answerFlags{set: map[string]bool{"arrival-cv2": true}, arrivalCV2: cv2}
		vs, err := f.decodeVariants(st)
		require.NoError(t, err)
		d := vs[0].decode
		d.Band.Lo = 0.02 // the fill read on the line, not at the band's bottom
		a, ok := whatif.SLOAtReplicas(d, whatif.ReplicasQuery{Headroom: 0.7}).MeanTPOT.(whatif.Answer)
		require.True(t, ok)
		return a.Value
	}
	assert.Greater(t, price(4), price(1))
}

// ExtrapolatedFill is judged at the fill the line is read at: under a burst
// that is above the answer's Basis.Fill (the replica's time average), so a
// fill under every reading can still be read past them.
func TestDecodeBurst_ExtrapolatedAtTheFillARequestSees(t *testing.T) {
	const top = 0.3
	pv := func(cv2 float64) pricedVariant {
		st := residenceState(true, false, 0)
		st.DecodeBurstFill = true
		f := answerFlags{set: map[string]bool{"arrival-cv2": true}, arrivalCV2: cv2}
		vs, err := f.decodeVariants(st)
		require.NoError(t, err)
		v := vs[0]
		v.state.LineReadings = []itl.Observation{{K: 0.1}, {K: top}}
		return v
	}
	burst := pv(4)
	e := burst.decode.SeenFill(0.1) - 0.1
	require.Positive(t, e, "the burst raises the fill a request sees")
	fill := top - e/2
	flagged := func(v pricedVariant) bool {
		a, ok := v.qualify(whatif.Answer{Value: 1, Basis: whatif.Basis{Fill: fill}}).(whatif.Answer)
		require.True(t, ok)
		return slices.Contains(a.Flags, whatif.ExtrapolatedFill)
	}
	assert.True(t, flagged(burst), "read at %g, past the top reading %g", fill+e, top)
	assert.False(t, flagged(pv(1)), "Poisson reads the line at %g, under %g", fill, top)
}
