package config

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// ExternalAnalyzerBody is one inference engine's query and per-replica target in
// a catalog definition. The field names mirror KEDA's Prometheus scaler
// (threshold is a string, e.g. "0.5").
//
// query must resolve to the model's TOTAL demand (every series it returns is
// summed) and threshold is what ONE scale-target replica — a pod, or an LWS
// group — can serve, not one engine instance. A sum-shaped query is safe at any
// granularity; count() and avg() are not, since they measure in DP ranks. See
// external.Body for the full note.
//
// Demand, Target, TargetReduce and EmptyIsMissing are the rule form
// (ExternalAnalyzerRules); a body that sets none of them is read exactly as
// before they existed.
type ExternalAnalyzerBody struct {
	Query     string `yaml:"query"`
	Threshold string `yaml:"threshold"`

	ExternalAnalyzerRules `yaml:",inline" learnrec:"omitzero"`
}

// ExternalAnalyzerDef is a catalog entry for one external analyzer. It carries a
// per-engine body keyed by engine ("vllm", "sglang"), or an engine-agnostic body
// given by the top-level fields when Engines is empty.
type ExternalAnalyzerDef struct {
	Engines   map[string]ExternalAnalyzerBody `yaml:"engines,omitempty"`
	Query     string                          `yaml:"query,omitempty"`
	Threshold string                          `yaml:"threshold,omitempty"`

	ExternalAnalyzerRules `yaml:",inline" learnrec:"omitzero"`
}

// ExternalAnalyzerRules are the settings of an external analyzer body beyond
// one model-level query and a constant threshold. Each is opt-in: left unset,
// the body behaves as it did before they existed. Every field is left out of a
// learner recording while unset, so a recording made before them reads back
// byte for byte.
type ExternalAnalyzerRules struct {
	// Demand is one demand query per role, in place of Query: what sizes a
	// P/D model, whose prefill and decode replicas measure different things.
	// A role no rule names is not sized by this analyzer.
	Demand []ExternalDemandRule `yaml:"demand,omitempty" learnrec:"omitzero"`
	// Target measures the per-replica target P per Pod instead of taking the
	// constant threshold. A threshold, when set, is used for a variant no
	// target query answers for, such as one with no Pod running.
	Target []ExternalTargetRule `yaml:"target,omitempty" learnrec:"omitzero"`
	// TargetReduce is how a variant's Pods' targets become one value: avg
	// (the default), median, min or max.
	TargetReduce string `yaml:"targetReduce,omitempty" learnrec:"omitzero"`
	// EmptyIsMissing reads a demand query that returns no series as missing
	// rather than as 0 demand: the analyzer reports nothing that cycle, as
	// when the query fails. Off by default, which is how an empty result was
	// always read.
	EmptyIsMissing bool `yaml:"emptyIsMissing,omitempty" learnrec:"omitzero"`
}

// ExternalDemandRule is the demand query for one role.
type ExternalDemandRule struct {
	Match ExternalRuleMatch `yaml:"match"`
	Query string            `yaml:"query"`
	// Threshold is this role's constant per-replica target, overriding the
	// body's. Optional.
	Threshold string `yaml:"threshold,omitempty" learnrec:"omitzero"`
	// OrZero reads an empty result for this role as 0 demand even when the
	// body sets EmptyIsMissing: for a signal that has no series at zero load.
	OrZero bool `yaml:"orZero,omitempty" learnrec:"omitzero"`
}

// ExternalTargetRule measures the per-replica target of the variants it
// matches: the first query in Queries that returns a usable value for a
// variant's Pods sets that variant's target.
type ExternalTargetRule struct {
	// Match selects the role. An empty match serves every role no other rule
	// names.
	Match   ExternalRuleMatch     `yaml:"match,omitempty" learnrec:"omitzero"`
	Queries []ExternalTargetQuery `yaml:"queries"`
}

// ExternalTargetQuery is one per-Pod target query and the token (E) logged
// when it is the one that answered.
type ExternalTargetQuery struct {
	Query string `yaml:"query"`
	E     string `yaml:"e,omitempty" learnrec:"omitzero"`
}

// ExternalRuleMatch selects which of a model's variants a rule serves, by the
// role the scaling manager resolved for the variant (its llm-d.ai/role
// label), not by a metric label.
type ExternalRuleMatch struct {
	Role string `yaml:"role,omitempty" learnrec:"omitzero"`
}

// The per-Pod target reductions an ExternalAnalyzerRules.TargetReduce may name.
const (
	TargetReduceAvg    = "avg"
	TargetReduceMedian = "median"
	TargetReduceMin    = "min"
	TargetReduceMax    = "max"
)

