package saturation

import (
	"context"
	"strings"
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

// Issue llm-d-incubation/llm-d-scaling-manager#258: the derived k2's batch is
// held where the variant's ITL line reaches its target. The tests set the
// switch by its name, so they run on a commit that predates it, and fail
// there.

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

// The line 0.05 x k + 0.012 s a token: a TPOT target of 20 ms is reached at a
// fill of 0.16, inside a band from 0.15. With no target set, the bound is the band's pricing point,
// at the line's ITL there. A target at or under B is met at no fill, and is
// no bound; a target past the line's fill of 1 is held at 1.
func TestK2BatchAtITLTarget_TheTarget(t *testing.T) {
	line := itl.Model{A: 0.05, B: 0.012}
	slo := itlBoundFor(line, 0.020, 0.68, 0.15)
	assert.True(t, slo.applies())
	assert.InDelta(t, 0.16, slo.fill, 1e-12)
	assert.Equal(t, itlTargetSLO, slo.source)

	none := itlBoundFor(line, 0, 0.68, 0.15)
	assert.True(t, none.applies())
	assert.InDelta(t, 0.68, none.fill, 1e-12)
	assert.InDelta(t, 0.046, none.target, 1e-12)
	assert.Equal(t, itlTargetPricingPoint, none.source)

	under := itlBoundFor(line, 0.010, 0.68, 0.15)
	assert.False(t, under.applies())
	assert.Equal(t, itlBoundUnderIntercept, under.why)

	far := itlBoundFor(line, 1, 0.68, 0.15)
	assert.InDelta(t, 1, far.fill, 1e-12)

	assert.False(t, itlBoundFor(itl.Model{}, 0.020, 0.68, 0.15).applies(), "no line")

	// 15 ms is reached at a fill of 0.06, under the band's lower edge of
	// 0.15: the line was not fitted there, and it is no bound.
	below := itlBoundFor(line, 0.015, 0.68, 0.15)
	assert.False(t, below.applies())
	assert.Equal(t, itlBoundBelowBand, below.why)
	assert.False(t, itlBatchBound{fill: 0.5}.applies(), "the zero value is the switch off")
}

// residentITL prices residentRow(0.3) as a decode replica of variant-d with
// the resident line and the ITL bound given, and returns its capacity and
// its P3 k2-decision line.
func residentITL(t *testing.T, resident residentKVLine, bound itlBatchBound) (*capacity.ReplicaCapacity, map[string]any) {
	t.Helper()
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		AcceleratorName: "H200", GpuCount: 1, EngineParams: deploymentParams(),
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	cfg := &config.ScalingPolicy{KvCacheThreshold: 0.8, ScaleUpThreshold: 0.85, QueueLengthThreshold: 5}
	rc := a.computeReplicaCapacity(residentRow(0.3), cfg, tuningOf(cfg), "test-model", "test-ns", 1, domain.RoleDecode, "H200",
		outputKeys{k2: 100, mu: 100}, 100, 1000, 0, whatif.DerivedMu{}, servedRates{}, false, false, replicaRouting{},
		false, resident, bound, logr.FromContextOrDiscard(ctx))
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

// The resident fixture (k2_resident_kv_test.go): 256 a rank at the budget,
// 268,800 at the context, 768,000 at the 3000-token reading, a cache of
// 1,000,000. A bound at a fill of 0.2 holds k2 at 200,000 either way: 66.7
// requests a rank at 3000 each, where N_steady is 256. A bound at 0.5 holds
// the reading's 768,000 at 500,000 and leaves the context's 268,800, under
// it, alone. A bound that does not apply leaves both.
func TestK2BatchAtITLTarget_HoldsTheBatch(t *testing.T) {
	read := residentKVLine{perRequest: 3000}
	at := func(fill float64) itlBatchBound {
		return itlBatchBound{on: true, fill: fill, target: 0.02, source: itlTargetSLO}
	}

	rc, line := residentITL(t, read, at(0.2))
	assert.Equal(t, int64(200_000), rc.EffectiveCapacity)
	assert.Equal(t, "itl", line["k2BatchBound"])
	assert.InDelta(t, 256, line["nSteady"], 1e-9)
	assert.InDelta(t, 0.2*1_000_000/3000.0, line["nITL"], 1e-9)
	assert.EqualValues(t, int64(residentAtCtx), line["k2AtSteady"])

	rc, line = residentITL(t, residentKVLine{}, at(0.2))
	assert.Equal(t, int64(200_000), rc.EffectiveCapacity, "at the context too")
	assert.InDelta(t, 0.2*1_000_000/1050.0, line["nITL"], 1e-9, "N_itl at the context's KV a request")

	rc, line = residentITL(t, read, at(0.5))
	assert.Equal(t, int64(500_000), rc.EffectiveCapacity)
	assert.Equal(t, "itl", line["k2BatchBound"])
	rc, line = residentITL(t, residentKVLine{}, at(0.5))
	assert.Equal(t, int64(residentAtCtx), rc.EffectiveCapacity)
	assert.Equal(t, "steady", line["k2BatchBound"])

	rc, line = residentITL(t, read, itlBatchBound{on: true, why: itlBoundDefaultB})
	assert.Equal(t, int64(768_000), rc.EffectiveCapacity, "a default intercept bounds nothing")
	assert.Equal(t, itlBoundDefaultB, line["itlBoundWhy"])
	assert.Equal(t, "steady", line["k2BatchBound"])
	assert.NotContains(t, line, "nITL")

	rc, line = residentITL(t, read, itlBatchBound{})
	assert.Equal(t, int64(768_000), rc.EffectiveCapacity, "off")
	for _, k := range []string{"nSteady", "nITL", "k2BatchBound", "itlBoundWhy", "itlTargetSource", "k2AtSteady"} {
		assert.NotContains(t, line, k, "off: the line as before")
	}
}

// itlSLOCycle runs one cycle of the rig with the policy's TPOT objective set
// to tpot (none at 0), logging to ctx's logger.
func itlSLOCycle(r *restartRig, ctx context.Context, rows []domain.ReplicaMetrics, tpot time.Duration, role string) {
	r.t.Helper()
	states := decodeVariants("v")
	states[0].Role = role
	roles := map[string]string{"v": role}
	r.tr.Observe(settleModel("test-ns", "test-model"), settlerow.Of(rows, roles), r.now)
	in := makeAnalyzerInput(rows, states)
	if tpot > 0 {
		in.Config.(*config.ScalingPolicy).SLO = &config.SLOPolicy{TPOT: &config.SLOObjective{Target: config.SLODuration{Duration: tpot}}}
	}
	_, err := r.a.Analyze(ctx, in)
	require.NoError(r.t, err)
	r.now = r.now.Add(restartCycle)
}

// itlBoundRun runs 40 cycles of three steady replicas whose ITL reads
// 8 ms + 40 ms x k, so v fits its own line, with the switch on or off and a
// TPOT objective of tpot, and returns the last cycle's P3 k2-decision line
// and its itl-fit line.
func itlBoundRun(t *testing.T, on bool, tpot time.Duration) (map[string]any, map[string]any) {
	t.Helper()
	return itlBoundRunAs(t, on, tpot, domain.RoleDecode)
}

// itlBoundRunAs is itlBoundRun with v in role.
func itlBoundRunAs(t *testing.T, on bool, tpot time.Duration, role string) (map[string]any, map[string]any) {
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
// 256-request steady batch's figure, 320,000. Off, k2 is that figure and the line
// carries no bound.
func TestK2BatchAtITLTarget_ThroughAnalyze(t *testing.T) {
	on, fit := itlBoundRun(t, true, 16*time.Millisecond)
	require.NotNil(t, fit, "premise: v fits a line")
	require.Equal(t, itl.TierOLS, fit["tier"], "premise: a full fit, its own B")
	require.Equal(t, "", on["itlBoundWhy"], "the line bounds: %v", on)
	assert.Equal(t, itlTargetSLO, on["itlTargetSource"])
	assert.Equal(t, "itl", on["k2BatchBound"])
	fill, ok := on["itlTargetFill"].(float64)
	require.True(t, ok)
	assert.InDelta(t, 0.2, fill, 0.02, "the fitted line reaches 16 ms near 0.2")
	assert.EqualValues(t, int64(fill*376_480), on["k2"])

	off, _ := itlBoundRun(t, false, 16*time.Millisecond)
	assert.NotContains(t, off, "k2BatchBound")
	assert.Equal(t, on["k2AtSteady"], off["k2"], "off: the steady batch's figure")
	assert.Less(t, toFloat(t, on["k2"]), toFloat(t, off["k2"]))

	// No objective: the band's pricing point, 0.68 on these thresholds.
	// The steady batch's figure, 256 x 1250 = 320,000, is 0.85 of the
	// replica's KV, past it, so the pricing point holds it at 0.68 of it.
	none, _ := itlBoundRun(t, true, 0)
	assert.Equal(t, itlTargetPricingPoint, none["itlTargetSource"])
	assert.Equal(t, "itl", none["k2BatchBound"])
	assert.EqualValues(t, int64(256_006), none["k2"], "0.68 x 376,480")
}

// An aggregated variant's line is report-only for its mu
// (itlLineReportOnly), and does not bound its k2 either.
func TestK2BatchAtITLTarget_AnAggregatedLineDoesNotBound(t *testing.T) {
	p3, fit := itlBoundRunAs(t, true, 16*time.Millisecond, domain.RoleBoth)
	require.NotNil(t, fit, "premise: v fits a line")
	assert.Equal(t, itlBoundNotDecode, p3["itlBoundWhy"])
	assert.Equal(t, "steady", p3["k2BatchBound"])
}

// A line borrowed from a sibling's configuration is not the variant's own:
// with a TPOT objective that would hold it, the batch stays N_steady and the
// line says why.
func TestK2BatchAtITLTarget_ABorrowedLineDoesNotBound(t *testing.T) {
	lend := func(a *SaturationAnalyzer) {
		a.k2AtITLTarget = true
		a.publishLine(a.itlPhysicsKey("test-ns", "test-model", "variant-d", "H200", 1, ""),
			"a fitted line", itl.Model{A: 0.05, B: 0.012}, time.Now())
	}
	_, p3, _ := residentRun(t, []domain.ReplicaMetrics{residentRow(0.3)}, []string{"variant-d"}, residentSwitches{}, lend)
	require.NotNil(t, p3)
	assert.Equal(t, itlBoundBorrowed, p3["itlBoundWhy"])
	assert.Equal(t, "steady", p3["k2BatchBound"])
	assert.EqualValues(t, int64(residentAtCtx), p3["k2"])
}

// toFloat reads a numeric log field.
func toFloat(t *testing.T, v any) float64 {
	t.Helper()
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	case int:
		return float64(x)
	}
	t.Fatalf("not a number: %T %v", v, v)
	return 0
}

// The switch is off from the constructor, and a replay names it.
func TestK2BatchAtITLTarget_OffFromTheConstructor(t *testing.T) {
	fields := NewSaturationAnalyzer(capacity.NewStore()).switchFields()
	require.Contains(t, fields, "k2-batch-held-at-itl-target")
	assert.False(t, *fields["k2-batch-held-at-itl-target"])
	assert.True(t, strings.Contains(strings.Join(ReplaySwitchNames(), ","), SwitchK2BatchAtITLTarget))
}
