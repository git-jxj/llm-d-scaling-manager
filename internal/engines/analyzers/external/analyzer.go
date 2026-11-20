// Package external implements a built-in analyzer that turns a config-driven
// PromQL definition into a domain.Analyzer, so the scaling manager can be extended with new
// scaling signals without writing Go (issue llm-d/llm-d-workload-variant-autoscaler#1455).
//
// An external analyzer mirrors KEDA scaler vocabulary: a demand query (the total
// signal D) and a per-replica target (threshold P), such that desired replicas =
// ceil(D / P). Like the collector's registerForEngine, the query body is
// per-engine: a Definition carries one Body per inference engine (vllm, sglang),
// plus an optional engine-agnostic body; Analyze selects the body matching the
// model's engine. It runs the selected query each cycle via the shared metrics
// source and reduces the result to D; P is the body's constant threshold.
//
// A body may instead give one demand query per role (DemandRule), which is
// what sizes a P/D model, and may measure P per Pod (TargetRule) rather than
// take the constant.
//
// Per-variant identity (cost, accelerator) is filled downstream by the capacity
// builder from the discovery step, so the wrapper emits only the measured (D, P)
// signal — the same contract the internal analyzers follow.
package external

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"sync"

	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
)

// queryNamePrefix namespaces external-analyzer queries in the shared QueryList so
// they cannot collide with the built-in template names.
const queryNamePrefix = "external:"

// reasonConstantTarget labels a per-replica capacity that came from a constant
// threshold rather than a measured query.
const reasonConstantTarget = "E1-const"

// reasonMeasuredTarget labels a per-replica capacity a target query measured.
// Which query answered (its E token) is logged, not carried here: the token
// is a word the operator chose.
const reasonMeasuredTarget = "E2-query"

// engineAgnostic is the Bodies key for a body that applies to any engine. It is
// used when no engine-specific body matches the model's engine.
const engineAgnostic = ""

// The per-Pod target reductions a Body's TargetReduce may name. Empty is
// ReduceAvg.
const (
	ReduceAvg    = "avg"
	ReduceMedian = "median"
	ReduceMin    = "min"
	ReduceMax    = "max"
)

// ErrEmptyResult is returned, wrapped, when a demand query returned no series
// and its body reads an empty result as missing (Body.EmptyIsMissing). The
// engine drops a result that errors, so the analyzer reports nothing for the
// model this cycle, exactly as when the query fails.
var ErrEmptyResult = errors.New("the query returned no series, which this analyzer reads as missing (emptyIsMissing)")

// Body is one engine's query and per-replica target for an external analyzer.
// The field names mirror KEDA's Prometheus scaler.
//
// # Units
//
// D and P must agree, and that agreement is the operator's to get right: it
// cannot be checked here, since PromQL semantics are not inspectable.
//
// Query returns D, the model's TOTAL demand — every series it returns is summed
// into one number. Threshold is P, what ONE scale-target replica (a pod, or an
// LWS group) can serve. desired = ceil(D / P), so P must be the whole pod's
// figure and not one engine instance's: a data-parallel pod runs several.
//
// A sum-shaped query needs nothing further, whatever granularity the underlying
// series have — summing across a pod's DP ranks and summing across pods give
// the same total. What breaks is a query whose value depends on how many series
// exist: count() counts DP ranks and avg() averages over them, so both measure
// in instances while P is in pods. Collapse to pod granularity first:
//
//	count(count by (pod) (...))
//	avg(avg by (pod) (...))
//
// The built-in analyzers get this for free: their per-replica figures come from
// domain.ReplicaMetrics, which the collector has already merged to one record
// per pod (collector.collapseToPods). Query runs straight against the metrics
// source and never passes through that merge.
type Body struct {
	// Query is a PromQL expression for the total demand D. It may reference
	// {{.namespace}} and {{.modelID}}; the metrics source escapes both before
	// substitution. All returned series are summed, so it must resolve to the
	// model's total demand and not to a per-replica breakdown of it.
	// Exactly one of Query and Demand is set.
	Query string
	// Threshold is the per-replica target P — the demand a single scale-target
	// replica can serve (see the Units note above). desired = ceil(D / threshold).
	// Must be > 0, or 0 (none) when Target or every Demand rule supplies P.
	Threshold float64

	// Demand is one demand query per role, in place of Query. The model's
	// demand for a role is its rule's summed result (RoleDemand on a P/D
	// model); a role no rule names is not defined for this analyzer, so its
	// variants are left out of the result.
	Demand []DemandRule
	// Target measures P per Pod. A variant no target query answers for falls
	// back to its role's threshold, then Threshold; with neither it is left
	// out of the result this cycle.
	Target []TargetRule
	// TargetReduce reduces a variant's Pods' targets to one value: ReduceAvg
	// (the default when empty), ReduceMedian, ReduceMin or ReduceMax.
	TargetReduce string
	// EmptyIsMissing makes a demand query that returns no series an error
	// (ErrEmptyResult) instead of 0 demand. A DemandRule's OrZero opts its
	// role back into 0.
	EmptyIsMissing bool
}

