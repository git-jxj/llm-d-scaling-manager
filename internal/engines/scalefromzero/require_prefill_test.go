package scalefromzero

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// A per-model requirePrefill lives in an entry under an ARBITRARY key and is
// identified by the model_id and namespace in its body -- a ConfigMap key cannot
// contain "#" or "/". It used to be looked up under "{modelID}#{namespace}", so
// only the default entry ever applied.
func TestRequirePrefillResolvesPerModelAndTierEntries(t *testing.T) {
	const (
		modelID   = "org/model"
		namespace = "tenant"
	)
	on, off := true, false
	group := func(tiers ...string) modelGroup {
		g := modelGroup{namespace: namespace, modelID: modelID}
		for _, tier := range tiers {
			va := llmdvariant.Variant{}
			va.Spec.ScalingPolicy = tier
			g.variants = append(g.variants, va)
		}
		return g
	}

	cases := []struct {
		name    string
		entries map[string]config.ScalingPolicy
		group   modelGroup
		want    bool
	}{
		{
			name: "override under an arbitrary key turns it on",
			entries: map[string]config.ScalingPolicy{
				"default": {},
				"my-model-override": {
					ModelID: modelID, Namespace: namespace,
					ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on},
				},
			},
			group: group(""),
			want:  true,
		},
		{
			name: "override under an arbitrary key turns the default off",
			entries: map[string]config.ScalingPolicy{
				"default": {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on}},
				"pd-model": {
					ModelID: modelID, Namespace: namespace,
					ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &off},
				},
			},
			group: group(""),
			want:  false,
		},
		{
			name: "the selected tier turns it on",
			entries: map[string]config.ScalingPolicy{
				"default":     {},
				"interactive": {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on}},
			},
			group: group("interactive"),
			want:  true,
		},
		{
			// Any one tier requiring prefill is enough, wherever it sorts.
			name: "the second of two tiers turns it on",
			entries: map[string]config.ScalingPolicy{
				"default": {},
				"a":       {},
				"b":       {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on}},
			},
			group: group("a", "b"),
			want:  true,
		},
		{
			name: "the second of two tiers turns it on, variants in reverse order",
			entries: map[string]config.ScalingPolicy{
				"default": {},
				"a":       {},
				"b":       {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on}},
			},
			group: group("b", "a"),
			want:  true,
		},
		{
			name: "no tier requires it",
			entries: map[string]config.ScalingPolicy{
				"default": {},
				"a":       {},
				"b":       {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &off}},
			},
			group: group("a", "b"),
			want:  false,
		},
		{
			// An untagged variant follows the tagged one, as the thresholds do:
			// the default entry is not folded in beside a named tier.
			name: "an untagged variant follows the tagged tier, not the default",
			entries: map[string]config.ScalingPolicy{
				"default": {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on}},
				"pd-gpu":  {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &off}},
			},
			group: group("pd-gpu", ""),
			want:  false,
		},
		{
			name: "another model's override does not apply",
			entries: map[string]config.ScalingPolicy{
				"default": {},
				"other": {
					ModelID: "org/other", Namespace: namespace,
					ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on},
				},
			},
			group: group(""),
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewTestConfig()
			cfg.UpdateScalingPolicyConfigForNamespace(namespace, tc.entries)
			e := &Engine{config: cfg}
			if got := e.requirePrefill(tc.group); got != tc.want {
				t.Fatalf("requirePrefill = %v, want %v", got, tc.want)
			}
		})
	}
}

// A DisaggregatedSet model wakes prefill and decode together by default; its
// policy can still say requirePrefill: false, and any other model keeps the
// decode-only default.
func TestRequirePrefillDefaultsOnForDisaggregatedSet(t *testing.T) {
	const namespace = "tenant"
	off := false
	group := func(kind string) modelGroup {
		va := llmdvariant.Variant{}
		va.Spec.ScaleTargetRef.Kind = kind
		return modelGroup{namespace: namespace, modelID: "org/model", variants: []llmdvariant.Variant{va}}
	}
	for _, tc := range []struct {
		name    string
		entries map[string]config.ScalingPolicy
		kind    string
		want    bool
	}{
		{name: "a DisaggregatedSet role, no setting", entries: map[string]config.ScalingPolicy{"default": {}},
			kind: constants.DisaggregatedSetRoleScalerKind, want: true},
		{name: "a DisaggregatedSet role, explicitly off",
			entries: map[string]config.ScalingPolicy{"default": {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &off}}},
			kind:    constants.DisaggregatedSetRoleScalerKind, want: false},
		{name: "a Deployment, no setting", entries: map[string]config.ScalingPolicy{"default": {}},
			kind: constants.DeploymentKind, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewTestConfig()
			cfg.UpdateScalingPolicyConfigForNamespace(namespace, tc.entries)
			if got := (&Engine{config: cfg}).requirePrefill(group(tc.kind)); got != tc.want {
				t.Fatalf("requirePrefill = %v, want %v", got, tc.want)
			}
		})
	}
	if !(&Engine{}).requirePrefill(group(constants.DisaggregatedSetRoleScalerKind)) {
		t.Fatal("with no config at all, a DisaggregatedSet model must still require prefill")
	}
}

// Mixed tiers and mixed kinds: a DisaggregatedSet model whose roles select
// different tiers requires prefill when any tier says so; one tier that says
// false and none that says true turns the DisaggregatedSet default off; a
// model with a Deployment variant beside a role still counts as served by a
// set.
func TestRequirePrefillMixedTiersAndKinds(t *testing.T) {
	const namespace = "tenant"
	on, off := true, false
	variant := func(kind, tier string) llmdvariant.Variant {
		va := llmdvariant.Variant{}
		va.Spec.ScaleTargetRef.Kind = kind
		va.Spec.ScalingPolicy = tier
		return va
	}
	role := constants.DisaggregatedSetRoleScalerKind
	entries := map[string]config.ScalingPolicy{
		"default": {},
		"off":     {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &off}},
		"on":      {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on}},
		"plain":   {},
	}
	for _, tc := range []struct {
		name     string
		variants []llmdvariant.Variant
		want     bool
	}{
		{"one tier off, one on", []llmdvariant.Variant{variant(role, "off"), variant(role, "on")}, true},
		{"one tier off, one silent", []llmdvariant.Variant{variant(role, "off"), variant(role, "plain")}, false},
		{"both silent", []llmdvariant.Variant{variant(role, "plain"), variant(role, "plain")}, true},
		{"a Deployment beside a role", []llmdvariant.Variant{variant(constants.DeploymentKind, ""), variant(role, "")}, true},
		{"Deployments only", []llmdvariant.Variant{variant(constants.DeploymentKind, ""), variant(constants.DeploymentKind, "")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewTestConfig()
			cfg.UpdateScalingPolicyConfigForNamespace(namespace, entries)
			g := modelGroup{namespace: namespace, modelID: "org/model", variants: tc.variants}
			if got := (&Engine{config: cfg}).requirePrefill(g); got != tc.want {
				t.Fatalf("requirePrefill = %v, want %v", got, tc.want)
			}
		})
	}
}
