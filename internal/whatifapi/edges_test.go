package whatifapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// perBucketAt is a plan request at 4 r/s with the per-bucket view and edges.
func perBucketAt(id string, edges []int) PlanRequest {
	return PlanRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(4)}}},
		Views: &ViewRequest{AllTrafficPerBucket: true}, BucketEdges: edges}
}

// A bucket is named by its upper edge: in k where the edge is a whole number
// of 1024 tokens, else in tokens, so the default edges keep their names and no
// two edges share one.
func TestBucketName(t *testing.T) {
	for tokens, want := range map[int]string{1: "1", 1000: "1000", 1024: "1k", 8000: "8000", 8192: "8k", 131072: "128k", 1 << 20: "1024k"} {
		assert.Equal(t, want, bucketName(tokens), "%d", tokens)
	}
	for _, b := range defaultBuckets {
		assert.Equal(t, b.Name, bucketName(b.UpperTokens), "the default bucket %s", b.Name)
	}
}

// A plan's edges are its buckets: one "if all traffic were bucket b" line per
// edge, in order, each priced with every prompt at its edge and named by it.
func TestBucketEdgesPriceTheirOwnLines(t *testing.T) {
	var mu sync.Mutex
	priced := map[float64]bool{}
	r := &stubResolver{onFleet: func(k Knobs) {
		mu.Lock()
		defer mu.Unlock()
		priced[k.PromptTokens] = true
	}}
	h := New(Config{Resolver: r, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	edges := []int{1000, 2048, 16384, 300000}
	out, _ := plan(t, h, perBucketAt(id, edges))
	require.Len(t, out.TTFT.PerBucket, len(edges))
	names := make([]string, 0, len(out.TTFT.PerBucket))
	for i, l := range out.TTFT.PerBucket {
		names = append(names, l.Bucket)
		assert.Equal(t, SemanticsAllTraffic, l.Semantics)
		require.NotNil(t, l.PromptTokens)
		assert.InDelta(t, float64(edges[i]), *l.PromptTokens, 0, "bucket %s", l.Bucket)
		assert.True(t, priced[float64(edges[i])], "no fleet priced at the edge %d", edges[i])
	}
	assert.Equal(t, []string{"1000", "2k", "16k", "300000"}, names)
	assert.Equal(t, edges, out.Scenario.BucketEdges, "the scenario names its edges")
	assert.Equal(t, map[string]float64{"1000": 0.2, "2k": 0.2, "16k": 0.2, "300000": 0.2}, out.Scenario.TTFT.BucketTargets,
		"every bucket takes the mix target")
	// The limits read the mix line alone: the edges move none of them.
	plain, _ := plan(t, h, perBucketAt(id, nil))
	assert.Equal(t, plain.Limits, out.Limits)
}

// The default edges, asked or left out, are one scenario: the same id, the
// same bytes, and no bucketEdges key; so a plan with none prices as before.
func TestDefaultBucketEdgesAreNoEdges(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	none, noneBody := plan(t, h, perBucketAt(id, nil))
	asked, askedBody := plan(t, h, perBucketAt(id, defaultEdges()))
	assert.Equal(t, none.ScenarioID, asked.ScenarioID)
	assert.Equal(t, string(noneBody), string(askedBody))
	assert.NotContains(t, string(noneBody), "bucketEdges")
	names := make([]string, 0, len(none.TTFT.PerBucket))
	for _, l := range none.TTFT.PerBucket {
		names = append(names, l.Bucket)
	}
	assert.Equal(t, []string{"1k", "4k", "8k", "32k", "128k"}, names)
}

// The edges are part of the scenario's id and of the limits' key: other edges
// are another scenario, priced on their own.
func TestBucketEdgesKeyTheScenario(t *testing.T) {
	h, _, limits := counted(t)
	id := inputsOf(t, h)
	a, _ := plan(t, h, perBucketAt(id, []int{2048, 16384}))
	require.Equal(t, 1, *limits)
	b, _ := plan(t, h, perBucketAt(id, []int{2048, 65536}))
	assert.NotEqual(t, a.ScenarioID, b.ScenarioID)
	assert.Equal(t, 2, *limits, "other edges priced no limits of their own")
	again, _ := plan(t, h, perBucketAt(id, []int{2048, 16384}))
	assert.Equal(t, a.ScenarioID, again.ScenarioID)
	assert.Equal(t, 2, *limits, "the same edges priced again")
}

// A bucket target names a bucket of the plan's edges.
func TestBucketTargetsFollowTheEdges(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	req := perBucketAt(id, []int{2000, 16384})
	req.SLO = &SLORequest{TTFT: &Objective{BucketTargets: map[string]float64{"2000": 0.5}}}
	out, _ := plan(t, h, req)
	require.Len(t, out.TTFT.PerBucket, 2)
	assert.InDelta(t, 0.5, out.TTFT.PerBucket[0].Target, 0)
	assert.InDelta(t, 0.2, out.TTFT.PerBucket[1].Target, 0)
}

// Edges asked wrongly are a 400 naming the edge and why, and a size refuses
// edges: it has no per-bucket view to shape.
func TestBucketEdgesRefused(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	thirteen := make([]int, 13)
	for i := range thirteen {
		thirteen[i] = 1024 * (i + 1)
	}
	noView := perBucketAt(id, []int{2048})
	noView.Views = nil
	stray := perBucketAt(id, []int{2048})
	stray.SLO = &SLORequest{TTFT: &Objective{BucketTargets: map[string]float64{"8k": 1}}}
	for name, c := range map[string]struct {
		req  PlanRequest
		want string
	}{
		"too many":     {perBucketAt(id, thirteen), "bucketEdges: 13 edges, want 1 to 12"},
		"zero":         {perBucketAt(id, []int{0, 1024}), "bucketEdges[0] 0, want an upper edge from 1 to 1048576 tokens"},
		"negative":     {perBucketAt(id, []int{1024, -5}), "bucketEdges[1] -5, want an upper edge"},
		"too large":    {perBucketAt(id, []int{1024, 1<<20 + 1}), "bucketEdges[1] 1048577, want an upper edge from 1 to 1048576 tokens"},
		"descending":   {perBucketAt(id, []int{4096, 1024}), "bucketEdges[1] 1024 is not above bucketEdges[0] 4096: want the edges ascending, each once"},
		"repeated":     {perBucketAt(id, []int{1024, 1024}), "bucketEdges[1] 1024 is not above bucketEdges[0] 1024"},
		"no view":      {noView, "bucketEdges need views.allTrafficPerBucket"},
		"stray target": {stray, "slo.ttft.bucketTargets.8k: no such bucket; the buckets are 2k"},
	} {
		code, body := post(t, h, "/v1/plan", c.req)
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", name, body)
		assert.Contains(t, string(body), c.want, name)
	}
	// An empty list is no edges asked, not the default; an edge that is not a
	// whole token count is no edge.
	for raw, want := range map[string]string{
		`[]`:       "bucketEdges: 0 edges, want 1 to 12",
		`[1024.5]`: "bucketEdges",
	} {
		code, body := post(t, h, "/v1/plan", json.RawMessage(`{"inputsId":"`+id+`","views":{"allTrafficPerBucket":true},"bucketEdges":`+raw+`}`))
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", raw, body)
		assert.Contains(t, string(body), want, raw)
	}
	size := sizeAt(id, 4, 0.05)
	size.BucketEdges = []int{2048}
	code, body := post(t, h, "/v1/size", size)
	assert.Equal(t, http.StatusBadRequest, code, "%s", body)
	assert.Contains(t, string(body), "bucketEdges: a size prices the mix line alone")
}

// The inputs answer the default buckets whatever a plan's edges: nothing
// learned is per bucket, so no plan's edges re-bucket them.
func TestInputsKeepTheDefaultBuckets(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	plan(t, h, perBucketAt(id, []int{2048}))
	code, body := post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"}})
	require.Equal(t, http.StatusOK, code, "%s", body)
	var in InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	assert.True(t, slices.Equal(Buckets(), in.Observed.Buckets))
	require.NotNil(t, in.Observed.Mix.Refused, "a learned mix per bucket would need re-bucketing to a plan's edges")
}

// The page carries the edges: a plain box, kept in the link, sent with the
// per-bucket view only and never with a size. The edges' rules are in
// scenario.js, run on fixtures by hack/check-planner-scenarios.sh.
func TestPageSetsBucketEdges(t *testing.T) {
	html, js, sc := pageText(t, "index.html"), pageText(t, "planner.js"), pageText(t, "scenario.js")
	assert.Contains(t, html, `id="bucketedges"`)
	for _, want := range []string{`if (k.perBucket && edges.length) req.bucketEdges = edges;`, `p.set("edges", k.edges);`,
		`k.edges = edgesInLink(g("edges") || "") ? g("edges") : "";`, `el("bucketedges").disabled = sizing || !k.perBucket;`, "delete req.views;"} {
		assert.Contains(t, js, want)
	}
	assert.NotContains(t, js, "function parseEdges", "the edges' rules are scenario.js's alone")
	for _, want := range []string{"function parseEdges(s)", "edges: {test: edgesInLink}", "const MAX_EDGES = 12, MAX_EDGE = 1 << 20;"} {
		assert.Contains(t, sc, want)
	}
}