// externalRuleRoles are the roles a rule's match may name.
var externalRuleRoles = []string{domain.RolePrefill, domain.RoleDecode, domain.RoleBoth}

// IsSet reports whether any rule-form setting is set.
func (r ExternalAnalyzerRules) IsSet() bool {
	return len(r.Demand) > 0 || len(r.Target) > 0 || r.TargetReduce != "" || r.EmptyIsMissing
}

// UsesRules reports whether d sets a rule-form setting anywhere: at the top
// level or in an engine body. Only such a definition is validated when the
// ConfigMap is read (ValidateRules); one that sets none is checked as before,
// when the engine builds it.
func (d ExternalAnalyzerDef) UsesRules() bool {
	if d.IsSet() {
		return true
	}
	for _, b := range d.Engines {
		if b.IsSet() {
			return true
		}
	}
	return false
}

// ValidateRules checks a definition that uses the rule form (UsesRules) and
// says what is wrong with it, naming the field. Every body is checked as it
// will run: its query or demand rules, the thresholds they need, and its
// target rules.
func (d ExternalAnalyzerDef) ValidateRules() error {
	if len(d.Engines) == 0 {
		return validateExternalBody(d.Query, d.Threshold, d.ExternalAnalyzerRules)
	}
	if d.Query != "" || d.Threshold != "" || d.IsSet() {
		return errors.New("engines is set, so the top-level query, threshold, demand, target, " +
			"targetReduce and emptyIsMissing are not used: move them into each engine body")
	}
	for _, engine := range slices.Sorted(maps.Keys(d.Engines)) {
		b := d.Engines[engine]
		if err := validateExternalBody(b.Query, b.Threshold, b.ExternalAnalyzerRules); err != nil {
			return fmt.Errorf("engines.%s: %w", engine, err)
		}
	}
	return nil
}

// validateExternalBody checks one body: query and demand are alternatives,
// each demand rule names a known role once, and every role it sizes has a
// threshold unless a target rule can measure one.
func validateExternalBody(query, threshold string, r ExternalAnalyzerRules) error {
	switch {
	case query != "" && len(r.Demand) > 0:
		return errors.New("set query (one demand for the whole model) or demand (one per role), not both")
	case query == "" && len(r.Demand) == 0:
		return errors.New("query or demand is required")
	}
	if threshold != "" {
		if err := checkThreshold(threshold); err != nil {
			return fmt.Errorf("threshold %q: %w", threshold, err)
		}
	}
	// served reports whether a target rule measures role: one that names it,
	// or one with no match.
	served := func(role string) bool {
		return slices.ContainsFunc(r.Target, func(t ExternalTargetRule) bool {
			return t.Match.Role == role || t.Match.Role == ""
		})
	}
	seen := map[string]bool{}
	for i, rule := range r.Demand {
		at := fmt.Sprintf("demand[%d]", i)
		if err := checkRole(rule.Match.Role, false); err != nil {
			return fmt.Errorf("%s.match.role: %w", at, err)
		}
		if seen[rule.Match.Role] {
			return fmt.Errorf("%s.match.role: role %q has another demand rule", at, rule.Match.Role)
		}
		seen[rule.Match.Role] = true
		if rule.Query == "" {
			return fmt.Errorf("%s.query is required", at)
		}
		if rule.Threshold != "" {
			if err := checkThreshold(rule.Threshold); err != nil {
				return fmt.Errorf("%s.threshold %q: %w", at, rule.Threshold, err)
			}
		} else if threshold == "" && !served(rule.Match.Role) {
			return fmt.Errorf("%s: no threshold for role %q: set threshold on the rule or the body, or add a target rule for the role", at, rule.Match.Role)
		}
	}
	// A model-level query serves every role, so every role needs a target.
	everyRoleServed := served(domain.RolePrefill) && served(domain.RoleDecode) && served(domain.RoleBoth)
	if query != "" && threshold == "" && !everyRoleServed {
		return errors.New("threshold is required: set it, or add a target rule with no match")
	}
	seenTarget := map[string]bool{}
	for i, rule := range r.Target {
		at := fmt.Sprintf("target[%d]", i)
		if err := checkRole(rule.Match.Role, true); err != nil {
			return fmt.Errorf("%s.match.role: %w", at, err)
		}
		if seenTarget[rule.Match.Role] {
			if rule.Match.Role == "" {
				return fmt.Errorf("%s: only one target rule may leave match out", at)
			}
			return fmt.Errorf("%s.match.role: role %q has another target rule", at, rule.Match.Role)
		}
		seenTarget[rule.Match.Role] = true
		if len(rule.Queries) == 0 {
			return fmt.Errorf("%s.queries: at least one query is required", at)
		}
		for j, q := range rule.Queries {
			if q.Query == "" {
				return fmt.Errorf("%s.queries[%d].query is required", at, j)
			}
		}
	}
	switch r.TargetReduce {
	case "", TargetReduceAvg, TargetReduceMedian, TargetReduceMin, TargetReduceMax:
	default:
		return fmt.Errorf("targetReduce %q: must be one of avg, median, min, max", r.TargetReduce)
	}
	if r.TargetReduce != "" && len(r.Target) == 0 {
		return errors.New("targetReduce is set but there is no target to reduce")
	}
	return nil
}

