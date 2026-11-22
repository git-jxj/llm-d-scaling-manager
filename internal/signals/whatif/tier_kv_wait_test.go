package whatif

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The window reads the engine's wait for a cached KV as its mean queue time
// on the rows with no request waiting for capacity, request-weighted; a row
// with a capacity queue, or none read, is left out.
func TestPromptWindow_TierWait(t *testing.T) {
	cached := map[string]float64{SourceDevice: 40000, SourceHost: 100000}
	row := func(rate, queue float64, capacity int, known bool) domain.ReplicaMetrics {
		rm := tierRow(rate, 50000, 3000, cached, domain.KVTransferRate{Kind: domain.KVTransferOffloadLoad, ByteRate: 1e9, SecondsRate: 0.1})
		rm.AvgQueueTime, rm.AvgQueueTimeKnown = queue, true
		rm.CapacityQueueKnown, rm.CapacityQueueLength = known, capacity
		return rm
	}
	w := &PromptWindow{}
	w.Note([]domain.ReplicaMetrics{row(1, 0.2, 0, true), row(3, 0.4, 0, true), row(2, 5, 2, true), row(2, 9, 0, false)}, 0, time.Unix(0, 0))
	tl, ok := w.TierLoad()
	if !ok {
		t.Fatal("want a tier load")
	}
	relNear(t, "wait", tl.WaitPerRequest, (1*0.2+3*0.4)/4, 1e-12)

	none := &PromptWindow{}
	none.Note([]domain.ReplicaMetrics{row(2, 5, 2, true)}, 0, time.Unix(0, 0))
	if tl, _ := none.TierLoad(); tl.WaitPerRequest != 0 {
		t.Errorf("a capacity queue only: wait %v, want 0", tl.WaitPerRequest)
	}
}
