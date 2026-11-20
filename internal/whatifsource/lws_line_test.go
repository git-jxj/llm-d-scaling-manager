package whatifsource

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// A DisaggregatedSet on LeaderWorkerSets with an engine per Pod, under a
// steady load its engines share unevenly: each group's mean fill is the
// same on every cycle, and only the engines spread. The recording made from
// the window learns the decode line from the engines' readings, where a
// line fitted from the groups' means had no spread to fit and pinned its
// intercept at the default.
func TestCapture_AnEnginePerPodGroupTeachesTheDecodeLine(t *testing.T) {
	srv := promtest.New()
	t.Cleanup(srv.Close)
	dec := decodeRole(everyPod)
	dec.itl = everyPod
	dec.fill = func(_, w int) float64 { return 0.2 + 0.1*float64(w) }
	dec.add(t, srv)
	prefillRole(everyPod).add(t, srv)
	const steps = 120
	opts := Options{Prometheus: apiFor(t, srv), Namespace: testNS, From: t0, To: t0.Add((steps - 1) * 15 * time.Second),
		Step: 15 * time.Second, KubeStateMetrics: true, Engine: inferenceengine.EngineVLLM,
		EngineArgs: []string{"--max-num-seqs", "64"}}
	var buf bytes.Buffer
	_, err := Capture(context.Background(), opts, &buf)
	require.NoError(t, err)
	zr, err := gzip.NewReader(&buf)
	require.NoError(t, err)
	raw, err := io.ReadAll(zr)
	require.NoError(t, err)
	rep, err := learnreplay.Replay(context.Background(), raw, learnreplay.Options{Mode: learnreplay.Generate})
	require.NoError(t, err)
	st, err := learnreplay.StateAt(context.Background(), rep.Recording, learnreplay.Options{}, learnreplay.StateQuery{})
	require.NoError(t, err)

	var line *learnreplay.VariantState
	for i := range st.Variants {
		if st.Variants[i].Variant == "m-decode" {
			line = &st.Variants[i]
		}
	}
	require.NotNil(t, line, "no decode variant in %+v", st.Variants)
	require.Equal(t, itl.TierOLS, line.Tier, "the decode line is fitted from the engines' spread")
	assert.InDelta(t, 0.02, line.Line.A, 0.001, "the engines' slope")
	assert.InDelta(t, 0.006, line.Line.B, 0.0005, "the engines' intercept, not the default's")
}
