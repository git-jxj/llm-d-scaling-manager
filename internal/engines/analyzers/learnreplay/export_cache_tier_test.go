package learnreplay

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// Every tier the cache-tier series names and every KV transfer kind is a
// word the export keeps. Before they were in the vocabulary, every recording
// made with the cache-tier capture was refused
// ("CachedTokenRateBySource.Source in saturation.Analyze holds a value that
// is not allowed after the export", and the same for KVTransferRates.Kind).
func TestVocabulary_HoldsEveryCacheTierSourceAndTransferKind(t *testing.T) {
	for _, word := range []string{
		whatif.SourceDevice, whatif.SourceHost, whatif.SourceDisk, whatif.SourceP2P,
		whatif.SourceExternalUnspecified,
		domain.KVTransferOffloadLoad, domain.KVTransferTierRead, domain.KVTransferNIXL,
	} {
		assert.True(t, vocabulary[word], "%q is not in the export's vocabulary", word)
	}
	for _, word := range append(whatif.Sources(), domain.KVTransferKinds()...) {
		assert.True(t, vocabulary[word], "%q is not in the export's vocabulary", word)
	}
}

// parsedLayouts are the memory layouts the vLLM and SGLang parsers write for
// every memory-layout flag they read.
func parsedLayouts(t *testing.T) []string {
	t.Helper()
	parse := func(engine inferenceengine.Engine, c corev1.Container) string {
		st := scaletarget.NewDeploymentAccessor(&appsv1.Deployment{Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{c}}}}})
		layout := capacity.ParseEngineArgs(engine, st).MemoryLayout
		require.NotEmpty(t, layout, "precondition: the parser wrote a layout")
		return layout
	}
	return []string{
		parse(inferenceengine.EngineVLLM, corev1.Container{Name: "vllm", Image: "vllm/vllm-openai:v0.11.0",
			Command: []string{"vllm", "serve", "org/m",
				"--num-redundant-experts", "32", "--kv-cache-memory-bytes", "40G", "--enable-eplb",
				"--eplb-config", `{"window_size": 1000, "num_redundant_experts": 32}`,
				"--all2all-backend", "deepep_low_latency"},
			Env: []corev1.EnvVar{{Name: "VLLM_ALL2ALL_BACKEND", Value: "deepep_high_throughput"}}}),
		parse(inferenceengine.EngineSGLang, corev1.Container{Name: "sglang", Image: "lmsysorg/sglang:v0.5",
			Command: []string{"python", "-m", "sglang.launch_server", "--model-path", "org/m",
				"--enable-dp-attention", "--moe-a2a-backend", "deepep"}}),
	}
}

// cacheTierRecording is a recording of one Analyze whose replica reads every
// cache tier and every KV transfer kind, of a variant filed with layout, all
// under names a cluster gives. edit, when set, changes the row before it is
// recorded.
func cacheTierRecording(t *testing.T, layout string, edit func(*domain.ReplicaMetrics)) []byte {
	t.Helper()
	const ns, model, pod = "payroll", "acme-llm", "payroll-7f9c4-x7k2p"
	in := ttftInput()
	in.Namespace, in.ModelID = ns, model
	row := &in.ReplicaMetrics[0]
	row.Namespace, row.ModelID, row.PodName = ns, model, pod
	for i, s := range whatif.Sources() {
		row.CachedTokenRateBySource = append(row.CachedTokenRateBySource, domain.SourceRate{Source: s, Rate: float64(i + 1)})
	}
	for _, k := range domain.KVTransferKinds() {
		row.KVTransferRates = append(row.KVTransferRates, domain.KVTransferRate{Kind: k, ByteRate: 1e9, SecondsRate: 0.25})
	}
	if edit != nil {
		edit(row)
	}

	f := newFixture(replayAt)
	f.buf.Reset()
	rec := learnrec.New(f.buf, 0)
	f.attach(rec)
	rec.Record(learnrec.KindStart, replayAt, defaultStart())
	f.store.FileScaleTarget(ns, model, "v", "H100", 1, capacity.EngineParams{Engine: inferenceengine.EngineVLLM,
		MaxNumSeqs: 64, ModelPath: model, Image: "img:1", MemoryLayout: layout}, false, replayAt)
	_, err := f.sat.Analyze(context.Background(), in)
	require.NoError(t, err)
	require.NoError(t, rec.Err())
	return bytes.Clone(f.buf.Bytes())
}

