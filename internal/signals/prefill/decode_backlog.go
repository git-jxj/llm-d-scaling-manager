package prefill

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A decode backlog: the requests a decode pod holds waiting on its own
// capacity. A decode engine's TTFT read while one stands is the queue's wait,
// not what the engine takes at low load: on the staircase recordings it read
// 9 to 17 s after an overload, against about 0.12 s at low load. The capacity
// planner refuses such a figure (whatif.DecodeTTFTUnderLoad) and the SLO
// analyzer, behind its switch, prices a calm one instead
// (slo.SwitchTTFTLowLoadDecodeTTFT). Both read the backlog here.

// DecodeTTFTLag is how long a decode backlog taints the decode engine's TTFT
// after the last cycle that read it. The TTFT is a rate over one minute
// (registration.QueryAvgTTFT), and a request that queued has its first token
// only once it leaves the queue, so for up to a minute the mean holds the
// queue's wait with the queue empty. The capacity series is held over the
// same minute (max_over_time[1m], registration.QueryCapacityQueueLength), so
// while that hold lasts the two drain together. It does not always last: on
// ttft-staircase-c-on a pod that was going away read 143 waiting, then 0 on
// the next cycle, and one pod read 6.8 s with its own capacity at 0. So the
// lag is the query's minute, again, and 30 s for the scrape, the fetch cache
// and the cycle. Neither interval is in the recording, so it is a constant.
const DecodeTTFTLag = 90 * time.Second

// DecodeBacklog is the requests a serving decode row holds waiting on its
// own capacity, and whether the row says so. Where vLLM says why requests
// wait (CapacityQueueKnown) it is the part waiting for scheduling capacity.
// The rest of a P/D decode queue is KV pulls in flight, which a request pays
// at any load: on the staircase goldens a calm decode read 1 to 26 of them,
// with a decode TTFT under 0.3 s. A row without the series (an older vLLM,
// SGLang) cannot tell the two apart, so it reads no backlog, unknown.
func DecodeBacklog(rm domain.ReplicaMetrics) (int, bool) {
	if !rm.CapacityQueueKnown {
		return 0, false
	}
	return rm.CapacityQueueLength, true
}

// DecodeBacklogOf is the largest DecodeBacklog of rows' serving rows, and
// whether one of them did not say why its requests wait.
func DecodeBacklogOf(rows []domain.ReplicaMetrics) (waiting int, unread bool) {
	for _, rm := range rows {
		if !Serving(rm) {
			continue
		}
		n, known := DecodeBacklog(rm)
		waiting, unread = max(waiting, n), unread || !known
	}
	return waiting, unread
}

// DecodeBacklogCarry is one decode variant's latest backlog and when it was
// read, which Carry holds for DecodeTTFTLag after the queue drains. The zero
// value carries nothing.
type DecodeBacklogCarry struct {
	last   int
	lastAt time.Time
	seen   bool
}

// Carry folds in a cycle's backlog, read at at, and returns the backlog the
// cycle's decode TTFT carries: the cycle's own, or the latest one read within
// DecodeTTFTLag before it; 0 with none.
func (c *DecodeBacklogCarry) Carry(waiting int, at time.Time) int {
	switch {
	case waiting > 0:
		c.last, c.lastAt, c.seen = waiting, at, true
		return waiting
	case c.seen && at.Sub(c.lastAt) <= DecodeTTFTLag:
		return c.last
	}
	return 0
}
