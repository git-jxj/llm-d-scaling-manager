package external_test

import (
	"context"
	"errors"
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/external"
)

// The registered names of the rule-form queries of an engine-agnostic body.
const (
	prefillQueryName = "external:queue-depth:demand@prefill"
	decodeQueryName  = "external:queue-depth:demand@decode"
	bothQueryName    = "external:queue-depth:demand@both"
)

// pdInput is a P/D model: one prefill variant with 2 ready replicas and one
// decode variant with 4.
func pdInput() domain.AnalyzerInput {
	return domain.AnalyzerInput{
		ModelID: "m", Namespace: "ns",
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "p1", Role: domain.RolePrefill, CurrentReplicas: 2},
			{VariantName: "d1", Role: domain.RoleDecode, CurrentReplicas: 4},
		},
	}
}

// roleDef is a definition with one demand rule per role: prefill at a
// threshold of its own (3), decode at the body's (5).
func roleDef() external.Definition {
	return external.Definition{
		Label: "queue-depth",
		Bodies: map[string]external.Body{"": {
			Threshold: 5,
			Demand: []external.DemandRule{
				{Role: domain.RolePrefill, Query: "sum(prefill_queue)", Threshold: 3},
				{Role: domain.RoleDecode, Query: "sum(decode_queue)"},
			},
		}},
	}
}

func values(vs ...float64) *source.MetricResult {
	out := &source.MetricResult{Values: []source.MetricValue{}}
	for _, v := range vs {
		out.Values = append(out.Values, source.MetricValue{Value: v})
	}
	return out
}

// podValues is a per-Pod result: pod name to value.
func podValues(byPod map[string]float64) *source.MetricResult {
	out := &source.MetricResult{Values: []source.MetricValue{}}
	for pod, v := range byPod {
		out.Values = append(out.Values, source.MetricValue{Labels: map[string]string{"pod": pod}, Value: v})
	}
	return out
}

func byVariant(res *domain.AnalyzerResult) map[string]domain.VariantCapacity {
	out := map[string]domain.VariantCapacity{}
	for _, vc := range res.VariantCapacities {
		out[vc.VariantName] = vc
	}
	return out
}

