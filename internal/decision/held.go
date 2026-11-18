package decision

import (
	"sync"
	"time"
)

// HeldStore holds, per model, what the steady-state engine established about
// which of its variants an InferencePool selects (docs/proposals/
// pool-selected-supply.md): the selected ones, and the held ones -- no pool
// selects them while another variant of the model is routed.
//
// The scale-from-zero engine reads it. In a routed model only a selected
// variant covers the model: a held one running does not, or a model whose
// routed variants are all at zero would never wake. And a held variant is
// never woken, because the router cannot reach it.
type HeldStore struct {
	mu sync.RWMutex
	// by is namespace/model -> its routing.
	by map[string]modelRouting
}

type modelRouting struct {
	held, selected map[string]bool
	at             time.Time
}

// NewHeldStore returns an empty store.
func NewHeldStore() *HeldStore {
	return &HeldStore{by: map[string]modelRouting{}}
}

// DefaultHeld is the store the steady-state engine writes and scale from zero
// reads.
var DefaultHeld = NewHeldStore()

func modelKey(namespace, model string) string { return namespace + "/" + model }

func setOf(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// Publish replaces a model's routing: the variants a pool selects and the
// held ones, by ScaledObject name. Publish only for a routed model; a model
// with neither list is cleared, which reads as "not routed".
func (s *HeldStore) Publish(namespace, model string, held, selected []string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by == nil {
		s.by = map[string]modelRouting{}
	}
	key := modelKey(namespace, model)
	if len(held) == 0 && len(selected) == 0 {
		delete(s.by, key)
		return
	}
	s.by[key] = modelRouting{held: setOf(held), selected: setOf(selected), at: at}
}

// routing returns a model's routing when it is no older than maxAge (0 is no
// bound). A stale publication is none: the engine that would renew it has
// stopped, and a hold nothing renews must not outlive it.
func (s *HeldStore) routing(namespace, model string, maxAge time.Duration, now time.Time) (modelRouting, bool) {
	r, ok := s.by[modelKey(namespace, model)]
	if !ok || (maxAge > 0 && now.Sub(r.at) > maxAge) {
		return modelRouting{}, false
	}
	return r, true
}

// Held reports whether a variant of a model is held.
func (s *HeldStore) Held(namespace, model, variant string, maxAge time.Duration, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.routing(namespace, model, maxAge, now)
	return ok && r.held[variant]
}

// Covers reports whether a running variant counts as covering its model: in
// a routed model only a variant a pool selects does; in a model with no
// routing published, every variant does, as before.
func (s *HeldStore) Covers(namespace, model, variant string, maxAge time.Duration, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.routing(namespace, model, maxAge, now)
	return !ok || r.selected[variant]
}

// Reset drops every publication. For tests.
func (s *HeldStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.by = map[string]modelRouting{}
}
