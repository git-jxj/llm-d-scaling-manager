package saturation

import (
	"math"
	"testing"
	"time"

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

// The floor line carries routerBacklogOwed, routerBacklogOwedBy,
// decodeServedShare and decodeKvFill only with the switch on, and only on
// decode: off, it is the line as it was before the switch.
func TestDecodeRouterBacklog_TheLogFieldsNeedTheSwitch(t *testing.T) {
	keys := []string{"routerBacklogOwed", "routerBacklogOwedBy", "decodeServedShare", "decodeKvFill"}
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
// analyzer orders completes beyond the arrivals; with none, the whole queue,
// weighed by decode's own evidence (weigh).
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
		rate       float64
		share      float64
		why        string
		weigh      bool
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
		// No prefill mu: the whole queue, weighed by decode's evidence.
		name: "no prefill mu", tf: noPrefill, variants: pd(1), rate: math.Inf(1), weigh: true,
	}, {
		name: "no lambda", tf: floor.Floor{ByRole: map[string]float64{domain.RoleDecode: 1}}, variants: pd(1), rate: 0,
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, weigh := decodeRouterBacklogRate(c.tf, c.variants, c.roleDemand, scaleUp)
			if math.IsInf(c.rate, 1) {
				require.True(t, math.IsInf(got.rate, 1), "rate %v", got.rate)
			} else {
				require.InDelta(t, c.rate, got.rate, 1e-9)
			}
			require.InDelta(t, c.share, got.share, 1e-9)
			require.Equal(t, c.why, got.why)
			require.Equal(t, c.weigh, weigh)
		})
	}
}

// readDecodeEvidence: the served share over decode's ready replicas x its
// mu, and the mean busiest-rank KV of its Ready replicas; nothing where mu,
// completions or a replica's KV is not read.
func TestDecodeRouterBacklog_TheEvidence(t *testing.T) {
	variants := []domain.VariantCapacity{
		{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 2},
		{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 1},
	}
	roleOf := map[string]string{"d": domain.RoleDecode, "p": domain.RolePrefill}
	row := func(variant string, kv float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{VariantName: variant, Ready: true, KvCacheUsage: kv}
	}
	rows := []domain.ReplicaMetrics{row("d", 0.8), row("d", 0.9), row("p", 0.1)}
	ev := readDecodeEvidence(rows, variants, roleOf, 19, 10)
	require.True(t, ev.ok)
	require.InDelta(t, 0.95, ev.share, 1e-9)
	require.InDelta(t, 0.85, ev.fill, 1e-9)
	require.False(t, readDecodeEvidence(rows, variants, roleOf, 19, 0).ok, "no mu")
	require.False(t, readDecodeEvidence(rows, variants, roleOf, 0, 10).ok, "no completions")
	require.False(t, readDecodeEvidence(rows[2:], variants, roleOf, 19, 10).ok, "no decode KV read")
}

// decodeAtMuWeight: the share of the queue in proportion to the smoothed
// served share (0.80-1.00) times the smoothed fill (0.9-1.0 of the KV
// threshold); none past decodeAtMuImplausible; falling by at most
// decodeOwingStep a cycle.
func TestDecodeRouterBacklog_TheWeight(t *testing.T) {
	const threshold = 0.8
	clock := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	fresh := func() *SaturationAnalyzer {
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.now = func() time.Time { return clock }
		return a
	}
	step := func(a *SaturationAnalyzer, share, fill float64) (float64, string) {
		clock = clock.Add(15 * time.Second)
		w, _, _, why := a.decodeAtMuWeight("ns|m", decodeEvidence{share: share, fill: fill, ok: true}, threshold)
		return w, why
	}
	for _, c := range []struct {
		name        string
		share, fill float64
		want        float64
		why         string
	}{
		{"at its mu and full", 1.0, 0.85, 1, decodeOwedDecodeAtMu},
		{"half into the share band", 0.9, 0.85, 0.5, decodeOwedDecodeAtMu},
		{"half into the fill band", 1.0, 0.76, 0.5, decodeOwedDecodeAtMu},
		{"under its mu", 0.8, 0.95, 0, ""},
		{"not full: prefill binds", 1.1, 0.43, 0, ""},
		{"no threshold", 1.0, 0.95, 0, ""},
		{"a mu error", 1.3, 0.95, 0, decodeOwedMuSuspect},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := fresh()
			th := threshold
			if c.name == "no threshold" {
				th = 0
			}
			clock = clock.Add(15 * time.Second)
			w, _, _, why := a.decodeAtMuWeight("ns|m", decodeEvidence{share: c.share, fill: c.fill, ok: true}, th)
			require.InDelta(t, c.want, w, 1e-9)
			require.Equal(t, c.why, why)
		})
	}

	t.Run("smoothed and held", func(t *testing.T) {
		a := fresh()
		for range 4 {
			w, _ := step(a, 1.0, 0.9)
			require.InDelta(t, 1, w, 1e-9)
		}
		// One reading under the band: the median of four holds.
		w, _ := step(a, 0.5, 0.9)
		require.InDelta(t, 1, w, 1e-9)
		// The evidence goes: the median falls under the band on the next
		// reading, and the charge falls a quarter a cycle from there.
		got := make([]float64, 0, 6)
		for range 6 {
			w, _ := step(a, 0.5, 0.9)
			got = append(got, w)
		}
		require.InDeltaSlice(t, []float64{0.75, 0.5, 0.25, 0, 0, 0}, got, 1e-9)
		// A mu error drops it at once.
		for range 4 {
			step(a, 1.0, 0.9)
		}
		w, why := step(a, 3, 0.9)
		require.InDelta(t, 1, w, 1e-9, "one reading: the median is still at 1.0")
		require.Equal(t, decodeOwedDecodeAtMu, why)
		step(a, 3, 0.9)
		w, why = step(a, 3, 0.9)
		require.Zero(t, w)
		require.Equal(t, decodeOwedMuSuspect, why)
	})

	t.Run("stale evidence is not read", func(t *testing.T) {
		a := fresh()
		step(a, 1.0, 0.9)
		clock = clock.Add(decodeOwingMaxAge + time.Second)
		w, _, _, _ := a.decodeAtMuWeight("ns|m", decodeEvidence{}, threshold)
		require.Zero(t, w)
	})
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
