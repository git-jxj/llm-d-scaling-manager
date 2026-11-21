package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/internal/learnertest"
)

// The closed-loop probe's hand-written cases that are not start probes
// (closed_loop_probe_test.go), at the measured starts
// (learnertest.MeasuredStarts: 15 s, 90 s, 463 s). Every row reports the
// true start as its measured one, as the start probes' rows do, so a 90 s
// run here is not the hand-written case's run, whose rows report none and
// price the default start.
//
// TestClosedLoopProbe_ARestoredFirstSightDoesNotTakeAFleetBelowNeed is not
// run here: its fleet orders need on occupancy alone once no replica queues,
// and no threshold tried moved it (the drain time, the first-read haircut,
// steadyFor, the scale-up target, the stabilization window), so at each start
// it would pass whatever changed. Its own comment says it holds the outcome,
// not the bound. Measured at 15 s, 90 s and 463 s it stays at 17 for a need
// of 17 through the restart, restored or waiting.
//
// A case with a load step is judged at verdictHorizon, as the start probes
// are: at 463 s the decode step's backlog clears at 44m9s, past its 40 min
// horizon.

// atStart is pc with its replicas starting in start, judged at
// verdictHorizon for a load step at step (zero for none).
func atStart(pc probeCase, step, start time.Duration) probeCase {
	pc.start = start
	pc.horizon = verdictHorizon(pc, step, start)
	return pc
}

// coldStart is the slowest of learnertest.MeasuredStarts: a cold JIT cache.
const coldStart = 463 * time.Second

// decodeStepAt is a restartStepCase at start (atStart). At coldStart it runs
// on the even split, as TestClosedLoopProbe_StartingReplicasAreCreditedNotReordered
// does. The difference, named: at the shortest-queue limit the decode step
// ends at 23 replicas for a need of 17 at a 463 s start, restart or not,
// over the 20.5 bound at the backlog allowance (the
// liveness/floor/closed-loop-*-start-463s-jsq ledger rows,
// ev-shindin/llm-scaling-manager#520), so a case there restates those rows.
func decodeStepAt(pc probeCase, start time.Duration) probeCase {
	pc.queueBlind = pc.queueBlind || start >= coldStart
	return atStart(pc, restartStepAt, start)
}

// TestClosedLoopProbe_ASteeringRouterKeepsARightMinorityFigure, at each start.
//
// Measured: peak 23, 29 and 26 at 15 s, 90 s and 463 s for a need of 17,
// final 18, 19 and 18; never under need.
func TestClosedLoopProbeAtStarts_ASteeringRouterKeepsARightMinorityFigure(t *testing.T) {
	learnertest.AtMeasuredStarts(t, func(t *testing.T, start time.Duration) {
		r := runProbe(t, atStart(steeredCase(), 0, start))
		t.Logf("need %d, final %d, peak %d, longest under need %s; fleet %s", r.need, r.final, r.peak, r.underFor, traceSummary(r.trace))
		require.Empty(t, r.verdict())
		require.Zero(t, r.underFor, "the fleet never sat under need")
		require.Positive(t, r.muCounts[muReasonSampled], "premise: the steered replicas were read")
	})
}

// TestClosedLoopProbe_AControllerRestartBeforeALoadStepSettlesAtNeed, with
// the queue priced by drain time (the default), at each start.
//
// The difference, named: at 15 s and 90 s the restart costs the step no time
// under need, and at 463 s it costs one cycle (15 s under need, against none
// without the restart; final 19 and peak 22 for a need of 17), the figure
// analyzer-learner.md's start-probe table records for restart-before-step at
// 463 s.
func TestClosedLoopProbeAtStarts_AControllerRestartBeforeALoadStepSettlesAtNeed(t *testing.T) {
	learnertest.AtMeasuredStarts(t, func(t *testing.T, start time.Duration) {
		control := runProbe(t, decodeStepAt(restartStepCase(0, false, 0), start))
		require.Empty(t, control.verdict(), "premise: without a restart the step settles at need")
		r := runProbe(t, decodeStepAt(restartStepCase(15*time.Minute, false, 0), start))
		t.Logf("need %d, final %d, peak %d; under need for %s, against %s without the restart",
			r.need, r.final, r.peak, r.underFor, control.underFor)
		require.Empty(t, r.verdict())
		allowance := time.Duration(0)
		if start >= coldStart {
			allowance = propCycle
		}
		require.LessOrEqual(t, r.underFor, control.underFor+allowance, "the first read prices the step the restart left without a mu")
	})
}

