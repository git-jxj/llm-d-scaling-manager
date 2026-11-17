package steadystate

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// routingCycle is one cycle's metadata as Route leaves it: the canary held
// when known, nothing established when not.
func routingCycle(known bool) []domain.VariantMetadata {
	metas := []domain.VariantMetadata{
		{VariantName: "routed", Namespace: "ns", ModelID: "m", CurrentReplicas: 2},
		{VariantName: "canary", Namespace: "ns", ModelID: "m", CurrentReplicas: 3},
	}
	if known {
		metas[0].RoutingKnown = true
		metas[1].RoutingKnown = true
		metas[1].HeldUnrouted, metas[1].UnroutedReplicas, metas[1].UnroutedReady = true, 3, 3
		metas[1].UnroutedPods = []string{"c-0", "c-1", "c-2"}
	}
	return metas
}

func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// A held variant stays held across a cycle whose reads failed, and the hold
// lapses to "counted as before" -- not to a release logged as one -- once the
// failure outlasts routingVerdictTTL. A later good read holds it again. The
// held Event is recorded on the change into the held state and then only
// every heldEventEvery, never once per cycle.
func TestHoldSurvivesAFailedReadForABoundedTime(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)
	rec := record.NewFakeRecorder(20)
	e := &Engine{Recorder: rec}
	va := &llmdvariant.Variant{ObjectMeta: metav1.ObjectMeta{Name: "canary", Namespace: "ns"}}
	variants := map[string]*llmdvariant.Variant{"ns/canary": va}
	t0 := time.Now()
	cycle := func(at time.Time, known bool) domain.VariantMetadata {
		metas := routingCycle(known)
		e.resolveRouting(context.Background(), "m", "ns", metas, nil, variants, at)
		return metas[1]
	}

	if m := cycle(t0, true); !m.HeldUnrouted {
		t.Fatal("cycle 1: the canary is not held")
	}
	events := drainEvents(rec)
	if len(events) != 1 {
		t.Fatalf("cycle 1: %d Events, want 1 on the change into held", len(events))
	}
	for _, want := range []string{constants.K8SEventHeldNotInPool, "is held at 3", "no InferencePool", "routed", "To release it", "LLMSM_POOL_SELECTED_SUPPLY=false"} {
		if !strings.Contains(events[0], want) {
			t.Errorf("Event %q does not contain %q", events[0], want)
		}
	}

	m := cycle(t0.Add(time.Minute), false)
	if !m.HeldUnrouted || m.UnroutedReplicas != 3 || len(m.UnroutedPods) != 3 {
		t.Fatalf("cycle 2, read failed: %+v, want the last verdict kept", m)
	}
	if !decision.DefaultHeld.Held("ns", "canary", 0, t0.Add(time.Minute)) {
		t.Error("cycle 2: scale from zero no longer sees the hold")
	}

	if m := cycle(t0.Add(2*time.Minute), true); !m.HeldUnrouted {
		t.Fatal("cycle 3: not held")
	}
	if n := len(drainEvents(rec)); n != 0 {
		t.Errorf("cycles 2-3: %d Events, want none: the hold did not change", n)
	}

	m = cycle(t0.Add(2*time.Minute+routingVerdictTTL+time.Second), false)
	if m.HeldUnrouted || m.UnroutedReplicas != 0 {
		t.Fatalf("read failing past the TTL: %+v, want counted as before", m)
	}
	if decision.DefaultHeld.Held("ns", "canary", 0, time.Now()) {
		t.Error("a lapsed hold is still published to scale from zero")
	}

	if m := cycle(t0.Add(heldEventEvery+time.Hour), true); !m.HeldUnrouted {
		t.Fatal("a good read after the lapse does not hold again")
	}
	if n := len(drainEvents(rec)); n != 1 {
		t.Errorf("after heldEventEvery: %d Events, want the reminder", n)
	}
}

// A variant never seen before, with a failed read, is counted as before.
func TestAnUnknownVerdictWithNoHistoryChangesNothing(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)
	e := &Engine{}
	metas := routingCycle(false)
	e.resolveRouting(context.Background(), "m", "ns", metas, nil, nil, time.Now())
	if metas[1].HeldUnrouted || metas[1].UnroutedReplicas != 0 {
		t.Fatalf("%+v: a variant with no history was held on a failed read", metas[1])
	}
}

// Every model of a namespace reads one InferencePool listing per cycle, from
// the uncached reader when there is one.
func TestPoolListerReadsOncePerNamespacePerCycle(t *testing.T) {
	t.Setenv("POOL_GROUP", "inference.networking.k8s.io")
	scheme := runtime.NewScheme()
	if err := v1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	var lists atomic.Int32
	reader := interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).Build(), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			lists.Add(1)
			return c.List(ctx, l, opts...)
		},
	})
	e := &Engine{apiReader: reader}
	pools := e.poolLister()
	for range 3 {
		if _, err := pools(context.Background(), "ns"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pools(context.Background(), "other"); err != nil {
		t.Fatal(err)
	}
	if n := lists.Load(); n != 2 {
		t.Fatalf("%d listings, want 2: one per namespace", n)
	}
	e.resetPoolReads()
	if _, err := pools(context.Background(), "ns"); err != nil {
		t.Fatal(err)
	}
	if n := lists.Load(); n != 3 {
		t.Fatalf("%d listings, want 3: a new cycle reads again", n)
	}
}

// LLMSM_POOL_SELECTED_SUPPLY=false: rows and metadata are left as before, and
// whatever an earlier cycle held is released.
func TestPoolSelectedSupplyOff(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)
	cfg := config.NewTestConfig()
	config.SetPoolSelectedSupplyForTest(cfg, false)
	e := &Engine{Config: cfg}
	decision.DefaultHeld.Publish("ns", "m", []string{"canary"}, time.Now())
	rows := []domain.ReplicaMetrics{{PodName: "c-0", VariantName: "canary"}}
	metas := []domain.VariantMetadata{{VariantName: "canary", Namespace: "ns", ModelID: "m", CurrentReplicas: 1}}
	got := e.routeModel(context.Background(), "m", "ns", metas, nil, nil, nil, rows, nil, time.Now())
	if len(got) != 1 || metas[0].HeldUnrouted || metas[0].RoutingKnown {
		t.Fatalf("rows=%v meta=%+v, want both untouched", got, metas[0])
	}
	if decision.DefaultHeld.Held("ns", "canary", 0, time.Now()) {
		t.Error("an earlier hold survives turning the feature off")
	}
}