// checkRole accepts one of externalRuleRoles, and an empty role when optional.
func checkRole(role string, optional bool) error {
	if role == "" {
		if optional {
			return nil
		}
		return errors.New("is required: one of prefill, decode, both")
	}
	if !slices.Contains(externalRuleRoles, role) {
		return fmt.Errorf("%q is not a role: one of prefill, decode, both", role)
	}
	return nil
}

// checkThreshold accepts a finite number greater than 0.
func checkThreshold(s string) error {
	v, err := strconv.ParseFloat(s, 64)
	if errors.Is(err, strconv.ErrRange) {
		return errors.New("out of range")
	}
	if err != nil {
		return errors.New("not a number")
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return errors.New("must be a finite number greater than 0")
	}
	return nil
}

// ExternalAnalyzerCatalog maps an analyzer label to its definition.
type ExternalAnalyzerCatalog map[string]ExternalAnalyzerDef

// ExternalAnalyzerCatalog returns a copy of the external analyzers declared on the
// cluster "default" scaling entry.
//
// It used to come from a ConfigMap of its own (scaling-manager-analyzers). Folding it onto the
// scaling-policy surface leaves one place to look for "how is scaling configured",
// and puts definitions in the same object as the tiers that select them — related
// settings that were previously two ConfigMaps apart, with nothing tying a
// policy's analyzer name to the definition it resolves against.
//
// Cluster-scoped, like limiters: read from the SYSTEM namespace's ConfigMap only.
// A workload namespace declaring analyzers is reported and ignored — see
// parseScalingPolicyConfig. Note this still lets a per-namespace scaling manager install define
// its own, since that install's own namespace IS its system namespace.
func (c *Config) ExternalAnalyzerCatalog() ExternalAnalyzerCatalog {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return copyAnalyzerCatalog(c.saturation.global[GlobalDefaultsKey].AnalyzerDefinitions)
}

// copyAnalyzerCatalog deep-copies a catalog (including each def's Engines map
// and rule lists) so stored and returned catalogs cannot be mutated through a
// shared reference.
func copyAnalyzerCatalog(src ExternalAnalyzerCatalog) ExternalAnalyzerCatalog {
	out := make(ExternalAnalyzerCatalog, len(src))
	for label, def := range src {
		cp := ExternalAnalyzerDef{Query: def.Query, Threshold: def.Threshold, ExternalAnalyzerRules: def.copyRules()}
		if def.Engines != nil {
			cp.Engines = make(map[string]ExternalAnalyzerBody, len(def.Engines))
			for engine, body := range def.Engines {
				body.ExternalAnalyzerRules = body.copyRules()
				cp.Engines[engine] = body
			}
		}
		out[label] = cp
	}
	return out
}

// copyRules deep-copies the rule lists.
func (r ExternalAnalyzerRules) copyRules() ExternalAnalyzerRules {
	out := r
	out.Demand = slices.Clone(r.Demand)
	if r.Target != nil {
		out.Target = make([]ExternalTargetRule, len(r.Target))
		for i, t := range r.Target {
			t.Queries = slices.Clone(t.Queries)
			out.Target[i] = t
		}
	}
	return out
}

// resetInvalidAnalyzerDefinitions removes each definition that uses the rule
// form (ExternalAnalyzerDef.UsesRules) and fails ValidateRules, and returns one
// IgnoredField per definition removed, naming it and saying why. The other
// definitions, and the rest of the entry, still apply: rejecting the entry
// would drop the default entry's limiters with it. A definition without the
// rule form is left as it is: the engine checks it when it builds it, as it
// always has.
func (c *ScalingPolicy) resetInvalidAnalyzerDefinitions() []IgnoredField {
	var ignored []IgnoredField
	for _, label := range slices.Sorted(maps.Keys(c.AnalyzerDefinitions)) {
		def := c.AnalyzerDefinitions[label]
		if !def.UsesRules() {
			continue
		}
		if err := def.ValidateRules(); err != nil {
			ignored = append(ignored, IgnoredField{
				Field:  "analyzerDefinitions",
				Value:  TruncateIgnoredValue(label),
				Reason: fmt.Errorf("this analyzer is not defined: %w", err),
			})
			delete(c.AnalyzerDefinitions, label)
		}
	}
	return ignored
}
