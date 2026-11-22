package main

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// capture prints which optional series the window held: one the fleet
// exports as present, with its increase, the rest as absent, and exits 0
// whatever is absent.
func TestCapture_PrintsTheOptionalSeriesFound(t *testing.T) {
	srv := fleetServer(t)
	var hits whatifsource.OptionalSeries
	for _, s := range whatifsource.OptionalSeriesList {
		if s.Name == "vllm:external_prefix_cache_hits_total" {
			hits = s
		}
	}
	require.NotEmpty(t, hits.Name)
	srv.Set(whatifsource.OptionalQuery(hits, "llm"), promtest.Const(nil, 42))
	out := filepath.Join(t.TempDir(), "run.capture.jsonl.gz")
	text, err := runCLI(captureArgs(srv.URL, out)...)
	require.NoError(t, err)

	checked := 0
	for _, s := range whatifsource.OptionalSeriesList {
		if s.Source != whatifsource.SourceSGLang {
			checked++
		}
	}
	assert.Contains(t, text, "optional series (cache tiers, offloading, KV transfer, TTFT outside the engines): 1 present, ")
	assert.Contains(t, text, "  present   vllm:external_prefix_cache_hits_total\n")
	assert.Contains(t, text, "  absent    vllm:prompt_tokens_cached_by_source_total\n")
	assert.Contains(t, text, "  absent    lmcache_mp_lookup_hit_tokens_total\n")
	assert.NotContains(t, text, "sglang:", "a vLLM capture checks no SGLang series")
	assert.Contains(t, text, "1 present, "+strconv.Itoa(checked-1)+" absent, 0 unchecked")
}

// --lmcache-namespace is where the LMCache server's series are checked;
// without it they are checked in --namespace, and read absent there.
func TestCapture_LMCacheNamespace(t *testing.T) {
	srv := fleetServer(t)
	var hit whatifsource.OptionalSeries
	for _, s := range whatifsource.OptionalSeriesList {
		if s.Name == "lmcache_mp_lookup_hit_tokens_total" {
			hit = s
		}
	}
	require.NotEmpty(t, hit.Name)
	srv.Set(whatifsource.OptionalQuery(hit, "lmc"), promtest.Const(nil, 7))
	out := filepath.Join(t.TempDir(), "run.capture.jsonl.gz")

	text, err := runCLI(captureArgs(srv.URL, out)...)
	require.NoError(t, err)
	assert.Contains(t, text, "  absent    lmcache_mp_lookup_hit_tokens_total\n")

	text, err = runCLI(captureArgs(srv.URL, out, "--lmcache-namespace", "lmc")...)
	require.NoError(t, err)
	assert.Contains(t, text, "  present   lmcache_mp_lookup_hit_tokens_total\n")
	assert.Contains(t, text, "  absent    vllm:external_prefix_cache_hits_total\n", "the engines' series stay in --namespace")
}