// DemandRule is the demand query for one role.
type DemandRule struct {
	// Role is the role the rule serves: domain.RolePrefill, RoleDecode or
	// RoleBoth.
	Role string
	// Query is the role's total demand, summed like Body.Query.
	Query string
	// Threshold is the role's constant P, overriding Body.Threshold; 0 for
	// none.
	Threshold float64
	// OrZero reads an empty result as 0 demand even under EmptyIsMissing.
	OrZero bool
}

// TargetRule measures P for the variants of one role, or of every role no
// other rule names when Role is empty.
type TargetRule struct {
	Role string
	// Queries are tried in order for each variant; the first that returns a
	// usable value for any of the variant's Pods sets its P.
	Queries []TargetQuery
}

// TargetQuery is a per-Pod target query. Each series must keep a pod (or
// pod_name) label: that is how its value is joined to a variant. E is the
// token logged when this query answered.
type TargetQuery struct {
	Query string
	E     string
}

// Definition describes a config-driven external analyzer with per-engine query
// bodies. A body under the engineAgnostic ("") key applies to any engine.
type Definition struct {
	// Label is the analyzer's unique name, used for policy selection and metrics.
	Label string
	// Bodies maps an inference engine ("vllm", "sglang") to its query body; the
	// empty-string key is the engine-agnostic body. At least one is required.
	Bodies map[string]Body
}

// Analyzer is the built-in wrapper that runs a Definition as a domain.Analyzer.
// SeriesGate rejects a series whose Pod is gone, so an external analyzer counts
// the same Pods the collector's builder would.
type SeriesGate func(ctx context.Context, namespace string, labels map[string]string) bool

type Analyzer struct {
	def    Definition
	source source.MetricsSource
	// queryNames maps an engine key to the query names registered for its
	// body.
	queryNames map[string]bodyQueries

	// DropSeries applies the collector's Pod gate to this analyzer's own query
	// results, so a deleted Pod stops contributing demand here as it does there.
	//
	// A field rather than a constructor argument because it is optional and
	// layered: the engine wires the collector's gate in after construction, and
	// an analyzer built without one sums everything, which is what it did before.
	DropSeries SeriesGate

	// clock stamps AnalyzedAt: the engine's cycle clock (WithClock), the wall
	// clock by default.
	clock clock.PassiveClock
}

// bodyQueries are the registered query names of one body: the demand query,
// one per demand rule, and one per target query of each target rule.
type bodyQueries struct {
	demand string
	rules  []string
	target [][]string
}

