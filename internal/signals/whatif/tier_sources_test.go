package whatif

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The tier load prices exactly the transfer kinds domain.KVTransferKinds
// lists, over tiers Sources lists. A reader that keeps only the code's own
// words (the recording export) reads both lists, so a kind or a tier the
// window prices and the lists miss would make it refuse every recording
// that carries it.
func TestTierKind_ReadsOnlyTheListedKindsAndSources(t *testing.T) {
	kinds := domain.KVTransferKinds()
	assert.True(t, slices.IsSorted(kinds))
	assert.True(t, slices.IsSorted(Sources()))
	assert.Len(t, tierKind, len(kinds), "a kind the window prices is not in domain.KVTransferKinds, or one listed is not priced")
	for kind, sources := range tierKind {
		assert.Contains(t, kinds, kind)
		for _, s := range sources {
			assert.Contains(t, Sources(), s)
		}
	}
	for _, s := range []string{SourceDevice, SourceHost, SourceDisk, SourceP2P, SourceExternalUnspecified} {
		assert.Contains(t, Sources(), s)
	}
}
