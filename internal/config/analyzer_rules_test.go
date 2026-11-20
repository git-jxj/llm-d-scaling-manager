package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// readDefault decodes a "default" entry and clears its unusable fields, as the
// ConfigMap read does (parseScalingPolicyConfigRejects).
func readDefault(t *testing.T, entry string) (ScalingPolicy, []IgnoredField) {
	t.Helper()
	p, err := DecodeScalingPolicy(entry)
	require.NoError(t, err)
	ignored := p.ResetUnusableFields()
	require.NoError(t, p.ValidateEntry(), "an invalid definition never fails the entry")
	return p, ignored
}

func TestExternalAnalyzerRulesAreRead(t *testing.T) {
	p, ignored := readDefault(t, `
analyzerDefinitions:
  queue-depth:
    threshold: "5"
    emptyIsMissing: true
    demand:
      - match: { role: prefill }
        query: sum(prefill_queue)
        threshold: "3"
        orZero: true
      - match: { role: decode }
        query: sum(decode_queue)
    target:
      - match: { role: decode }
        queries:
          - { query: avg by (pod) (kv), e: epp }
          - { query: avg by (pod) (kv2) }
    targetReduce: median
`)
	assert.Empty(t, ignored)
	def := p.AnalyzerDefinitions["queue-depth"]
	assert.True(t, def.UsesRules())
	assert.True(t, def.EmptyIsMissing)
	assert.Equal(t, "median", def.TargetReduce)
	require.Len(t, def.Demand, 2)
	assert.Equal(t, ExternalDemandRule{Match: ExternalRuleMatch{Role: "prefill"}, Query: "sum(prefill_queue)", Threshold: "3", OrZero: true}, def.Demand[0])
	require.Len(t, def.Target, 1)
	assert.Equal(t, "decode", def.Target[0].Match.Role)
	assert.Equal(t, []ExternalTargetQuery{{Query: "avg by (pod) (kv)", E: "epp"}, {Query: "avg by (pod) (kv2)"}}, def.Target[0].Queries)
}

// A definition written before the rule form existed is read exactly as
// before: not validated here, so even a broken one stays in the catalog for
// the engine to skip and log, as it always has.
func TestExternalAnalyzerWithoutRulesIsNotValidatedOnRead(t *testing.T) {
	p, ignored := readDefault(t, `
analyzerDefinitions:
  old:
    query: sum(q)
    threshold: "not-a-number"
`)
	assert.Empty(t, ignored)
	assert.Contains(t, p.AnalyzerDefinitions, "old")
	assert.False(t, p.AnalyzerDefinitions["old"].UsesRules())
}

