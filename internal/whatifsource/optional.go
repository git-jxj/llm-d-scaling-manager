package whatifsource

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/prometheus/common/model"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// Tier says what an optional series is for, in the words the operator
// pages use.
type Tier string

// The tiers of OptionalSeries. A series the capture replays is "required
// today" (docs/guides/capacity-planner/metrics.md); the optional series are
// one of the other two.
const (
	// TierCachedFleet is a series the planner needs to price TTFT on a
	// fleet whose prompts hit a cache tier, an offload tier or a peer.
	TierCachedFleet Tier = "needed for TTFT on cached fleets"
	// TierDiagnostic is a series that explains an answer, and is not
	// priced.
	TierDiagnostic Tier = "diagnostic"
	// TierOutsideEngines is a diagnostic series that explains the TTFT
	// spent outside the engines: in the router, its plugins, the P/D
	// sidecar, or the engine's HTTP front end before the scheduler.
	TierOutsideEngines Tier = "diagnostic: TTFT outside the engines"
)

// Where an optional series comes from (OptionalSeries.Source).
const (
	SourceVLLM    = "vllm"
	SourceSGLang  = "sglang"
	SourceLMCache = "lmcache"
	SourceRouter  = "router"
	SourceSidecar = "sidecar"
)

// OptionalSeries is one series a capture checks for but does not replay:
// the cache-tier, offloading and KV-transfer series an operator collects
// for TTFT on cached fleets, and the router, sidecar and engine front-end
// series that explain the TTFT spent outside the engines. The capture records whether the window held
// it, and a counter's increase, in the window line
// (learnreplay.CaptureWindow.Optional); an absent one is recorded as absent
// and never fails the capture.
type OptionalSeries struct {
	// Name is the series as Prometheus stores it: a counter's _total name,
	// a histogram's _sum, _count or _bucket.
	Name string
	// Source is SourceVLLM, SourceSGLang, SourceLMCache or SourceRouter.
	// A capture with --engine vllm checks no SGLang series, and the other
	// way round.
	Source string
	Tier   Tier
	// By is the label a counter's increase is broken down by; empty for
	// none.
	By string
	// Match is more label matchers, comma-separated, that select the
	// series; empty for none.
	Match string
}

// counter reports whether the series accumulates: a counter, or a
// histogram's _sum or _count, whose increase over the window is recorded.
func (s OptionalSeries) counter() bool {
	return strings.HasSuffix(s.Name, "_total") || strings.HasSuffix(s.Name, "_sum") ||
		strings.HasSuffix(s.Name, "_count")
}