// New constructs an external analyzer from def and registers each body's query
// into src's shared QueryList. Registration uses Upsert, so re-initialization
// (e.g. a catalog reload) is safe.
func New(def Definition, src source.MetricsSource) (*Analyzer, error) {
	if def.Label == "" {
		return nil, errors.New("external analyzer: label is required")
	}
	if len(def.Bodies) == 0 {
		return nil, fmt.Errorf("external analyzer %q: at least one query body is required", def.Label)
	}
	if src == nil {
		return nil, fmt.Errorf("external analyzer %q: metrics source is required", def.Label)
	}

	queryNames := make(map[string]bodyQueries, len(def.Bodies))
	for _, engineKey := range slices.Sorted(maps.Keys(def.Bodies)) {
		body := def.Bodies[engineKey]
		if err := validateBody(body); err != nil {
			return nil, fmt.Errorf("external analyzer %q (%s): %w", def.Label, engineLabel(engineKey), err)
		}
		register := func(name, template, what string) error {
			if err := src.QueryList().Upsert(source.QueryTemplate{
				Name:        name,
				Type:        source.QueryTypePromQL,
				Template:    template,
				Params:      []string{source.ParamNamespace, source.ParamModelID},
				Description: fmt.Sprintf("%s for external analyzer %q (%s)", what, def.Label, engineLabel(engineKey)),
			}); err != nil {
				return fmt.Errorf("external analyzer %q (%s): registering %s query: %w", def.Label, engineLabel(engineKey), what, err)
			}
			return nil
		}
		var names bodyQueries
		if body.Query != "" {
			names.demand = queryName(def.Label, engineKey, "demand")
			if err := register(names.demand, body.Query, "demand (D)"); err != nil {
				return nil, err
			}
		}
		for _, rule := range body.Demand {
			name := queryName(def.Label, engineKey, "demand@"+rule.Role)
			if err := register(name, rule.Query, "demand (D) of role "+rule.Role); err != nil {
				return nil, err
			}
			names.rules = append(names.rules, name)
		}
		for i, rule := range body.Target {
			var qs []string
			for j, q := range rule.Queries {
				name := queryName(def.Label, engineKey, fmt.Sprintf("target@%s:%d", roleOrAny(rule.Role), j))
				if err := register(name, q.Query, fmt.Sprintf("per-Pod target (P) rule %d query %d", i, j)); err != nil {
					return nil, err
				}
				qs = append(qs, name)
			}
			names.target = append(names.target, qs)
		}
		queryNames[engineKey] = names
	}

	return &Analyzer{def: def, source: src, queryNames: queryNames, clock: clock.RealClock{}}, nil
}

// validateBody checks the body's shape: the config read checks the same
// definition with a message naming its fields (config.ValidateRules); this is
// the guard for a Definition built any other way.
func validateBody(body Body) error {
	if (body.Query == "") == (len(body.Demand) == 0) {
		return errors.New("exactly one of query and demand rules is required")
	}
	measured := len(body.Target) > 0
	served := func(role string) bool { return targetRuleFor(body.Target, role) >= 0 }
	if body.Query != "" && !measured && len(body.Demand) == 0 {
		// The original form, checked exactly as it always was: a NaN
		// threshold is let through and sizes nothing (see the reference).
		if body.Threshold <= 0 {
			return fmt.Errorf("threshold must be > 0, got %v", body.Threshold)
		}
	} else if !(body.Threshold >= 0) || math.IsInf(body.Threshold, 0) {
		return fmt.Errorf("threshold must be > 0, got %v", body.Threshold)
	} else if body.Query != "" && body.Threshold == 0 &&
		!(served(domain.RolePrefill) && served(domain.RoleDecode) && served(domain.RoleBoth)) {
		return errors.New("threshold is required unless a target rule serves every role")
	}
	seen := map[string]bool{}
	for i, rule := range body.Demand {
		switch {
		case !isRole(rule.Role):
			return fmt.Errorf("demand rule %d: role %q is not prefill, decode or both", i, rule.Role)
		case seen[rule.Role]:
			return fmt.Errorf("demand rule %d: role %q has another rule", i, rule.Role)
		case rule.Query == "":
			return fmt.Errorf("demand rule %d: query is required", i)
		case !(rule.Threshold >= 0) || math.IsInf(rule.Threshold, 0):
			return fmt.Errorf("demand rule %d: threshold must be > 0, got %v", i, rule.Threshold)
		case rule.Threshold == 0 && body.Threshold == 0 && !served(rule.Role):
			return fmt.Errorf("demand rule %d: role %q has no threshold and no target", i, rule.Role)
		}
		seen[rule.Role] = true
	}
	seenTarget := map[string]bool{}
	for i, rule := range body.Target {
		switch {
		case rule.Role != "" && !isRole(rule.Role):
			return fmt.Errorf("target rule %d: role %q is not prefill, decode or both", i, rule.Role)
		case seenTarget[rule.Role]:
			return fmt.Errorf("target rule %d: role %q has another rule", i, roleOrAny(rule.Role))
		case len(rule.Queries) == 0:
			return fmt.Errorf("target rule %d: at least one query is required", i)
		}
		for j, q := range rule.Queries {
			if q.Query == "" {
				return fmt.Errorf("target rule %d query %d: query is required", i, j)
			}
		}
		seenTarget[rule.Role] = true
	}
	switch body.TargetReduce {
	case "", ReduceAvg, ReduceMedian, ReduceMin, ReduceMax:
	default:
		return fmt.Errorf("targetReduce %q is not avg, median, min or max", body.TargetReduce)
	}
	return nil
}

