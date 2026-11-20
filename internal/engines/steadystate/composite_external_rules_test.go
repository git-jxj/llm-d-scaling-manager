package steadystate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/external"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// cannedSource is a metrics source whose every query returns the series in
// values: one series per number, none when the slice is empty.
type cannedSource struct {
	ql     *source.QueryList
	values map[string][]float64
}

func newCannedSource() *cannedSource {
	return &cannedSource{ql: source.NewQueryList(), values: map[string][]float64{}}
}

func (s *cannedSource) QueryList() *source.QueryList { return s.ql }

func (s *cannedSource) Refresh(_ context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	out := map[string]*source.MetricResult{}
	for _, q := range spec.Queries {
		r := &source.MetricResult{QueryName: q, Values: []source.MetricValue{}}
		for _, v := range s.values[q] {
			r.Values = append(r.Values, source.MetricValue{Value: v})
		}
		out[q] = r
	}
	return out, nil
}

func (s *cannedSource) Get(string, map[string]string) *source.CachedValue { return nil }

// pdSaturation is saturation's entry for a P/D model: prefill variant "p"
// with 2 replicas, decode variant "d" with 4.
func pdSaturation() allocation.NamedAnalyzerResult {
	return allocation.NamedAnalyzerResult{
		Name: domain.SaturationAnalyzerName,
		Live: true,
		Result: &domain.AnalyzerResult{
			AnalyzerName: domain.SaturationAnalyzerName, ModelID: "m", Namespace: "ns",
			RoleDemand:  map[string]float64{domain.RolePrefill: 150, domain.RoleDecode: 900},
			TotalDemand: 1050,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 2, PerReplicaCapacity: 100, TotalDemand: 150, Reason: "measured"},
				{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 4, PerReplicaCapacity: 300, TotalDemand: 900, Reason: "measured"},
			},
		},
	}
}

// externalOnPD runs a real external analyzer for that model: body is its
// definition, and its queries return values.
func externalOnPD(t *testing.T, body external.Body, values map[string][]float64) allocation.NamedAnalyzerResult {
	t.Helper()
	src := newCannedSource()
	src.values = values
	a, err := external.New(external.Definition{Label: "queue-depth", Bodies: map[string]external.Body{"": body}}, src)
	require.NoError(t, err)
	res, err := a.Analyze(context.Background(), domain.AnalyzerInput{
		ModelID: "m", Namespace: "ns",
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "p", Role: domain.RolePrefill, CurrentReplicas: 2},
			{VariantName: "d", Role: domain.RoleDecode, CurrentReplicas: 4},
		},
	})
	require.NoError(t, err)
	return allocation.NamedAnalyzerResult{Name: "queue-depth", Live: true, Result: res}
}

// TestExternalRoleRulesSizeAPDModel: listed alone on a P/D model, an external
// analyzer with a demand rule per role grows one role and shrinks the other,
// each from its own rule: prefill asks for 4 replicas (12 / 3) of the 2 it
// runs, decode for 2 (10 / 5) of its 4. With one model-level query it sized
// neither, so the policy held both roles both ways.
func TestExternalRoleRulesSizeAPDModel(t *testing.T) {
	sat := pdSaturation()
	ext := externalOnPD(t, external.Body{
		Threshold: 5,
		Demand: []external.DemandRule{
			{Role: domain.RolePrefill, Query: "sum(prefill_queue)", Threshold: 3},
			{Role: domain.RoleDecode, Query: "sum(decode_queue)"},
		},
	}, map[string][]float64{
		"external:queue-depth:demand@prefill": {12},
		"external:queue-depth:demand@decode":  {10},
	})

	composite := buildComposite(context.Background(), []allocation.NamedAnalyzerResult{sat, ext},
		[]allocation.NamedAnalyzerResult{ext}, 0.85, 0.7)

	assert.Nil(t, composite.Holds, "each role is sized by its own rule")
	assert.Positive(t, composite.RoleCapacities[domain.RolePrefill].RequiredCapacity, "prefill grows")
	assert.Zero(t, composite.RoleCapacities[domain.RolePrefill].SpareCapacity)
	assert.Positive(t, composite.RoleCapacities[domain.RoleDecode].SpareCapacity, "decode shrinks")
	assert.Zero(t, composite.RoleCapacities[domain.RoleDecode].RequiredCapacity)

	// One model-level query, as every definition was before the rules: the
	// same signal sizes no role of a P/D model.
	modelLevel := externalOnPD(t, external.Body{Query: "sum(queue)", Threshold: 5},
		map[string][]float64{"external:queue-depth:demand": {22}})
	held := buildComposite(context.Background(), []allocation.NamedAnalyzerResult{sat, modelLevel},
		[]allocation.NamedAnalyzerResult{modelLevel}, 0.85, 0.7)
	assert.Equal(t, map[string]allocation.RoleHold{
		domain.RolePrefill: {NoScaleUp: true, NoScaleDown: true},
		domain.RoleDecode:  {NoScaleUp: true, NoScaleDown: true},
	}, held.Holds)
}

