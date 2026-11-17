package saturation

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The digests a FingerprintVersion 4 controller (v0.11.2 and earlier) wrote
// for the configurations TestASnapshotFromFingerprintVersion3RestoresAsAChangeOfArguments
// uses, computed at c277d47 (version 4, the vLLM defaults already resolved):
// the snapshot the first restart after the upgrade to version 5 reads.
const (
	v4FullConfig = "707df74aeb634d34"
	v4FullArgs   = "6137831bc8562d04"
	v4PartConfig = "127d68fc4f834c5b"
	v4PartArgs   = "fb8026b37dea4849"
)

// The first restart after the upgrade from version 4: version 5 changed how
// the memory share, block size and step budget are derived (vLLM's defaults
// per release), so every stored digest differs. Each variant restores as
// after a change of arguments -- prior for a configuration read in full,
// incomplete for one read in part -- and nothing is installed as its own:
// a figure measured under the old derivation never prices the variant as its
// own.
func TestASnapshotFromFingerprintVersion4RestoresAsAChangeOfArguments(t *testing.T) {
	if capacity.FingerprintVersion != 5 {
		t.Fatalf("FingerprintVersion is %d: this test pins the upgrade from 4 to 5", capacity.FingerprintVersion)
	}
	part := func() *capacity.EngineParams {
		p := acParams(0)
		p.Unresolved = []string{"block_size"}
		return p
	}
	for name, tc := range map[string]struct {
		params       func() *capacity.EngineParams
		config, args string
		want         RestoreOutcome
	}{
		"read in full": {func() *capacity.EngineParams { return acParams(0) }, v4FullConfig, v4FullArgs, RestoredPrior},
		"read in part": {part, v4PartConfig, capacity.IncompleteArgsPrefix + v4PartArgs, RestoredIncomplete},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.params().Fingerprint() == tc.config || tc.params().ArgsFingerprint() == tc.args {
				t.Fatal("precondition: the version 4 digest is this version's")
			}
			a, _, clock := waProcess(tc.params())
			waRun(t, a, clock, 4000, 6)
			saved := a.ExportLearnedState()
			const v = "test-ns/variant-a"
			if _, ok := saved.Args[v]; !ok {
				t.Fatalf("precondition: the snapshot records the arguments: %v", saved.Args)
			}
			saved.Config[v], saved.Args[v] = tc.config, tc.args

			second, _, _ := waProcess(tc.params())
			second.RestoreLearnedState(saved)
			r, done := second.installRestored("test-ns", "test-model", "variant-a")
			if !done {
				t.Fatal("precondition: the restore was not checked")
			}
			if r.Installed || r.Outcome != tc.want || r.Priors == 0 {
				t.Errorf("installed=%v outcome=%s priors=%d, want outcome %s, nothing installed, the k2 kept as a prior",
					r.Installed, r.Outcome, r.Priors, tc.want)
			}
		})
	}
}
