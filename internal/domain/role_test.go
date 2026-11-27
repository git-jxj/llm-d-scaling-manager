package domain

import (
	"strings"
	"testing"
)

func TestCanonicalRole(t *testing.T) {
	for in, want := range map[string]string{"": RoleBoth, RoleBoth: RoleBoth, RolePrefill: RolePrefill, RoleDecode: RoleDecode} {
		if got := CanonicalRole(in); got != want {
			t.Errorf("CanonicalRole(%q) = %q, want %q", in, got, want)
		}
		if got := RoleOfVC(VariantCapacity{Role: in}); got != want {
			t.Errorf("RoleOfVC(role %q) = %q, want %q", in, got, want)
		}
	}
}

// The readings of a busy pod of each role, as a P/D lab run reads them: a
// prefill pod makes one token a request and computes the prompts; a decode
// pod pulls the KV through NIXL and computes none; an aggregated pod
// computes the prompts and generates.
var (
	prefillReading = ReplicaMetrics{VariantName: "a", PodName: "a-1", AvgOutputTokens: 1,
		RequestRate: 6.7, RequestRateKnown: true, PrefillComputedTokenRate: 669, PrefillComputedTokenRateKnown: true,
		KVTransferRates: []KVTransferRate{{Kind: KVTransferNIXL}}}
	decodeReading = ReplicaMetrics{VariantName: "b", PodName: "b-1", AvgOutputTokens: 1198,
		RequestRate: 5.9, RequestRateKnown: true, PrefillComputedTokenRateKnown: true,
		KVTransferRates:   []KVTransferRate{{Kind: KVTransferNIXL, ByteRate: 2e8, SecondsRate: 0.1}},
		AvgKVTransferTime: 0.02, AvgKVTransferTimeKnown: true}
	bothReading = ReplicaMetrics{VariantName: "c", PodName: "c-1", AvgOutputTokens: 300,
		RequestRate: 2, RequestRateKnown: true, PrefillComputedTokenRate: 1800, PrefillComputedTokenRateKnown: true}
)

func TestRoleSignals_EachRoleFromItsReadings(t *testing.T) {
	// A decode pod whose engine exports no KV-transfer series still reads
	// decode: it generates and computes no prompt.
	decodeNoKV := decodeReading
	decodeNoKV.KVTransferRates, decodeNoKV.AvgKVTransferTimeKnown = nil, false
	// A decode pod that prefills short prompts locally (selective P/D)
	// computes some prompt tokens, and is read decode by its KV.
	decodeLocal := decodeReading
	decodeLocal.PrefillComputedTokenRate = 500
	// An idle aggregated pod: no request in the last minute, so its
	// computed-prompt rate of 0 says nothing.
	idle := bothReading
	idle.RequestRate, idle.PrefillComputedTokenRate = 0, 0
	sglang := bothReading
	sglang.PrefillComputedTokenRate, sglang.PrefillComputedTokenRateKnown = 0, false
	for _, c := range []struct {
		name string
		r    ReplicaMetrics
		want string
	}{
		{"prefill", prefillReading, RolePrefill},
		{"decode", decodeReading, RoleDecode},
		{"decode without KV series", decodeNoKV, RoleDecode},
		{"decode prefilling locally", decodeLocal, RoleDecode},
		{"aggregated", bothReading, RoleBoth},
		{"idle aggregated", idle, ""},
		{"no computed-prompt series", sglang, ""},
		{"no readings at all", ReplicaMetrics{}, ""},
	} {
		var s RoleSignals
		s.Observe(c.r)
		if got := s.Role(); got != c.want {
			t.Errorf("%s: role %q, want %q (%+v)", c.name, got, c.want, s)
		}
	}
}

func TestRoleSignals_MajorityOfDecidedReadings(t *testing.T) {
	var s RoleSignals
	for range 3 {
		s.Observe(prefillReading)
	}
	s.Observe(bothReading)
	s.Observe(ReplicaMetrics{})
	if s.Role() != RolePrefill {
		t.Fatalf("3 prefill readings of 4 decided: role %q", s.Role())
	}
	if got, want := s.Evidence(), "prefill at 3 of 5 readings"; !strings.HasPrefix(got, want) {
		t.Errorf("evidence %q, want it to start %q", got, want)
	}
	s.Observe(bothReading)
	s.Observe(bothReading)
	if s.Role() != "" {
		t.Errorf("3 prefill, 3 both: role %q, want none", s.Role())
	}
}

func TestRoleFromSignals_PerVariantWithEachPodNamed(t *testing.T) {
	second := decodeReading
	second.PodName = "b-2"
	got := RoleFromSignals([]ReplicaMetrics{prefillReading, decodeReading, second, bothReading, decodeReading})
	for v, want := range map[string]string{"a": RolePrefill, "b": RoleDecode, "c": RoleBoth} {
		if got[v].Role() != want {
			t.Errorf("variant %s: role %q, want %q", v, got[v].Role(), want)
		}
	}
	if n := got["b"]["b-1"].Readings; n != 2 {
		t.Errorf("b-1 read %d times, want 2", n)
	}
	ev := got["b"].Evidence()
	if !strings.HasPrefix(ev, "b-1 decode at 2 of 2 readings") || !strings.Contains(ev, "; b-2 decode at 1 of 1") {
		t.Errorf("evidence %q does not name each pod", ev)
	}
}

func TestResolveRole_LabelsWinAndDisagreementIsNoted(t *testing.T) {
	sig := RoleFromSignals([]ReplicaMetrics{decodeReading})["b"]

	role, note := ResolveRole("b", RoleDecode, true, sig)
	if role != RoleDecode || note != "" {
		t.Errorf("agreeing label: %q %q", role, note)
	}
	role, note = ResolveRole("b", RolePrefill, true, sig)
	if role != RolePrefill {
		t.Errorf("the label wins: role %q", role)
	}
	if want := "variant b: labelled prefill, engine signals read decode: b-1 decode at 1 of 1"; !strings.HasPrefix(note, want) {
		t.Errorf("disagreement note %q, want it to start %q", note, want)
	}
	role, note = ResolveRole("b", "", false, sig)
	if role != RoleDecode || !strings.HasPrefix(note, "variant b: role decode from engine signals, no role label: b-1 decode") {
		t.Errorf("unlabelled: %q %q", role, note)
	}
	role, note = ResolveRole("b", RoleBoth, false, RoleFromSignals([]ReplicaMetrics{{VariantName: "b", PodName: "b-1"}})["b"])
	if role != RoleBoth || !strings.HasPrefix(note, "variant b: no role label and engine signals read no role, read as both") {
		t.Errorf("undecided: %q %q", role, note)
	}
	if role, note = ResolveRole("b", RoleBoth, false, nil); role != RoleBoth || note != "" {
		t.Errorf("no readings: %q %q", role, note)
	}
}
