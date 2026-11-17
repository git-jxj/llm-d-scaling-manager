package whatif

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// q32jDecode is the decode role of the Qwen3-32B TP2 P/D staircase q32j as the
// planner priced its 8000/200 phases: the held line of the warm-up recording
// (A = 42.50 ms, B = 10.43 ms), C = 764,672 tokens, KVreq = 8114 tokens, OL =
// 200, batch cap 256, two decode replicas.
func q32jDecode(tier string) Decode {
	return Decode{
		Line:           itl.Model{A: 0.04249685049858612, B: 0.010425088112190157},
		Band:           DefaultBand(),
		CapacityTokens: 764672,
		KVPerRequest:   8114.099089776806,
		OutputTokens:   200,
		ReplicaCap:     256,
		Replicas:       2,
		Tier:           tier,
	}
}

// q32jStep is one 8000/200 step: the rate sent and the mean TPOT the client
// measured over the step's window.
type q32jStep struct {
	phase string
	rate  float64
	mean  float64 // seconds
}

// q32jSteps are the B and C phases of q32j, flow control off and on: the
// decode pods ran at a KV fill of 0.02 to 0.06, under the band's bottom.
var q32jSteps = []q32jStep{
	{"B-off", 1.619, 0.01152}, {"B-off", 2.429, 0.01183}, {"B-off", 3.036, 0.01205}, {"B-off", 3.238, 0.01214},
	{"B-off", 3.440, 0.01230}, {"B-off", 3.643, 0.01262}, {"B-off", 3.845, 0.01256}, {"B-off", 4.048, 0.01301},
	{"B-on", 1.619, 0.01155}, {"B-on", 2.429, 0.01184}, {"B-on", 3.036, 0.01204}, {"B-on", 3.238, 0.01213},
	{"B-on", 3.440, 0.01232}, {"B-on", 3.643, 0.01239}, {"B-on", 3.845, 0.01279},
	{"C-off", 1.619, 0.01171}, {"C-off", 2.429, 0.01196}, {"C-off", 3.036, 0.01243}, {"C-off", 3.238, 0.01245},
	{"C-off", 3.340, 0.01253}, {"C-off", 3.440, 0.01251}, {"C-off", 3.540, 0.01268}, {"C-off", 3.643, 0.01271},
	{"C-off", 3.743, 0.01285}, {"C-off", 3.845, 0.01280}, {"C-off", 4.048, 0.01301},
	{"C-on", 1.619, 0.01179}, {"C-on", 2.429, 0.01211}, {"C-on", 3.036, 0.01243}, {"C-on", 3.238, 0.01270},
	{"C-on", 3.340, 0.01268},
}

// The mean TPOT at each q32j step, priced at the fill the step runs at, is
// within 5% of what the client measured, and within 2.5% on average. Read at
// the band's bottom, as it was, every step priced 16.80 ms: 29% to 46% high.
func TestMeanTPOTAtTheFill_Q32jStaircase(t *testing.T) {
	d := q32jDecode(TierHeld)
	var sum float64
	for _, s := range q32jSteps {
		slo := SLOAtReplicas(d, ReplicasQuery{Rate: s.rate})
		mean := mustAnswer(t, slo.MeanTPOT)
		err := mean.Value/s.mean - 1
		sum += err
		if err < -0.05 || err > 0.05 {
			t.Errorf("%s at %.3f r/s: mean TPOT %.2f ms, measured %.2f ms, error %+.1f%%, want within 5%%",
				s.phase, s.rate, 1000*mean.Value, 1000*s.mean, 100*err)
		}
		if !mean.HasFlag(BelowFittedBand) {
			t.Errorf("%s at %.3f r/s: fill %.3f is below the band and not flagged: %v", s.phase, s.rate, mean.Basis.Fill, mean.Flags)
		}
	}
	if avg := sum / float64(len(q32jSteps)); avg < -0.025 || avg > 0.025 {
		t.Errorf("mean error over %d steps %+.1f%%, want within 2.5%%", len(q32jSteps), 100*avg)
	}
}

// A line whose intercept was not measured on the variant keeps the band's
// bottom as its floor: the same q32j line, pinned, prices 16.80 ms at every
// step, today's figure.
func TestMeanTPOTAtTheFill_UnmeasuredInterceptKeepsTheBandFloor(t *testing.T) {
	for _, tier := range []string{TierPinnedB, TierBorrowed, TierAboveBand, ""} {
		d := q32jDecode(tier)
		for _, s := range q32jSteps {
			mean := mustAnswer(t, SLOAtReplicas(d, ReplicasQuery{Rate: s.rate}).MeanTPOT)
			near(t, tier+" "+s.phase+" mean TPOT", mean.Value, 0.016800, 0.0000005)
		}
		if _, refused := RateForTPOT(d, RateQuery{Target: TPOTTarget{Seconds: 0.0125}, Headroom: 1}).(Unanswerable); !refused {
			t.Errorf("tier %q: a 12.5 ms target under the band's floor of 16.80 ms was answered", tier)
		}
	}
}

// Questions 1 and 2 agree: the rate question 1 allows at a TPOT target, fed
// back to question 2, prices that TPOT, below the band as well as in it.
func TestRateForTPOTRoundTripsThroughSLOAtReplicas(t *testing.T) {
	ratio := TPOTRatio{Value: 1.3, Assumed: true}
	for _, tier := range []string{TierOLS, TierHeld, TierPinnedB} {
		d := q32jDecode(tier)
		// The floor: B for a measured intercept, the band's bottom otherwise.
		lowest := d.Line.B
		if tier == TierPinnedB {
			lowest = d.Line.ITLAt(d.Band.Lo)
		}
		for _, ms := range []float64{10.6, 11.5, 12.2, 13, 16, 16.8, 20, 30} {
			for _, pct := range []float64{0, 0.9} {
				target := TPOTTarget{Seconds: ms / 1000}
				r := 1.0
				if pct > 0 {
					target.Percentile, target.Ratio, r = pct, ratio, ratio.Value
				}
				res := RateForTPOT(d, RateQuery{Target: target, Headroom: 1})
				if lowest*r >= target.Seconds {
					if u, ok := res.(Unanswerable); !ok || u.Reason != TargetBelowFloor {
						t.Errorf("%s %.1f ms p%g: under the floor %.2f ms, got %#v", tier, ms, 100*pct, 1000*lowest*r, res)
					}
					continue
				}
				q1 := mustAnswer(t, res)
				if len(q1.Basis.BoundBy) != 1 || q1.Basis.BoundBy[0] != BoundTPOT {
					continue // the knee, not the target, set the rate
				}
				query := ReplicasQuery{Rate: q1.Value}
				if pct > 0 {
					query.PercentileTarget = &target
				}
				slo := SLOAtReplicas(d, query)
				got := slo.MeanTPOT
				if pct > 0 {
					got = slo.PercentileTPOT
				}
				q2 := mustAnswer(t, got)
				near(t, tier+" round trip", q2.Value, target.Seconds, 1e-9)
				if q1.HasFlag(BelowFittedBand) != q2.HasFlag(BelowFittedBand) {
					t.Errorf("%s %.1f ms p%g: question 1 flags below band %v, question 2 %v",
						tier, ms, 100*pct, q1.HasFlag(BelowFittedBand), q2.HasFlag(BelowFittedBand))
				}
				near(t, tier+" floor", q2.Basis.Floor, q1.Basis.Floor, 1e-12)
			}
		}
	}
}
