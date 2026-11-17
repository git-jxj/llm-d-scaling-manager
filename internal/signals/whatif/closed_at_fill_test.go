package whatif

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// q32jDecode is the decode role of the Qwen3-32B TP2 P/D staircase q32j as the
// planner priced its 8000/200 phases: the held line of the warm-up recording
// (A = 42.50 ms, B = 10.43 ms), C = 764,672 tokens, KVreq = 8114 tokens, OL =
// 200, batch cap 256, two decode replicas, read below the band as the planner
// reads it, on the floor the recording read there.
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
		BelowBand:      BelowBand{Read: true, LeastITL: q32jLeastITL},
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

// q32jLeastITL is the floor below the band of the q32j warm-up recording:
// the 10th percentile of the decode replicas' mean ITLs read there off a
// ramp (learnreplay's VariantState.BelowBandITL).
const q32jLeastITL = 0.010819188

// The mean TPOT at each q32j step, priced at the fill the step runs at, on
// the recording's floor below the band, is within 5% of what the client
// measured, and within 2.5% on average. Read at the band's bottom, as it
// was, every step priced 16.80 ms: 29% to 46% high.
func TestMeanTPOTAtTheFill_Q32jStaircase(t *testing.T) {
	for _, least := range []float64{q32jLeastITL} {
		d := q32jDecode(TierHeld)
		d.BelowBand.LeastITL = least
		var sum float64
		for _, s := range q32jSteps {
			slo := SLOAtReplicas(d, ReplicasQuery{Rate: s.rate})
			mean := mustAnswer(t, slo.MeanTPOT)
			err := mean.Value/s.mean - 1
			sum += err
			if err < -0.05 || err > 0.05 {
				t.Errorf("floor %.2f ms, %s at %.3f r/s: mean TPOT %.2f ms, measured %.2f ms, error %+.1f%%, want within 5%%",
					1000*least, s.phase, s.rate, 1000*mean.Value, 1000*s.mean, 100*err)
			}
			if !mean.HasFlag(BelowFittedBand) {
				t.Errorf("%s at %.3f r/s: fill %.3f is below the band and not flagged: %v", s.phase, s.rate, mean.Basis.Fill, mean.Flags)
			}
		}
		if avg := sum / float64(len(q32jSteps)); avg < -0.025 || avg > 0.025 {
			t.Errorf("floor %.2f ms: mean error over %d steps %+.1f%%, want within 2.5%%", 1000*least, len(q32jSteps), 100*avg)
		}
	}
}

// Where the intercept came from decides the floor: a B from a full fit (ols,
// or the stored B of a pinned-B, held or above-band line) is read at the
// fill; a default or borrowed B, or one of unknown provenance, keeps the
// band's bottom, today's 16.80 ms on the q32j line. A pinned-B or held line
// on the default B is refused outright.
func TestMeanTPOTAtTheFill_TheInterceptsProvenanceSetsTheFloor(t *testing.T) {
	const step = 3.238 // B-off step 4: the fill 0.042, measured 12.14 ms
	atFill := 0.010425088112190157 + 0.04249685049858612*0.0419
	cases := []struct {
		name     string
		tier     string
		defaultB bool
		read     bool
		want     float64 // 0: refused
	}{
		{"ols: B fitted", TierOLS, false, true, atFill},
		{"pinned-B on the last full fit's B", TierPinnedB, false, true, atFill},
		{"held on the last full fit's B", TierHeld, false, true, atFill},
		{"above-band on the last full fit's B", TierAboveBand, false, true, atFill},
		{"above-band on the default B", TierAboveBand, true, true, -1},
		{"pinned-B on the default B", TierPinnedB, true, true, 0},
		{"held on the default B", TierHeld, true, true, 0},
		{"borrowed: another variant's B", TierBorrowed, false, true, 0.016800},
		{"unknown tier", "", false, true, 0.016800},
		{"ols, read as the controller reads it", TierOLS, false, false, 0.016800},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := q32jDecode(c.tier)
			d.BelowBand.Read = c.read
			if c.defaultB {
				d.Line.B = itl.DefaultBaselineSec
			}
			res := SLOAtReplicas(d, ReplicasQuery{Rate: step}).MeanTPOT
			switch {
			case c.want == 0:
				if u, ok := res.(Unanswerable); !ok || u.Reason != PinnedDefaultB {
					t.Fatalf("want PinnedDefaultB, got %#v", res)
				}
			case c.want < 0:
				near(t, "mean TPOT", mustAnswer(t, res).Value, d.Line.ITLAt(d.Band.Lo), 1e-12)
			default:
				near(t, "mean TPOT", mustAnswer(t, res).Value, c.want, 0.00002)
			}
		})
	}
}

