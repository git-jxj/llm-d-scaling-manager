package saturation

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// Issue llm-d-incubation/llm-d-scaling-manager#258: where the policy sets a
// TPOT objective, the derived k2's batch is held where the variant's own ITL
// line reaches it. The tests set the switch by its name, so they run on a
// commit that predates it, and fail there.

// tpotPolicy is cfg with a TPOT objective of target, read by the SLO
// analyzer listed beside saturation.
func tpotPolicy(cfg *config.ScalingPolicy, target time.Duration) {
	cfg.SLO = &config.SLOPolicy{TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: target}}}
	cfg.Analyzers = []config.AnalyzerScoreConfig{{Name: "saturation"}, {Name: "slo"}}
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

// A line the key fitted itself, with an intercept it measured, bounds the
// batch; any other names why not.
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
// the band's pricing point is not a latency. A target at or under B is met at
// no fill; one the line reaches outside the band is read off it where it was
// not fitted. Each is no bound.
func TestK2BatchAtITLTarget_TheTarget(t *testing.T) {
	line := itl.Model{A: 0.05, B: 0.012}
	slo := itlBoundFor(line, 0.020, 0.15, 0.8)
	assert.True(t, slo.applies())
	assert.InDelta(t, 0.16, slo.fill, 1e-12)
	assert.Equal(t, itlTargetSLO, slo.source)

	none := itlBoundFor(line, 0, 0.15, 0.8)
	assert.False(t, none.applies())
	assert.Equal(t, itlBoundNoTarget, none.why)

	for target, why := range map[float64]string{
		0.010: itlBoundUnderIntercept,
		0.015: itlBoundBelowBand,
		1:     itlBoundAboveBandFill,
	} {
		b := itlBoundFor(line, target, 0.15, 0.8)
		assert.False(t, b.applies(), "%g s", target)
		assert.Equal(t, why, b.why, "%g s", target)
	}
	edge := itlBoundFor(line, 0.012+0.05*0.8, 0.15, 0.8)
	assert.True(t, edge.applies())
	assert.InDelta(t, 0.8, edge.fill, 1e-9)

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
// bound given, the budget switch as stated, and returns its capacity and its
// P3 k2-decision line.
func priceBound(t *testing.T, rm domain.ReplicaMetrics, params *capacity.EngineParams, budget bool,
	bound itlBatchBound) (*capacity.ReplicaCapacity, map[string]any) {
	t.Helper()
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", rm.VariantName, capacity.Record{
		AcceleratorName: "H200", GpuCount: 1, EngineParams: params,
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchK2DecodeBudgetAtComputedPrompt: budget}))
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
	require.NotNil(t, p3, "a P3 k2-decision line")
	return rc, p3
}

