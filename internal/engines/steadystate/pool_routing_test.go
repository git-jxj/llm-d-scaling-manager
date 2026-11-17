package steadystate

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// applyRouting over the ways an analyzer fills ReplicaCount. Each case that
// expects a change fails on the parent commit, where nothing is subtracted.
func TestApplyRoutingTakesUnroutedReplicasOutOfSupply(t *testing.T) {
	cases := []struct {
		name                 string
		vc                   domain.VariantCapacity
		m                    domain.VariantMetadata
		wantCount, wantPend  int
		wantPerReplicaIsZero bool
	}{
		{
			name:      "every replica routed: unchanged",
			vc:        domain.VariantCapacity{ReplicaCount: 3, ObservedReplicas: 3, PendingReplicas: 1, PerReplicaCapacity: 10},
			m:         domain.VariantMetadata{CurrentReplicas: 4},
			wantCount: 3, wantPend: 1,
		},
		{
			// Rows already exclude the unselected Pod, so the count stands; it
			// came back as pending (CurrentReplicas - rows) and must leave it.
			name:      "rows counted, one Ready Pod outside the pool",
			vc:        domain.VariantCapacity{ReplicaCount: 2, ObservedReplicas: 2, PendingReplicas: 1, PerReplicaCapacity: 10},
			m:         domain.VariantMetadata{CurrentReplicas: 3, UnroutedReplicas: 1, UnroutedReady: 1},
			wantCount: 2, wantPend: 0,
		},
		{
			// No rows: the count is scale-target status and still includes it.
			name:      "status counted, one Ready Pod outside the pool",
			vc:        domain.VariantCapacity{ReplicaCount: 3, PendingReplicas: 1, PerReplicaCapacity: 10},
			m:         domain.VariantMetadata{CurrentReplicas: 4, UnroutedReplicas: 1, UnroutedReady: 1},
			wantCount: 2, wantPend: 1,
		},
		{
			name:      "status counted, one starting Pod outside the pool",
			vc:        domain.VariantCapacity{ReplicaCount: 3, PendingReplicas: 1, PerReplicaCapacity: 10},
			m:         domain.VariantMetadata{CurrentReplicas: 4, UnroutedReplicas: 1},
			wantCount: 3, wantPend: 0,
		},
		{
			name:      "held variant contributes nothing and is not sized",
			vc:        domain.VariantCapacity{ReplicaCount: 2, ObservedReplicas: 2, PendingReplicas: 1, StuckReplicas: 1, PerReplicaCapacity: 10},
			m:         domain.VariantMetadata{CurrentReplicas: 3, HeldUnrouted: true, UnroutedReplicas: 3, UnroutedReady: 2},
			wantCount: 0, wantPend: 0, wantPerReplicaIsZero: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vc := tc.vc
			applyRouting(&vc, tc.m)
			if vc.ReplicaCount != tc.wantCount || vc.PendingReplicas != tc.wantPend {
				t.Fatalf("ReplicaCount=%d PendingReplicas=%d, want %d and %d",
					vc.ReplicaCount, vc.PendingReplicas, tc.wantCount, tc.wantPend)
			}
			if (vc.PerReplicaCapacity == 0) != tc.wantPerReplicaIsZero {
				t.Fatalf("PerReplicaCapacity=%v, want zero=%t", vc.PerReplicaCapacity, tc.wantPerReplicaIsZero)
			}
		})
	}
}

func TestDropUnroutedRows(t *testing.T) {
	rows := []domain.ReplicaMetrics{
		{PodName: "a-0", VariantName: "va"},
		{PodName: "a-2", VariantName: "va"},
		{PodName: "b-0", VariantName: "vb"},
		{PodName: "bridge", VariantName: "vb", FromWarmPool: true},
	}
	metas := []domain.VariantMetadata{
		{VariantName: "va", UnroutedPods: []string{"a-2"}},
		{VariantName: "vb", HeldUnrouted: true},
	}
	got := dropUnroutedRows(rows, metas)
	if len(got) != 1 || got[0].PodName != "a-0" {
		t.Fatalf("kept %+v, want only a-0", got)
	}
	if len(dropUnroutedRows(rows, []domain.VariantMetadata{{VariantName: "va"}})) != len(rows) {
		t.Fatal("rows dropped with nothing unrouted")
	}
}

