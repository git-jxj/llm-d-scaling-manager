package whatif

import (
	"slices"
	"testing"
)

// QueueProcessorSharing prices processor sharing alone at a spread past MaxPSPromptCV and
// at bursty arrivals, where the price is otherwise the larger of the two
// queues: the mean reads no higher, and is the processor-sharing one.
func TestPS_ByEngineClearsTheSpread(t *testing.T) {
	wide := psFleet(3, 2)
	wide.Decode = nil // the prefill side alone: its knee is the fleet's
	if !wide.Prefill.BeyondProcessorSharing() {
		t.Fatal("cv 3 at Ca^2 2: beyond processor sharing without the engine's word")
	}
	by := wide
	by.Prefill.Queue = QueueChoice{Model: QueueProcessorSharing, Slots: 64}
	if by.Prefill.BeyondProcessorSharing() {
		t.Fatal("QueueProcessorSharing: processor sharing alone")
	}
	knee := PrefillKnee(wide.Prefill).(Answer).Value
	rate := 0.6 * knee
	capped, ok := TTFTAt(wide, rate, 0).(TTFT)
	if !ok {
		t.Fatalf("capped: %#v", TTFTAt(wide, rate, 0))
	}
	alone, ok := TTFTAt(by, rate, 0).(TTFT)
	if !ok {
		t.Fatalf("by engine: %#v", TTFTAt(by, rate, 0))
	}
	if !slices.Contains(capped.Flags, SpreadBeyondProcessorSharing) || slices.Contains(alone.Flags, SpreadBeyondProcessorSharing) {
		t.Errorf("flags: capped %v, by engine %v", capped.Flags, alone.Flags)
	}
	if !(alone.Seconds < capped.Seconds) {
		t.Errorf("by engine %.4f s, want under the larger of the two %.4f s", alone.Seconds, capped.Seconds)
	}
	ps := wide
	ps.Prefill.PromptMaxRatio, ps.Prefill.ArrivalCV2 = 1.5, 1
	if ps.Prefill.BeyondProcessorSharing() {
		t.Fatal("the support bound at Poisson clears processor sharing")
	}
}

// Past the slots the busiest pod runs at once, the first-come queue in front
// of them is no longer rare: the price is the larger of the two queues again,
// flagged PSSlotsExceeded. Below, processor sharing alone.
func TestPS_ByEngineFallsBackPastTheSlots(t *testing.T) {
	fl := psFleet(3, 1)
	fl.Decode = nil
	fl.Prefill.Queue = QueueChoice{Model: QueueProcessorSharing, Slots: 4}
	knee := PrefillKnee(fl.Prefill).(Answer).Value
	high, ok := TTFTAt(fl, 0.9*knee, 0).(TTFT)
	if !ok {
		t.Fatalf("%#v", TTFTAt(fl, 0.9*knee, 0))
	}
	if !slices.Contains(high.Flags, PSSlotsExceeded) {
		t.Errorf("at 0.9 of the knee on 4 slots: flags %v, want PSSlotsExceeded", high.Flags)
	}
	plain := fl
	plain.Prefill.Queue = QueueChoice{}
	capped, _ := TTFTAt(plain, 0.9*knee, 0).(TTFT)
	if high.Seconds < capped.Seconds-1e-12 {
		t.Errorf("past the slots %.4f s, want the larger of the two, %.4f s", high.Seconds, capped.Seconds)
	}
	low, ok := TTFTAt(fl, 0.2*knee, 0).(TTFT)
	if !ok || slices.Contains(low.Flags, PSSlotsExceeded) {
		t.Errorf("at 0.2 of the knee: %#v", low)
	}
}