// A definition in the rule form that cannot run is removed when the ConfigMap
// is read, and the warning names the analyzer and the field. The other
// definitions and the rest of the entry still apply.
func TestInvalidExternalAnalyzerRulesAreRefusedOnRead(t *testing.T) {
	for _, tc := range []struct {
		name, def, want string
	}{
		{"query and demand", `
    query: sum(q)
    threshold: "1"
    demand: [{match: {role: decode}, query: sum(d)}]`, "set query (one demand for the whole model) or demand (one per role), not both"},
		{"no role", `
    threshold: "1"
    demand: [{query: sum(d)}]`, "demand[0].match.role: is required: one of prefill, decode, both"},
		{"unknown role", `
    threshold: "1"
    demand: [{match: {role: prefil}, query: sum(d)}]`, `demand[0].match.role: "prefil" is not a role`},
		{"a role twice", `
    threshold: "1"
    demand: [{match: {role: decode}, query: a}, {match: {role: decode}, query: b}]`, `demand[1].match.role: role "decode" has another demand rule`},
		{"no query", `
    threshold: "1"
    demand: [{match: {role: decode}}]`, "demand[0].query is required"},
		{"no threshold", `
    demand: [{match: {role: decode}, query: sum(d)}]`, `demand[0]: no threshold for role "decode"`},
		{"bad rule threshold", `
    demand: [{match: {role: decode}, query: sum(d), threshold: "0"}]`, `demand[0].threshold "0": must be a finite number greater than 0`},
		{"NaN body threshold", `
    threshold: "NaN"
    demand: [{match: {role: decode}, query: sum(d)}]`, `threshold "NaN": must be a finite number greater than 0`},
		{"bad reduce", `
    query: sum(q)
    threshold: "1"
    target: [{queries: [{query: t}]}]
    targetReduce: p99`, `targetReduce "p99": must be one of avg, median, min, max`},
		{"reduce with no target", `
    query: sum(q)
    threshold: "1"
    targetReduce: max`, "targetReduce is set but there is no target to reduce"},
		{"target with no queries", `
    query: sum(q)
    target: [{match: {role: decode}}]`, "target[0].queries: at least one query is required"},
		{"two catch-all targets", `
    query: sum(q)
    target: [{queries: [{query: a}]}, {queries: [{query: b}]}]`, "target[1]: only one target rule may leave match out"},
		{"top level beside engines", `
    emptyIsMissing: true
    engines:
      vllm: {query: sum(q), threshold: "1"}`, "engines is set, so the top-level"},
		{"bad engine body", `
    engines:
      vllm:
        demand: [{match: {role: decode}, query: sum(d)}]`, `engines.vllm: demand[0]: no threshold for role "decode"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ignored := readDefault(t, `
kvCacheThreshold: 0.8
analyzerDefinitions:
  good:
    query: sum(q)
    threshold: "1"
  bad:`+tc.def+`
`)
			require.Len(t, ignored, 1)
			assert.Equal(t, "analyzerDefinitions", ignored[0].Field)
			assert.Equal(t, "bad", ignored[0].Value)
			assert.ErrorContains(t, ignored[0].Reason, "this analyzer is not defined: "+tc.want)
			assert.NotContains(t, p.AnalyzerDefinitions, "bad")
			assert.Contains(t, p.AnalyzerDefinitions, "good", "the other definitions still apply")
			assert.Equal(t, 0.8, p.KvCacheThreshold, "the rest of the entry still applies")
		})
	}
}

// The rule lists are copied too: a consumer changing what it was handed must
// not change the queries the scaling manager runs.
func TestExternalAnalyzerCatalogCopiesRules(t *testing.T) {
	c := &Config{}
	c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{
		GlobalDefaultsKey: {AnalyzerDefinitions: ExternalAnalyzerCatalog{
			"a": {ExternalAnalyzerRules: ExternalAnalyzerRules{
				Demand: []ExternalDemandRule{{Match: ExternalRuleMatch{Role: "decode"}, Query: "original"}},
				Target: []ExternalTargetRule{{Queries: []ExternalTargetQuery{{Query: "original"}}}},
			}},
			"b": {Engines: map[string]ExternalAnalyzerBody{"vllm": {ExternalAnalyzerRules: ExternalAnalyzerRules{
				Demand: []ExternalDemandRule{{Query: "original"}},
			}}}},
		}},
	})

	got := c.ExternalAnalyzerCatalog()
	got["a"].Demand[0].Query = "mutated"
	got["a"].Target[0].Queries[0].Query = "mutated"
	got["b"].Engines["vllm"].Demand[0].Query = "mutated"

	fresh := c.ExternalAnalyzerCatalog()
	assert.Equal(t, "original", fresh["a"].Demand[0].Query)
	assert.Equal(t, "original", fresh["a"].Target[0].Queries[0].Query)
	assert.Equal(t, "original", fresh["b"].Engines["vllm"].Demand[0].Query)
}

// The sample's definitions all run: none is dropped on read, and its P/D one
// uses the rule form.
func TestExternalAnalyzerSampleIsValid(t *testing.T) {
	raw, err := os.ReadFile("../../config/samples/analyzers/external-analyzers.yaml")
	require.NoError(t, err)
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &cm))
	assert.Empty(t, UnknownScalingPolicyKeys(cm.Data[GlobalDefaultsKey]))
	p, ignored := readDefault(t, cm.Data[GlobalDefaultsKey])
	assert.Empty(t, ignored)
	require.Contains(t, p.AnalyzerDefinitions, "queue-depth")
	assert.True(t, p.AnalyzerDefinitions["queue-depth"].UsesRules())
	assert.Len(t, p.AnalyzerDefinitions, 3)
}
