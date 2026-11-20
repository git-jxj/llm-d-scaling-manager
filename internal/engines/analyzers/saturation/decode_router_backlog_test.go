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
// prefill has no mu and decode completes under its own, so the order is the
// same.
func TestDecodeRouterBacklog_PrefillBoundIsUnchanged(t *testing.T) {
	g := fcGenFor(fcPDPrefillBound, 1)
	off, err := runFlowControl(fcPDPrefillBound, g, false, nil)
	require.NoError(t, err)
	on, err := runFlowControl(fcPDPrefillBound.owingBacklog(), g, false, nil)
	require.NoError(t, err)
	require.Equal(t, off.replicas, on.replicas)
}

// The floor line carries routerBacklogOwed, routerBacklogOwedBy and
// decodeServedShare only with the switch on, and only on decode: off, it is
// the line as it was before the switch.
func TestDecodeRouterBacklog_TheLogFieldsNeedTheSwitch(t *testing.T) {
	keys := []string{"routerBacklogOwed", "routerBacklogOwedBy", "decodeServedShare"}
	g := fcGenFor(fcPD, 1)
	ctx, logs := observedCtx(t)
	_, err := runFlowControlLogged(ctx, fcPD, g, false, nil)
	require.NoError(t, err)
	lines := logs.FilterMessage("throughput-demand-floor").All()
	require.NotEmpty(t, lines)
	for _, l := range lines {
		for _, k := range keys {
			require.NotContains(t, l.ContextMap(), k, "off")
		}
	}

	ctx, logs = observedCtx(t)
	_, err = runFlowControlLogged(ctx, fcPD.owingBacklog(), g, false, nil)
	require.NoError(t, err)
	var owed bool
	for _, l := range logs.FilterMessage("throughput-demand-floor").All() {
		m := l.ContextMap()
		if m["role"] != domain.RoleDecode {
			for _, k := range keys {
				require.NotContains(t, m, k, "on, %v", m["role"])
			}
			continue
		}
		for _, k := range keys {
			require.Contains(t, m, k, "on, decode")
		}
		owed = owed || m["routerBacklogOwedBy"] == decodeOwedDecodeAtMu
	}
	require.True(t, owed, "on: decode-bound P/D is owed the queue on some cycle")
}

