package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// stairBOn is the P/D staircase with flow control on: two prefill and two
// decode replicas, and a decode line the planner answers on. P1 learns
// neither V_P nor a on it, so the report's P/D cases assume both.
const stairBOn = "../../internal/engines/analyzers/learnreplay/testdata/goldens/ttft-staircase-b-on/recording.jsonl.gz"

// stairAssumed is the P/D fleet's assumed prefill and the targets it is
// judged by: V_P and a, so that the prefill side prices, and a TTFT and a
// TPOT target the staircase's fleet can meet.
var stairAssumed = []string{"--assume-vp", "200000", "--assume-a", "20ms", "--ttft", "2s@p90", "--tpot", "30ms"}

// atStair is at-replicas on the staircase with the assumed prefill and
// targets, and args.
func atStair(args ...string) []string {
	return slices.Concat([]string{"at-replicas", "--recording", stairBOn}, stairAssumed, args)
}

// The report's golden outputs: a P/D fleet at the load the window saw
// (under-provisioned: the TTFT target is missed there), the same as JSON, one
// over-provisioned by a decode replica at a stated load, and a decode-only
// answer where the prefill side does not price. The JSON golden is named
// .json, not -json: TestServeParity covers the plan's JSON goldens, and the
// report's parity with the API is TestAtReplicas_APIIsTheCommand.
func TestAtReplicasGolden(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
	}{
		{"at-replicas-staircase", atStair(), 0},
		{"at-replicas-staircase.json", atStair("-o", "json"), 0},
		{"at-replicas-over", atStair("--decode", "3", "--load-seen", "20"), 0},
		// V_P is refused on the sweep: the report answers for decode alone,
		// and exits 3 for the role that refused.
		{"at-replicas-decode-only", []string{"at-replicas", "--recording", outputSpreadRun, "--tpot", "5ms"}, 3},
		// Three shapes by four loads: the measured one, a short and a long
		// prompt the recording never saw (EXTRAPOLATED), at two shares of each
		// shape's knee, a stated rate and the load seen grown by a fifth. Past
		// the knee at that load the TTFT is refused, and so exit 3.
		{"at-replicas-shapes", atStair("--shape", "measured", "--shape", "1k/1k", "--shape", "16k/200/0.3",
			"--load", "50%", "--load", "80%", "--load", "5r/s", "--load", "seen", "--load-seen-x", "1.2"), 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { checkGolden(t, c.name, c.code, c.args) })
	}
}

// stairState is the staircase replayed once, to its last cycle.
func stairState(t *testing.T) *learnreplay.State {
	t.Helper()
	f := answerFlags{recording: stairBOn, cycle: "last", set: map[string]bool{}, warn: io.Discard}
	st, err := f.readState(context.Background())
	require.NoError(t, err)
	return st
}

// stairQuery is the report's question on the staircase: the assumed prefill,
// the targets, and np prefill and nd decode replicas at load.
func stairQuery(np, nd int, load float64) whatifapi.ReportQuery {
	vp, a := 200000.0, 0.020
	return whatifapi.ReportQuery{Prefill: np, Decode: nd, LoadSeen: load, TTFTPercentiles: []float64{0.9}, TPOTPercentile: 0.9,
		TTFT: &whatifapi.ReportTarget{Seconds: 2, Percentile: 0.9}, TPOT: &whatifapi.ReportTarget{Seconds: 0.030},
		Assume: whatifapi.Assume{VP: &vp, A: &a}}
}

// stairReport prices q on st.
func stairReport(t *testing.T, st *learnreplay.State, q whatifapi.ReportQuery) *whatifapi.ReportResponse {
	t.Helper()
	rep, err := buildReport(context.Background(), st, &servedState{st: st, decodeVariant: "variant-01"}, q)
	require.NoError(t, err)
	return rep
}

// verdictRank orders the verdicts: more replicas never move a fleet down it.
func verdictRank(t *testing.T, v string) int {
	t.Helper()
	switch v {
	case whatifapi.VerdictUnder:
		return 0
	case whatifapi.VerdictRight:
		return 1
	case whatifapi.VerdictOver:
		return 2
	}
	t.Fatalf("verdict %q, want one of the three", v)
	return -1
}