var _ = Describe("external.Analyzer demand rules per role", func() {
	var fs *fakeSource

	BeforeEach(func() {
		fs = newFakeSource()
	})

	It("reports each role's demand and threshold, so a P/D model can be sized", func() {
		a, err := external.New(roleDef(), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results[prefillQueryName] = values(4, 5)
		fs.results[decodeQueryName] = values(30)

		res, err := a.Analyze(context.Background(), pdInput())
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RoleDemand).To(Equal(map[string]float64{domain.RolePrefill: 9, domain.RoleDecode: 30}))
		Expect(res.TotalDemand).To(Equal(39.0))
		vcs := byVariant(res)
		Expect(vcs["p1"].PerReplicaCapacity).To(Equal(3.0), "the prefill rule's own threshold")
		Expect(vcs["d1"].PerReplicaCapacity).To(Equal(5.0), "the body's threshold")

		// The need the composite reads per variant: D[role] / P.
		n, ok := allocation.TotalReplicas(res, vcs["p1"])
		Expect(ok).To(BeTrue())
		Expect(n).To(Equal(3.0))
		n, ok = allocation.TotalReplicas(res, vcs["d1"])
		Expect(ok).To(BeTrue())
		Expect(n).To(Equal(6.0))
	})

	It("leaves out a role no rule names: it is not defined, neither sized nor zero", func() {
		def := roleDef()
		body := def.Bodies[""]
		body.Demand = body.Demand[:1] // prefill only
		def.Bodies[""] = body
		a, err := external.New(def, fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results[prefillQueryName] = values(6)

		res, err := a.Analyze(context.Background(), pdInput())
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RoleDemand).To(Equal(map[string]float64{domain.RolePrefill: 6}))
		Expect(byVariant(res)).NotTo(HaveKey("d1"))
		_, present := res.RoleDemand[domain.RoleDecode]
		Expect(present).To(BeFalse())
	})

	It("is not defined for a model none of whose roles a rule names, and runs no query", func() {
		a, err := external.New(roleDef(), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.err = errors.New("no query may run")

		res, err := a.Analyze(context.Background(), domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns",
			VariantStates: []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: 2}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(BeNil())
	})

	It("sizes an aggregated model from a both rule, in the model-level layout", func() {
		def := external.Definition{Label: "queue-depth", Bodies: map[string]external.Body{"": {
			Threshold: 4,
			Demand:    []external.DemandRule{{Role: domain.RoleBoth, Query: "sum(q)"}},
		}}}
		a, err := external.New(def, fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results[bothQueryName] = values(12)

		res, err := a.Analyze(context.Background(), domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns",
			VariantStates: []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: 2}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RoleDemand).To(BeNil())
		Expect(res.TotalDemand).To(Equal(12.0))
		n, ok := allocation.TotalReplicas(res, res.VariantCapacities[0])
		Expect(ok).To(BeTrue())
		Expect(n).To(Equal(3.0))
	})

	It("fails, rather than reporting the other role alone, when one role's query fails", func() {
		a, err := external.New(roleDef(), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results[prefillQueryName] = values(6)
		fs.results[decodeQueryName] = &source.MetricResult{Error: errors.New("bad decode query")}

		res, err := a.Analyze(context.Background(), pdInput())
		Expect(err).To(MatchError(ContainSubstring("bad decode query")))
		Expect(err).To(MatchError(ContainSubstring("role decode")))
		Expect(res).To(BeNil())
	})

	It("registers one query per rule", func() {
		_, err := external.New(roleDef(), fs)
		Expect(err).NotTo(HaveOccurred())
		Expect(fs.ql.Get(prefillQueryName)).NotTo(BeNil())
		Expect(fs.ql.Get(decodeQueryName)).NotTo(BeNil())
		Expect(fs.ql.Get("external:queue-depth:demand")).To(BeNil(), "no model-level query is registered")
	})

	DescribeTable("New refuses a malformed body",
		func(body external.Body, want string) {
			_, err := external.New(external.Definition{Label: "l", Bodies: map[string]external.Body{"": body}}, fs)
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("query and demand rules", external.Body{Query: "q", Threshold: 1,
			Demand: []external.DemandRule{{Role: "prefill", Query: "p"}}}, "exactly one of query and demand"),
		Entry("an unknown role", external.Body{Threshold: 1,
			Demand: []external.DemandRule{{Role: "encode", Query: "p"}}}, `role "encode" is not`),
		Entry("a role twice", external.Body{Threshold: 1,
			Demand: []external.DemandRule{{Role: "decode", Query: "a"}, {Role: "decode", Query: "b"}}}, "has another rule"),
		Entry("a rule with no threshold anywhere", external.Body{
			Demand: []external.DemandRule{{Role: "decode", Query: "a"}}}, "no threshold and no target"),
		Entry("a NaN threshold beside a target", external.Body{Query: "q", Threshold: math.NaN(),
			Target: []external.TargetRule{{Queries: []external.TargetQuery{{Query: "t"}}}}}, "threshold must be > 0"),
		Entry("an unknown reduction", external.Body{Query: "q", Threshold: 1, TargetReduce: "p99"}, `targetReduce "p99"`),
		Entry("a target rule with no query", external.Body{Query: "q",
			Target: []external.TargetRule{{}}}, "at least one query"),
	)
})

var _ = Describe("external.Analyzer empty results", func() {
	var fs *fakeSource

	BeforeEach(func() {
		fs = newFakeSource()
	})

	missingDef := func() external.Definition {
		d := agnosticDef()
		body := d.Bodies[""]
		body.EmptyIsMissing = true
		d.Bodies[""] = body
		return d
	}

	It("reads a query that returned no series as 0 demand by default, as it always has", func() {
		a, err := external.New(agnosticDef(), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results[agnosticQueryName] = values()

		res, err := a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns",
			VariantStates: []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: 2}}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.TotalDemand).To(BeZero())
	})

	// The proposal's "missing" state: no reading this cycle, never a fabricated
	// 0. The engine drops a result that errors, which is what holds the model
	// for the grace window.
	It("with emptyIsMissing, reads no series as missing, not as 0", func() {
		a, err := external.New(missingDef(), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results[agnosticQueryName] = values()

		res, err := a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns",
			VariantStates: []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: 2}}})
		Expect(err).To(MatchError(external.ErrEmptyResult))
		Expect(res).To(BeNil(), "missing: no result at all, so no demand of 0")
	})

	It("with emptyIsMissing, reads a query the source did not run as missing too", func() {
		a, err := external.New(missingDef(), fs)
		Expect(err).NotTo(HaveOccurred())
		Expect(fs.results).NotTo(HaveKey(agnosticQueryName), "fixture: the query must have no result")

		_, err = a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns"})
		Expect(err).To(MatchError(external.ErrEmptyResult))
	})

	It("with emptyIsMissing, still reads a series of value 0 as 0 demand", func() {
		a, err := external.New(missingDef(), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results[agnosticQueryName] = values(0)

		res, err := a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns",
			VariantStates: []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: 2}}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.TotalDemand).To(BeZero())
	})

	It("lets a role's orZero read its empty result as 0 under emptyIsMissing", func() {
		def := roleDef()
		body := def.Bodies[""]
		body.EmptyIsMissing = true
		body.Demand[0].OrZero = true // prefill
		def.Bodies[""] = body
		a, err := external.New(def, fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results[prefillQueryName] = values()
		fs.results[decodeQueryName] = values(10)

		res, err := a.Analyze(context.Background(), pdInput())
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RoleDemand).To(Equal(map[string]float64{domain.RolePrefill: 0, domain.RoleDecode: 10}))

		fs.results[decodeQueryName] = values() // decode has no orZero
		_, err = a.Analyze(context.Background(), pdInput())
		Expect(err).To(MatchError(external.ErrEmptyResult))
	})
})