// The held variant end to end through the capacity build and the default
// optimizer: its idle capacity is not supply, so the routed variant scales
// up, and the held one -- the cheaper, which the optimizer would otherwise
// pick -- stays at its count. On the parent commit the held variant's two
// replicas are supply and nothing scales.
func TestHeldVariantIsNotSupplyAndIsNotScaled(t *testing.T) {
	result := &domain.AnalyzerResult{
		ModelID: "m", Namespace: "ns", TotalDemand: 300,
		VariantCapacities: []domain.VariantCapacity{
			{VariantName: "routed", ReplicaCount: 2, ObservedReplicas: 2, PerReplicaCapacity: 100, TotalDemand: 300},
			{VariantName: "canary", ReplicaCount: 2, ObservedReplicas: 2, PerReplicaCapacity: 100},
		},
	}
	metas := map[string]domain.VariantMetadata{
		"routed": {VariantName: "routed", ModelID: "m", Namespace: "ns", Cost: 2, CurrentReplicas: 2},
		"canary": {VariantName: "canary", ModelID: "m", Namespace: "ns", Cost: 1, CurrentReplicas: 2,
			HeldUnrouted: true, UnroutedReplicas: 2, UnroutedReady: 2},
	}
	nr := allocation.NamedAnalyzerResult{Name: "saturation", Result: result}
	buildCapacities(context.Background(), &nr, metas, 0.85, 0.7)
	// As newNamedResult does after the build.
	nr.Remaining = nr.RequiredCapacity
	nr.Spare = nr.SpareCapacity
	if nr.TotalSupply != 200 {
		t.Fatalf("TotalSupply = %v, want 200: the held variant's replicas are not supply", nr.TotalSupply)
	}
	req := allocation.ModelScalingRequest{
		ModelID: "m", Namespace: "ns",
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "routed", CurrentReplicas: 2},
			{VariantName: "canary", CurrentReplicas: 2},
		},
		Variants:        []domain.VariantMetadata{metas["routed"], metas["canary"]},
		CompositeSignal: nr,
	}
	out := map[string]int{}
	for _, d := range allocation.NewCostAwareOptimizer().Optimize(context.Background(), []allocation.ModelScalingRequest{req}, nil) {
		out[d.VariantName] = d.TargetReplicas
	}
	if out["canary"] != 2 {
		t.Fatalf("held variant target = %d, want its current 2", out["canary"])
	}
	if out["routed"] <= 2 {
		t.Fatalf("routed variant target = %d, want a scale-up above 2", out["routed"])
	}
}

// The Event on a held variant says it is held, why, and how to release it.
func TestHeldVariantEventSaysWhyAndHowToRelease(t *testing.T) {
	rec := record.NewFakeRecorder(10)
	e := &Engine{Recorder: rec, vaEventTracker: map[string]bool{}}
	va := &llmdvariant.Variant{ObjectMeta: metav1.ObjectMeta{Name: "canary", Namespace: "ns"}}
	metas := []domain.VariantMetadata{
		{VariantName: "routed", Namespace: "ns", CurrentReplicas: 2},
		{VariantName: "canary", Namespace: "ns", CurrentReplicas: 2, HeldUnrouted: true, UnroutedReplicas: 2},
	}
	e.reportRouting(context.Background(), "m", "ns", metas, map[string]*llmdvariant.Variant{"ns/canary": va})
	select {
	case ev := <-rec.Events:
		for _, want := range []string{constants.K8SEventHeldNotInPool, "is held at 2", "no InferencePool", "routed", "To release it"} {
			if !strings.Contains(ev, want) {
				t.Errorf("event %q does not contain %q", ev, want)
			}
		}
	default:
		t.Fatal("no Event recorded for the held variant")
	}
}
