package saturation

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/floor"
)

// The controller runs with SwitchDecodeOwesRouterBacklog off: decode-bound
// P/D under flow control is still ordered on the arrivals alone, and the
// ledgered case known-truth/floor/flow-control-unlearned-mu-pd-decode says so.
func TestDecodeRouterBacklog_OffFromTheConstructor(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	require.False(t, a.decodeOwesRouterBacklog)
	require.Contains(t, ReplaySwitchNames(), SwitchDecodeOwesRouterBacklog)
}

// Decode-bound P/D under flow control (issue
// llm-d-incubation/llm-d-scaling-manager#51), seed 1 of the property case:
// off, decode is ordered 3.0 replicas against the 5.7 the arrivals and the
// router's queue need; on, it is charged the queue and orders at least the
// truth, and no more than the learned order.
func TestDecodeRouterBacklog_DecodeBoundPDIsChargedTheQueue(t *testing.T) {
	err := checkFlowControlFloor(fcPD, nil)(1)
	require.Error(t, err, "off: decode-bound P/D is ordered under the truth")
	require.Contains(t, err.Error(), "under the truth")
	require.NoError(t, checkFlowControlFloor(fcPD.owingBacklog(), nil)(1))
}

// Where prefill binds, decode is owed none of the router's queue, on or off:
// prefill has no mu and decode is not full, so the gate stays shut and the
// order is the same.
func TestDecodeRouterBacklog_PrefillBoundIsUnchanged(t *testing.T) {
	g := fcGenFor(fcPDPrefillBound, 1)
	off, err := runFlowControl(fcPDPrefillBound, g, false, nil)
	require.NoError(t, err)
	on, err := runFlowControl(fcPDPrefillBound.owingBacklog(), g, false, nil)
	require.NoError(t, err)
	require.Equal(t, off.replicas, on.replicas)
}

// decodeRouterBacklogRate: with a prefill term, what the prefill the
// analyzer orders completes beyond the arrivals; with none, the whole queue
// while decode is full and none below its scale-up fill.
func TestDecodeRouterBacklog_CappedAtWhatPrefillHandsOn(t *testing.T) {
	const scaleUp = 0.85
	pd := func(prefillReady int) []domain.VariantCapacity {
		return []domain.VariantCapacity{
			{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 2, PerReplicaCapacity: 100_000},
			{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: prefillReady, PerReplicaCapacity: 50_000},
		}
	}
	withPrefill := func(lambda, mu, prefillTokens float64) floor.Floor {
		return floor.Floor{Lambda: lambda,
			ByRole: map[string]float64{domain.RoleDecode: 1, domain.RolePrefill: prefillTokens},
			Terms:  map[string]floor.Term{domain.RolePrefill: {Mu: mu, PerReplica: 50_000}},
		}
	}
	noPrefill := floor.Floor{Lambda: 30, ByRole: map[string]float64{domain.RoleDecode: 1}}

	cases := []struct {
		name       string
		tf         floor.Floor
		variants   []domain.VariantCapacity
		roleDemand map[string]float64
		residency  map[string]float64
		rate       float64
		why        string
	}{{
		// One ready prefill; its demand at the threshold orders two
		// (84,000 / (0.85 x 50,000) = 1.98, rounded up), so 2 x 25 - 30 =
		// 20 req/s.
		name: "prefill spare", tf: withPrefill(30, 25, 0), variants: pd(1),
		roleDemand: map[string]float64{domain.RolePrefill: 84_000}, rate: 20, why: decodeOwedPrefillSpare,
	}, {
		// The floor's prefill figure orders more than occupancy does: three.
		name: "prefill floor orders", tf: withPrefill(30, 25, 120_000), variants: pd(1),
		roleDemand: map[string]float64{domain.RolePrefill: 10_000}, rate: 45, why: decodeOwedPrefillSpare,
	}, {
		// Prefill binds: two ready at 14 req/s each pass on less than lambda.
		name: "prefill binds", tf: withPrefill(30, 14, 0), variants: pd(2),
		roleDemand: map[string]float64{domain.RolePrefill: 10_000}, rate: 0,
	}, {
		// No prefill mu, decode resident 180,000 of 200,000 ready (0.9 >= 0.85).
		name: "decode full", tf: noPrefill, variants: pd(1),
		roleDemand: map[string]float64{domain.RoleDecode: 500_000},
		residency:  map[string]float64{domain.RoleDecode: 320_000},
		rate:       math.Inf(1), why: decodeOwedDecodeFull,
	}, {
		// No prefill mu, decode resident 120,000 of 200,000 (0.6): it takes
		// all prefill passes on, so prefill binds.
		name: "decode below its fill", tf: noPrefill, variants: pd(1),
		roleDemand: map[string]float64{domain.RoleDecode: 440_000},
		residency:  map[string]float64{domain.RoleDecode: 320_000}, rate: 0,
	}, {
		name: "no lambda", tf: floor.Floor{ByRole: map[string]float64{domain.RoleDecode: 1}}, variants: pd(1),
		roleDemand: map[string]float64{domain.RoleDecode: 500_000}, rate: 0,
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rate, why := decodeRouterBacklogRate(c.tf, c.variants, c.roleDemand, c.residency, scaleUp)
			if math.IsInf(c.rate, 1) {
				require.True(t, math.IsInf(rate, 1), "rate %v", rate)
			} else {
				require.InDelta(t, c.rate, rate, 1e-9)
			}
			require.Equal(t, c.why, why)
		})
	}
}