// With no floor read below the band the line is not extrapolated there: a
// fitted line keeps the band's bottom, 16.80 ms on q32j, and a target under
// it is refused, as before the line was read below the band.
func TestMeanTPOTAtTheFill_NoReadingBelowTheBandKeepsTheBandFloor(t *testing.T) {
	for _, tier := range []string{TierOLS, TierPinnedB, TierHeld, TierAboveBand} {
		d := q32jDecode(tier)
		d.BelowBand.LeastITL = 0
		for _, s := range q32jSteps {
			mean := mustAnswer(t, SLOAtReplicas(d, ReplicasQuery{Rate: s.rate}).MeanTPOT)
			near(t, tier+" "+s.phase+" mean TPOT", mean.Value, 0.016800, 0.0000005)
		}
		if u, ok := RateForTPOT(d, RateQuery{Target: TPOTTarget{Seconds: 0.0125}, Headroom: 1}).(Unanswerable); !ok || u.Reason != TargetBelowFloor {
			t.Errorf("%s: a 12.5 ms target under the band's floor of 16.80 ms was not refused: %#v", tier, u)
		}
	}
}

// The least ITL read below the band floors the line there: a fill under the
// floor's reads the floor, one above it the line, and a floor at or above
// the band's bottom gives the band's bottom back. A floor under B is B.
func TestMeanTPOTAtTheFill_TheLeastITLBelowTheBandFloorsIt(t *testing.T) {
	d := q32jDecode(TierOLS)
	// A floor of 12.0 ms sits at the fill 0.0371; 1.619 r/s runs at 0.019.
	d.BelowBand.LeastITL = 0.0120
	low := mustAnswer(t, SLOAtReplicas(d, ReplicasQuery{Rate: 1.619}).MeanTPOT)
	near(t, "under the floor's fill", low.Value, 0.0120, 1e-12)
	near(t, "the floor", low.Basis.Floor, 0.0120, 1e-12)
	if !low.HasFlag(BelowFittedBand) {
		t.Errorf("not flagged below the band: %v", low.Flags)
	}
	// 4.048 r/s runs at 0.055, above the floor's fill.
	high := mustAnswer(t, SLOAtReplicas(d, ReplicasQuery{Rate: 4.048}).MeanTPOT)
	near(t, "above the floor's fill", high.Value, d.Line.ITLAt(high.Basis.Fill), 1e-12)
	if u, ok := RateForTPOT(d, RateQuery{Target: TPOTTarget{Seconds: 0.0118}, Headroom: 1}).(Unanswerable); !ok || u.Reason != TargetBelowFloor {
		t.Errorf("a target under the floor was answered: %#v", u)
	}

	d.BelowBand.LeastITL = 0.0200
	near(t, "a floor above the band's bottom", LowestFill(d.Band, d.Tier, d.Line, d.BelowBand.LeastITL), d.Band.Lo, 0)
	d.BelowBand.LeastITL = 0.0090
	near(t, "a floor under B", LowestFill(d.Band, d.Tier, d.Line, d.BelowBand.LeastITL), 0, 0)
}

// Questions 1 and 2 agree: the rate question 1 allows at a TPOT target, fed
// back to question 2, prices that TPOT, below the band as well as in it.
func TestRateForTPOTRoundTripsThroughSLOAtReplicas(t *testing.T) {
	ratio := TPOTRatio{Value: 1.3, Assumed: true}
	checked := 0
	for _, tier := range []string{TierOLS, TierHeld, TierPinnedB, TierBorrowed} {
		for _, least := range []float64{0, 0.0108, 0.0115} {
			d := q32jDecode(tier)
			d.BelowBand.LeastITL = least
			// The floor: the floor read below the band for a fitted
			// intercept; the band's bottom with none, or for a borrowed one.
			lowest := max(d.Line.B, least)
			if tier == TierBorrowed || least == 0 {
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
					checked++
				}
			}
		}
	}
	// Every answered case here is bound by its target, not the knee: the
	// line at the band's top is 43.8 ms, above every target. A change that
	// made them knee-bound would skip them all, so the count is held.
	if checked < 60 {
		t.Errorf("%d cases round-tripped, want at least 60", checked)
	}
}
