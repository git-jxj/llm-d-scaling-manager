package whatifsource

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

const (
	imageV034 = "registry.example.com/vllm/vllm-openai:v0.34.0"
	imageV032 = "registry.example.com/vllm/vllm-openai:v0.32.1"
	imageV027 = "registry.example.com/vllm/vllm-openai:v0.27.1"
)

// releaseScan is a window of variants whose Pods ran images (counted per Pod
// per step) and whose Deployments carry vllm-version annotations.
func releaseScan(variants map[string]map[string]int, annotations map[string]map[string]int) *windowScan {
	scan := &windowScan{variants: map[string]*variantInfo{}}
	for name, images := range variants {
		scan.variants[name] = &variantInfo{model: testModel, role: "decode", gpus: map[int]int{}, accels: map[string]int{},
			images: images, versions: annotations[name]}
	}
	return scan
}

// variantRelease is each variant's release and where it was read from.
func variantRelease(t *testing.T, scan *windowScan, opts Options) (map[string]string, []string) {
	t.Helper()
	out, flags := windowVariants(scan, opts)
	got := map[string]string{}
	for _, v := range out {
		got[v.Name] = v.Params.VLLMVersion.String() + " " + v.Params.VLLMVersionFrom
	}
	return got, flags
}

// Two variants are read on their own: each runs its own release.
func TestRelease_EachVariantHasItsOwn(t *testing.T) {
	opts := Options{Engine: inferenceengine.EngineVLLM, Accelerator: "NVIDIA-H200", GPUsPerReplica: 1}
	got, flags := variantRelease(t, releaseScan(map[string]map[string]int{
		"m-old": {imageV027: 30}, "m-new": {imageV034: 30}}, nil), opts)
	assert.Equal(t, map[string]string{"m-old": "0.27 image tag", "m-new": "0.34 image tag"}, got)
	for _, f := range flags {
		assert.NotContains(t, f, "vLLM")
	}
}

// A variant whose Pods ran two releases in the window is read as the one
// most of them ran, and the capture says so, naming the others.
func TestRelease_ARolloutInTheWindowIsNamed(t *testing.T) {
	opts := Options{Engine: inferenceengine.EngineVLLM, Accelerator: "NVIDIA-H200", GPUsPerReplica: 1}
	got, flags := variantRelease(t, releaseScan(map[string]map[string]int{
		"m-dec": {imageV034: 40, imageV032: 20}, "m-pre": {imageV027: 30}}, nil), opts)
	assert.Equal(t, map[string]string{"m-dec": "0.34 image tag", "m-pre": "0.27 image tag"}, got)
	assert.Contains(t, flags, "variant m-dec ran more than one vLLM release in the window: mostly 0.34, also 0.32; read as 0.34")
	for _, f := range flags {
		assert.NotContains(t, f, "variant m-pre ran")
	}

	// The annotation decides first, and its values are the ones counted.
	got, flags = variantRelease(t, releaseScan(map[string]map[string]int{"m-dec": {imageV027: 60}},
		map[string]map[string]int{"m-dec": {"0.32": 10, "v0.34.0": 50}}), opts)
	assert.Equal(t, map[string]string{"m-dec": "0.34 annotation"}, got)
	assert.Contains(t, flags, "variant m-dec ran more than one vLLM release in the window: mostly 0.34, also 0.32; read as 0.34")

	// The release most Pods ran wins, counted by release, not by image: two
	// patch tags of 0.34 outnumber one 0.32 tag that is more common alone,
	// and the image is the most common one of that release. Annotations
	// that spell one release two ways count together too.
	out, flags := windowVariants(releaseScan(map[string]map[string]int{
		"m-dec": {imageV034: 20, "registry.example.com/vllm/vllm-openai:v0.34.1": 25, imageV032: 30}}, nil), opts)
	require.Len(t, out, 1)
	assert.Equal(t, capacity.VLLMVersion{Major: 0, Minor: 34}, out[0].Params.VLLMVersion)
	assert.Equal(t, "registry.example.com/vllm/vllm-openai:v0.34.1", out[0].Params.Image)
	assert.Contains(t, flags, "variant m-dec ran more than one vLLM release in the window: mostly 0.34, also 0.32; read as 0.34")
	got, _ = variantRelease(t, releaseScan(map[string]map[string]int{"m-dec": {imageV027: 60}},
		map[string]map[string]int{"m-dec": {"0.34": 20, "v0.34.0": 25, "0.32": 30}}), opts)
	assert.Equal(t, map[string]string{"m-dec": "0.34 annotation"}, got)
}

