package saturation

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// Issue llm-d-incubation/llm-d-scaling-manager#258: where the policy sets a
// TPOT objective, a decode replica's k2 is held where its variant's own ITL
// line reaches it. The tests set the switch by its name, so they run on a
// commit that predates it, and fail there.

// tpotPolicy is cfg with a TPOT objective of target, read by the SLO
// analyzer listed beside saturation.
func tpotPolicy(cfg *config.ScalingPolicy, target time.Duration) {
	cfg.SLO = &config.SLOPolicy{TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: target}}}
	cfg.Analyzers = []config.AnalyzerScoreConfig{{Name: "saturation"}, {Name: "slo"}}
}

// lastLine is the last line logged as msg, nil with none.
func lastLine(logs *observer.ObservedLogs, msg string) map[string]any {
	all := logs.FilterMessage(msg).All()
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1].ContextMap()
}

// The target is read only with the SLO analyzer listed, as the TTFT pricing
// is: a target the analyzer does not read is no target.
func TestK2BatchAtITLTarget_TheTargetNeedsTheSLOAnalyzer(t *testing.T) {
	cfg := &config.ScalingPolicy{}
	tpotPolicy(cfg, 20*time.Millisecond)
	assert.InDelta(t, 0.020, sloTPOTTarget(cfg), 1e-12)
	cfg.Analyzers = []config.AnalyzerScoreConfig{{Name: "saturation"}}
	assert.Zero(t, sloTPOTTarget(cfg), "slo unlisted")
	assert.Zero(t, sloTPOTTarget(&config.ScalingPolicy{}), "no target")
	assert.Zero(t, sloTPOTTarget(nil))
}

// A line the key fitted itself, with an intercept it measured, bounds k2; any
// other names why not.
func TestK2BatchAtITLTarget_OnlyALearnedLineBounds(t *testing.T) {
	line := itl.Model{A: 0.05, B: 0.012}
	cases := map[string]struct {
		r    itl.Reading
		want string
	}{
		"a full fit":                 {itl.Reading{Model: line, Tier: itl.TierOLS, Ready: true}, ""},
		"a full fit resting":         {itl.Reading{Model: line, Tier: itl.TierOLS}, itlBoundResting},
		"pinned at a measured B":     {itl.Reading{Model: line, Tier: itl.TierPinnedB, BaselineLearned: true}, ""},
		"pinned through its floor":   {itl.Reading{Model: line, Tier: itl.TierPinnedB, PinnedAtFloor: true}, ""},
		"pinned at the default B":    {itl.Reading{Model: line, Tier: itl.TierPinnedB}, itlBoundDefaultB},
		"no line":                    {itl.Reading{}, itlBoundNoLine},
		"a held slope":               {itl.Reading{Model: line, Tier: itl.TierOLS, Ready: true, SlopeHeld: true}, itlBoundHeld},
		"a held tier":                {itl.Reading{Model: line, Tier: itl.TierPinnedB, BaselineLearned: true, TierHeld: true}, itlBoundHeld},
		"one-sided at a prior on B":  {itl.Reading{Model: line, Tier: itl.TierPinnedB, BaselineLearned: true, PinnedAtBPrior: true}, itlBoundOneSided},
		"one-sided at the least ITL": {itl.Reading{Model: line, Tier: itl.TierPinnedB, BaselineLearned: true, PinnedAtLeastITL: true}, itlBoundOneSided},
		"fitted above the band":      {itl.Reading{Model: line, Tier: itl.TierOLS, Ready: true, AboveBandFit: true}, itlBoundAboveBand},
	}
	for name, c := range cases {
		assert.Equal(t, c.want, k2ITLDistrust(c.r), name)
	}
}

