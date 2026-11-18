package scalefromzero

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// heldMaxAge bounds how old the steady-state engine's routing publication may
// be and still count. It republishes every cycle for every model it knows, so
// a publication this old means it has stopped, and a hold nothing renews
// lapses rather than outliving it.
const heldMaxAge = 10 * time.Minute

// routedPopulation applies the steady-state engine's routing verdict
// (decision.DefaultHeld) to the variants this engine reads. wakeable drops the
// parked variants that are held: the router cannot reach them, so waking one
// serves nothing. covering keeps the running variants that cover their model:
// in a routed model only one a pool selects, so a model whose routed variants
// are all at zero still wakes while a held canary runs. A model with no
// routing published is read as before.
func routedPopulation(inactive, active []llmdvariant.Variant, now time.Time) (wakeable, covering []llmdvariant.Variant) {
	wakeable = make([]llmdvariant.Variant, 0, len(inactive))
	for _, va := range inactive {
		if decision.DefaultHeld.Held(va.Namespace, va.Spec.ModelID, va.Name, heldMaxAge, now) {
			continue
		}
		wakeable = append(wakeable, va)
	}
	covering = make([]llmdvariant.Variant, 0, len(active))
	for _, va := range active {
		if !decision.DefaultHeld.Covers(va.Namespace, va.Spec.ModelID, va.Name, heldMaxAge, now) {
			continue
		}
		covering = append(covering, va)
	}
	return wakeable, covering
}
