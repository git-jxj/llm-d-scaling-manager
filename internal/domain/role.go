package domain

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// CanonicalRole reads an empty role as RoleBoth. Every role comparison goes
// through it -- the analyzers, aggregation and the composite -- so a variant
// with no role label is keyed the same way everywhere.
func CanonicalRole(role string) string {
	if role == "" {
		return RoleBoth
	}
	return role
}

// HasPrefill reports whether rolesByVariant (variant name to role) names a
// prefill variant: the model is split into prefill and decode, and its decode
// replicas' waiting counts hold requests waiting for their KV from prefill.
// Whether the prefill variant has a Ready replica does not matter; the split
// is the model's shape.
func HasPrefill(rolesByVariant map[string]string) bool {
	for _, role := range rolesByVariant {
		if CanonicalRole(role) == RolePrefill {
			return true
		}
	}
	return false
}

// RoleOfVC is vc's role, canonicalized (CanonicalRole).
func RoleOfVC(vc VariantCapacity) string {
	return CanonicalRole(vc.Role)
}

// The thresholds a reading's role is read by (RoleSignals).
const (
	// prefillMaxOutputTokens is the most output tokens a request a prefill
	// pod is read to make. A prefill pod makes one token a request -- the
	// router sends it max_tokens=1 and hands the KV on -- so its
	// AvgOutputTokens reads 1; a pod above this generates.
	prefillMaxOutputTokens = 1.5
	// minPromptTokensComputed is the fewest prompt tokens a request an
	// engine that runs prompts computes: at least the prompt's last token,
	// even on a full prefix-cache hit. A decode pod that received the
	// prompt's KV computes none.
	minPromptTokensComputed = 1.0
)

// RoleSignals tallies what one pod's engine readings say its role is, one
// reading per row Observe is given (RoleFromSignals is the per-variant
// form). A reading reads:
//
//   - prefill: about one output token a request (AvgOutputTokens at most
//     1.5) and no KV arriving;
//   - decode: more than one output token a request, and KV arriving
//     through NIXL or under one prompt token computed a request while
//     serving (PrefillComputedTokenRate about 0);
//   - both (aggregated): more than one output token a request, prompt
//     tokens computed while serving, and no KV arriving;
//   - nothing, else: an idle pod, or an engine that does not export the
//     series (SGLang has no computed-prompt counter).
//
// The labels are the source of truth where they exist (ResolveRole); the
// signals stand in where they do not, which is how both the controller and
// the capacity planner tell an unlabelled pod's role without reading its
// name.
type RoleSignals struct {
	// Readings counts every row observed, and Prefill, Decode and Both the
	// ones that read each role.
	Readings, Prefill, Decode, Both int
	// KVArrived and NoPrompt count the decode readings by what read them:
	// KV arriving through NIXL, and no prompt computed. A reading can count
	// in both.
	KVArrived, NoPrompt int
}

// Observe adds one reading of the pod.
func (s *RoleSignals) Observe(r ReplicaMetrics) {
	s.Readings++
	kvIn := kvArrived(r)
	serving := r.RequestRateKnown && r.RequestRate > 0
	computedKnown := serving && r.PrefillComputedTokenRateKnown
	noPrompt := computedKnown && r.PrefillComputedTokenRate/r.RequestRate < minPromptTokensComputed
	oneToken := r.AvgOutputTokens > 0 && r.AvgOutputTokens <= prefillMaxOutputTokens
	generates := r.AvgOutputTokens > prefillMaxOutputTokens
	switch {
	case oneToken && !kvIn:
		s.Prefill++
	case generates && (kvIn || noPrompt):
		s.Decode++
		if kvIn {
			s.KVArrived++
		}
		if noPrompt {
			s.NoPrompt++
		}
	case generates && computedKnown:
		s.Both++
	}
}

// kvArrived reports whether KV arrived at the pod through NIXL in the
// reading: a decode pod pulls the prompt's KV from prefill, so its NIXL
// connector counts the bytes and the transfer time; the prefill pod that
// serves them counts neither.
func kvArrived(r ReplicaMetrics) bool {
	for _, t := range r.KVTransferRates {
		if t.Kind == KVTransferNIXL && t.ByteRate > 0 {
			return true
		}
	}
	return r.AvgKVTransferTimeKnown && r.AvgKVTransferTime > 0
}