// WithClock sets the clock AnalyzedAt is stamped on: the engine passes its
// cycle clock. Call it before the analyzer is used.
func (a *Analyzer) WithClock(c clock.PassiveClock) *Analyzer {
	a.clock = c
	return a
}

// Name returns the analyzer's label.
func (a *Analyzer) Name() string { return a.def.Label }

// Analyze selects the query body for the model's engine, runs it, and produces a
// (D, P) result: D is the summed query value, P is the body's constant threshold
// or the measured target.
// When no body matches the model's engine, or no demand rule matches any of
// the model's roles, the analyzer does not apply to this model and returns a
// nil result (the engine skips it — the not-defined state, distinct from a
// present-but-zero result).
func (a *Analyzer) Analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	engineKey, ok := a.selectBody(modelEngine(input.VariantStates))
	if !ok {
		return nil, nil
	}
	body, names := a.def.Bodies[engineKey], a.queryNames[engineKey]

	// The roles this body defines demand for, and each one's constant P.
	var (
		totalDemand float64
		roleDemand  map[string]float64
		constant    = map[string]float64{}
		defined     func(role string) bool
	)
	if body.Query != "" {
		demand, err := a.queryDemand(ctx, names.demand, input, body.EmptyIsMissing, false)
		if err != nil {
			return nil, err
		}
		totalDemand = demand
		defined = func(string) bool { return true }
	} else {
		roles := modelRoles(input.VariantStates)
		byRole := map[string]float64{}
		for i, rule := range body.Demand {
			if !slices.Contains(roles, rule.Role) {
				continue
			}
			demand, err := a.queryDemand(ctx, names.rules[i], input, body.EmptyIsMissing, rule.OrZero)
			if err != nil {
				return nil, fmt.Errorf("role %s: %w", rule.Role, err)
			}
			byRole[rule.Role] = demand
			if rule.Threshold > 0 {
				constant[rule.Role] = rule.Threshold
			}
		}
		if len(byRole) == 0 {
			return nil, nil
		}
		// In role order: a float sum in a map's random order differs in its
		// last bit from one cycle to the next.
		for _, role := range slices.Sorted(maps.Keys(byRole)) {
			totalDemand += byRole[role]
		}
		// The layout saturation uses: per-role demand only on a P/D model. An
		// aggregated model's one role is the model, and its demand TotalDemand.
		if !slices.Equal(roles, []string{domain.RoleBoth}) {
			roleDemand = byRole
		}
		defined = func(role string) bool { _, ok := byRole[role]; return ok }
	}

	measured := a.measureTargets(ctx, body, names, input, defined)

	vcs := make([]domain.VariantCapacity, 0, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		role := domain.CanonicalRole(vs.Role)
		if !defined(role) {
			continue
		}
		p, reason := measured[vs.VariantName], reasonMeasuredTarget
		if !(p > 0) {
			p, reason = constant[role], reasonConstantTarget
			if !(p > 0) {
				p = body.Threshold
			}
		}
		if !(p > 0) && measured != nil {
			// No target this cycle: missing, not 0. Left out, so no need is
			// read and no 0 is published for it. Only with target rules: a
			// constant threshold is used as it always was, even a NaN one
			// (which sizes nothing, as documented).
			ctrl.LoggerFrom(ctx).V(logging.VERBOSE).Info("external analyzer has no per-replica target for a variant this cycle",
				"analyzer", a.def.Label, "namespace", input.Namespace, "modelID", input.ModelID, "variant", vs.VariantName)
			continue
		}
		readyCount := vs.CurrentReplicas - vs.PendingReplicas
		if readyCount < 0 {
			readyCount = 0
		}
		// Cost/AcceleratorName are intentionally unset: the capacity builder fills
		// per-variant identity from discovery. The wrapper emits only P.
		vcs = append(vcs, domain.VariantCapacity{
			VariantName:        vs.VariantName,
			Role:               vs.Role,
			ReplicaCount:       readyCount,
			PendingReplicas:    vs.PendingReplicas,
			PerReplicaCapacity: p,
			Reason:             reason,
		})
	}
	if len(body.Target) > 0 && len(vcs) == 0 && len(input.VariantStates) > 0 {
		return nil, fmt.Errorf("external analyzer %q: no target query answered for any variant, and none has a threshold", a.def.Label)
	}

	return &domain.AnalyzerResult{
		AnalyzerName:      a.def.Label,
		ModelID:           input.ModelID,
		Namespace:         input.Namespace,
		AnalyzedAt:        a.clock.Now(),
		VariantCapacities: vcs,
		TotalDemand:       totalDemand,
		// Per-role demand on a P/D model when the body has demand rules; nil
		// for the model-level query, which therefore cannot size a P/D model.
		RoleDemand: roleDemand,
		// Supply, utilization, and RoleCapacities are assembled downstream by the
		// engine's capacity-build step; the wrapper emits only the (D, P) signal.
		// RequiredCapacity/SpareCapacity are written by the engine post-step.
	}, nil
}

