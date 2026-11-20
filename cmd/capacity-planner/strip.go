package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// strip-latency writes a recording or a capture with every measured
// latency taken out: what a user shares so that a prediction is made from
// the traffic and the fleet alone (capacity-planner predict). The lengths,
// rates, replica counts and the rest are kept as they are.

// latencyKeys are the keys a recording or a capture carries a measured
// latency under, at any depth: the router's (RouterLatency, with its flow
// control wait), and each engine row's TTFT, inter-token, service, queue,
// prefill and KV-transfer times, means and histograms, with their presence
// bits.
var latencyKeys = map[string]bool{
	"RouterLatency": true, "FlowControlWaitBuckets": true,
	"TTFTBuckets": true, "TPOTBuckets": true, "QueueTimeBuckets": true,
	"AvgTTFT": true, "AvgTTFTKnown": true,
	"AvgITL": true, "AvgITLKnown": true, "AvgITLEmpty": true,
	"AvgServiceTime": true, "AvgServiceTimeKnown": true,
	"AvgQueueTime": true, "AvgQueueTimeKnown": true,
	"AvgPrefillTime": true, "AvgPrefillTimeKnown": true,
	"AvgKVTransferTime": true, "AvgKVTransferTimeKnown": true,
}

// latencyKinds are the events a recording carries a latency-derived
// figure in, dropped whole: the decode ITL line each cycle published or
// borrowed, fitted from the inter-token latencies.
var latencyKinds = map[string]bool{"saturation.publishLine": true, "saturation.borrowLine": true}

// runStripLatency is `capacity-planner strip-latency`.
func runStripLatency(_ context.Context, args []string, out, _ io.Writer) error {
	fs := newFlags("strip-latency", out)
	in := fs.String("in", "", "the recording or capture to strip, gzipped or not (required)")
	dst := fs.String("o", "", "where to write it; a name ending in .gz is gzipped (required)")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	if *in == "" || *dst == "" {
		return usagef("-in and -o are required")
	}
	b, err := readMaybeGzipped(*in)
	if err != nil {
		return usageError{err}
	}
	var n int
	err = writeFileAtomic(*dst, func(w io.Writer) error {
		var err error
		n, err = stripLatency(bytes.NewReader(b), w, strings.HasSuffix(*dst, ".gz"))
		return err
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s: %d lines, every measured latency taken out\n", *dst, n)
	return nil
}

// stripLatency copies the JSON lines of r to w with every latencyKeys key
// removed at any depth and every latencyKinds event dropped, gzipped when
// gz; n is the lines written. A line that is not JSON is an error: a file
// half stripped is worse than none.
func stripLatency(r io.Reader, w io.Writer, gz bool) (n int, err error) {
	dst := w
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(w)
		dst = zw
	}
	bw := bufio.NewWriter(dst)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	line, dropped := 0, 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var v any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return n, usagef("line %d: not JSON: %v", line, err)
		}
		if m, ok := v.(map[string]any); ok {
			if k, _ := m["Kind"].(string); latencyKinds[k] {
				dropped++
				continue
			}
			// A recording numbers its events in sequence: the events after a
			// dropped one move up.
			if seq, ok := m["Seq"].(json.Number); ok && dropped > 0 {
				if n, err := seq.Int64(); err == nil {
					m["Seq"] = n - int64(dropped)
				}
			}
		}
		b, err := json.Marshal(dropLatency(v))
		if err != nil {
			return n, err
		}
		if _, err := bw.Write(append(b, '\n')); err != nil {
			return n, err
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return n, usagef("line %d: %v", line+1, err)
	}
	if err := bw.Flush(); err != nil {
		return n, err
	}
	if zw != nil {
		return n, zw.Close()
	}
	return n, nil
}

// dropLatency is v with every latencyKeys key removed, at any depth.
func dropLatency(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if latencyKeys[k] {
				delete(x, k)
				continue
			}
			x[k] = dropLatency(e)
		}
	case []any:
		for i, e := range x {
			x[i] = dropLatency(e)
		}
	}
	return v
}
