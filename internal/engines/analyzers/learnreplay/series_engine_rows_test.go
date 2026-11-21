package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A cycle keeps what the rows say beside the router's rate: every decode
// row's request rate summed, whatever variants are counted, and the counted
// variants' replicas as their states give them (issue
// llm-d-incubation/llm-d-scaling-manager#331).
func TestSampleOfKeepsTheEngineRateAndTheReplicas(t *testing.T) {
	at := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	in := seriesInput(36, seriesDecodeRow("d1", 3, 1000, 100, 0), seriesDecodeRow("d1", 1, 1000, 100, 0),
		seriesDecodeRow("d2", 5, 1000, 100, 0))
	in.VariantStates[0].CurrentReplicas = 4
	in.VariantStates[1].CurrentReplicas = 8
	in.VariantStates[2].CurrentReplicas = 3
	s := sampleOf(in, at, SeriesVariants{Prefill: "p1", Decode: "d1"})
	assert.Equal(t, 36.0, s.ArrivalRate)
	assert.Equal(t, 9.0, s.EngineRate, "every decode row's, d2's too")
	assert.Equal(t, 2, s.DecodePods)
	assert.Equal(t, 8, s.DecodeReplicas, "d1's")
	assert.Equal(t, 4, s.PrefillReplicas)

	all := sampleOf(in, at, SeriesVariants{})
	assert.Equal(t, 11, all.DecodeReplicas, "d1's and d2's")

	in.VariantStates[0].Role = domain.RoleDecode
	assert.Zero(t, sampleOf(in, at, SeriesVariants{Prefill: "p1", Decode: "d1"}).PrefillReplicas, "no prefill variant stated")
}