// The verdict is monotone in N: adding a replica of either role never moves a
// fleet from over- to right-sized, nor from right- to under-provisioned. The
// sweep must see all three verdicts, so that it is not met by one verdict
// everywhere. Every fleet is priced from one replay.
func TestAtReplicas_VerdictMonotoneInN(t *testing.T) {
	st := stairState(t)
	const maxP, maxD = 3, 5
	rank := map[[2]int]int{}
	seen := map[string]bool{}
	for np := 1; np <= maxP; np++ {
		for nd := 1; nd <= maxD; nd++ {
			rep := stairReport(t, st, stairQuery(np, nd, 20))
			rank[[2]int{np, nd}] = verdictRank(t, rep.Shapes[0].Verdict.Verdict)
			seen[rep.Shapes[0].Verdict.Verdict] = true
		}
	}
	assert.Len(t, seen, 3, "the sweep saw %v", seen)
	for np := 1; np <= maxP; np++ {
		for nd := 1; nd <= maxD; nd++ {
			r := rank[[2]int{np, nd}]
			if nd < maxD {
				assert.LessOrEqual(t, r, rank[[2]int{np, nd + 1}], "%d prefill: %d decode then %d", np, nd, nd+1)
			}
			if np < maxP {
				assert.LessOrEqual(t, r, rank[[2]int{np + 1, nd}], "%d decode: %d prefill then %d", nd, np, np+1)
			}
		}
	}
}

// The verdict names the role that can shrink, and is judged at the load, not
// at the operating point: at 20 r/s, two prefill and three decode replicas
// hold with one decode replica fewer, not with one prefill replica fewer.
func TestAtReplicas_OverProvisionedNamesTheRole(t *testing.T) {
	st := stairState(t)
	rep := stairReport(t, st, stairQuery(2, 3, 20))
	assert.Equal(t, whatifapi.VerdictOver, rep.Shapes[0].Verdict.Verdict, rep.Shapes[0].Verdict.Reason)
	assert.Equal(t, []string{roleDecode}, rep.Shapes[0].Verdict.Shrink)
	require.Len(t, rep.Shapes[0].Verdict.Checks, 3)
	for _, c := range rep.Shapes[0].Verdict.Checks {
		assert.True(t, strings.HasPrefix(c.Reason, "20.00 r/s "), "each fleet is checked at the load: %s", c.Reason)
	}
	byFleet := func(np, nd int) whatifapi.ReportCheck {
		i := slices.IndexFunc(rep.Shapes[0].Verdict.Checks, func(c whatifapi.ReportCheck) bool { return c.Prefill == np && c.Decode == nd })
		require.GreaterOrEqual(t, i, 0)
		return rep.Shapes[0].Verdict.Checks[i]
	}
	assert.True(t, byFleet(2, 3).Holds)
	assert.True(t, byFleet(2, 2).Holds, byFleet(2, 2).Reason)
	assert.False(t, byFleet(1, 3).Holds, byFleet(1, 3).Reason)

	// Two decode replicas are right-sized at that load: one fewer of either
	// role misses.
	rep = stairReport(t, st, stairQuery(2, 2, 20))
	assert.Equal(t, whatifapi.VerdictRight, rep.Shapes[0].Verdict.Verdict, rep.Shapes[0].Verdict.Reason)
	assert.Empty(t, rep.Shapes[0].Verdict.Shrink)
	// One decode replica is under-provisioned: 20 r/s is past 0.85 of its knee.
	rep = stairReport(t, st, stairQuery(2, 1, 20))
	assert.Equal(t, whatifapi.VerdictUnder, rep.Shapes[0].Verdict.Verdict, rep.Shapes[0].Verdict.Reason)
}

