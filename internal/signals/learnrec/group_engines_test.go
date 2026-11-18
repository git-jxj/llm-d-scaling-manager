package learnrec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A group's engines ride in its row and read back as written, so a replay
// fits the ITL line from the engines the controller fitted it from; a row
// without them writes neither field.
func TestCodec_GroupEnginesReadBack(t *testing.T) {
	engine := func(pod string, k float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, KvUsageMean: k, KvUsageMeanKnown: true, AvgITL: 0.006 + 0.02*k,
			AvgITLKnown: true, EngineRanks: 1, Ready: true}
	}
	row := domain.ReplicaMetrics{PodName: "g-0", VariantName: "d", EngineRanks: 2, Ready: true,
		GroupEngines: []domain.ReplicaMetrics{engine("g-0", 0.2), engine("g-0-1", 0.4)}}
	in := Input{ModelID: "m", Namespace: "ns", ReplicaMetrics: []domain.ReplicaMetrics{row}}
	b, err := Marshal(in)
	require.NoError(t, err)
	var out Input
	require.NoError(t, Unmarshal(b, &out))
	assert.Equal(t, in, out)
	again, err := Marshal(out)
	require.NoError(t, err)
	assert.Equal(t, string(b), string(again))
	assert.NotContains(t, string(b), `"EngineOf"`)

	in.ReplicaMetrics[0].GroupEngines = nil
	b, err = Marshal(in)
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"GroupEngines"`)
}