// The line 0.05 x k + 0.012 s a token: a TPOT objective of 20 ms is reached
// at a fill of 0.16, inside a band of [0.15, 0.8]. No objective is no bound:
// the band's pricing point is not a latency. A target the line reaches under
// the band, or at or under B, is held at the band's lower edge, so a tighter
// target never asks for fewer replicas; one past the band at its upper edge.
func TestK2BatchAtITLTarget_TheTarget(t *testing.T) {
	line := itl.Model{A: 0.05, B: 0.012}
	slo := itlBoundFor(line, 0.020, 0.15, 0.8)
	assert.True(t, slo.applies())
	assert.InDelta(t, 0.16, slo.fill, 1e-12)
	assert.Equal(t, itlTargetSLO, slo.source)
	assert.Empty(t, slo.clamp)

	none := itlBoundFor(line, 0, 0.15, 0.8)
	assert.False(t, none.applies())
	assert.Equal(t, itlBoundNoTarget, none.why)

	for target, want := range map[float64]struct {
		fill  float64
		clamp string
	}{
		0.010: {0.15, itlClampLow},
		0.012: {0.15, itlClampLow},
		0.015: {0.15, itlClampLow},
		1:     {0.8, itlClampHigh},
	} {
		b := itlBoundFor(line, target, 0.15, 0.8)
		assert.True(t, b.applies(), "%g s", target)
		assert.InDelta(t, want.fill, b.fill, 1e-12, "%g s", target)
		assert.Equal(t, want.clamp, b.clamp, "%g s", target)
	}
	tighter, looser := itlBoundFor(line, 0.0119, 0.15, 0.8), itlBoundFor(line, 0.0199, 0.15, 0.8)
	assert.LessOrEqual(t, tighter.fill, looser.fill, "a tighter target never holds k2 higher")

	assert.False(t, itlBoundFor(itl.Model{}, 0.020, 0.15, 0.8).applies(), "no line")
	assert.False(t, itlBatchBound{fill: 0.5}.applies(), "the zero value is the switch off")
}

const (
	// boundKV is boundRow's cache.
	boundKV = int64(1_000_000)
	// boundAtSteady is the derived k2 at the steady batch: 256 a rank (a
	// budget of 8192 does not bind: 8192 x 100 / 1100 = 745) at 1000 + 100/2.
	boundAtSteady = 256 * 1050
)

// boundRow is a decode pod of variant-d, 1000 in and 100 out, a cache of
// 1,000,000 tokens 30 % full over 100 running requests.
func boundRow() domain.ReplicaMetrics {
	return hitRow("pod-1", 1000, 100, boundKV, 0.3, 100)
}

// priceBound prices rm as a decode replica of variant-d under params with the
// bound given and the budget switch as stated, edits applied to the analyzer
// first, and returns its capacity, its P3 k2-decision line (nil with none)
// and its k2-itl-bound line (nil with none).
func priceBound(t *testing.T, rm domain.ReplicaMetrics, params *capacity.EngineParams, budget bool,
	bound itlBatchBound, edits ...func(*SaturationAnalyzer)) (*capacity.ReplicaCapacity, map[string]any, map[string]any) {
	t.Helper()
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", rm.VariantName, capacity.Record{
		AcceleratorName: "H200", GpuCount: 1, EngineParams: params,
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchK2DecodeBudgetAtComputedPrompt: budget}))
	for _, edit := range edits {
		edit(a)
	}
	cfg := &config.ScalingPolicy{KvCacheThreshold: 0.8, ScaleUpThreshold: 0.85, QueueLengthThreshold: 5}
	rc := a.computeReplicaCapacity(rm, cfg, tuningOf(cfg), "test-model", "test-ns", 1, domain.RoleDecode, "H200",
		outputKeys{k2: rm.AvgOutputTokens, mu: rm.AvgOutputTokens}, rm.AvgOutputTokens, rm.AvgInputTokens, 0,
		whatif.DerivedMu{}, servedRates{}, false, false, replicaRouting{}, false, bound, logr.FromContextOrDiscard(ctx))
	require.NotNil(t, rc)
	var p3 map[string]any
	for _, e := range logs.FilterMessage("k2-decision").All() {
		if f := e.ContextMap(); f["priority"] == capacity.K2SrcDerived.String() {
			p3 = f
		}
	}
	return rc, p3, lastLine(logs, "k2-itl-bound")
}

// at is a bound at fill, from a 20 ms objective.
func at(fill float64) itlBatchBound {
	return itlBatchBound{on: true, fill: fill, target: 0.02, source: itlTargetSLO}
}