// The load judged at is the load the window saw by default: the mean of the
// cycles that read an arrival rate above 0.
func TestAtReplicas_LoadIsTheWindowsMean(t *testing.T) {
	st := stairState(t)
	var sum float64
	n := 0
	for _, x := range st.ArrivalRates {
		if x > 0 {
			sum += x
			n++
		}
	}
	require.Positive(t, n)
	require.Len(t, st.ArrivalRates, st.Cycles, "one rate per cycle")
	rep := stairReport(t, st, stairQuery(0, 0, 0))
	require.NotNil(t, rep.Load.Rate.Value)
	assert.InDelta(t, sum/float64(n), *rep.Load.Rate.Value, 1e-12)
	assert.Equal(t, "observed mean", rep.Load.Source)
	assert.Equal(t, n, rep.Load.CyclesWithTraffic)
	i := slices.IndexFunc(rep.Shapes[0].Table, func(r whatifapi.ReportRow) bool { return r.Label == "load seen" })
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, *rep.Load.Rate.Value, rep.Shapes[0].Table[i].Rate)
	assert.True(t, strings.HasPrefix(rep.Shapes[0].Verdict.Checks[0].Reason, rateText(sum/float64(n))+" "), rep.Shapes[0].Verdict.Checks[0].Reason)
}

// The 100% row is the knee itself: its rate is KneeRate's on the decode
// variant where decode binds, and the prefill knee's where prefill does, to
// the bit.
func TestAtReplicas_KneeRowIsTheKnee(t *testing.T) {
	st := stairState(t)
	for _, c := range []struct {
		name  string
		np    int
		binds string
	}{{"decode binds", 2, roleDecode}, {"prefill binds", 1, rolePrefill}} {
		t.Run(c.name, func(t *testing.T) {
			s := &servedState{st: st, decodeVariant: "variant-01"}
			rep, err := buildReport(context.Background(), st, s, stairQuery(c.np, 2, 20))
			require.NoError(t, err)
			i := slices.IndexFunc(rep.Shapes[0].Table, func(r whatifapi.ReportRow) bool { return r.ShareOfKnee != nil && *r.ShareOfKnee == 1 })
			require.GreaterOrEqual(t, i, 0, "a 100%% row")
			require.NotNil(t, rep.Shapes[0].Capacity.FleetKnee.Value)
			assert.Equal(t, *rep.Shapes[0].Capacity.FleetKnee.Value, rep.Shapes[0].Table[i].Rate)
			sf, err := s.fleetAt(whatifapi.Knobs{Prefill: c.np, Decode: 2, TTFTPercentile: 0.9, Headroom: 0.85,
				Assume: stairQuery(0, 0, 0).Assume})
			require.NoError(t, err)
			var knee whatif.Result = whatif.KneeRate(sf.pv.decode)
			if c.binds == rolePrefill {
				knee = whatif.PrefillKnee(sf.pf.fleet.Prefill)
			}
			k, ok := knee.(whatif.Answer)
			require.True(t, ok, "%+v", knee)
			assert.Equal(t, k.Value, rep.Shapes[0].Table[i].Rate)
			assert.Equal(t, c.binds, rep.Shapes[0].Capacity.FleetKnee.Binds)
			// At the knee the binding role is saturated: decode's TPOT, or
			// prefill's TTFT.
			fig := rep.Shapes[0].Table[i].TPOTMean
			if c.binds == rolePrefill {
				fig = rep.Shapes[0].Table[i].TTFT[0].Figure
			}
			assert.Equal(t, "saturated", figText(fig, "%.1f", 1))
		})
	}
}

// POST /v1/report is the command: the same question asked of the API gives
// the command's JSON, but for the recording's path, which the API never
// names.
func TestAtReplicas_APIIsTheCommand(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile(stairBOn)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stair.jsonl.gz"), b, 0o644))
	h := whatifapi.New(whatifapi.Config{Resolver: &servedSources{recordings: dir, warn: io.Discard}, WhatifSchema: "schema-v3"}).Handler()
	in, _ := apiInputs(t, h, "stair.jsonl.gz", "")
	code, body := apiPost(t, h, "/v1/report", whatifapi.ReportRequest{InputsID: in.InputsID,
		Fleet: &whatifapi.FleetKnob{Decode: ip(3)}, LoadSeen: fp(20), Shapes: []string{"measured", "1k/1k"},
		SLO:    &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p90", Target: fp(2)}, TPOT: &whatifapi.Objective{Statistic: statMean, Target: fp(0.030)}},
		Assume: &whatifapi.Assume{VP: fp(200000), A: fp(0.020)}})
	require.Equal(t, http.StatusOK, code, "%s", body)
	var got whatifapi.ReportResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&got))

	text, exit := runCmd(t, atStair("--decode", "3", "--load-seen", "20", "--shape", "measured", "--shape", "1k/1k", "-o", "json")...)
	require.Equal(t, 0, exit, text)
	var want whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(text), &want))
	assert.Equal(t, stairBOn, want.Source.Recording)
	want.Source.Recording = ""
	assert.Equal(t, want, got)
	assert.Equal(t, whatifapi.VerdictOver, got.Shapes[0].Verdict.Verdict)
}