// A recording made with the cache-tier capture exports: the tiers, the
// transfer kinds and the memory layout are the code's own words and figures,
// kept as they are, while every name is renamed, and the export replays byte
// for byte. Before, every such recording was refused on
// CachedTokenRateBySource.Source, KVTransferRates.Kind and MemoryLayout.
func TestExport_KeepsTheCacheTierSeriesAndTheMemoryLayout(t *testing.T) {
	for _, layout := range parsedLayouts(t) {
		raw := cacheTierRecording(t, layout, nil)
		for _, want := range []string{`"CachedTokenRateBySource":`, `"KVTransferRates":`, `"MemoryLayout":`} {
			require.Contains(t, string(raw), want, "precondition: the recording holds it")
		}

		out, _, err := Export(raw, ExportOptions{Key: []byte("k")})
		require.NoError(t, err, "the export refused the layout %q", layout)
		for _, secret := range []string{"payroll", "acme-llm"} {
			assert.NotContains(t, string(out), secret, "the export kept a name")
		}
		for _, s := range whatif.Sources() {
			assert.Contains(t, string(out), `"Source":"`+s+`"`, "the export changed a tier")
		}
		for _, k := range domain.KVTransferKinds() {
			assert.Contains(t, string(out), `"Kind":"`+k+`"`, "the export changed a transfer kind")
		}
		assert.Contains(t, string(out), `"MemoryLayout":"`+strings.ReplaceAll(layout, `"`, `\"`)+`"`,
			"the export changed the memory layout")

		r, err := Replay(context.Background(), out, Options{Mode: Fidelity})
		require.NoError(t, err)
		assert.Empty(t, r.Failed())
		assert.Empty(t, r.Mismatches)
		assert.Equal(t, string(out), string(r.Recording), "the export replays byte for byte")
	}
}

// A name standing in any of the cache-tier fields is still refused: a tier or
// a transfer kind the code does not define, and a memory layout with a key no
// parser writes or a value that could be a name. The refusal does not repeat
// the name.
func TestExport_RefusesANameInTheCacheTierFields(t *testing.T) {
	const secret = "zz-secret-team"
	layout := parsedLayouts(t)[0]
	cases := map[string]struct {
		layout string
		edit   func(*domain.ReplicaMetrics)
		field  string
	}{
		"a tier": {layout: layout, field: "CachedTokenRateBySource.Source",
			edit: func(r *domain.ReplicaMetrics) { r.CachedTokenRateBySource[0].Source = secret }},
		"a transfer kind": {layout: layout, field: "KVTransferRates.Kind",
			edit: func(r *domain.ReplicaMetrics) { r.KVTransferRates[0].Kind = secret }},
		"a layout key":              {layout: layout + ";" + secret + "=true", field: "MemoryLayout"},
		"a layout backend":          {layout: "all2all_backend=" + secret, field: "MemoryLayout"},
		"an SGLang layout backend":  {layout: "enable_dp_attention=true;moe_a2a_backend=" + secret, field: "MemoryLayout"},
		"a layout size":             {layout: "kv_cache_memory_bytes=" + secret, field: "MemoryLayout"},
		"a string in the EPLB JSON": {layout: `eplb_config={"window_size":"` + secret + `"}`, field: "MemoryLayout"},
		"an env backend":            {layout: "env.VLLM_ALL2ALL_BACKEND=" + secret, field: "MemoryLayout"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw := cacheTierRecording(t, tc.layout, tc.edit)
			require.Contains(t, string(raw), secret, "precondition: the recording holds the name")
			out, _, err := Export(raw, ExportOptions{Key: []byte("k")})
			require.Error(t, err, "the export let a name through")
			assert.Nil(t, out)
			assert.Contains(t, err.Error(), tc.field, "the refusal names the field")
			assert.NotContains(t, err.Error(), secret, "the refusal repeats the name")
		})
	}
}
