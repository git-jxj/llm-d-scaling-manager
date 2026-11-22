package collector

import (
	"context"
	"sort"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// extractCacheTiers reads the series that say where a prompt's cached
// tokens came from (registration/cache_tiers.go) into the instances the
// model-scoped queries already found, as extractTTFTInputs reads the TTFT
// model's: supplementary, never defining an instance, and a failed or
// unissued query leaves each unknown.
func (c *ReplicaMetricsCollector) extractCacheTiers(ctx context.Context, modelID, namespace string,
	results map[string]*source.MetricResult, podData map[string]*podMetricData) {
	c.extractNonNegative(ctx, modelID, namespace, results[registration.QueryExternalPrefixCacheHitTokenRate], podData,
		func(d *podMetricData) (*float64, *bool) { return &d.externalHitRate, &d.externalHitRateKnown })
	c.extractNonNegative(ctx, modelID, namespace, results[registration.QueryExternalPrefixCacheQueryTokenRate], podData,
		func(d *podMetricData) (*float64, *bool) { return &d.externalQueryRate, &d.externalQueryRateKnown })
	result := results[registration.QueryCachedTokenRateBySource]
	if result == nil || result.HasError() {
		return
	}
	for _, value := range result.Values {
		if value.Labels[seriesModelLabel] != modelID {
			continue
		}
		src := value.Labels["source"]
		if src == "" || !value.Finite() || value.Value < 0 {
			continue
		}
		instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
		if instanceKey == "" || podData[instanceKey] == nil {
			continue
		}
		d := podData[instanceKey]
		if d.cachedBySource == nil {
			d.cachedBySource = map[string]float64{}
		}
		d.cachedBySource[src] = value.Value
	}
}

// sortedSources is a by-source split as a row carries it, sorted by source;
// nil when none was read, so an absent split is never an empty one.
func sortedSources(bySource map[string]float64) []domain.SourceRate {
	if len(bySource) == 0 {
		return nil
	}
	out := make([]domain.SourceRate, 0, len(bySource))
	for s, r := range bySource {
		out = append(out, domain.SourceRate{Source: s, Rate: r})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

// mergeSources is one pod's by-source split from its ranks': each source's
// rate summed. Nil unless every rank had a split, as mergeBuckets reads a
// histogram.
func mergeSources(group []domain.ReplicaMetrics) []domain.SourceRate {
	bySource := map[string]float64{}
	for _, m := range group {
		if len(m.CachedTokenRateBySource) == 0 {
			return nil
		}
		for _, s := range m.CachedTokenRateBySource {
			bySource[s.Source] += s.Rate
		}
	}
	return sortedSources(bySource)
}