// A bound at a fill of 0.2 holds k2 at 200,000, 190 requests a rank at the
// context where N_steady is 256. One at 0.5 leaves the steady batch's 268,800,
// under it, alone. One that does not apply leaves it, and says why. Off,
// nothing is logged. On a pod of four data-parallel ranks the bound is still
// 0.2 of the pod's KV and N_itl a rank's.
func TestK2BatchAtITLTarget_HoldsTheBatch(t *testing.T) {
	rc, _, line := priceBound(t, boundRow(), deploymentParams(), false, at(0.2))
	assert.Equal(t, int64(200_000), rc.EffectiveCapacity)
	assert.Equal(t, k2BoundITL, line["k2BatchBound"])
	assert.InDelta(t, 256, line["nSteady"], 1e-9)
	assert.InDelta(t, 0.2*float64(boundKV)/1050, line["nITL"], 1e-9)
	assert.EqualValues(t, int64(boundAtSteady), line["k2Unbounded"])
	assert.Equal(t, capacity.K2SrcDerived.String(), line["priority"])

	rc, _, line = priceBound(t, boundRow(), deploymentParams(), false, at(0.5))
	assert.Equal(t, int64(boundAtSteady), rc.EffectiveCapacity)
	assert.Equal(t, k2BoundNone, line["k2BatchBound"])

	rc, _, line = priceBound(t, boundRow(), deploymentParams(), false, itlBatchBound{on: true, why: itlBoundDefaultB})
	assert.Equal(t, int64(boundAtSteady), rc.EffectiveCapacity, "a default intercept bounds nothing")
	assert.Equal(t, itlBoundDefaultB, line["itlBoundWhy"])
	assert.NotContains(t, line, "nITL")

	rc, p3, line := priceBound(t, boundRow(), deploymentParams(), false, itlBatchBound{})
	assert.Equal(t, int64(boundAtSteady), rc.EffectiveCapacity, "off")
	assert.Nil(t, line, "off: no k2-itl-bound line")
	assert.NotNil(t, p3)

	four := boundRow()
	four.EngineRanks = 4
	rc, _, line = priceBound(t, four, deploymentParams(), false, at(0.2))
	assert.Equal(t, int64(200_000), rc.EffectiveCapacity)
	assert.EqualValues(t, int64(4*boundAtSteady), line["k2Unbounded"])
	assert.InDelta(t, 0.2*float64(boundKV)/4/1050, line["nITL"], 1e-9)
}

// A measured window holds k2 too: a replica whose window records 900,000
// tokens (P2-hist) is priced at 200,000 under a bound at 0.2, and the window
// is not changed.
func TestK2BatchAtITLTarget_AMeasuredWindowIsHeldToo(t *testing.T) {
	_, p3, _ := priceBound(t, boundRow(), deploymentParams(), false, itlBatchBound{})
	require.NotNil(t, p3)
	key, ok := p3["historyKey"].(string)
	require.True(t, ok)
	var a *SaturationAnalyzer
	window := func(x *SaturationAnalyzer) { a = x; setDonor(x, key, 900_000) }
	rc, _, line := priceBound(t, boundRow(), deploymentParams(), false, at(0.2), window)
	assert.Equal(t, capacity.K2SrcHistorical.String(), line["priority"])
	assert.EqualValues(t, int64(900_000), line["k2Unbounded"])
	assert.Equal(t, int64(200_000), rc.EffectiveCapacity)
	assert.InDelta(t, 900_000, a.computeCapacityHistory[key].Average(), 1e-9, "the window itself is not changed")

	rc, _, _ = priceBound(t, boundRow(), deploymentParams(), false, itlBatchBound{}, func(x *SaturationAnalyzer) {
		setDonor(x, key, 900_000)
	})
	assert.Equal(t, int64(800_000), rc.EffectiveCapacity, "off: the window, held at k1")
}

// asked is the decode replicas the optimizer orders for demand d at a
// per-replica capacity p, from r replicas (the replay's open-loop sizing).
func asked(d, p float64, r int) int {
	if req := d/0.85 - float64(r)*p; req > 0 {
		return r + int(math.Ceil(req/p))
	}
	if spare := float64(r)*p - d/0.70; spare > 0 {
		return max(1, r-int(math.Floor(spare/p)))
	}
	return r
}