var _ = Describe("external.Analyzer measured targets", func() {
	var fs *fakeSource

	const (
		primary  = "external:kv:target@any:0"
		fallback = "external:kv:target@any:1"
	)

	BeforeEach(func() {
		fs = newFakeSource()
	})

	// Two variants, each with two Pods reporting metrics.
	input := func() domain.AnalyzerInput {
		return domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns",
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "a", CurrentReplicas: 2},
				{VariantName: "b", CurrentReplicas: 2},
			},
			ReplicaMetrics: []domain.ReplicaMetrics{
				{PodName: "a-1", VariantName: "a"}, {PodName: "a-2", VariantName: "a"},
				{PodName: "b-1", VariantName: "b"}, {PodName: "b-2", VariantName: "b"},
			},
		}
	}

	def := func(threshold float64, reduce string) external.Definition {
		return external.Definition{Label: "kv", Bodies: map[string]external.Body{"": {
			Query: "sum(d)", Threshold: threshold, TargetReduce: reduce,
			Target: []external.TargetRule{{Queries: []external.TargetQuery{
				{Query: "avg by (pod) (epp)", E: "epp"},
				{Query: "avg by (pod) (vllm)", E: "vllm"},
			}}},
		}}}
	}

	It("averages each variant's Pods by default", func() {
		a, err := external.New(def(0, ""), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results["external:kv:demand"] = values(100)
		fs.results[primary] = podValues(map[string]float64{"a-1": 10, "a-2": 20, "b-1": 40, "b-2": 40})

		res, err := a.Analyze(context.Background(), input())
		Expect(err).NotTo(HaveOccurred())
		vcs := byVariant(res)
		Expect(vcs["a"].PerReplicaCapacity).To(Equal(15.0))
		Expect(vcs["b"].PerReplicaCapacity).To(Equal(40.0))
		Expect(vcs["a"].Reason).To(Equal("E2-query"))
	})

	DescribeTable("reduces a variant's Pods as targetReduce says",
		func(reduce string, want float64) {
			a, err := external.New(def(0, reduce), fs)
			Expect(err).NotTo(HaveOccurred())
			fs.results["external:kv:demand"] = values(1)
			fs.results[primary] = podValues(map[string]float64{"a-1": 10, "a-2": 30, "b-1": 1, "b-2": 1})

			res, err := a.Analyze(context.Background(), input())
			Expect(err).NotTo(HaveOccurred())
			Expect(byVariant(res)["a"].PerReplicaCapacity).To(Equal(want))
		},
		Entry("avg", "avg", 20.0),
		Entry("median", "median", 20.0),
		Entry("min", "min", 10.0),
		Entry("max", "max", 30.0),
	)

	It("falls back, per variant, to the next query when the first has nothing for it", func() {
		a, err := external.New(def(0, ""), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results["external:kv:demand"] = values(1)
		fs.results[primary] = podValues(map[string]float64{"a-1": 10, "a-2": 10})
		fs.results[fallback] = podValues(map[string]float64{"a-1": 99, "b-1": 7, "b-2": 9})

		res, err := a.Analyze(context.Background(), input())
		Expect(err).NotTo(HaveOccurred())
		vcs := byVariant(res)
		Expect(vcs["a"].PerReplicaCapacity).To(Equal(10.0), "the first query answered for a")
		Expect(vcs["b"].PerReplicaCapacity).To(Equal(8.0), "the fallback answered for b")
	})

	It("falls back past a query that failed", func() {
		a, err := external.New(def(0, ""), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results["external:kv:demand"] = values(1)
		fs.results[primary] = &source.MetricResult{Error: errors.New("bad")}
		fs.results[fallback] = podValues(map[string]float64{"a-1": 5, "b-1": 6})

		res, err := a.Analyze(context.Background(), input())
		Expect(err).NotTo(HaveOccurred())
		Expect(byVariant(res)["a"].PerReplicaCapacity).To(Equal(5.0))
	})

	It("drops a Pod value of 0, NaN or Inf: it is no per-replica capacity", func() {
		a, err := external.New(def(0, ""), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results["external:kv:demand"] = values(1)
		fs.results[primary] = podValues(map[string]float64{"a-1": 0, "a-2": 8, "b-1": math.NaN(), "b-2": math.Inf(1)})
		fs.results[fallback] = podValues(map[string]float64{"b-1": 3})

		res, err := a.Analyze(context.Background(), input())
		Expect(err).NotTo(HaveOccurred())
		vcs := byVariant(res)
		Expect(vcs["a"].PerReplicaCapacity).To(Equal(8.0))
		Expect(vcs["b"].PerReplicaCapacity).To(Equal(3.0))
	})

	It("uses the threshold for a variant no query answers for", func() {
		a, err := external.New(def(2, ""), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results["external:kv:demand"] = values(1)
		fs.results[primary] = podValues(map[string]float64{"a-1": 10})

		res, err := a.Analyze(context.Background(), input())
		Expect(err).NotTo(HaveOccurred())
		vcs := byVariant(res)
		Expect(vcs["b"].PerReplicaCapacity).To(Equal(2.0))
		Expect(vcs["b"].Reason).To(Equal("E1-const"))
	})

	It("leaves out a variant with no target and no threshold: missing, not 0", func() {
		a, err := external.New(def(0, ""), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results["external:kv:demand"] = values(1)
		fs.results[primary] = podValues(map[string]float64{"a-1": 10})

		res, err := a.Analyze(context.Background(), input())
		Expect(err).NotTo(HaveOccurred())
		Expect(byVariant(res)).To(HaveKey("a"))
		Expect(byVariant(res)).NotTo(HaveKey("b"))
	})

	It("fails when no variant has a target: the analyzer has no reading", func() {
		a, err := external.New(def(0, ""), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results["external:kv:demand"] = values(1)

		res, err := a.Analyze(context.Background(), input())
		Expect(err).To(MatchError(ContainSubstring("no target query answered")))
		Expect(res).To(BeNil())
	})

	It("ignores a Pod the collector does not know, and a warm-pool Pod", func() {
		a, err := external.New(def(0, ""), fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results["external:kv:demand"] = values(1)
		in := input()
		in.ReplicaMetrics = append(in.ReplicaMetrics, domain.ReplicaMetrics{PodName: "pool-1", VariantName: "a", FromWarmPool: true})
		fs.results[primary] = podValues(map[string]float64{"a-1": 10, "pool-1": 1000, "stranger": 1000, "b-1": 4})

		res, err := a.Analyze(context.Background(), in)
		Expect(err).NotTo(HaveOccurred())
		Expect(byVariant(res)["a"].PerReplicaCapacity).To(Equal(10.0))
	})

	It("measures each role from its own target rule", func() {
		d := external.Definition{Label: "kv", Bodies: map[string]external.Body{"": {
			Demand: []external.DemandRule{
				{Role: domain.RolePrefill, Query: "p"}, {Role: domain.RoleDecode, Query: "d"},
			},
			Target: []external.TargetRule{
				{Role: domain.RoleDecode, Queries: []external.TargetQuery{{Query: "decode_target"}}},
				{Queries: []external.TargetQuery{{Query: "any_target"}}},
			},
		}}}
		a, err := external.New(d, fs)
		Expect(err).NotTo(HaveOccurred())
		fs.results["external:kv:demand@prefill"] = values(10)
		fs.results["external:kv:demand@decode"] = values(10)
		fs.results["external:kv:target@decode:0"] = podValues(map[string]float64{"d-1": 5, "p-1": 999})
		fs.results["external:kv:target@any:0"] = podValues(map[string]float64{"p-1": 2, "d-1": 999})

		in := pdInput()
		in.ReplicaMetrics = []domain.ReplicaMetrics{{PodName: "p-1", VariantName: "p1"}, {PodName: "d-1", VariantName: "d1"}}
		res, err := a.Analyze(context.Background(), in)
		Expect(err).NotTo(HaveOccurred())
		vcs := byVariant(res)
		Expect(vcs["p1"].PerReplicaCapacity).To(Equal(2.0))
		Expect(vcs["d1"].PerReplicaCapacity).To(Equal(5.0))
	})
})
