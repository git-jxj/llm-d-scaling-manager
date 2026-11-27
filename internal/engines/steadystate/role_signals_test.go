package steadystate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// roleFixture is one variant of a P/D model whose Deployment may carry a
// role label, and the reading of its one pod.
type roleFixture struct {
	name, label string
	row         domain.ReplicaMetrics
}

// roleInputs is the variants, scale targets, rows, states and metadata the
// engine holds for fixtures, each state's role the one discovery resolves
// (the label, else both).
func roleInputs(fixtures ...roleFixture) (map[string]*llmdvariant.Variant, map[string]scaletarget.ScaleTargetAccessor,
	[]domain.ReplicaMetrics, []domain.VariantReplicaState, []domain.VariantMetadata) {
	variants := map[string]*llmdvariant.Variant{}
	targets := map[string]scaletarget.ScaleTargetAccessor{}
	rows := make([]domain.ReplicaMetrics, 0, len(fixtures))
	states := make([]domain.VariantReplicaState, 0, len(fixtures))
	metas := make([]domain.VariantMetadata, 0, len(fixtures))
	for _, f := range fixtures {
		va := &llmdvariant.Variant{ObjectMeta: metav1.ObjectMeta{Name: f.name, Namespace: "ns"}}
		va.Spec.ScaleTargetRef.Name = f.name
		variants[utils.GetNamespacedKey("ns", f.name)] = va
		deploy := &appsv1.Deployment{}
		if f.label != "" {
			deploy.Spec.Template.Labels = map[string]string{variantmeta.RoleLabel: f.label}
		}
		st := scaletarget.NewDeploymentAccessor(deploy)
		targets[utils.GetNamespacedKey("ns", f.name)] = st
		row := f.row
		row.VariantName, row.PodName = f.name, f.name+"-pod"
		rows = append(rows, row)
		role := variantmeta.RoleFromScaleTarget(st)
		states = append(states, domain.VariantReplicaState{VariantName: f.name, Role: role})
		metas = append(metas, domain.VariantMetadata{VariantName: f.name, Role: role})
	}
	return variants, targets, rows, states, metas
}

var (
	prefillRow = domain.ReplicaMetrics{AvgOutputTokens: 1, RequestRate: 6.7, RequestRateKnown: true,
		PrefillComputedTokenRate: 669, PrefillComputedTokenRateKnown: true}
	decodeRow = domain.ReplicaMetrics{AvgOutputTokens: 1198, RequestRate: 5.9, RequestRateKnown: true,
		PrefillComputedTokenRateKnown: true,
		KVTransferRates:               []domain.KVTransferRate{{Kind: domain.KVTransferNIXL, ByteRate: 2e8, SecondsRate: 0.1}}}
	bothRow = domain.ReplicaMetrics{AvgOutputTokens: 300, RequestRate: 2, RequestRateKnown: true,
		PrefillComputedTokenRate: 1800, PrefillComputedTokenRateKnown: true}
)

func rolesByName(states []domain.VariantReplicaState, metas []domain.VariantMetadata) (map[string]string, map[string]string) {
	s, m := map[string]string{}, map[string]string{}
	for _, v := range states {
		s[v.VariantName] = v.Role
	}
	for _, v := range metas {
		m[v.VariantName] = v.Role
	}
	return s, m
}

func TestResolveRolesFromSignals_UnlabelledVariantsTakeTheSignalsRole(t *testing.T) {
	variants, targets, rows, states, metas := roleInputs(
		roleFixture{name: "p", row: prefillRow}, roleFixture{name: "d", row: decodeRow}, roleFixture{name: "a", row: bothRow})
	gotStates, gotMetas := resolveRolesFromSignals(context.Background(), "m", rows, states, metas, targets, variants)
	want := map[string]string{"p": domain.RolePrefill, "d": domain.RoleDecode, "a": domain.RoleBoth}
	s, m := rolesByName(gotStates, gotMetas)
	assert.Equal(t, want, s, "variant states")
	assert.Equal(t, want, m, "variant metadata: the role join reads the same role")
	for _, v := range states {
		assert.Equal(t, domain.RoleBoth, v.Role, "the caller's slice is not written")
	}
}

func TestResolveRolesFromSignals_ALabelWins(t *testing.T) {
	variants, targets, rows, states, metas := roleInputs(
		roleFixture{name: "p", label: domain.RolePrefill, row: decodeRow}, roleFixture{name: "d", label: domain.RoleDecode, row: decodeRow})
	gotStates, gotMetas := resolveRolesFromSignals(context.Background(), "m", rows, states, metas, targets, variants)
	s, m := rolesByName(gotStates, gotMetas)
	want := map[string]string{"p": domain.RolePrefill, "d": domain.RoleDecode}
	assert.Equal(t, want, s)
	assert.Equal(t, want, m)
}

func TestResolveRolesFromSignals_NoReadingsOrNoScaleTargetKeepTheRole(t *testing.T) {
	variants, targets, _, states, metas := roleInputs(roleFixture{name: "d", row: decodeRow})
	gotStates, _ := resolveRolesFromSignals(context.Background(), "m", nil, states, metas, targets, variants)
	assert.Equal(t, domain.RoleBoth, gotStates[0].Role, "no rows: discovery's role")

	_, _, rows, states, metas := roleInputs(roleFixture{name: "d", row: decodeRow})
	gotStates, _ = resolveRolesFromSignals(context.Background(), "m", rows, states, metas, nil, nil)
	assert.Equal(t, domain.RoleBoth, gotStates[0].Role, "no scale target: nothing says the role was not declared")
}

func TestDeclaredRole(t *testing.T) {
	for label, want := range map[string]bool{"": false, "unknown": false, domain.RoleBoth: true,
		domain.RolePrefill: true, domain.RoleDecode: true} {
		deploy := &appsv1.Deployment{}
		if label != "" {
			deploy.Spec.Template.Labels = map[string]string{variantmeta.RoleLabel: label}
		}
		_, ok := variantmeta.DeclaredRole(scaletarget.NewDeploymentAccessor(deploy))
		assert.Equal(t, want, ok, "label %q", label)
	}
	_, ok := variantmeta.DeclaredRole(nil)
	assert.False(t, ok)
}
