package decision

import (
	"sync"
	"time"
)

// HeldStore holds, per model, the variants the steady-state engine holds
// because no InferencePool selects them while another variant of the model is
// routed (docs/proposals/pool-selected-supply.md).
//
// The scale-from-zero engine reads it: a held variant must not count as the
// model's running decode, or a model whose routed variants are all at zero is
// never woken, and it must not be woken itself, because the router cannot
// reach it.
type HeldStore struct {
	mu sync.RWMutex
	// by is namespace/model -> the held variants, by ScaledObject name.
	by map[string]heldSet
}

type heldSet struct {
	namespace string
	variants  map[string]bool
	at        time.Time
}

// NewHeldStore returns an empty store.
func NewHeldStore() *HeldStore {
	return &HeldStore{by: map[string]heldSet{}}
}

// DefaultHeld is the store the steady-state engine writes and scale from zero
// reads.
var DefaultHeld = NewHeldStore()

// Publish replaces a model's held variants. An empty list clears the model.
func (s *HeldStore) Publish(namespace, model string, variants []string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by == nil {
		s.by = map[string]heldSet{}
	}
	key := namespace + "/" + model
	if len(variants) == 0 {
		delete(s.by, key)
		return
	}
	set := heldSet{namespace: namespace, variants: make(map[string]bool, len(variants)), at: at}
	for _, v := range variants {
		set.variants[v] = true
	}
	s.by[key] = set
}

// Held reports whether a variant is held, by a publication no older than
// maxAge (0 is no bound). A stale publication answers false: the engine that
// would refresh it has stopped, and a hold nothing renews must not outlive it.
func (s *HeldStore) Held(namespace, variant string, maxAge time.Duration, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, set := range s.by {
		if set.namespace != namespace || !set.variants[variant] {
			continue
		}
		return maxAge <= 0 || now.Sub(set.at) <= maxAge
	}
	return false
}

// Reset drops every publication. For tests.
func (s *HeldStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.by = map[string]heldSet{}
}
