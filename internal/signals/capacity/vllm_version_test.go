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
		"vllm/vllm-openai:v0.27.1,vllm/vllm-openai:v0.27.0": {0, 27},
		"vllm/vllm-openai:v0.27.1,vllm/vllm-openai:v0.34.0": {},
		"vllm/vllm-openai:v0.27.1,quay.io/x/router:v0.27.1": {},
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
// tier for it, and a name not in the table must say so.
func TestAcceleratorMemoryTiers(t *testing.T) {
	cases := []struct {
		name string
		tier string // "large" >=160 GiB, "mid" >=70 GiB not A100, "small", "unknown"
	}{
		{"NVIDIA-B200", "large"}, {"NVIDIA-GB200", "large"}, {"B300", "large"},
		{"AMD-Instinct-MI300X", "large"},
		{"NVIDIA-H100-80GB-HBM3", "mid"}, {"nvidia-h100-80gb", "mid"}, {"H200", "mid"},
		{"NVIDIA-H20", "mid"}, {"NVIDIA-GH200-480GB", "mid"}, {"NVIDIA-A800-SXM4-80GB", "mid"},
		{"NVIDIA-A100-SXM4-80GB", "small"}, {"A100_NVLINK_80GB", "small"}, {"nvidia-tesla-a100", "small"},
		{"NVIDIA-L40S", "small"}, {"NVIDIA-L4", "small"}, {"NVIDIA-A10G", "small"}, {"Tesla-T4", "small"},
		{"", "unknown"}, {"some-new-accelerator", "unknown"},
	}
	for _, tc := range cases {
		gib, a100, ok := acceleratorMemoryGiB(tc.name)
		got := "unknown"
		switch {
		case !ok:
		case gib >= 160:
			got = "large"
		case gib >= 70 && !a100:
			got = "mid"
		default:
			got = "small"
		}
		if got != tc.tier {
			t.Errorf("%q: tier %s (%.0f GiB, a100=%v), want %s", tc.name, got, gib, a100, tc.tier)
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