// TestExternalEmptyIsMissingHoldsForTheGrace: a demand query that returns no
// series is, with emptyIsMissing, no reading: the model does not shrink on
// saturation's smaller need while the analyzer is inside its grace, and once
// the grace has run out it no longer counts. Without the switch the same empty
// result is a reading of 0 demand, and the model shrinks at once.
func TestExternalEmptyIsMissingHoldsForTheGrace(t *testing.T) {
	const query = "external:queue-depth:demand"
	for _, tc := range []struct {
		name           string
		emptyIsMissing bool
	}{{"switch on", true}, {"switch off", false}} {
		t.Run(tc.name, func(t *testing.T) {
			src := newCannedSource()
			ext, err := external.New(external.Definition{Label: "queue-depth", Bodies: map[string]external.Body{
				"": {Query: "sum(queue)", Threshold: 200, EmptyIsMissing: tc.emptyIsMissing},
			}}, src)
			require.NoError(t, err)
			sat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName,
				result: liveEntry(domain.SaturationAnalyzerName, 0, 200).Result}
			e := &Engine{analyzers: testAnalyzers(sat, analyzerEntry{name: "queue-depth", analyzer: ext})}
			cfg := config.ScalingPolicy{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70,
				Analyzers: []config.AnalyzerScoreConfig{{Name: domain.SaturationAnalyzerName}, {Name: "queue-depth"}}}
			states := []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: 3}}
			cycle := func() *allocation.ModelScalingRequest {
				t.Helper()
				req, err := e.collectV2ModelRequest(context.Background(), "m", "ns", nil, cfg, states, nil, nil, nil, nil, 0)
				require.NoError(t, err)
				return req
			}

			src.values[query] = []float64{0}
			require.Positive(t, cycle().CompositeSignal.SpareCapacity, "setup: both analyzers read the model as idle")

			src.values[query] = nil // the query now returns no series
			req := cycle()
			if !tc.emptyIsMissing {
				assert.Nil(t, req.CompositeSignal.Holds, "off: no series is 0 demand, a reading")
				assert.Positive(t, req.CompositeSignal.SpareCapacity, "off: the model shrinks")
				return
			}
			assert.Equal(t, map[string]allocation.RoleHold{domain.RoleBoth: {NoScaleDown: true}}, req.CompositeSignal.Holds,
				"on: no series is missing, not 0, so the model does not shrink")
			assert.Zero(t, req.CompositeSignal.SpareCapacity)

			e.lastGoodAnalysis[utils.GetNamespacedKey("ns", "m")]["queue-depth"] = time.Now().Add(-time.Hour)
			req = cycle()
			assert.Nil(t, req.CompositeSignal.Holds, "out of its grace, it no longer counts")
			assert.Positive(t, req.CompositeSignal.SpareCapacity)
		})
	}
}
