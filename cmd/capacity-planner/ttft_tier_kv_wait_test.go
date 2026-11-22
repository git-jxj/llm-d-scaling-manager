package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// tierWaitFactor is the fixture's engine queue time over its host tier's
// load per request: a loaded request waits for a step boundary past its
// transfer, as the client fleet's did at 3 to 6 times.
const tierWaitFactor = 4.0

// kvWaitRecording is tierRecording(t, true) with each row that read the
// split given an engine queue time of tierWaitFactor times its load per
// request and no request waiting for capacity, as a fleet whose queue is
// its KV wait alone reads.
func kvWaitRecording(t *testing.T) []byte {
	t.Helper()
	in := tierRecording(t, true)
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(in))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	n := 0
	for sc.Scan() {
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		var ev map[string]any
		require.NoError(t, dec.Decode(&ev))
		n += injectWait(t, ev["Data"])
		b, err := json.Marshal(ev)
		require.NoError(t, err)
		out.Write(append(b, '\n'))
	}
	require.NoError(t, sc.Err())
	require.Positive(t, n)
	return out.Bytes()
}

// injectWait gives each row under v that carries a tier split the queue
// time of kvWaitRecording, and returns how many it gave one.
func injectWait(t *testing.T, v any) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["CachedTokenRateBySource"]; ok {
			rate := jsonFloat(t, x["RequestRate"])
			var host float64
			for _, s := range x["CachedTokenRateBySource"].([]any) {
				if m := s.(map[string]any); m["Source"] == "host" {
					switch h := m["Rate"].(type) {
					case float64:
						host = h
					case json.Number:
						host = jsonFloat(t, h)
					}
				}
			}
			// Only a row that already reads no capacity queue: the replay's V_P
			// is learned from the others' queue, as recorded.
			if rate > 0 && x["CapacityQueueKnown"] == true && jsonFloat(t, x["CapacityQueueLength"]) == 0 {
				x["AvgQueueTime"] = tierWaitFactor * host / rate * tierSecondsPerToken
				x["AvgQueueTimeKnown"] = true
				n++
			}
		}
		for _, c := range x {
			n += injectWait(t, c)
		}
	case []any:
		for _, c := range x {
			n += injectWait(t, c)
		}
	}
	return n
}

// With slo.SwitchTTFTTierKVWait on beside the tier load, a request pays the
// engine's measured wait for its KV, tierWaitFactor times the transfer;
// off, the transfer alone, as slo.SwitchTTFTTierLoad prices it. The knee
// does not move.
func TestTTFT_TierKVWaitPricesTheMeasuredWait(t *testing.T) {
	t.Parallel()
	st := tierState(t, kvWaitRecording(t))
	load := tierFleet(t, st, true, false, true, whatifapi.Knobs{Headroom: 0.8})
	cp := *st
	cp.Prefill = append(cp.Prefill[:0:0], st.Prefill...)
	cp.Prefill[0].TierKVWait = true
	wait := tierFleet(t, &cp, true, false, true, whatifapi.Knobs{Headroom: 0.8})

	transfer := injectedLoad(t, load)
	require.Greater(t, transfer, 0.01)
	tLoad, tWait := meanTTFT(t, load), meanTTFT(t, wait)
	t.Logf("transfer %.1f ms; tier load %.1f ms, with the wait %.1f ms", 1000*transfer, 1000*tLoad.Parts.TierLoad, 1000*tWait.Parts.TierLoad)
	assert.InDelta(t, transfer, tLoad.Parts.TierLoad, 1e-9, "the switch off: the transfer's seconds")
	tl, ok := wait.pf.promptWindow.TierLoad()
	require.True(t, ok)
	factor := tl.WaitPerRequest / tl.SecondsPerRequest
	assert.InDelta(t, tierWaitFactor, factor, 0.5, "the window reads the injected wait over the transfer")
	assert.InDelta(t, factor*transfer, tWait.Parts.TierLoad, 1e-6*transfer, "on: the engine's measured wait")
	assert.InDelta(t, (factor-1)*transfer, tWait.Seconds-tLoad.Seconds, 1e-5)
	assert.InDelta(t, load.PrefillKnee().(whatif.Answer).Value, wait.PrefillKnee().(whatif.Answer).Value, 1e-12)
	assert.Contains(t, strings.Join(wait.pf.in.Assumed, "; "), "measured wait for the KV")
	assert.NotContains(t, strings.Join(load.pf.in.Assumed, "; "), "measured wait for the KV")
}

// A wait no longer than the transfer prices the transfer: the switch only
// raises the term where the engine measured more.
func TestTTFT_TierKVWaitBelowTheTransferIsTheTransfer(t *testing.T) {
	t.Parallel()
	st := tierState(t, tierRecording(t, true))
	cp := *st
	cp.Prefill = append(cp.Prefill[:0:0], st.Prefill...)
	cp.Prefill[0].TierKVWait = true
	on := tierFleet(t, &cp, true, false, true, whatifapi.Knobs{Headroom: 0.8})
	off := tierFleet(t, st, true, false, true, whatifapi.Knobs{Headroom: 0.8})
	assert.InDelta(t, meanTTFT(t, off).Seconds, meanTTFT(t, on).Seconds, 1e-12)
}