// TestClosedLoopProbe_ARestoredFirstSightCarriesTheStep, at each start.
func TestClosedLoopProbeAtStarts_ARestoredFirstSightCarriesTheStep(t *testing.T) {
	learnertest.AtMeasuredStarts(t, func(t *testing.T, start time.Duration) {
		control := decodeStepAt(restartStepCase(15*time.Minute, true, 0), start)
		base := runProbe(t, control)
		withSight := control
		withSight.sightings = true
		r := runProbe(t, withSight)
		cycles, lower, higher := traceDelta(r.trace, base.trace)
		t.Logf("need %d, final %d, peak %d; under need for %s, against %s with the snapshot alone; fleet differs on %d cycles, lower by %d at most, higher by %d",
			r.need, r.final, r.peak, r.underFor, base.underFor, cycles, lower, higher)
		require.EqualValues(t, 4, r.sightings, "premise: every replica's first sight came back")
		require.Empty(t, r.verdict())
		require.LessOrEqual(t, r.underFor, base.underFor, "skipping the wait never leaves the fleet under need for longer")
	})
}

// TestClosedLoopProbe_AShortStabilizationThroughARestartSettlesAtNeed, at
// each start.
func TestClosedLoopProbeAtStarts_AShortStabilizationThroughARestartSettlesAtNeed(t *testing.T) {
	learnertest.AtMeasuredStarts(t, func(t *testing.T, start time.Duration) {
		long := runProbe(t, decodeStepAt(restartStepCase(15*time.Minute, false, 0), start))
		short := runProbe(t, decodeStepAt(restartStepCase(15*time.Minute, false, 2*time.Minute), start))
		t.Logf("need %d, final %d, peak %d; under need for %s at 120 s, %s at 300 s", short.need, short.final, short.peak,
			short.underFor, long.underFor)
		require.Empty(t, short.verdict())
		require.Equal(t, long.underFor, short.underFor)
	})
}

// TestClosedLoopProbe_ASlowReplicaInsideTheServedBoundStaysUnderTheBound's
// negative control, at each start: with the unread replicas not credited
// (SwitchMuUnreadAtTarget off) the slow period takes the fleet over the
// bound at every start.
//
// The difference, named: at 15 s and 90 s the low figure then seals the
// fleet (final 23 against a bound of 21.7), and at 463 s it does not
// (final 20): the order that lands 463 s later meets a replica that has
// recovered, and the figure is rewritten before the fleet settles. The
// assertions with the credit on are the start probes'
// (liveness/mu/closed-loop-slow-pod-055-start-*).
func TestClosedLoopProbeAtStarts_WithoutTheUnreadCreditASlowReplicaTakesTheFleetOverTheBound(t *testing.T) {
	learnertest.AtMeasuredStarts(t, func(t *testing.T, start time.Duration) {
		noCredit := slowReplicaCase(0.55)
		noCredit.switches = map[string]bool{SwitchMuUnreadAtTarget: false}
		c := runProbe(t, atStart(noCredit, 0, start))
		bound := overBound(c.need)
		t.Logf("unread replicas at what they serve: need %d, final %d, peak %d (bound %.1f); %s", c.need, c.final, c.peak, bound, c.verdict())
		require.Greater(t, float64(c.peak), bound, "the credit is what keeps the slow period under the bound")
	})
}

// steeredCase is TestClosedLoopProbe_ASteeringRouterKeepsARightMinorityFigure's
// fleet: eighteen replicas at mu 1 under 12 req/s, four of them sent twice
// the share by a queue-blind router, and a lull at 2 req/s from 10 to 45 min.
func steeredCase() probeCase {
	pods := make([]float64, 18)
	for i := range pods {
		pods[i] = 1
	}
	return probeCase{
		name: "steered", pods: pods, addedMu: 1, meanMu: 1,
		lambda: func(e time.Duration) float64 {
			if e >= 10*time.Minute && e < 45*time.Minute {
				return 2
			}
			return 12
		},
		weight: func(i int, _ time.Duration) float64 {
			if i < 4 {
				return 2
			}
			return 1
		},
		queueBlind: true,
		horizon:    60 * time.Minute,
	}
}