// What the workload shows wins over the flags, as in the controller, and a
// flag that disagrees is named; the flags stand in only where the workload
// shows nothing, and nothing at all is flagged as unknown.
func TestRelease_TheFlagsStandInAndDisagreementIsNamed(t *testing.T) {
	base := Options{Engine: inferenceengine.EngineVLLM, Accelerator: "NVIDIA-H200", GPUsPerReplica: 1}
	live := map[string]map[string]int{"m-dec": {imageV034: 30}}

	opts := base
	opts.VLLMVersion, opts.EngineImage = "0.27", imageV032
	got, flags := variantRelease(t, releaseScan(live, nil), opts)
	assert.Equal(t, map[string]string{"m-dec": "0.34 image tag"}, got)
	assert.Contains(t, flags, "variant m-dec runs vLLM 0.34 (from image tag), not 0.27 as --vllm-version states: the workload's is used")
	assert.Contains(t, flags, "variant m-dec runs vLLM 0.34 (from image tag), not 0.32 as --engine-image states: the workload's is used")

	// An llm-d image names no release: the flag gives it, and the engine's
	// own image is still the one the stand-in carries.
	llmd := map[string]map[string]int{"m-dec": {"ghcr.io/llm-d/llm-d-cuda:v0.4.0": 30}}
	opts = base
	opts.VLLMVersion = "0.34"
	out, flags := windowVariants(releaseScan(llmd, nil), opts)
	require.Len(t, out, 1)
	assert.Equal(t, capacity.VLLMVersion{Major: 0, Minor: 34}, out[0].Params.VLLMVersion)
	assert.Equal(t, VLLMVersionFromFlag, out[0].Params.VLLMVersionFrom)
	assert.Equal(t, "ghcr.io/llm-d/llm-d-cuda:v0.4.0", out[0].Params.Image)
	assert.NotContains(t, flags, fmt.Sprintf(FlagVLLMVersionUnknown, 1, 1))

	// Two flags that disagree, with nothing from the workload: the
	// conflict is named, and --vllm-version is used.
	opts = base
	opts.VLLMVersion, opts.EngineImage = "0.27", imageV034
	got, flags = variantRelease(t, releaseScan(map[string]map[string]int{"m-dec": {}}, nil), opts)
	assert.Equal(t, map[string]string{"m-dec": "0.27 --vllm-version"}, got)
	assert.Contains(t, flags, "variant m-dec: --vllm-version 0.27 and --engine-image (0.34) disagree; read as 0.27, from --vllm-version")
	opts.VLLMVersion = "0.34"
	_, flags = variantRelease(t, releaseScan(map[string]map[string]int{"m-dec": {}}, nil), opts)
	for _, f := range flags {
		assert.NotContains(t, f, "disagree")
	}

	got, flags = variantRelease(t, releaseScan(llmd, nil), base)
	assert.Equal(t, map[string]string{"m-dec": " none"}, got)
	assert.Contains(t, flags, fmt.Sprintf(FlagVLLMVersionUnknown, 1, 1))
}

// The engine container's image is the one whose tag names a release, else
// a Pod's only image; a Pod whose sidecars hide the engine names none.
func TestRelease_TheEngineImageOfAPod(t *testing.T) {
	assert.Equal(t, imageV034, engineImageOf([]string{"ghcr.io/llm-d/llm-d-routing-sidecar:v0.4.0", imageV034}))
	assert.Equal(t, "ghcr.io/llm-d/llm-d-cuda:v0.4.0", engineImageOf([]string{"ghcr.io/llm-d/llm-d-cuda:v0.4.0"}))
	assert.Empty(t, engineImageOf([]string{"ghcr.io/llm-d/llm-d-cuda:v0.4.0", "ghcr.io/llm-d/llm-d-routing-sidecar:v0.4.0"}))
	assert.Empty(t, engineImageOf(nil))
}
