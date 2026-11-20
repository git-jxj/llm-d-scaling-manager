package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// padKind is a kind of an owner no replay drives: a padded recording's extra
// events are skipped by every reader, so its answer is the original's.
const padKind = "padding.Note"

// writePadded writes to path the recording at from, gzipped, with about pad
// bytes of padKind events spread after its lines, renumbered: a large
// recording, a day-long one's size, whose answer is the small one's.
func writePadded(t *testing.T, from, path string, pad int) {
	t.Helper()
	gz, err := os.ReadFile(from)
	require.NoError(t, err)
	raw, err := gunzipped(gz)
	require.NoError(t, err)
	rec, err := learnrec.Read(raw)
	require.NoError(t, err)
	require.NotEmpty(t, rec.Events)
	f, err := os.Create(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()
	z, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	require.NoError(t, err)
	w := bufio.NewWriter(z)
	const chunk = 64 << 10
	data := []byte(`{"Pad":"` + strings.Repeat("x", chunk) + `"}`)
	perEvent := pad / len(rec.Events) / chunk
	seq := uint64(0)
	line := func(now time.Time, kind string, d []byte) {
		seq++
		b, err := learnrec.Line(seq, now, kind, d)
		require.NoError(t, err)
		_, err = w.Write(b)
		require.NoError(t, err)
	}
	for _, ev := range rec.Events {
		line(ev.Now, ev.Kind, ev.Data)
		for range perEvent {
			line(ev.Now, padKind, data)
		}
	}
	require.NoError(t, w.Flush())
	require.NoError(t, z.Close())
	require.Positive(t, perEvent, "a recording of %d events and %d bytes of padding", len(rec.Events), pad)
}

// heapPeak samples the heap that each collection found live, every few
// milliseconds while fn runs, and returns the most it saw above what was
// live before fn ran. Garbage not yet swept is not counted: it is the
// collector's lag, not what fn holds.
func heapPeak(fn func()) uint64 {
	sample := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	read := func() uint64 {
		metrics.Read(sample)
		return sample[0].Value.Uint64()
	}
	runtime.GC()
	before := read()
	var peak atomic.Uint64
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			if v := read(); v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	fn()
	close(done)
	<-stopped
	if p := peak.Load(); p > before {
		return p - before
	}
	return 0
}

// A recording is resolved in passes, never held: a recording padded to
// 256 MiB with events no reader uses holds at most 32 MiB more than the
// small recording it pads while it is resolved, and answers as that one
// does. Read whole, as serve did, it held the recording near three times
// over: the bytes, each event's line and data, and the replay's own
// recording.
func TestServeResolvesALargeRecordingWithoutHoldingIt(t *testing.T) {
	const pad = 256 << 20
	dir := t.TempDir()
	writePadded(t, olsSweep, filepath.Join(dir, "padded.jsonl.gz"), pad)
	b, err := os.ReadFile(olsSweep)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain.jsonl.gz"), b, 0o644))
	src := &servedSources{recordings: dir, warn: io.Discard}
	resolve := func(name string) *whatifapi.Resolved {
		res, err := src.Resolve(context.Background(), whatifapi.InputsRequest{Model: "model-02",
			Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: name}})
		require.NoError(t, err)
		return res
	}
	// A collection at a tenth of the live heap: the garbage each pass leaves
	// is not what the test weighs.
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	var plain, padded *whatifapi.Resolved
	small := heapPeak(func() { plain = resolve("plain.jsonl.gz") })
	held := heapPeak(func() { padded = resolve("padded.jsonl.gz") })
	t.Logf("resolving the recording held %d MiB at most, padded to %d MiB %d MiB", small>>20, pad>>20, held>>20)
	assert.Less(t, held, small+pad/8, "the recording was held: %d MiB of a %d MiB recording, %d MiB unpadded",
		held>>20, pad>>20, small>>20)

	assert.Equal(t, "padded.jsonl.gz", padded.Inputs.Source.Name)
	padded.Inputs.Source.Name = plain.Inputs.Source.Name
	assert.Equal(t, plain.Inputs, padded.Inputs, "the padding changed the inputs")
	assert.Equal(t, plain.Series, padded.Series, "the padding changed the series")
	assert.NotEqual(t, plain.Content, padded.Content, "two recordings of other bytes share an id")
}
