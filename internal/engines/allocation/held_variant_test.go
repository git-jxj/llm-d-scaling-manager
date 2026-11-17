package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// A held variant (no InferencePool selects it) is never moved and never
// counts as covering its model. These pin the enforcer and the cost-aware
// optimizer's cheapest-at-1 rule to that.

func idleEnforcer() *Enforcer {
	return NewEnforcer(func(context.Context, inferenceengine.Engine, string, string, time.Duration) (float64, error) {
		return 0, nil
	}, nil)
}

// Scale-to-zero parks the routed variant and leaves the held one at its count.
func TestScaleToZeroLeavesAHeldVariantAtItsCount(t *testing.T) {
	on := true
	decisions := []domain.VariantDecision{
		{VariantName: "routed", ModelID: "m", Namespace: "ns", Cost: 2, CurrentReplicas: 1, TargetReplicas: 1},
		{VariantName: "canary", ModelID: "m", Namespace: "ns", Cost: 1, CurrentReplicas: 3, TargetReplicas: 3, HeldUnrouted: true},
	}
	policy := &config.ScalingPolicy{ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &on, RetentionPeriod: "10m"}}
	idleEnforcer().EnforcePolicyOnDecisions(context.Background(), "m", "ns", decisions, policy, "cost-aware", inferenceengine.EngineVLLM)
	if decisions[0].TargetReplicas != 0 {
		t.Errorf("routed target = %d, want 0: an idle model parks", decisions[0].TargetReplicas)
	}
	if decisions[1].TargetReplicas != 3 {
		t.Errorf("held target = %d, want its current 3: nothing moves a held variant", decisions[1].TargetReplicas)
	}
}

// With scale-to-zero off, the minimum of one replica goes to a routed variant
// even though the held one has replicas: the router cannot reach those.
func TestMinimumReplicaIgnoresAHeldVariant(t *testing.T) {
	off := false
	decisions := []domain.VariantDecision{
		{VariantName: "routed", ModelID: "m", Namespace: "ns", Cost: 2, CurrentReplicas: 1, TargetReplicas: 0},
		{VariantName: "canary", ModelID: "m", Namespace: "ns", Cost: 1, CurrentReplicas: 3, TargetReplicas: 3, HeldUnrouted: true},
	}
	policy := &config.ScalingPolicy{ScaleToZero: &config.ScaleToZeroEnvelope{Enabled: &off}}
	idleEnforcer().EnforcePolicyOnDecisions(context.Background(), "m", "ns", decisions, policy, "cost-aware", inferenceengine.EngineVLLM)
	if decisions[0].TargetReplicas != 1 {
		t.Errorf("routed target = %d, want 1: the held variant's replicas do not cover the model", decisions[0].TargetReplicas)
	}
	if decisions[1].TargetReplicas != 3 {
		t.Errorf("held target = %d, want 3", decisions[1].TargetReplicas)
	}
}

// An idle model sheds replicas: the routed variant keeps one, though the held
// variant is cheaper and has replicas, and the held one is not touched.
func TestCheapestAtOneProtectsTheRoutedVariant(t *testing.T) {
	// Cost-descending, as sortVariantsForScaleDown orders them: the held
	// canary is the cheapest and comes last.
	sorted := []variantRecord{
		{VariantMetadata: domain.VariantMetadata{VariantName: "routed", Cost: 2}, PerReplicaCapacity: 100},
		{VariantMetadata: domain.VariantMetadata{VariantName: "canary", Cost: 1, HeldUnrouted: true}},
	}
	targets := map[string]int{"routed": 2, "canary": 3}
	scaleDownVariantSet(context.Background(), sorted, targets, nil,
		func(variantRecord) int { return 10 }, func(variantRecord, int) {})
	if targets["routed"] != 1 {
		t.Errorf("routed target = %d, want 1: the cheapest routed variant is kept at one", targets["routed"])
	}
	if targets["canary"] != 3 {
		t.Errorf("held target = %d, want 3", targets["canary"])
	}
	if anyHasReplicas(sorted[1:], targets) {
		t.Error("a held variant's replicas count as covering the model")
	}
}

// The optimizer's decision carries the hold, so the enforcer can skip it.
func TestDecisionCarriesTheHold(t *testing.T) {
	req := ModelScalingRequest{
		ModelID: "m", Namespace: "ns",
		VariantStates:   []domain.VariantReplicaState{{VariantName: "canary", CurrentReplicas: 3}},
		CompositeSignal: NamedAnalyzerResult{Name: "saturation", Result: &domain.AnalyzerResult{ModelID: "m", Namespace: "ns"}},
	}
	vcs := map[string]variantRecord{"canary": {VariantMetadata: domain.VariantMetadata{VariantName: "canary", HeldUnrouted: true}}}
	ds := buildDecisionsWithOptimizer(req, buildStateMap(req.VariantStates), vcs, map[string]int{"canary": 3}, "cost-aware")
	if len(ds) != 1 || !ds[0].HeldUnrouted || ds[0].TargetReplicas != 3 {
		t.Fatalf("decision %+v, want held at 3", ds)
	}
}
