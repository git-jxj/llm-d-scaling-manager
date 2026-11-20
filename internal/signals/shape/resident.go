package shape

// The KV a running request is measured to hold, priced against a shape
// (issues llm-d-incubation/llm-d-scaling-manager#187 and #188). The capacity
// planner and the saturation analyzer read the same figure, the tokens in use
// over the running requests, and price it by one rule, PriceResident:
//
//   - below the shape's KVreq, the prompt discounted by the prefix-cache hit
//     rate, it is held at KVreq. Below it is requests part way through their
//     prefill, or a spread of output lengths D under-states, not more sharing
//     than the hit rate allows.
//   - above the whole context, IL + D x OL/2, it is priced as read. An engine
//     can hold more per request than its prompt and output (a sparse-attention
//     indexer's keys, a speculative draft model's KV) in the same pool, and a
//     cap at the context under-prices the request by that factor.
//   - except on an engine whose reading over-states above the context
//     (SGLang: the busiest rank's usage and running count against the pod's
//     whole capacity, so about the rank count high on data-parallel
//     attention), where it is held at the context.
//
// A reading above AboveContextFactor times the context is priced as read and
// marked, so a reader sees it.

// AboveContextFactor is how far above the whole context a reading is before
// PriceResident marks it (ResidentAboveContext).
const AboveContextFactor = 2

// ResidentPrice is which rule priced a resident reading (PriceResident).
type ResidentPrice int

const (
	// ResidentAsRead is a reading from the shape's KVreq up to
	// AboveContextFactor times the whole context, priced as read.
	ResidentAsRead ResidentPrice = iota
	// ResidentAtDiscount is a reading below the shape's KVreq, priced at it.
	ResidentAtDiscount
	// ResidentAboveContext is a reading above AboveContextFactor times the
	// whole context, priced as read.
	ResidentAboveContext
	// ResidentHeldAtContext is a reading above the whole context on an engine
	// whose reading over-states there, priced at the context.
	ResidentHeldAtContext
)

// Context is the KV a resident request of s holds with no prefix-cache hit:
// IL + D x OL/2, s's spread of output lengths kept. A literal's
// OutputDispersion of 0 reads as 1 (WithOutputDispersion).
func (s Shape) Context() float64 {
	return New(s.AvgInputTokens, s.AvgOutputTokens, 0).WithOutputDispersion(s.OutputDispersion).KVreq
}

// PriceResident is the KV a running request of s is priced at, given the KV
// one was measured to hold, and which rule priced it (the file comment).
// heldAtContext says the engine's reading over-states above the context
// (SGLang). measured is taken as given: the caller decides whether it is a
// reading at all.
func (s Shape) PriceResident(measured float64, heldAtContext bool) (float64, ResidentPrice) {
	whole := s.Context()
	switch {
	case measured < s.KVreq:
		return s.KVreq, ResidentAtDiscount
	case heldAtContext && measured > whole:
		return whole, ResidentHeldAtContext
	case measured > AboveContextFactor*whole:
		return measured, ResidentAboveContext
	default:
		return measured, ResidentAsRead
	}
}