// decodeRouterBacklogRate: with a prefill term, what the prefill the
// analyzer orders completes beyond the arrivals; with none, the whole queue
// in proportion to how far decode's served share is into the band
// decodeAtMuFrom-decodeAtMuFull, whatever its fill.
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
	// No prefill mu; decode's two ready replicas complete 10 req/s each at
	// their mu.
	noPrefill := floor.Floor{Lambda: 30, ByRole: map[string]float64{domain.RoleDecode: 1},
		Terms: map[string]floor.Term{domain.RoleDecode: {Mu: 10, PerReplica: 100_000}}}

	cases := []struct {
		name       string
		tf         floor.Floor
		variants   []domain.VariantCapacity
		roleDemand map[string]float64
		served     float64
		rate       float64
		share      float64
		why        string
	}{{
		// One ready prefill; its demand at the threshold orders two
		// (84,000 / (0.85 x 50,000) = 1.98, rounded up), so 2 x 25 - 30 =
		// 20 req/s.
		name: "prefill spare", tf: withPrefill(30, 25, 0), variants: pd(1),
		roleDemand: map[string]float64{domain.RolePrefill: 84_000}, rate: 20, share: 1, why: decodeOwedPrefillSpare,
	}, {
		// The floor's prefill figure orders more than occupancy does: three.
		name: "prefill floor orders", tf: withPrefill(30, 25, 120_000), variants: pd(1),
		roleDemand: map[string]float64{domain.RolePrefill: 10_000}, rate: 45, share: 1, why: decodeOwedPrefillSpare,
	}, {
		// Prefill binds: two ready at 14 req/s each pass on less than lambda.
		name: "prefill binds", tf: withPrefill(30, 14, 0), variants: pd(2),
		roleDemand: map[string]float64{domain.RolePrefill: 10_000}, rate: 0,
	}, {
		// No prefill mu, decode completes 20 req/s, 2 x its mu of 10: all of
		// the queue.
		name: "decode at its mu", tf: noPrefill, variants: pd(1), served: 20,
		rate: math.Inf(1), share: 1, why: decodeOwedDecodeAtMu,
	}, {
		// 19.5 req/s is 0.975 of 2 x 10, half way into the band: half of it.
		name: "decode in the band", tf: noPrefill, variants: pd(1), served: 19.5,
		rate: math.Inf(1), share: 0.5, why: decodeOwedDecodeAtMu,
	}, {
		// 18 req/s is 0.9 of what decode completes at its mu: it takes all
		// prefill passes on, so prefill binds, however full decode reads.
		name: "decode under its mu", tf: noPrefill, variants: pd(1), served: 18,
		roleDemand: map[string]float64{domain.RoleDecode: 1_000_000}, rate: 0,
	}, {
		name: "decode completions unread", tf: noPrefill, variants: pd(1), rate: 0,
	}, {
		name: "decode with no mu", variants: pd(1), served: 20, rate: 0,
		tf: floor.Floor{Lambda: 30, ByRole: map[string]float64{domain.RoleDecode: 1}},
	}, {
		name: "no lambda", variants: pd(1), served: 20, rate: 0,
		tf: floor.Floor{ByRole: map[string]float64{domain.RoleDecode: 1}, Terms: noPrefill.Terms},
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decodeRouterBacklogRate(c.tf, c.variants, c.roleDemand, c.served, scaleUp)
			if math.IsInf(c.rate, 1) {
				require.True(t, math.IsInf(got.rate, 1), "rate %v", got.rate)
			} else {
				require.InDelta(t, c.rate, got.rate, 1e-9)
			}
			require.InDelta(t, c.share, got.share, 1e-9)
			require.Equal(t, c.why, got.why)
		})
	}
}

// owed is min(Q, rate x H) x share, and 0 for a product that is not a finite
// positive number (an unbounded rate over a NaN horizon).
func TestDecodeRouterBacklog_Owed(t *testing.T) {
	inf := decodeOwedRead{rate: math.Inf(1), share: 0.5}
	require.InDelta(t, 100, inf.owed(200, 60), 1e-9)
	require.Zero(t, inf.owed(200, math.NaN()))
	require.Zero(t, inf.owed(math.Inf(1), 60))
	require.InDelta(t, 600, decodeOwedRead{rate: 10, share: 1}.owed(1000, 60), 1e-9)
	require.Zero(t, decodeOwedRead{}.owed(1000, 60))
}

// The review's probe: prefill-bound P/D whose decode reads 0.70-0.80 of its
// KV. With the charge read off decode's fill (resident at least scaleUp of
// its supply) decode was owed the whole queue, and ordered 1.4-3.1x what it
// orders off (seed 5 at 0.80: 8.13 against a truth of 3.89). Decode completes
// what prefill passes on, under its mu, so it is owed none of the queue.
func TestDecodeRouterBacklog_PrefillBoundWithDecodeFullIsUnchanged(t *testing.T) {
	for _, fill := range []float64{0.70, 0.75, 0.80} {
		full := func(rm *domain.ReplicaMetrics) {
			rm.KvCacheUsage, rm.KvUsageInstant = fill, fill
			rm.TokensInUse = int64(fill * float64(rm.TotalKvCapacityTokens))
		}
		for seed := uint64(1); seed <= 5; seed++ {
			g := fcGenFor(fcPDPrefillBound, seed)
			off, err := runFlowControl(fcPDPrefillBound, g, false, full)
			require.NoError(t, err)
			on, err := runFlowControl(fcPDPrefillBound.owingBacklog(), g, false, full)
			require.NoError(t, err)
			require.Equal(t, off.replicas, on.replicas, "fill %.2f seed %d", fill, seed)
		}
	}
}
