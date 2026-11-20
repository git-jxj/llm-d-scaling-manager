package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// strip-latency takes every latency key out, at any depth, drops the
// events fitted from latency, renumbers the events after them, and keeps
// everything else.
func TestStripLatency(t *testing.T) {
	in := strings.Join([]string{
		`{"Seq":1,"Kind":"start","Data":{"Commit":"x"}}`,
		`{"Seq":2,"Kind":"saturation.Analyze","Data":{"Result":{"x":1},"Input":{"ArrivalRate":3.5,"RouterLatency":{"WindowSeconds":300,"TTFTBuckets":[]},` +
			`"ReplicaMetrics":[{"PodName":"p","AvgITL":0.01,"AvgITLKnown":true,"AvgInputTokens":1000,"TTFTBuckets":[{"UpperBound":1}],` +
			`"GroupEngines":[{"AvgITL":0.02,"KvUsageMean":0.5}]}]}}}`,
		`{"Seq":3,"Kind":"saturation.publishLine","Data":{"A":0.001,"B":0.006}}`,
		`{"Seq":4,"Kind":"settle.Observe","Data":{"AvgTTFT":1.5,"Rate":12345678901234567890}}`,
	}, "\n")
	var out bytes.Buffer
	st, err := stripLatency(strings.NewReader(in+"\n"+`{"Seq":5,"Kind":"saturation.An`), &out, false)
	require.NoError(t, err)
	assert.Equal(t, stripStats{kept: 2, dropped: 2, torn: true}, st)
	got := out.String()
	for _, gone := range []string{"AvgITL", "TTFTBuckets", "AvgTTFT", "publishLine", `"B":0.006`, "settle.Observe", "Result"} {
		assert.NotContains(t, got, gone)
	}
	for _, kept := range []string{`"ArrivalRate":3.5`, `"AvgInputTokens":1000`, `"KvUsageMean":0.5`, `"Commit":"x"`,
		`"RouterLatency":{"WindowSeconds":300}`} {
		assert.Contains(t, got, kept)
	}
	assert.Contains(t, got, `"Kind":"saturation.Analyze","Seq":2`, "the events are numbered from 1")

	_, err = stripLatency(strings.NewReader("{not json\n{}"), &bytes.Buffer{}, false)
	require.Error(t, err, "a line that is not JSON, and not the last, is an error")
	assert.Equal(t, 2, exitCode(err))
}