// The live shape: a P/D decode with --max-num-batched-tokens 256 whose budget
// is charged the prompt it computes (#201 on), pricing the four-rank pod at
// 7,911,384 tokens, the batch three replicas held fitting one. Its line reads
// 10 ms + 20 ms x k a token. A demand of 6,000,000 tokens on one replica then
// asks 1 at the steady batch. A TPOT objective of 14 ms is reached at a fill
// of 0.2, 6,400,000 of the pod's 32,000,000: it asks 2. One of 20 ms is
// reached at 0.5, over the steady batch's figure: it asks 1. With #201 off
// the budget prices the pod at 713,688 and asks 10, the bound or not.
func TestK2BatchAtITLTarget_TheLiveShape(t *testing.T) {
	line := itl.Model{A: 0.020, B: 0.010}
	const demand = 6_000_000.0
	ask := func(budget bool, tpot float64) (int, int64) {
		b := itlBoundFor(line, tpot, 0.15, 0.8)
		rc, _, _ := priceBound(t, budgetRow(), budgetParams(), budget, b)
		return asked(demand, float64(rc.EffectiveCapacity), 1), rc.EffectiveCapacity
	}
	n, k2 := ask(true, 0)
	assert.Equal(t, 1, n, "#201 on, no objective: one replica carries it")
	assert.Equal(t, int64(7_911_384), k2)
	n, k2 = ask(true, 0.014)
	assert.Equal(t, 2, n, "14 ms: two")
	assert.Equal(t, int64(6_400_000), k2)
	n, _ = ask(true, 0.020)
	assert.Equal(t, 1, n, "20 ms: one")
	n, _ = ask(false, 0.014)
	assert.Equal(t, 10, n, "#201 off")
}

// itlSLOCycle runs one cycle of the rig with v in role and the policy's TPOT
// objective set to tpot (none at 0), logging to ctx's logger.
func itlSLOCycle(r *restartRig, ctx context.Context, rows []domain.ReplicaMetrics, tpot time.Duration, role string) {
	r.t.Helper()
	states := decodeVariants("v")
	states[0].Role = role
	r.tr.Observe(settleModel("test-ns", "test-model"), settlerow.Of(rows, map[string]string{"v": role}), r.now)
	in := makeAnalyzerInput(rows, states)
	if tpot > 0 {
		tpotPolicy(in.Config.(*config.ScalingPolicy), tpot)
	}
	_, err := r.a.Analyze(ctx, in)
	require.NoError(r.t, err)
	r.now = r.now.Add(restartCycle)
}

// itlBoundRun runs 40 cycles of three steady replicas whose ITL reads
// 8 ms + 40 ms x k, so v, in role, fits its own line, with the switch on or
// off and a TPOT objective of tpot, and returns the last cycle's
// k2-itl-bound line (nil with none), its P3 k2-decision line and its itl-fit
// line.
func itlBoundRun(t *testing.T, on bool, tpot time.Duration, role string) (bound, p3, fit map[string]any) {
	t.Helper()
	r := itlRig(t, map[string]bool{SwitchK2BatchAtITLTarget: on})
	r.a.capacityStore.Update("test-ns", "test-model", "v", capacity.Record{GpuCount: 1, EngineParams: deploymentParams(),
		LearnedFrom: "deployment"}, r.now)
	for range 40 {
		ctx, logs := observedCtx(t)
		itlSLOCycle(r, ctx, itlSettleFleet(), tpot, role)
		bound, p3 = lastLine(logs, "k2-itl-bound"), nil
		for _, e := range logs.FilterMessage("k2-decision").All() {
			if f := e.ContextMap(); f["priority"] == capacity.K2SrcDerived.String() {
				p3 = f
			}
		}
		fit = lastLine(logs, "itl-fit")
	}
	require.NotNil(t, p3, "a P3 k2-decision line")
	return bound, p3, fit
}