// selectBody returns the body key for engine, falling back to the
// engine-agnostic body. ok is false when neither matches.
func (a *Analyzer) selectBody(engine string) (string, bool) {
	if _, ok := a.def.Bodies[engine]; ok {
		return engine, true
	}
	if _, ok := a.def.Bodies[engineAgnostic]; ok {
		return engineAgnostic, true
	}
	return "", false
}

// queryDemand runs the given query name for the model and returns the sum of
// all returned series — the total demand.
//
// A failed query is an error, never zero demand: an outage is not a
// measurement. The engine discards an analyzer result that errors
// (runRegisteredAnalyzer), so this analyzer reports nothing for the model this
// cycle and its llmsm_analyzer_demand series is dropped rather than set to 0.
//
// A query that returned no series, or no result at all (which a source does
// only for a query it did not run), reads as zero unless emptyIsMissing is
// set and orZero is not; then it is ErrEmptyResult, handled like a failure.
func (a *Analyzer) queryDemand(ctx context.Context, qName string, input domain.AnalyzerInput, emptyIsMissing, orZero bool) (float64, error) {
	res, err := a.refresh(ctx, qName, input)
	if err != nil {
		return 0, fmt.Errorf("external analyzer %q: demand query: %w", a.def.Label, err)
	}
	if res == nil || len(res.Values) == 0 {
		if emptyIsMissing && !orZero {
			return 0, fmt.Errorf("external analyzer %q: demand query: %w", a.def.Label, ErrEmptyResult)
		}
		if !orZero {
			reportEmptyOnce(ctx, a.def.Label, qName, input.Namespace, input.ModelID)
		}
		return 0, nil
	}
	var demand float64
	for _, v := range res.Values {
		// The SAME Pod gate the collector's builder applies, so an external
		// analyzer measures the same population as a built-in one.
		//
		// It has to be applied here because this analyzer never builds a per-Pod
		// record: it runs the operator's query and sums the series itself. Without
		// this, a per-Pod body counts deleted Pods for as long as Prometheus keeps
		// their series -- the same defect the builder prevents, on a path the
		// builder cannot see.
		//
		// An already-aggregated body has no Pod label left to judge and is summed
		// untouched; see collector.SeriesPodIsGone.
		if a.DropSeries != nil && a.DropSeries(ctx, input.Namespace, v.Labels) {
			continue
		}
		demand += v.Value
	}
	return demand, nil
}

