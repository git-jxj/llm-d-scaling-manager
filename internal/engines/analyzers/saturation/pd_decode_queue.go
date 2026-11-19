package saturation

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// decodeCapacityView returns rm, a replica of a disaggregated model, with its
// waiting queue as decode capacity reads it if it is a decode replica; any
// other role's row is returned as it is. decodeCapacityViews decides whether
// the model is disaggregated.
//
// vLLM counts a decode request whose KV is still in flight from prefill in
// num_requests_waiting. That request has its blocks and is waiting on the
// network, not on this replica, so it is not a backlog this replica's
// capacity drains: read as one, a decode at KV 0.5 with 768 such requests
// tripped the saturation test, recorded a k2 at half its cache, and was
// charged the 768 as queue demand and floor backlog.
//
//   - Where vLLM says why each request waits (CapacityQueueKnown, v0.20.0
//     and later), the queue is the part waiting for scheduling capacity
//     (reason="capacity"). A request that cannot get blocks for its pull
//     waits there, so a full decode still reads queued.
//   - Where vLLM also says which deferred requests already have their KV
//     (LandedQueueKnown, v0.32.0 and later: the "completed_waiting" stage
//     of num_requests_kv_fetch_by_stage), those are added: their pull is
//     done and they wait for this replica to schedule them, which the
//     scheduler does only inside its waiting loop -- a loop that stops at
//     max_num_seqs, whether or not that cap was stated, and when a block
//     cannot be allocated. So below a stated cap, at a default one, and on
//     a full decode alike, a landed pull is a backlog this replica drains.
//     Capped at the full count, for two reasons. The two are one-minute
//     peaks, which need not fall in the same instant. And a landed request
//     the scheduler has promoted but not yet scheduled may be counted in
//     both the capacity part and completed_waiting; every landed request is
//     one of the waiting ones, so min(full, ...) bounds that double count.
//     Without the series the queue is the capacity part alone, as before.
//   - Except at the sequence cap (maxNumSeqs, the engine's max_num_seqs
//     when its flag or the llm-d.ai/max-num-seqs annotation states it,
//     never a default or a learned cap: atCapBatchCap, and
//     domain.ReplicaMetrics.AtBatchCap): there the queue is the full
//     count. vLLM keeps a request deferred from the moment its pull starts
//     until it is scheduled, and the scheduler promotes a landed pull only
//     when it looks at it, which it stops doing once the batch is at
//     max_num_seqs. So at the cap every request whose pull began
//     below it reads deferred, landed or not, and each of them waits for a
//     batch slot on this replica. Read as the capacity part alone, a capped
//     decode's backlog -- the one ev-shindin/llm-scaling-manager#287 measured at 256 running and 768
//     waiting -- would record no k2 and charge no queue demand.
//   - Where it does not, the queue is the full count, as before. Which part
//     of it is in flight cannot be told, and reading too little of it is the
//     direction that breaks SLOs: a decode capped by --max-num-seqs below a
//     full cache queues for real, and dropping that queue would record no k2
//     and leave its capacity at k1.
//
// Which readers take the view, and which keep the raw row, follows the same
// asymmetry: each takes the reading that errs toward stricter learning and
// toward keeping capacity.
//
//   - The view prices the replica's own capacity and demand -- the saturation
//     test and k2 record (computeK2), the k2 settle gate, the throughput
//     admission's unqueued test (servedBounds), the local queue demand
//     (waitingQueueDemand), the floor's backlog (ReplicaCapacity.QueueLength)
//     and the idle admission (admissionOf), where a row reading idle records
//     nothing.
//   - The raw row is kept where a deferred request counted as waiting only
//     holds more: decode's saturation as prefill sees it (roleSaturated,
//     which gates prefill's learning and holds its demand), the busiest-queue
//     load that keeps a replica from being given away
//     (busiestQueueLoadByVariant), and the idle tests (replicaIdle,
//     kvFromInstant), since a request in flight to a replica is load on it.
//     roleSaturated also requires decode's KV full (kvFull), and on a full
//     decode a landed pull that cannot be scheduled reads deferred too (it
//     is promoted only when it is scheduled), so the view there would clear a
//     decode that is genuinely full and queued and let prefill learn decode's
//     back-pressure as its own k2.
//
// Aggregated replicas, prefill, and decode of a model with no prefill variant
// serve whole requests, and their queue stays the full count.
func decodeCapacityView(rm domain.ReplicaMetrics, role string, maxNumSeqs int64) domain.ReplicaMetrics {
	if domain.CanonicalRole(role) != domain.RoleDecode || !rm.CapacityQueueKnown ||
		rm.AtBatchCap(maxNumSeqs) {
		return rm
	}
	full, fullBusiest := rm.QueueLength, rm.BusiestRankQueue()
	rm.MaxRankQueueLength = rm.BusiestRankCapacityQueue()
	rm.QueueLength = rm.CapacityQueueLength
	if rm.LandedQueueKnown {
		rm.QueueLength = min(full, rm.QueueLength+rm.LandedQueueLength)
		rm.MaxRankQueueLength = min(fullBusiest, rm.MaxRankQueueLength+rm.BusiestRankLandedQueue())
	}
	return rm
}

