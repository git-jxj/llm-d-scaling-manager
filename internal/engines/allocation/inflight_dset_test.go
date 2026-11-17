package allocation

import (
	"encoding/json"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// A DisaggregatedSet role is named for the ledger as the role scaler it is
// read through, not as the LeaderWorkerSet whose groups it counts: the
// restore reads it back by that kind.
func TestScaleTargetRefOf_DisaggregatedSetRole(t *testing.T) {
	set := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.DisaggregatedSetAPIVersion,
		"kind":       constants.DisaggregatedSetKind,
		"metadata":   map[string]any{"name": "glm", "namespace": "ns"},
		"spec": map[string]any{"roles": []any{map[string]any{
			"name": "decode",
			"spec": map[string]any{"replicas": int64(2), "leaderWorkerTemplate": map[string]any{
				"size": int64(2), "workerTemplate": map[string]any{},
			}},
		}}},
	}}
	acc, err := scaletarget.NewDisaggregatedSetStaticRoleAccessor(set, "decode")
	if err != nil {
		t.Fatal(err)
	}
	if !scaletarget.IsLeaderWorkerSet(acc) {
		t.Fatal("control: a role counts LeaderWorkerSet groups, so the LWS branch would also match it")
	}
	want := ScaleTargetRef{Kind: constants.DisaggregatedSetRoleScalerKind, Name: "glm-decode"}
	if got := ScaleTargetRefOf(acc); got != want {
		t.Fatalf("ScaleTargetRefOf = %v, want %v", got, want)
	}
}

// A hold persisted for a DisaggregatedSet role is restored and counted from
// its own target, by the kind it was written with.
func TestARestoredDisaggregatedSetRoleHoldKeepsItsTarget(t *testing.T) {
	t0 := time.Unix(100_000, 0)
	ref := ScaleTargetRef{Kind: constants.DisaggregatedSetRoleScalerKind, Name: "glm-decode"}
	l := NewInFlightLedger()
	p := PersistedInFlight{Version: InFlightFormat, Entries: map[string]PersistedInFlightEntry{
		"ns/glm-decode": {GPUs: 8, Want: 2, Placed: 1, Expires: t0.Add(time.Minute), TTLSeconds: 300,
			TargetKind: ref.Kind, TargetName: ref.Name},
	}}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalInFlight(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	var asked ScaleTargetRef
	restored, dropped := l.RestoreInFlightWith(back, t0, InFlightTTL, func(_ string, r ScaleTargetRef) func() (Placed, bool) {
		asked = r
		return func() (Placed, bool) { return Placed{Whole: 1}, true }
	})
	if restored != 1 || dropped != 0 {
		t.Fatalf("restored %d, dropped %d; want 1 and 0", restored, dropped)
	}
	if asked != ref {
		t.Fatalf("the restore asked for %v, want %v", asked, ref)
	}
}
