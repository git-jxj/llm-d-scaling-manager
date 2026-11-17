package scalefromzero

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// A model whose only running variant is held (no InferencePool selects it) is
// not covered, so it still wakes, and the held variant is never a wake
// candidate. A stale publication no longer holds.
func TestAHeldVariantNeitherCoversNorWakes(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)
	now := time.Now()
	va := func(name string) llmdvariant.Variant {
		return llmdvariant.Variant{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Spec:       llmdvariant.Spec{ModelID: "m"},
		}
	}
	active := []llmdvariant.Variant{va("canary")}
	inactive := []llmdvariant.Variant{va("routed"), va("canary-parked")}

	if !activeRoleCoverage(withoutHeld(active, now), nil)[modelKey("ns", "m")].decode {
		t.Fatal("precondition: with nothing held, the running canary covers decode")
	}

	decision.DefaultHeld.Publish("ns", "m", []string{"canary", "canary-parked"}, now)
	if activeRoleCoverage(withoutHeld(active, now), nil)[modelKey("ns", "m")].decode {
		t.Error("a held variant counted as the model's running decode: a model whose routed variants are at zero would never wake")
	}
	got := withoutHeld(inactive, now)
	if len(got) != 1 || got[0].Name != "routed" {
		t.Errorf("wake candidates = %v, want only routed", got)
	}

	if len(withoutHeld(active, now.Add(heldMaxAge+time.Minute))) != 1 {
		t.Error("a publication older than heldMaxAge still holds")
	}
}