// refresh runs one registered query for the model. It returns the query's
// result, nil when the source returned none, and an error when the query
// failed.
func (a *Analyzer) refresh(ctx context.Context, qName string, input domain.AnalyzerInput) (*source.MetricResult, error) {
	results, err := a.source.Refresh(ctx, source.RefreshSpec{
		Queries: []string{qName},
		Params: map[string]string{
			source.ParamModelID:   input.ModelID,
			source.ParamNamespace: input.Namespace,
		},
	})
	if err != nil {
		return nil, err
	}
	res := results[qName]
	// Prometheus already returns an error above when this, its only query,
	// failed. This covers a source that reports a query's failure only in the
	// result.
	if res != nil && res.HasError() {
		return nil, res.Error
	}
	return res, nil
}

// measureTargets returns the measured P of each variant of a defined role that
// a target query answered for. Each variant takes the first of its rule's
// queries, in order, that returned a usable value for any of its Pods; a
// query is run only while a variant it serves still has none. A value that is
// not finite or not above 0 is no value: a per-replica capacity of 0 is a
// division by zero in D / P, not a reading.
func (a *Analyzer) measureTargets(ctx context.Context, body Body, names bodyQueries, input domain.AnalyzerInput,
	defined func(string) bool) map[string]float64 {
	if len(body.Target) == 0 {
		return nil
	}
	logger := ctrl.LoggerFrom(ctx).V(logging.VERBOSE)
	podVariant := make(map[string]string, len(input.ReplicaMetrics))
	for _, rm := range input.ReplicaMetrics {
		if rm.PodName != "" && rm.VariantName != "" && !rm.FromWarmPool {
			podVariant[rm.PodName] = rm.VariantName
		}
	}
	out := map[string]float64{}
	for i, rule := range body.Target {
		pending := map[string]bool{}
		for _, vs := range input.VariantStates {
			role := domain.CanonicalRole(vs.Role)
			if defined(role) && targetRuleFor(body.Target, role) == i {
				pending[vs.VariantName] = true
			}
		}
		for j, q := range rule.Queries {
			if len(pending) == 0 {
				break
			}
			res, err := a.refresh(ctx, names.target[i][j], input)
			if err != nil || res == nil {
				logger.Info("external analyzer target query did not answer; trying the next",
					"analyzer", a.def.Label, "namespace", input.Namespace, "modelID", input.ModelID,
					"e", q.E, "query", j, "error", err)
				continue
			}
			perVariant := map[string][]float64{}
			for _, v := range res.Values {
				variant := podVariant[seriesPodName(v.Labels)]
				if !pending[variant] || !(v.Value > 0) || math.IsInf(v.Value, 0) {
					continue
				}
				if a.DropSeries != nil && a.DropSeries(ctx, input.Namespace, v.Labels) {
					continue
				}
				perVariant[variant] = append(perVariant[variant], v.Value)
			}
			for _, variant := range slices.Sorted(maps.Keys(perVariant)) {
				out[variant] = reduce(body.TargetReduce, perVariant[variant])
				delete(pending, variant)
				logger.Info("external analyzer measured a per-replica target",
					"analyzer", a.def.Label, "namespace", input.Namespace, "modelID", input.ModelID,
					"variant", variant, "e", q.E, "query", j, "pods", len(perVariant[variant]), "target", out[variant])
			}
		}
	}
	return out
}

