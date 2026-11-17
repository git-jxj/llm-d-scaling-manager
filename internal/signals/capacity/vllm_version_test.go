package capacity

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/annotations"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

func TestParseVLLMVersion(t *testing.T) {
	cases := map[string]VLLMVersion{
		"0.26":            {0, 26},
		"v0.27.1":         {0, 27},
		"0.32.0":          {0, 32},
		"v0.34.0-cu130":   {0, 34},
		"0.34.0rc1":       {0, 34},
		"0.33.0+g87ecf7d": {0, 33},
		"v1.2.0":          {1, 2},
		"":                {},
		"latest":          {},
		"nightly":         {},
		"0":               {},
		"0.0":             {},
		"v":               {},
		"0.x":             {},
		"sha-1a2b3c4":     {},
		"20260101-build":  {},
	}
	for in, want := range cases {
		if got := ParseVLLMVersion(in); got != want {
			t.Errorf("ParseVLLMVersion(%q) = %v, want %v", in, got, want)
		}
	}
}

// Only vLLM's own repository names vLLM's release in its tag. Another
// project's tag is that project's version, and reading it as vLLM's would
// give an engine a release it does not run.
func TestVLLMVersionFromImage(t *testing.T) {
	cases := map[string]VLLMVersion{
		"vllm/vllm-openai:v0.26.0":                         {0, 26},
		"docker.io/vllm/vllm-openai:v0.27.1":               {0, 27},
		"registry.example.com/mirror/vllm-openai:v0.34.0":  {0, 34},
		"vllm/vllm-openai-rocm:v0.32.0":                    {0, 32},
		"vllm/vllm-openai:v0.34.0@sha256:0123":             {0, 34},
		"localhost:5000/vllm-openai:v0.30.0":               {0, 30},
		"vllm/vllm-openai:latest":                          {},
		"vllm/vllm-openai@sha256:0123":                     {},
		"localhost:5000/vllm-openai":                       {},
		"quay.io/llm-d/llm-d-cuda:v0.4.0":                  {},
		"registry.example.com/team/llm-d-cuda:sha-1a2b3c4": {},
		"ghcr.io/llm-d/llm-d-inference-sim:v0.9.0":         {},
		"": {},
		"vllm/vllm-openai:v0.27.1,vllm/vllm-openai:v0.27.0":      {0, 27},
		"vllm/vllm-openai:v0.27.1,vllm/vllm-openai:v0.34.0":      {},
		"vllm/vllm-openai:v0.27.1,quay.io/x/router:v0.27.1":      {},
		"vllm/vllm-openai:v0.34.0rc1":                            {0, 34},
		"vllm/vllm-openai:nightly-0123456789abcdef":              {},
		"registry.example.com/team/vllm:0.5.2":                   {},
		"vllm/vllm:v0.34.0":                                      {},
		"registry.example.com/x/vllm-openai-patched:v0.33.0-fix": {},
	}
	for in, want := range cases {
		if got := VLLMVersionFromImage(in); got != want {
			t.Errorf("VLLMVersionFromImage(%q) = %v, want %v", in, got, want)
		}
	}
}

// The annotation is read when the tag says nothing, and wins when both do.
func TestVLLMVersionAnnotationWinsOverTheTag(t *testing.T) {
	cases := []struct {
		annotation, image string
		want              VLLMVersion
	}{
		{"", imageV0271, VLLMVersion{0, 27}},
		{"0.34", imageUnknown, VLLMVersion{0, 34}},
		{"0.34", imageV0271, VLLMVersion{0, 34}},
		{"not a version", imageV0271, VLLMVersion{0, 27}},
		{"not a version", imageUnknown, VLLMVersion{}},
	}
	for _, tc := range cases {
		annots := map[string]string{}
		if tc.annotation != "" {
			annots[annotations.VLLMVersion] = tc.annotation
		}
		if got := vllmVersionOf(annots, tc.image); got != tc.want {
			t.Errorf("annotation %q, image %q: got %v, want %v", tc.annotation, tc.image, got, tc.want)
		}
	}
}

func TestVLLMVersionOrdering(t *testing.T) {
	v := VLLMVersion{0, 34}
	if !v.AtLeast(0, 34) || !v.AtLeast(0, 28) || v.AtLeast(0, 35) || v.AtLeast(1, 0) {
		t.Errorf("AtLeast is wrong for %v", v)
	}
	if (VLLMVersion{}).AtLeast(0, 0) {
		t.Error("an unknown release is at least nothing: it must never get a newer default")
	}
	if got := v.String(); got != "0.34" {
		t.Errorf("String() = %q", got)
	}
	if got := (VLLMVersion{}).String(); got != "" {
		t.Errorf("unknown String() = %q, want empty", got)
	}
}

// Node labels spell one accelerator several ways; each must land in vLLM's
// budget tier for it on v0.34: 16384 at 160 GiB or more, 8192 at 70 GiB or
// more unless an A100, 2048 otherwise, and 8192 for a name not in the table.
func TestAcceleratorBudgetTiers(t *testing.T) {
	v034 := VLLMVersion{0, 34}
	cases := map[string]int64{
		"NVIDIA-B200": 16384, "NVIDIA-GB200": 16384, "B300": 16384, "NVIDIA-B100": 16384,
		"AMD-Instinct-MI300X": 16384, "AMD-Instinct-MI308X": 16384,
		"NVIDIA-H100-80GB-HBM3": 8192, "nvidia-h100-80gb": 8192, "H200": 8192,
		"NVIDIA-H20": 8192, "NVIDIA-GH200-480GB": 8192, "NVIDIA-A800-SXM4-80GB": 8192,
		"AMD-Instinct-MI300A":   8192,
		"NVIDIA-A100-SXM4-80GB": 2048, "A100_NVLINK_80GB": 2048, "nvidia-tesla-a100": 2048,
		"NVIDIA-A800-SXM4-40GB": 2048, "NVIDIA-L40S": 2048, "NVIDIA-L20": 2048,
		"NVIDIA-L4": 2048, "NVIDIA-A10G": 2048, "Tesla-T4": 2048,
		"": 8192, "some-new-accelerator": 8192,
	}
	for name, want := range cases {
		if got := vllmDefaultMaxNumBatchedTokens(v034, name); got != want {
			t.Errorf("%q on v0.34: budget %d, want %d", name, got, want)
		}
	}
}

// vLLM moved the GB10 into the 2048 tier at v0.33 despite its memory. Before
// that it ran 8192; an unknown release gets the smaller.
func TestGB10BudgetByRelease(t *testing.T) {
	for _, tc := range []struct {
		v    VLLMVersion
		want int64
	}{
		{VLLMVersion{0, 27}, 8192}, {VLLMVersion{0, 32}, 8192},
		{VLLMVersion{0, 33}, 2048}, {VLLMVersion{0, 34}, 2048}, {VLLMVersion{}, 2048},
	} {
		if got := vllmDefaultMaxNumBatchedTokens(tc.v, "NVIDIA-GB10"); got != tc.want {
			t.Errorf("GB10 on %q: budget %d, want %d", tc.v.String(), got, tc.want)
		}
	}
}

// SGLang has its own defaults; a vLLM release must never be applied to it.
func TestSGLangIsNotGivenVLLMDefaults(t *testing.T) {
	st := versionedDeployment("vllm/vllm-openai:v0.34.0", nil)
	p := ParseEngineArgsOn(inferenceengine.EngineSGLang, st, acceleratorB)
	if p.VLLMVersion.Known() {
		t.Errorf("SGLang engine read a vLLM release: %v", p.VLLMVersion)
	}
}