// add is s with o's readings added.
func (s RoleSignals) add(o RoleSignals) RoleSignals {
	return RoleSignals{Readings: s.Readings + o.Readings, Prefill: s.Prefill + o.Prefill,
		Decode: s.Decode + o.Decode, Both: s.Both + o.Both,
		KVArrived: s.KVArrived + o.KVArrived, NoPrompt: s.NoPrompt + o.NoPrompt}
}

// Role is the role most of the readings that read one read, "" when no
// role has more than half of them.
func (s RoleSignals) Role() string {
	decided := s.Prefill + s.Decode + s.Both
	best, n := "", 0
	for _, c := range []struct {
		role string
		n    int
	}{{RolePrefill, s.Prefill}, {RoleDecode, s.Decode}, {RoleBoth, s.Both}} {
		if c.n > n {
			best, n = c.role, c.n
		}
	}
	if 2*n <= decided {
		return ""
	}
	return best
}

// Evidence names what the readings say, in one phrase: the role and at how
// many readings, and the signal that read it.
func (s RoleSignals) Evidence() string {
	switch s.Role() {
	case RolePrefill:
		return fmt.Sprintf("prefill at %d of %d readings (about 1 output token a request, no KV arriving)",
			s.Prefill, s.Readings)
	case RoleDecode:
		return fmt.Sprintf("decode at %d of %d readings (KV arrived through NIXL at %d, no prompt computed at %d)",
			s.Decode, s.Readings, s.KVArrived, s.NoPrompt)
	case RoleBoth:
		return fmt.Sprintf("both at %d of %d readings (prompt computed and more than 1 output token a request)",
			s.Both, s.Readings)
	}
	return fmt.Sprintf("no role at %d readings (prefill %d, decode %d, both %d)",
		s.Readings, s.Prefill, s.Decode, s.Both)
}

// VariantRoleSignals is one variant's RoleSignals, by Pod name.
type VariantRoleSignals map[string]RoleSignals

// RoleFromSignals tallies each variant's Pods' readings in rows, by variant
// name: one reading per row, so rows taken over several cycles read each
// Pod at each of them.
func RoleFromSignals(rows []ReplicaMetrics) map[string]VariantRoleSignals {
	out := map[string]VariantRoleSignals{}
	for _, r := range rows {
		v := out[r.VariantName]
		if v == nil {
			v = VariantRoleSignals{}
			out[r.VariantName] = v
		}
		s := v[r.PodName]
		s.Observe(r)
		v[r.PodName] = s
	}
	return out
}

// Signals is every Pod's readings together.
func (v VariantRoleSignals) Signals() RoleSignals {
	var s RoleSignals
	for _, p := range v {
		s = s.add(p)
	}
	return s
}

// Role is the role the variant's readings read together (RoleSignals.Role).
func (v VariantRoleSignals) Role() string { return v.Signals().Role() }

// Evidence names each Pod's evidence, in Pod order.
func (v VariantRoleSignals) Evidence() string {
	parts := make([]string, 0, len(v))
	for _, pod := range slices.Sorted(maps.Keys(v)) {
		parts = append(parts, pod+" "+v[pod].Evidence())
	}
	return strings.Join(parts, "; ")
}

// ResolveRole is variant's role from its role label and its engine signals.
// A label wins where there is one (labelled): the role is label, and note
// says so when the signals read another role. Without one, the signals
// decide where they read a role, and label (the caller's default) stands
// where they do not; note names the evidence either way. note is "" when
// there is nothing to say: a label the signals agree with, or no readings.
func ResolveRole(variant, label string, labelled bool, sig VariantRoleSignals) (role, note string) {
	read := sig.Role()
	switch {
	case labelled:
		if read != "" && read != CanonicalRole(label) {
			return label, fmt.Sprintf("variant %s: labelled %s, engine signals read %s: %s",
				variant, CanonicalRole(label), read, sig.Evidence())
		}
		return label, ""
	case read != "":
		return read, fmt.Sprintf("variant %s: role %s from engine signals, no role label: %s",
			variant, read, sig.Evidence())
	case sig.Signals().Readings > 0:
		return label, fmt.Sprintf("variant %s: no role label and engine signals read no role, read as %s: %s",
			variant, CanonicalRole(label), sig.Evidence())
	}
	return label, ""
}
