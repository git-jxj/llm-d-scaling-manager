package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The flow-control gate's waits are on the wire only where the gate is
// priced: parts.routerWait and parts.burstWait are absent from the JSON when
// both are 0 (every TTFT with the switch off, and below the onset), and
// present with their figures when the gate is priced.
func TestWire_GateWaitsOnlyWhenPriced(t *testing.T) {
	parts := func(t *testing.T, tt whatif.TTFT) map[string]any {
		t.Helper()
		w := toWireTTFT(tt)
		require.NotNil(t, w)
		raw, err := json.Marshal(w)
		require.NoError(t, err)
		var got struct {
			Answer struct {
				Parts map[string]any `json:"parts"`
			} `json:"answer"`
		}
		require.NoError(t, json.Unmarshal(raw, &got), "%s", raw)
		require.NotNil(t, got.Answer.Parts, "%s", raw)
		return got.Answer.Parts
	}

	unpriced := parts(t, whatif.TTFT{Seconds: 1, Parts: whatif.TTFTParts{EngineWait: 0.2}})
	assert.NotContains(t, unpriced, "routerWait")
	assert.NotContains(t, unpriced, "burstWait")
	assert.Contains(t, unpriced, "engineWait", "precondition: the parts are read")

	priced := parts(t, whatif.TTFT{Seconds: 9, Flags: []whatif.Flag{whatif.FlowControlWaitPriced},
		Parts: whatif.TTFTParts{RouterWait: 2.67, BurstWait: 4.34}})
	assert.Equal(t, 2.67, priced["routerWait"])
	assert.Equal(t, 4.34, priced["burstWait"])
}
