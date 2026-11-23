package collector

import (
	"context"
	"slices"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// kvTransferReading is one kind of KV transfer on an instance as read: its
// byte and seconds rates, each with a presence bit.
type kvTransferReading struct {
	bytes, seconds           float64
	bytesKnown, secondsKnown bool
}

// kvTransferQueries is each kind's byte and seconds query
// (registration/kv_transfers.go).
var kvTransferQueries = []struct {
	kind           string
	bytes, seconds string
}{
	{domain.KVTransferOffloadLoad, registration.QueryKVOffloadLoadByteRate, registration.QueryKVOffloadLoadSecondsRate},
	{domain.KVTransferTierRead, registration.QueryKVTierReadByteRate, registration.QueryKVTierReadSecondsRate},
	{domain.KVTransferNIXL, registration.QueryNIXLByteRate, registration.QueryNIXLSecondsRate},
}

// extractKVTransfers reads the KV transfer series into the instances the
// model-scoped queries already found, as extractCacheTiers reads the cached
// tokens: supplementary, never defining an instance; a failed or unissued
// query leaves its rate unknown, and a kind is kept only with both rates.
func (c *ReplicaMetricsCollector) extractKVTransfers(ctx context.Context, modelID, namespace string,
	results map[string]*source.MetricResult, podData map[string]*podMetricData) {
	for _, q := range kvTransferQueries {
		reading := func(d *podMetricData) *kvTransferReading {
			if d.kvTransfers == nil {
				d.kvTransfers = map[string]*kvTransferReading{}
			}
			if d.kvTransfers[q.kind] == nil {
				d.kvTransfers[q.kind] = &kvTransferReading{}
			}
			return d.kvTransfers[q.kind]
		}
		c.extractNonNegative(ctx, modelID, namespace, results[q.bytes], podData, func(d *podMetricData) (*float64, *bool) {
			r := reading(d)
			return &r.bytes, &r.bytesKnown
		})
		c.extractNonNegative(ctx, modelID, namespace, results[q.seconds], podData, func(d *podMetricData) (*float64, *bool) {
			r := reading(d)
			return &r.seconds, &r.secondsKnown
		})
	}
}

// sortedTransfers is a row's KV transfers, the kinds with both rates read,
// sorted by kind; nil with none, so an absent reading is never an empty one.
func sortedTransfers(byKind map[string]*kvTransferReading) []domain.KVTransferRate {
	var out []domain.KVTransferRate
	for kind, r := range byKind {
		if r != nil && r.bytesKnown && r.secondsKnown {
			out = append(out, domain.KVTransferRate{Kind: kind, ByteRate: r.bytes, SecondsRate: r.seconds})
		}
	}
	slices.SortFunc(out, func(a, b domain.KVTransferRate) int { return strings.Compare(a.Kind, b.Kind) })
	return out
}

// mergeTransfers is one pod's KV transfers from its ranks': each kind's
// rates summed, a kind kept only when every rank had it, as mergeSources
// reads the cached tokens.
func mergeTransfers(group []domain.ReplicaMetrics) []domain.KVTransferRate {
	byKind := map[string]*kvTransferReading{}
	count := map[string]int{}
	for _, m := range group {
		for _, t := range m.KVTransferRates {
			r := byKind[t.Kind]
			if r == nil {
				r = &kvTransferReading{bytesKnown: true, secondsKnown: true}
				byKind[t.Kind] = r
			}
			r.bytes += t.ByteRate
			r.seconds += t.SecondsRate
			count[t.Kind]++
		}
	}
	for kind, n := range count {
		if n != len(group) {
			delete(byKind, kind)
		}
	}
	return sortedTransfers(byKind)
}
