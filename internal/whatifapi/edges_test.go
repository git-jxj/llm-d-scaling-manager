package whatifapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
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
	nine := make([]int, 9)
	for i := range nine {
		nine[i] = 1024 * (i + 1)
	}
	noView := perBucketAt(id, []int{2048})
	noView.Views = nil
	stray := perBucketAt(id, []int{2048})
	stray.SLO = &SLORequest{TTFT: &Objective{BucketTargets: map[string]float64{"8k": 1}}}
	for name, c := range map[string]struct {
		req  PlanRequest
		want string
	}{
		"too many":     {perBucketAt(id, nine), "bucketEdges: 9 edges, want 1 to 8"},
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
		`[]`:       "bucketEdges: 0 edges, want 1 to 8",
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
		`const le = linkedEdges(g("edges"));`, `el("bucketedges").disabled = sizing || !k.perBucket;`, "delete req.views;",
		"state.linkNote = le.note;", "setStatus(state.linkNote, true);", `box.setAttribute("aria-invalid", why ? "true" : "false");`, "beyondContext(l)", "bucketSeries(n, beyond)",
		`EdgeBeyondContext: ["`, `maxModelLen: "context window"`} {
		assert.Contains(t, js, want)
	}
	assert.NotContains(t, js, "function parseEdges", "the edges' rules are scenario.js's alone")
	for _, want := range []string{"function parseEdges(s)", "edges: {test: edgesInLink}", "const MAX_EDGES = 8, MAX_EDGE = 1 << 20;", "function linkedEdges(v)", "function edgesBoxProblem(raw, inUse)", `row("Buckets", ea, ec`} {
		assert.Contains(t, sc, want)
	}
}

// A bucket's curves are the fleet's at its edge, cached by the fleet's
// knobs: an edge two plans share is priced once, whatever the other edges.
func TestSharedEdgeIsCached(t *testing.T) {
	h, curves, _ := counted(t)
	id := inputsOf(t, h)
	plan(t, h, perBucketAt(id, []int{2048, 16384}))
	require.Equal(t, 3, *curves, "the mix line and two bucket lines")
	plan(t, h, perBucketAt(id, []int{2048, 65536}))
	assert.Equal(t, 4, *curves, "2048 priced again: only 65536 is new")
	plan(t, h, perBucketAt(id, []int{16384, 65536}))
	assert.Equal(t, 4, *curves, "both edges were priced before")
}

// ctxInputs resolves the stub's source for model: ctx-16k has a 16k context
// window, ctx-refused and ctx-zero carry the term with none.
func ctxInputs(t *testing.T, h http.Handler, model string) string {
	t.Helper()
	code, body := post(t, h, "/v1/inputs", InputsRequest{Model: model, Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"}})
	require.Equal(t, http.StatusOK, code, "%s", body)
	var in InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	return in.InputsID
}

// hasBeyond reports whether f carries EdgeBeyondContext, on the figure or on
// its refusal.
func hasBeyond(f Figure) bool {
	return slices.Contains(f.Flags, FlagEdgeBeyondContext) || (f.Refused != nil && slices.Contains(f.Refused.Flags, FlagEdgeBeyondContext))
}

// beyondOn is where a plan raises EdgeBeyondContext.
func beyondOn(p PlanResponse) []string {
	for _, u := range p.Flags {
		if u.Flag == FlagEdgeBeyondContext {
			return u.On
		}
	}
	return nil
}

// A line whose edge and output together pass the known context window is
// flagged on its two points and in the plan's flag list; a line within it,
// the mix line and the limits are not; with the window unknown, refused or 0
// nothing is flagged. The line is still priced: the flag says the engines
// would refuse such requests.
func TestEdgeBeyondContext(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	edges := []int{8192, 16384, 32768}
	// The stub observes no output length: the edge alone is weighed.
	out, _ := plan(t, h, perBucketAt(ctxInputs(t, h, "ctx-16k"), edges))
	require.Len(t, out.TTFT.PerBucket, 3)
	for _, l := range out.TTFT.PerBucket {
		beyond := l.Bucket == "32k"
		assert.Equal(t, beyond, hasBeyond(l.AtRate.Figure), "bucket %s point A", l.Bucket)
		assert.Equal(t, beyond, hasBeyond(l.AtTarget.Rate), "bucket %s point B", l.Bucket)
		assert.NotEmpty(t, l.Curve, "bucket %s: still priced", l.Bucket)
	}
	assert.False(t, hasBeyond(out.TTFT.Line.AtRate.Figure) || hasBeyond(out.Limits.Fleet.Figure), "the mix line and the limits are not a bucket's")
	assert.Equal(t, []string{"ttft.perBucket.32k.atRate", "ttft.perBucket.32k.atTarget"}, beyondOn(out))

	// A stated output of 1000 tokens: 16384 + 1000 passes 16384, so 16k is
	// flagged too, and 8192 + 1000 is not.
	req := perBucketAt(ctxInputs(t, h, "ctx-16k"), edges)
	req.Traffic.Mix = &Mix{OutputTokens: 1000}
	withOut, _ := plan(t, h, req)
	assert.Equal(t, []string{"ttft.perBucket.16k.atRate", "ttft.perBucket.16k.atTarget", "ttft.perBucket.32k.atRate", "ttft.perBucket.32k.atTarget"},
		beyondOn(withOut))

	for _, model := range []string{"", "ctx-refused", "ctx-zero"} {
		unknown, _ := plan(t, h, perBucketAt(ctxInputs(t, h, model), edges))
		assert.Nil(t, beyondOn(unknown), "model %q: no window, no flag", model)
		for _, l := range unknown.TTFT.PerBucket {
			assert.False(t, hasBeyond(l.AtRate.Figure) || hasBeyond(l.AtTarget.Rate), "model %q bucket %s: no window, no flag", model, l.Bucket)
		}
	}
}

// Where the points are refused, the flag lands on the refusal.
func TestEdgeBeyondContextOnARefusal(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{refusePrefill: true}, WhatifSchema: "schema-v3"}).Handler()
	out, _ := plan(t, h, perBucketAt(ctxInputs(t, h, "ctx-16k"), []int{32768}))
	require.Len(t, out.TTFT.PerBucket, 1)
	l := out.TTFT.PerBucket[0]
	for name, f := range map[string]Figure{"point A": l.AtRate.Figure, "point B": l.AtTarget.Rate} {
		require.NotNil(t, f.Refused, "%s: the stub refuses V_P", name)
		assert.Nil(t, f.Value, name)
		assert.Contains(t, f.Refused.Flags, FlagEdgeBeyondContext, name)
		assert.NotContains(t, f.Flags, FlagEdgeBeyondContext, "%s: a refused figure's flags are its refusal's", name)
	}
	require.NotNil(t, out.Limits.Prefill.Refused)
	assert.NotContains(t, out.Limits.Prefill.Refused.Flags, FlagEdgeBeyondContext, "the limits' refusal took a bucket's flag")
}