// Through Analyze: v fits its own line, 8 ms + 40 ms x k, from its replicas'
// readings. With a TPOT objective of 16 ms the line reaches it at a fill of
// 0.2, so k2 is held at 0.2 of a replica's 376,480 tokens, 75,296, under the
// 256-request steady batch's figure, 320,000. Off, k2 is that figure and
// nothing is logged. With no objective there is no bound, and the line says
// so.
func TestK2BatchAtITLTarget_ThroughAnalyze(t *testing.T) {
	on, _, fit := itlBoundRun(t, true, 16*time.Millisecond, domain.RoleDecode)
	require.NotNil(t, fit, "premise: v fits a line")
	require.Equal(t, itl.TierOLS, fit["tier"], "premise: a full fit, its own B")
	require.NotNil(t, on)
	require.Equal(t, "", on["itlBoundWhy"], "the line bounds: %v", on)
	assert.Equal(t, itlTargetSLO, on["itlTargetSource"])
	assert.Equal(t, k2BoundITL, on["k2BatchBound"])
	fill, ok := on["itlTargetFill"].(float64)
	require.True(t, ok)
	assert.InDelta(t, 0.2, fill, 0.02, "the fitted line reaches 16 ms near 0.2")
	assert.EqualValues(t, int64(fill*376_480), on["k2"])

	off, offP3, _ := itlBoundRun(t, false, 16*time.Millisecond, domain.RoleDecode)
	assert.Nil(t, off, "off: no k2-itl-bound line")
	assert.Equal(t, on["k2Unbounded"], offP3["k2"], "off: the steady batch's figure")

	none, noneP3, _ := itlBoundRun(t, true, 0, domain.RoleDecode)
	require.NotNil(t, none)
	assert.Equal(t, itlBoundNoTarget, none["itlBoundWhy"])
	assert.Equal(t, k2BoundNone, none["k2BatchBound"])
	assert.Equal(t, offP3["k2"], noneP3["k2"])
}

// An aggregated variant is not bound: its line is report-only for its mu
// (itlLineReportOnly).
func TestK2BatchAtITLTarget_AnAggregatedVariantIsNotBound(t *testing.T) {
	bound, _, fit := itlBoundRun(t, true, 16*time.Millisecond, domain.RoleBoth)
	require.NotNil(t, fit, "premise: v fits a line")
	require.NotNil(t, bound)
	assert.Equal(t, itlBoundNotDecode, bound["itlBoundWhy"])
	assert.Equal(t, k2BoundNone, bound["k2BatchBound"])
}

// A line borrowed from a sibling's configuration is not the variant's own.
// It reaches a TPOT objective of 22 ms at a fill of 0.2, which would hold
// boundRow's k2 at 200,000; it stays at the steady batch's 268,800, and the
// line says why.
func TestK2BatchAtITLTarget_ABorrowedLineDoesNotBound(t *testing.T) {
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		AcceleratorName: "H200", GpuCount: 1, EngineParams: deploymentParams(),
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchK2BatchAtITLTarget: true}))
	a.publishLine(a.itlPhysicsKey("test-ns", "test-model", "variant-d", "H200", 1, ""),
		"a fitted line", itl.Model{A: 0.05, B: 0.012}, time.Now())
	in := makeAnalyzerInput([]domain.ReplicaMetrics{boundRow()}, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
	})
	tpotPolicy(in.Config.(*config.ScalingPolicy), 22*time.Millisecond)
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	line := lastLine(logs, "k2-itl-bound")
	require.NotNil(t, line)
	assert.Equal(t, itlBoundBorrowed, line["itlBoundWhy"])
	assert.Equal(t, k2BoundNone, line["k2BatchBound"])
	assert.EqualValues(t, int64(boundAtSteady), line["k2"], "trusted, the line would hold it at 200,000")
}

// The switch is on from the constructor, and a replay names it.
func TestK2BatchAtITLTarget_OnFromTheConstructor(t *testing.T) {
	fields := NewSaturationAnalyzer(capacity.NewStore()).switchFields()
	require.Contains(t, fields, "k2-batch-held-at-itl-target")
	assert.True(t, *fields["k2-batch-held-at-itl-target"])
	assert.Contains(t, ReplaySwitchNames(), SwitchK2BatchAtITLTarget)
}
