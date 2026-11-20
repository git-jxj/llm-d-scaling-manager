package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A start that overlapped a rollout of its variant is not folded
// (SwitchStartHeldThroughRollout, issue #220). A DisaggregatedSet rollout
// behind a drain can run for hours; a scale-up during it, and the new
// revision's first starts, say how long the rollout took, not how long a
// replica of the variant takes to start. Each test runs the switch on and
// off and asserts both figures, so each fails with the switch off.

// rolloutAnalyzer is confirmAnalyzer's: a measured 70 s from Pod d0, with
// SwitchStartHeldThroughRollout set to on.
func rolloutAnalyzer(t *testing.T, on bool) *SaturationAnalyzer {
	t.Helper()
	a := confirmAnalyzer(t, true)
	if err := a.SetSwitchesForReplay(map[string]bool{SwitchStartHeldThroughRollout: on}); err != nil {
		t.Fatal(err)
	}
	return a
}

// rolloutCycle is one noteReplicaStartFor for confirmKey, the clock 30 s on,
// the variant's rows reporting two revisions or not.
func rolloutCycle(a *SaturationAnalyzer, rolling bool, rows ...domain.ReplicaMetrics) {
	at := a.now().Add(30 * time.Second)
	a.now = func() time.Time { return at }
	a.noteReplicaStartFor(startNote{key: confirmKey, namespace: "ns", variant: confirmVariant, revisionsMixed: rolling},
		rows, logr.Discard())
}

// readyRow is Pod u1, a scale-up Ready at ready, reporting start seconds.
func readyRow(start float64, ready time.Time) domain.ReplicaMetrics {
	rm := confirmReplica("u1", start)
	rm.ReadySince = ready
	return rm
}

// A scale-up that becomes Ready while the variant rolls out reports 200 s.
// On, the estimate stays at 70, and the Pod stays counted: the cycle after
// the rollout ended does not fold it either. Off, it is folded (0.7*70 +
// 0.3*200 = 109).
func TestStartRollout_AStartDuringARolloutIsNotFolded(t *testing.T) {
	for _, on := range []bool{true, false} {
		a := rolloutAnalyzer(t, on)
		at := a.now().Add(30 * time.Second)
		rolloutCycle(a, true, confirmReplica("d0", 70), readyRow(200, at))
		rolloutCycle(a, false, confirmReplica("d0", 70), readyRow(200, at))
		want := 70.0
		if !on {
			want = 109
		}
		if got := a.startSeconds[confirmKey]; !near(got, want) {
			t.Fatalf("switch %t: estimate = %v, want %v", on, got, want)
		}
	}
}

// A Pod created while the variant rolled out and Ready only after it ended:
// created before the last cycle that saw the rollout, so its start overlapped
// it. On, not folded; off, folded.
func TestStartRollout_APodCreatedDuringARolloutIsNotFoldedAfterIt(t *testing.T) {
	for _, on := range []bool{true, false} {
		a := rolloutAnalyzer(t, on)
		rolloutCycle(a, true, confirmReplica("d0", 70))
		rolloutCycle(a, false, confirmReplica("d0", 70))
		// Ready now, 200 s after it was created: a minute before the
		// rollout's last cycle.
		ready := a.now().Add(30 * time.Second)
		rolloutCycle(a, false, confirmReplica("d0", 70), readyRow(200, ready))
		want := 70.0
		if !on {
			want = 109
		}
		if got := a.startSeconds[confirmKey]; !near(got, want) {
			t.Fatalf("switch %t: estimate = %v, want %v", on, got, want)
		}
	}
}

// The control: a Pod created after the rollout's last cycle is a start of
// the variant, and is folded either way.
func TestStartRollout_APodCreatedAfterARolloutIsFolded(t *testing.T) {
	for _, on := range []bool{true, false} {
		a := rolloutAnalyzer(t, on)
		rolloutCycle(a, true, confirmReplica("d0", 70))
		for range 10 {
			rolloutCycle(a, false, confirmReplica("d0", 70))
		}
		// Created 100 s ago: five minutes after the rollout's last cycle.
		ready := a.now().Add(30 * time.Second)
		rolloutCycle(a, false, confirmReplica("d0", 70), readyRow(100, ready))
		if got, want := a.startSeconds[confirmKey], 0.7*70+0.3*100; !near(got, want) {
			t.Fatalf("switch %t: estimate = %v, want %v", on, got, want)
		}
	}
}