// OptionalSeriesList is every optional series, in the order the operator
// page lists them (docs/guides/capacity-planner/metrics.md, "Cache tiers,
// offloading and KV transfer" and "TTFT outside the engines"). A test keeps the page, the record guide's
// CACHE_METRICS list and this list in step. The versions and flags that
// produce each are on that page.
var OptionalSeriesList = []OptionalSeries{
	// vLLM: tokens served by a KV connector, and the tier that served them.
	{Name: "vllm:external_prefix_cache_queries_total", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:external_prefix_cache_hits_total", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:prompt_tokens_by_source_total", Source: SourceVLLM, Tier: TierCachedFleet, By: "source"},
	{Name: "vllm:prompt_tokens_cached_total", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:prompt_tokens_cached_by_source_total", Source: SourceVLLM, Tier: TierCachedFleet, By: "source"},
	// vLLM: KV offloading (the OffloadingConnector, --kv-offloading-size).
	{Name: "vllm:kv_offload_load_bytes_total", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:kv_offload_load_time_total", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:kv_offload_store_bytes_total", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_store_time_total", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_total_bytes_total", Source: SourceVLLM, Tier: TierCachedFleet, By: "transfer_type"},
	{Name: "vllm:kv_offload_total_time_total", Source: SourceVLLM, Tier: TierCachedFleet, By: "transfer_type"},
	{Name: "vllm:kv_offload_tiering_chunk_queries_total", Source: SourceVLLM, Tier: TierCachedFleet, By: "tier"},
	{Name: "vllm:kv_offload_tiering_chunk_hits_total", Source: SourceVLLM, Tier: TierCachedFleet, By: "tier"},
	{Name: "vllm:kv_offload_tiering_read_bytes_total", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_tiering_read_time_total", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:kv_offload_config_info", Source: SourceVLLM, Tier: TierDiagnostic},
	// vLLM: P/D transfer over NIXL, beyond the transfer time a capture
	// replays.
	{Name: "vllm:nixl_bytes_transferred_sum", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:nixl_bytes_transferred_count", Source: SourceVLLM, Tier: TierCachedFleet},
	{Name: "vllm:nixl_xfer_time_seconds_bucket", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:nixl_post_time_seconds_sum", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:nixl_post_time_seconds_count", Source: SourceVLLM, Tier: TierDiagnostic},
	{Name: "vllm:nixl_num_failed_transfers_total", Source: SourceVLLM, Tier: TierDiagnostic},
	// LMCache, multi-process server (its own port).
	{Name: "lmcache_mp_lookup_requested_tokens_total", Source: SourceLMCache, Tier: TierCachedFleet},
	{Name: "lmcache_mp_lookup_hit_tokens_total", Source: SourceLMCache, Tier: TierCachedFleet},
	{Name: "lmcache_mp_lookup_hit_l1_tokens_total", Source: SourceLMCache, Tier: TierCachedFleet},
	{Name: "lmcache_mp_lookup_hit_l2_tokens_total", Source: SourceLMCache, Tier: TierCachedFleet},
	{Name: "lmcache_mp_lookup_prompt_tokens_total", Source: SourceLMCache, Tier: TierDiagnostic},
	{Name: "lmcache_mp_lookup_request_hit_tokens_total", Source: SourceLMCache, Tier: TierDiagnostic},
	// LMCache, in-process connector (on the engine's own /metrics).
	{Name: "lmcache:num_requested_tokens_total", Source: SourceLMCache, Tier: TierCachedFleet},
	{Name: "lmcache:num_hit_tokens_total", Source: SourceLMCache, Tier: TierCachedFleet},
	{Name: "lmcache:num_lookup_tokens_total", Source: SourceLMCache, Tier: TierDiagnostic},
	{Name: "lmcache:num_lookup_hits_total", Source: SourceLMCache, Tier: TierDiagnostic},
	{Name: "lmcache:time_to_retrieve_sum", Source: SourceLMCache, Tier: TierDiagnostic},
	{Name: "lmcache:time_to_retrieve_count", Source: SourceLMCache, Tier: TierDiagnostic},
	// llm-d router: cached tokens as the engine reported them, and the
	// prefix producer's prediction.
	{Name: "llm_d_epp_request_cached_tokens_sum", Source: SourceRouter, Tier: TierCachedFleet},
	{Name: "llm_d_epp_request_cached_tokens_count", Source: SourceRouter, Tier: TierCachedFleet},
	{Name: "llm_d_epp_request_cached_tokens_bucket", Source: SourceRouter, Tier: TierCachedFleet},
	{Name: "llm_d_epp_prefix_predicted_cached_tokens_sum", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_predicted_cached_tokens_count", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_best_predicted_cached_tokens_sum", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_best_available_cached_tokens_sum", Source: SourceRouter, Tier: TierDiagnostic},
	{Name: "llm_d_epp_prefix_prompt_tokens_sum", Source: SourceRouter, Tier: TierDiagnostic},
	// SGLang: the hit tier of each prefill token.
	{Name: "sglang:prefill_effective_tokens_total", Source: SourceSGLang, Tier: TierCachedFleet, By: "mode"},
	{Name: "sglang:uncached_prompt_tokens_histogram_sum", Source: SourceSGLang, Tier: TierCachedFleet},
	{Name: "sglang:uncached_prompt_tokens_histogram_count", Source: SourceSGLang, Tier: TierCachedFleet},
	{Name: "sglang:uncached_prompt_tokens_histogram_bucket", Source: SourceSGLang, Tier: TierDiagnostic},
	// TTFT outside the engines: the router's own time, its plugins and
	// render calls, the P/D sidecar's prefill leg, and the engine's time
	// before and around its scheduler.
	{Name: "llm_d_epp_request_processing_duration_seconds_sum", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_request_processing_duration_seconds_count", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_request_processing_duration_seconds_bucket", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_token_producer_render_duration_seconds_sum", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_token_producer_render_duration_seconds_count", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_plugin_duration_seconds_sum", Source: SourceRouter, Tier: TierOutsideEngines, By: "extension_point"},
	{Name: "llm_d_epp_plugin_duration_seconds_count", Source: SourceRouter, Tier: TierOutsideEngines, By: "extension_point"},
	{Name: "llm_d_epp_request_size_bytes_sum", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_request_size_bytes_count", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_request_duration_seconds_sum", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_epp_request_duration_seconds_count", Source: SourceRouter, Tier: TierOutsideEngines},
	{Name: "llm_d_disagg_sidecar_prefill_duration_seconds_sum", Source: SourceSidecar, Tier: TierOutsideEngines},
	{Name: "llm_d_disagg_sidecar_prefill_duration_seconds_count", Source: SourceSidecar, Tier: TierOutsideEngines},
	{Name: "vllm:e2e_request_latency_seconds_sum", Source: SourceVLLM, Tier: TierOutsideEngines},
	{Name: "vllm:e2e_request_latency_seconds_count", Source: SourceVLLM, Tier: TierOutsideEngines},
	{Name: "vllm:request_render_duration_seconds_sum", Source: SourceVLLM, Tier: TierOutsideEngines},
	{Name: "vllm:request_render_duration_seconds_count", Source: SourceVLLM, Tier: TierOutsideEngines},
	{Name: "http_request_duration_seconds_sum", Source: SourceVLLM, Tier: TierOutsideEngines, Match: `handler="/v1/chat/completions"`},
	{Name: "http_request_duration_seconds_count", Source: SourceVLLM, Tier: TierOutsideEngines, Match: `handler="/v1/chat/completions"`},
}

// optionalFor is the optional series a capture of engine checks: every
// one but the other engine's.
func optionalFor(engine inferenceengine.Engine) []OptionalSeries {
	skip := SourceSGLang
	if engine == inferenceengine.EngineSGLang {
		skip = SourceVLLM
	}
	var out []OptionalSeries
	for _, s := range OptionalSeriesList {
		if s.Source != skip {
			out = append(out, s)
		}
	}
	return out
}

// OptionalQuery is the PromQL that checks s in namespace: a counter's sum
// (by its breakdown label), whose steps give its increase; any other
// series' count. Both are instant vectors, answered at every step of the
// window like the capture's own queries.
func OptionalQuery(s OptionalSeries, namespace string) string {
	match := `namespace="` + source.EscapePromQLValue(namespace) + `"`
	if s.Match != "" {
		match += "," + s.Match
	}
	sel := s.Name + "{" + match + "}"
	if !s.counter() {
		return "count(" + sel + ")"
	}
	if s.By != "" {
		return "sum by (" + s.By + ") (" + sel + ")"
	}
	return "sum(" + sel + ")"
}

// checkOptional checks every optional series of the capture's engine over
// the window, on an API of its own: one range query each, at the
// capture's step, in the parts the capture's own queries take. A query
// that fails is recorded as unchecked and the rest go on; only a
// cancelled ctx stops them. It returns the series as found, and the range
// queries and samples it took.
func checkOptional(ctx context.Context, opts Options, maxSamples int) ([]learnreplay.CaptureSeries, int, int, error) {
	api := newWindowAPI(opts.Prometheus, opts.From, opts.To, opts.Step, opts.RangeQueryPoints, nil)
	var out []learnreplay.CaptureSeries
	queries, held := 0, 0
	for _, s := range optionalFor(opts.Engine) {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		// Each answer is read and dropped: the bound is per query, not on
		// the sum, so one large series does not stop the checks after it.
		api.mu.Lock()
		held += api.samples
		api.samples, api.maxSamples = 0, maxSamples
		api.mu.Unlock()
		queries++
		ns := opts.Namespace
		if strings.HasPrefix(s.Name, "lmcache_mp_") && opts.LMCacheNamespace != "" {
			ns = opts.LMCacheNamespace
		}
		byTime, _, err := api.fetch(ctx, OptionalQuery(s, ns))
		if err != nil {
			if ctx.Err() != nil {
				return nil, 0, 0, ctx.Err()
			}
			out = append(out, learnreplay.CaptureSeries{Name: s.Name, Err: err.Error()})
			continue
		}
		out = append(out, summarize(s, byTime))
	}
	return out, queries, held + api.held(), nil
}

// summarize is what a series' answer over the window says: whether it had
// a sample, and a counter's increase, read from its sums step by step as
// rate() reads a counter: a fall is a restart, and the value after it
// counts in full.
func summarize(s OptionalSeries, byTime map[model.Time]model.Vector) learnreplay.CaptureSeries {
	c := learnreplay.CaptureSeries{Name: s.Name, Present: len(byTime) > 0}
	if !c.Present || !s.counter() {
		return c
	}
	last := map[string]float64{}
	by := map[string]float64{}
	for _, at := range slices.Sorted(maps.Keys(byTime)) {
		for _, sample := range byTime[at] {
			key := string(sample.Metric[model.LabelName(s.By)])
			v := float64(sample.Value)
			prev, seen := last[key]
			switch {
			case !seen:
			case v >= prev:
				by[key] += v - prev
			default:
				by[key] += v
			}
			last[key] = v
		}
	}
	for _, v := range by {
		c.Increase += v
	}
	if s.By != "" && len(by) > 0 {
		c.By = by
	}
	return c
}

// OptionalText is one line per optional series checked: present, with a
// counter's increase, absent, or unchecked and why. present and absent
// count them.
func OptionalText(found []learnreplay.CaptureSeries) (lines []string, present, absent int) {
	for _, c := range found {
		switch {
		case c.Err != "":
			lines = append(lines, fmt.Sprintf("unchecked %s: %s", c.Name, c.Err))
		case !c.Present:
			absent++
			lines = append(lines, "absent    "+c.Name)
		default:
			present++
			var line strings.Builder
			line.WriteString("present   " + c.Name)
			if c.Increase != 0 || c.By != nil {
				fmt.Fprintf(&line, "  increase %.4g", c.Increase)
			}
			for _, k := range slices.Sorted(maps.Keys(c.By)) {
				name := k
				if name == "" {
					name = `""`
				}
				fmt.Fprintf(&line, "  %s=%.4g", name, c.By[k])
			}
			lines = append(lines, line.String())
		}
	}
	return lines, present, absent
}