// withFlag copies: two flags added to one figure, or to one refusal, with
// room in their backing arrays, never write over each other or the original.
func TestWithFlagCopies(t *testing.T) {
	base := make([]string, 1, 4)
	base[0] = "A"
	f := Figure{Flags: base}
	g, h := withFlag(f, "X"), withFlag(f, "Y")
	assert.Equal(t, []string{"A", "X"}, g.Flags)
	assert.Equal(t, []string{"A", "Y"}, h.Flags)
	assert.Equal(t, []string{"A"}, f.Flags)
	rbase := make([]string, 1, 4)
	rbase[0] = "R"
	r := Figure{Refused: &Refusal{Reason: "Saturated", Flags: rbase}}
	g, h = withFlag(r, "X"), withFlag(r, "Y")
	assert.Equal(t, []string{"R", "X"}, g.Refused.Flags)
	assert.Equal(t, []string{"R", "Y"}, h.Refused.Flags)
	assert.Equal(t, []string{"R"}, r.Refused.Flags, "the original refusal changed")
	assert.Nil(t, g.Flags)
}

// Each bucket line's point B is its own edge's limit, not a default bucket's
// and not another edge's: the limit entry is priced on the plan's edges.
func TestBucketPointBFollowsItsEdge(t *testing.T) {
	r := &stubResolver{}
	h := New(Config{Resolver: r, WhatifSchema: "schema-v3"}).Handler()
	out, _ := plan(t, h, perBucketAt(inputsOf(t, h), []int{2048, 16384}))
	require.Len(t, out.TTFT.PerBucket, 2)
	rates := make([]float64, 0, 2)
	for _, l := range out.TTFT.PerBucket {
		require.NotNil(t, l.AtTarget.Rate.Value, "bucket %s: %+v", l.Bucket, l.AtTarget.Rate.Refused)
		fl, err := r.Fleet(Knobs{Prefill: out.Scenario.Fleet.Prefill, Decode: out.Scenario.Fleet.Decode, PromptTokens: *l.PromptTokens,
			TTFTPercentile: 0.9, Headroom: out.Scenario.Headroom})
		require.NoError(t, err)
		_, want, _ := fl.PlainLimits(l.Target, 0, out.Scenario.Headroom)
		a, ok := want.(whatif.Answer)
		require.True(t, ok)
		assert.InDelta(t, a.Value, *l.AtTarget.Rate.Value, 1e-12, "bucket %s", l.Bucket)
		rates = append(rates, *l.AtTarget.Rate.Value)
	}
	assert.Greater(t, rates[0], rates[1], "a longer prompt serves fewer requests")
}

// A size refuses bucketEdges before it looks the inputs up: an unknown
// inputsId with edges is the edges' 400, not the id's 404.
func TestSizeRefusesEdgesFirst(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	req := sizeAt("sha256:none", 4, 0.05)
	req.BucketEdges = []int{2048}
	code, body := post(t, h, "/v1/size", req)
	assert.Equal(t, http.StatusBadRequest, code, "%s", body)
	assert.Contains(t, string(body), "bucketEdges: a size prices the mix line alone")
}