// The mark of a rollout is dropped once no Pod created before it can still
// report a credible start, so a variant that rolled once does not keep it.
func TestStartRollout_TheMarkIsDroppedWhenItCanNoLongerMatter(t *testing.T) {
	a := rolloutAnalyzer(t, true)
	rolloutCycle(a, true, confirmReplica("d0", 70))
	if _, ok := a.startRolledAt[confirmKey]; !ok {
		t.Fatal("the rollout was not marked")
	}
	for range int(maxRestoredStartSeconds/30) + 1 {
		rolloutCycle(a, false, confirmReplica("d0", 70))
	}
	if at, ok := a.startRolledAt[confirmKey]; ok {
		t.Fatalf("the mark of %v is kept past %v s", at, maxRestoredStartSeconds)
	}

	off := rolloutAnalyzer(t, false)
	rolloutCycle(off, true, confirmReplica("d0", 70))
	if len(off.startRolledAt) != 0 {
		t.Fatalf("switch off: %v marked, want nothing kept", off.startRolledAt)
	}
}

// The wiring: Analyze reads the variant as rolling out when its replicas
// report two revisions, or when its scale target says so, and the start of
// a replica that reports then is not folded. Off, it is.
func TestStartRollout_AnalyzeHoldsStartsWhileRollingOut(t *testing.T) {
	states := []domain.VariantReplicaState{{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 2, GPUsPerReplica: 1}}
	run := func(on, storeRolling bool, revA, revB string) int {
		a, store, _ := waProcess(mtParams(256, "vllm:v1"))
		if err := a.SetSwitchesForReplay(map[string]bool{SwitchStartHeldThroughRollout: on}); err != nil {
			t.Fatal(err)
		}
		r4Rolling(store, mtParams(256, "vllm:v1"), storeRolling)
		// Two cycles, the starts reported on the second: the scale target
		// marks a rollout on its second cycle.
		for _, start := range []float64{0, 90} {
			ra := makeReplicaMetrics("pod-a", "variant-a", 4000, 16000, 6, 100, 50)
			ra.Ready, ra.StartSeconds, ra.Revision = true, start, revA
			rb := makeReplicaMetrics("pod-b", "variant-a", 4000, 16000, 6, 100, 50)
			rb.Ready, rb.StartSeconds, rb.Revision = true, start, revB
			if _, err := analyzeConfirmed(context.Background(), a, makeAnalyzerInput([]domain.ReplicaMetrics{ra, rb}, states)); err != nil {
				t.Fatal(err)
			}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.startSeconds)
	}
	for _, tc := range []struct {
		name         string
		storeRolling bool
		revA, revB   string
	}{
		{name: "two revisions", revA: "rev-1", revB: "rev-2"},
	} {
		if n := run(true, tc.storeRolling, tc.revA, tc.revB); n != 0 {
			t.Errorf("%s, switch on: %d start estimates, want none folded", tc.name, n)
		}
		if n := run(false, tc.storeRolling, tc.revA, tc.revB); n != 1 {
			t.Errorf("%s, switch off: %d start estimates, want the starts folded", tc.name, n)
		}
	}
	if n := run(true, false, "rev-1", "rev-1"); n != 1 {
		t.Errorf("one revision, not rolling out: %d start estimates, want the starts folded", n)
	}
	// The scale target's status alone does not hold a start: it also reads
	// rolling out while a scale write is not yet observed, and while a
	// LeaderWorkerSet-backed role's new group starts -- the starts to learn.
	if n := run(true, true, "rev-1", "rev-1"); n != 1 {
		t.Errorf("one revision, the scale target rolling out: %d start estimates, want the starts folded", n)
	}
}

// After a rollout, a row with no Ready time cannot place its start: held,
// since read from now a Pod created during the rollout would pass.
func TestStartRollout_ARowWithNoReadyTimeIsHeldAfterARollout(t *testing.T) {
	for _, on := range []bool{true, false} {
		a := rolloutAnalyzer(t, on)
		rolloutCycle(a, true, confirmReplica("d0", 70))
		rolloutCycle(a, false, confirmReplica("d0", 70), confirmReplica("u1", 200))
		want := 70.0
		if !on {
			want = 109
		}
		if got := a.startSeconds[confirmKey]; !near(got, want) {
			t.Fatalf("switch %t: estimate = %v, want %v", on, got, want)
		}
	}
}

// A variant held from its first start has a rollout mark and no estimate;
// the sweep drops the mark when the variant goes stale.
func TestStartRollout_TheSweepDropsAMarkWithNoEstimate(t *testing.T) {
	a := rolloutAnalyzer(t, true)
	const key = "ns|model|other|H100|1"
	a.noteReplicaStartFor(startNote{key: key, namespace: "ns", variant: "other", revisionsMixed: true}, nil, logr.Discard())
	if _, ok := a.startRolledAt[key]; !ok {
		t.Fatal("the rollout was not marked")
	}
	if _, ok := a.startSeconds[key]; ok {
		t.Fatal("precondition: the key has no estimate")
	}
	later := a.now().Add(30 * 24 * time.Hour)
	a.now = func() time.Time { return later }
	a.EvictStaleHistory()
	if _, ok := a.startRolledAt[key]; ok {
		t.Fatal("a stale key's rollout mark survived the sweep")
	}
}