// decodeCapacityViews is decodeCapacityView over rows when the model is
// disaggregated (domain.HasPrefill), and rows as they are when it is not.
// maxNumSeqs is each variant's stated max_num_seqs (atCapBatchCap: zero, or
// absent, when neither its flag nor its annotation states one).
func decodeCapacityViews(rows []domain.ReplicaMetrics, rolesByVariant map[string]string,
	maxNumSeqs map[string]int64) []domain.ReplicaMetrics {
	if !domain.HasPrefill(rolesByVariant) {
		return rows
	}
	out := make([]domain.ReplicaMetrics, len(rows))
	for i, rm := range rows {
		out[i] = decodeCapacityView(rm, rolesByVariant[rm.VariantName], maxNumSeqs[rm.VariantName])
	}
	return out
}

// SwitchDecodeLandedNeedsABound is the replay switch for a landed request on
// a decode replica that has room (issue
// llm-d-incubation/llm-d-scaling-manager#201).
//
// decodeCapacityView adds the landed requests to the queue because the
// scheduler promotes a landed pull only inside its waiting loop, which stops
// at max_num_seqs and when a block cannot be allocated. A replica below its
// stated cap on every rank, with its busiest rank's KV under the threshold,
// stops at neither: a landed request waits for the next step, and the
// one-minute peak of that wait is a burst of arrivals, not a backlog. Read as
// one, five landed requests on a decode rank at 24 % KV, its pod running 9
// sequences against a cap of 64 per rank, met the queue threshold of 5,
// recorded a k2 at 24 % of the cache, and priced the variant there for the
// rest of the day.
//
// On, such a replica's queue is the capacity part alone, as without the
// landed series. Where the cap is not stated, the running count is not read,
// or the busiest rank's KV is at or over the threshold, the landed requests
// stay in, as before. Off, as before.
//
// Off from the constructor: it moves what decode records and charges, and
// the default changes only after review.
const SwitchDecodeLandedNeedsABound = "decode-landed-queue-needs-a-bound"

// landedWithoutBound returns view, decodeCapacityView's reading of raw, with
// the landed requests taken back out where raw cannot be at a bound
// (SwitchDecodeLandedNeedsABound): a decode of a P/D model whose stated cap
// maxNumSeqs is above its running count summed over ranks -- so no rank is at
// it -- and whose busiest rank's KV is under kvThreshold. Any other row is
// view unchanged.
func landedWithoutBound(view, raw domain.ReplicaMetrics, role string, maxNumSeqs int64, kvThreshold float64) domain.ReplicaMetrics {
	if domain.CanonicalRole(role) != domain.RoleDecode || !raw.CapacityQueueKnown || !raw.LandedQueueKnown ||
		maxNumSeqs <= 0 || !raw.RunningRequestsKnown || int64(raw.RunningRequests) >= maxNumSeqs ||
		!(raw.BusiestRankKvUsage() < kvThreshold) {
		return view
	}
	view.QueueLength = raw.CapacityQueueLength
	view.MaxRankQueueLength = raw.BusiestRankCapacityQueue()
	return view
}