// A report asked wrongly is refused before anything is priced.
func TestAtReplicas_RefusesABadQuestion(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"load 0", atStair("--load", "0r/s"), "--load"},
		{"load-seen 0", atStair("--load-seen", "0"), "--load-seen"},
		{"a shape of one length", atStair("--shape", "8k"), "--shape"},
		{"headroom 1", atStair("--headroom", "1"), "--headroom"},
		{"ttft without a unit", atStair("--ttft", "2"), "--ttft"},
		{"tpot percentile 100", atStair("--tpot-percentile", "100"), "--tpot-percentile"},
		{"a flag it does not read", atStair("--ps-onsets"), "--ps-onsets"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, code := runCmd(t, c.args...)
			assert.Equal(t, 2, code, out)
			var b bytes.Buffer
			err := run(context.Background(), c.args, &b, io.Discard)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

// shapeKnees is the fleet knee at each shape, every shape priced in one
// report from one replay.
func shapeKnees(t *testing.T, st *learnreplay.State, shapes ...string) []float64 {
	t.Helper()
	q := stairQuery(0, 0, 20)
	for _, s := range shapes {
		sh, err := whatifapi.ParseReportShape(s)
		require.NoError(t, err)
		q.Shapes = append(q.Shapes, sh)
	}
	rep := stairReport(t, st, q)
	require.Len(t, rep.Shapes, len(shapes))
	knees := make([]float64, len(shapes))
	for i, s := range rep.Shapes {
		require.NotNil(t, s.Capacity.FleetKnee.Value, "%s: %+v", s.Label, s.Capacity.FleetKnee)
		knees[i] = *s.Capacity.FleetKnee.Value
	}
	return knees
}

// The knee falls as a request grows: as IL rises at a fixed OL (the prompt
// costs prefill more and holds more KV), and as OL rises at a fixed IL
// (a request holds its slot longer). Each step must fall, not only not
// rise, so a shape that is not priced fails here.
func TestAtReplicas_KneeFallsAsTheShapeGrows(t *testing.T) {
	st := stairState(t)
	for _, c := range []struct {
		name   string
		shapes []string
	}{
		{"IL at OL 200", []string{"1k/200", "2k/200", "4k/200", "8k/200", "16k/200"}},
		{"OL at IL 8k", []string{"8k/100", "8k/200", "8k/400", "8k/800"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			knees := shapeKnees(t, st, c.shapes...)
			for i := 1; i < len(knees); i++ {
				assert.Less(t, knees[i], knees[i-1], "%s then %s", c.shapes[i-1], c.shapes[i])
			}
		})
	}
}

// A shape far from the recording's is flagged, not hidden; the measured one
// never is.
func TestAtReplicas_FarShapesAreExtrapolated(t *testing.T) {
	st := stairState(t)
	q := stairQuery(0, 0, 20)
	for _, s := range []string{"measured", "8000/200", "1k/1k"} {
		sh, err := whatifapi.ParseReportShape(s)
		require.NoError(t, err)
		q.Shapes = append(q.Shapes, sh)
	}
	rep := stairReport(t, st, q)
	assert.False(t, rep.Shapes[0].Extrapolated)
	assert.False(t, rep.Shapes[1].Extrapolated, rep.Shapes[1].ExtrapolatedWhy)
	assert.True(t, rep.Shapes[2].Extrapolated)
	assert.Contains(t, rep.Shapes[2].Flags, string(extrapolatedShape))
}