// A bound at a fill of 0.2 holds k2 at 200,000, 190 requests a rank at the
// context where N_steady is 256. One at 0.5 leaves the steady batch's 268,800,
// under it, alone. One that does not apply leaves it, and says why. Off, the
// line is as before. On a pod of four data-parallel ranks the bound is still
// 0.2 of the pod's KV and N_itl a rank's.
func TestK2BatchAtITLTarget_HoldsTheBatch(t *testing.T) {
	at := func(fill float64) itlBatchBound {
		return itlBatchBound{on: true, fill: fill, target: 0.02, source: itlTargetSLO}
	}
	rc, line := priceBound(t, boundRow(), deploymentParams(), false, at(0.2))
	assert.Equal(t, int64(200_000), rc.EffectiveCapacity)
	assert.Equal(t, k2BoundITL, line["k2BatchBound"])
	assert.InDelta(t, 256, line["nSteady"], 1e-9)
	assert.InDelta(t, 0.2*float64(boundKV)/1050, line["nITL"], 1e-9)
	assert.EqualValues(t, int64(boundAtSteady), line["k2AtSteady"])

	rc, line = priceBound(t, boundRow(), deploymentParams(), false, at(0.5))
	assert.Equal(t, int64(boundAtSteady), rc.EffectiveCapacity)
	assert.Equal(t, k2BoundSteady, line["k2BatchBound"])

	rc, line = priceBound(t, boundRow(), deploymentParams(), false, itlBatchBound{on: true, why: itlBoundDefaultB})
	assert.Equal(t, int64(boundAtSteady), rc.EffectiveCapacity, "a default intercept bounds nothing")
	assert.Equal(t, itlBoundDefaultB, line["itlBoundWhy"])
	assert.NotContains(t, line, "nITL")

	rc, line = priceBound(t, boundRow(), deploymentParams(), false, itlBatchBound{})
	assert.Equal(t, int64(boundAtSteady), rc.EffectiveCapacity, "off")
	for _, k := range []string{"nSteady", "nITL", "k2BatchBound", "itlBoundWhy", "itlTargetSource", "k2AtSteady"} {
		assert.NotContains(t, line, k, "off: the line as before")
	}

	four := boundRow()
	four.EngineRanks = 4
	rc, line = priceBound(t, four, deploymentParams(), false, at(0.2))
	assert.Equal(t, int64(200_000), rc.EffectiveCapacity)
	assert.EqualValues(t, int64(4*boundAtSteady), line["k2AtSteady"])
	assert.InDelta(t, 0.2*float64(boundKV)/4/1050, line["nITL"], 1e-9)
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
		if tpot == 0 {
			b = itlBatchBound{on: true, why: itlBoundNoTarget}
		}
		rc, _ := priceBound(t, budgetRow(), budgetParams(), budget, b)
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
// off and a TPOT objective of tpot, and returns the last cycle's P3
// k2-decision line and its itl-fit line.
func itlBoundRun(t *testing.T, on bool, tpot time.Duration, role string) (map[string]any, map[string]any) {
	t.Helper()
	r := itlRig(t, map[string]bool{SwitchK2BatchAtITLTarget: on})
	r.a.capacityStore.Update("test-ns", "test-model", "v", capacity.Record{GpuCount: 1, EngineParams: deploymentParams(),
		LearnedFrom: "deployment"}, r.now)
	var p3, fit map[string]any
	for range 40 {
		ctx, logs := observedCtx(t)
		itlSLOCycle(r, ctx, itlSettleFleet(), tpot, role)
		p3, fit = nil, nil
		for _, e := range logs.FilterMessage("k2-decision").All() {
			if f := e.ContextMap(); f["priority"] == capacity.K2SrcDerived.String() {
				p3 = f
			}
		}
		if fits := logs.FilterMessage("itl-fit").All(); len(fits) > 0 {
			fit = fits[len(fits)-1].ContextMap()
		}
	}
	require.NotNil(t, p3, "a P3 k2-decision line")
	return p3, fit
}

// Through Analyze: v fits its own line, 8 ms + 40 ms x k, from its replicas'
// readings. With a TPOT objective of 16 ms the line reaches it at a fill of
// 0.2, so k2 is held at 0.2 of a replica's 376,480 tokens, 75,296, under the
// 256-request steady batch's figure, 320,000. Off, k2 is that figure and the
// line carries no bound. With no objective there is no bound, and the line
// says so.
func TestK2BatchAtITLTarget_ThroughAnalyze(t *testing.T) {
	on, fit := itlBoundRun(t, true, 16*time.Millisecond, domain.RoleDecode)
	require.NotNil(t, fit, "premise: v fits a line")
	require.Equal(t, itl.TierOLS, fit["tier"], "premise: a full fit, its own B")
	require.Equal(t, "", on["itlBoundWhy"], "the line bounds: %v", on)
	assert.Equal(t, itlTargetSLO, on["itlTargetSource"])
	assert.Equal(t, k2BoundITL, on["k2BatchBound"])
	fill, ok := on["itlTargetFill"].(float64)
	require.True(t, ok)
	assert.InDelta(t, 0.2, fill, 0.02, "the fitted line reaches 16 ms near 0.2")
	assert.EqualValues(t, int64(fill*376_480), on["k2"])

	off, _ := itlBoundRun(t, false, 16*time.Millisecond, domain.RoleDecode)
	assert.NotContains(t, off, "k2BatchBound")
	assert.Equal(t, on["k2AtSteady"], off["k2"], "off: the steady batch's figure")

	none, _ := itlBoundRun(t, true, 0, domain.RoleDecode)
	assert.Equal(t, itlBoundNoTarget, none["itlBoundWhy"])
	assert.Equal(t, k2BoundSteady, none["k2BatchBound"])
	assert.Equal(t, off["k2"], none["k2"])
}

// An aggregated variant's line is report-only for its mu
// (itlLineReportOnly), and does not bound its k2 either.
func TestK2BatchAtITLTarget_AnAggregatedLineDoesNotBound(t *testing.T) {
	p3, fit := itlBoundRun(t, true, 16*time.Millisecond, domain.RoleBoth)
	require.NotNil(t, fit, "premise: v fits a line")
	assert.Equal(t, itlBoundNotDecode, p3["itlBoundWhy"])
	assert.Equal(t, k2BoundSteady, p3["k2BatchBound"])
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
	var p3 map[string]any
	for _, e := range logs.FilterMessage("k2-decision").All() {
		if f := e.ContextMap(); f["priority"] == capacity.K2SrcDerived.String() {
			p3 = f
		}
	}
	require.NotNil(t, p3)
	assert.Equal(t, itlBoundBorrowed, p3["itlBoundWhy"])
	assert.Equal(t, k2BoundSteady, p3["k2BatchBound"])
	assert.EqualValues(t, int64(boundAtSteady), p3["k2"], "trusted, the line would hold it at 200,000")
}

// The switch is on from the constructor, and a replay names it.
func TestK2BatchAtITLTarget_OnFromTheConstructor(t *testing.T) {
	fields := NewSaturationAnalyzer(capacity.NewStore()).switchFields()
	require.Contains(t, fields, "k2-batch-held-at-itl-target")
	assert.True(t, *fields["k2-batch-held-at-itl-target"])
	assert.Contains(t, ReplaySwitchNames(), SwitchK2BatchAtITLTarget)
}
