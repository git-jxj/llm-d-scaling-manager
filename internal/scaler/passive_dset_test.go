package scaler

import (
	"context"
	"errors"
	"fmt"
	"testing"

	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// passiveFor marks the listed models passive.
type passiveFor map[string]bool

func (p passiveFor) Model(_, modelID string) (bool, string) { return p[modelID], "test" }

// TestPassiveHoldsADisaggregatedSetRole: a ScaledObject per role targets the
// role's DisaggregatedSetRoleScaler. For a passive model the scaler answers
// the role's CURRENT count, read from the role scaler's /scale, whatever the
// decision store holds -- a steady-state count, a scale-from-zero wake, a
// park -- so the HPA holds. A failed read answers an error, never a number:
// with no fallback KEDA and the HPA then change nothing, where 0 would park
// the role and the decision would move it.
func TestPassiveHoldsADisaggregatedSetRole(t *testing.T) {
	gvk := schema.FromAPIVersionAndKind(constants.DisaggregatedSetAPIVersion, constants.DisaggregatedSetRoleScalerKind)
	const target = "glm-decode"
	md := map[string]string{registry.ModelIDKey: "org/GLM"}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: gvk.Group, Resource: "disaggregatedsetrolescalers/scale"}, target, errors.New("rbac"))
	for _, tc := range []struct {
		name       string
		current    *int64
		scaleErr   error
		wantValue  int64
		wantActive bool
		wantCode   codes.Code
	}{
		{name: "running role holds its count", current: ptr.To(int64(3)), wantValue: 3, wantActive: true},
		{name: "parked role stays parked", current: ptr.To(int64(0)), wantValue: 0},
		{name: "role scaler missing: an error, not a count", wantCode: codes.NotFound},
		{name: "scale read forbidden: an error, not a count", current: ptr.To(int64(3)), scaleErr: forbidden, wantCode: codes.PermissionDenied},
	} {
		for _, decided := range []int32{0, 1, 7} {
			t.Run(fmt.Sprintf("%s, decision %d", tc.name, decided), func(t *testing.T) {
				f := scaleFixture{gvk: gvk, name: target, scale: tc.current, scaleErr: tc.scaleErr,
					object: map[string]any{"spec": map[string]any{}}}
				store := decision.NewStore()
				store.Set(undecidedNS, target, decided)
				h := crdHandler(t, store, f, &readCounts{}).WithPassive(passiveFor{"org/GLM": true})
				ctx := context.Background()

				got, err := getMetrics(ctx, h, md)
				active, aerr := h.IsActive(ctx, &pb.ScaledObjectRef{Namespace: undecidedNS, Name: "so", ScalerMetadata: md})
				if tc.wantCode != codes.OK {
					if status.Code(err) != tc.wantCode {
						t.Fatalf("decision %d: GetMetrics = %d, %v; want code %s", decided, got, err, tc.wantCode)
					}
					if aerr == nil {
						t.Fatalf("decision %d: IsActive = %v with no error; an unreadable count must not answer", decided, active.GetResult())
					}
					return
				}
				if err != nil || got != tc.wantValue {
					t.Fatalf("decision %d: GetMetrics = %d, %v; want the current %d", decided, got, err, tc.wantValue)
				}
				if aerr != nil || active.GetResult() != tc.wantActive {
					t.Fatalf("decision %d: IsActive = %v, %v; want %v", decided, active.GetResult(), aerr, tc.wantActive)
				}
			})
		}
	}

	// The control: the same role, active, answers the decision.
	f := scaleFixture{gvk: gvk, name: target, scale: ptr.To(int64(3)), object: map[string]any{"spec": map[string]any{}}}
	store := decision.NewStore()
	store.Set(undecidedNS, target, 7)
	h := crdHandler(t, store, f, &readCounts{}).WithPassive(passiveFor{})
	if got, err := getMetrics(context.Background(), h, md); err != nil || got != 7 {
		t.Fatalf("active: GetMetrics = %d, %v; want the decision 7", got, err)
	}
}