// targetRuleFor returns the index of the target rule serving role: the one
// that names it, else the one with no role, else -1.
func targetRuleFor(rules []TargetRule, role string) int {
	if i := slices.IndexFunc(rules, func(r TargetRule) bool { return r.Role == role }); i >= 0 {
		return i
	}
	return slices.IndexFunc(rules, func(r TargetRule) bool { return r.Role == "" })
}

// reduce reduces a variant's per-Pod targets, at least one, to one value.
func reduce(how string, values []float64) float64 {
	switch how {
	case ReduceMin:
		return slices.Min(values)
	case ReduceMax:
		return slices.Max(values)
	case ReduceMedian:
		s := slices.Clone(values)
		sort.Float64s(s)
		n := len(s)
		if n%2 == 1 {
			return s[n/2]
		}
		return (s[n/2-1] + s[n/2]) / 2
	default:
		var sum float64
		for _, v := range values {
			sum += v
		}
		return sum / float64(len(values))
	}
}

// seriesPodName is the Pod a series names: its "pod" label, else "pod_name",
// the same two conventions the collector accepts (collector.seriesPodName).
func seriesPodName(labels map[string]string) string {
	if p := labels["pod"]; p != "" {
		return p
	}
	return labels["pod_name"]
}

// reportedEmpty holds each query an empty result was logged for. Analyzers
// are rebuilt from the catalog every cycle, so this is the package's, not an
// Analyzer's, or the line would be logged every cycle.
var reportedEmpty sync.Map

// reportEmptyOnce logs, once per query and model, that a demand query
// returned no series and is read as 0 demand: a renamed metric or a label
// typo reads that way too, and lets the model scale down.
func reportEmptyOnce(ctx context.Context, label, qName, namespace, modelID string) {
	if _, seen := reportedEmpty.LoadOrStore(qName+"/"+namespace+"/"+modelID, struct{}{}); seen {
		return
	}
	ctrl.LoggerFrom(ctx).Info("External analyzer demand query returned no series, read as 0 demand; "+
		"set emptyIsMissing: true to read it as missing instead (logged once per query and model)",
		"analyzer", label, "query", qName, "namespace", namespace, "modelID", modelID)
}

// modelEngine returns the model's inference engine from its variant states.
// MVP: it uses the first variant that declares an engine (models are assumed
// engine-homogeneous); mixed-engine models are a follow-up.
func modelEngine(states []domain.VariantReplicaState) string {
	for _, vs := range states {
		if vs.Engine != "" {
			return vs.Engine
		}
	}
	return ""
}

// modelRoles returns the model's distinct canonical roles.
func modelRoles(states []domain.VariantReplicaState) []string {
	var roles []string
	for _, vs := range states {
		if role := domain.CanonicalRole(vs.Role); !slices.Contains(roles, role) {
			roles = append(roles, role)
		}
	}
	return roles
}

// isRole reports whether role is one a rule may name.
func isRole(role string) bool {
	return role == domain.RolePrefill || role == domain.RoleDecode || role == domain.RoleBoth
}

// roleOrAny renders a target rule's role in a query name and an error.
func roleOrAny(role string) string {
	if role == "" {
		return "any"
	}
	return role
}

// queryName builds the registered query name for an analyzer label, engine
// key and query kind. The engine-agnostic body has no engine suffix, so the
// model-level demand query keeps the name it always had.
func queryName(label, engineKey, kind string) string {
	if engineKey == engineAgnostic {
		return queryNamePrefix + label + ":" + kind
	}
	return queryNamePrefix + label + ":" + kind + ":" + engineKey
}

// engineLabel renders an engine key for error messages.
func engineLabel(engineKey string) string {
	if engineKey == engineAgnostic {
		return "engine-agnostic"
	}
	return "engine=" + engineKey
}
