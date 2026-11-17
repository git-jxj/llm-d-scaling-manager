package scalefromzero

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// heldMaxAge bounds how old the steady-state engine's held publication may be
// and still count. It republishes every cycle, so a publication this old means
// it has stopped, and a hold nothing renews lapses rather than outliving it.
const heldMaxAge = 10 * time.Minute

// withoutHeld drops the variants the steady-state engine holds because no
// InferencePool selects them (decision.DefaultHeld).
func withoutHeld(vas []llmdvariant.Variant, now time.Time) []llmdvariant.Variant {
	out := vas[:0:0]
	for _, va := range vas {
		if decision.DefaultHeld.Held(va.Namespace, va.Name, heldMaxAge, now) {
			continue
		}
		out = append(out, va)
	}
	return out
}
