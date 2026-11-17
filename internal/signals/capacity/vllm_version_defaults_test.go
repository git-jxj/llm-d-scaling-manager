package capacity

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// This file uses only what existed before releases were read
// (LoadFromScaleTarget and Get), so it runs unchanged against the parent
// commit as the negative control.

// versionAnnotation is annotations.VLLMVersion, spelled out so this file
// builds where the constant does not exist.
const versionAnnotation = "llm-d.ai/vllm-version"

// The images each supported release is pulled as, and one no tag describes.
const (
	imageV026     = "vllm/vllm-openai:v0.26.0"
	imageV0271    = "vllm/vllm-openai:v0.27.1"
	imageV032     = "vllm/vllm-openai:v0.32.0"
	imageV034     = "vllm/vllm-openai:v0.34.0"
	imageUnknown  = "registry.example.com/team/engine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	acceleratorH  = "NVIDIA-H100-80GB-HBM3"
	acceleratorB  = "NVIDIA-B200"
	acceleratorA  = "NVIDIA-A100-SXM4-80GB"
	acceleratorL  = "NVIDIA-L40S"
	acceleratorGB = "NVIDIA-GB200"
)

// versionedDeployment is a vLLM Deployment on image with the given flags
// after `vllm serve m`, and the given scale-target annotations.
func versionedDeployment(image string, annots map[string]string, flags ...string) scaletarget.ScaleTargetAccessor {
	return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Annotations: annots},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:    "vllm",
				Image:   image,
				Command: []string{"vllm", "serve", "m"},
				Args:    flags,
			}}},
		}},
	})
}

// filedParams is what the store files for the deployment on accelerator.
func filedParams(t *testing.T, accelerator string, st scaletarget.ScaleTargetAccessor) *EngineParams {
	t.Helper()
	s := NewStore()
	s.LoadFromScaleTarget("ns", "m", "v", accelerator, 1, st, time.Now())
	rec := s.Get("ns", "m", "v")
	if rec == nil || rec.EngineParams == nil {
		t.Fatal("no record filed")
	}
	return rec.EngineParams
}

// A flag the engine is not given runs at its release's default. Each case is
// one release, and the unknown image keeps the values used before releases
// were read.
func TestUnsetFlagsTakeTheReleasesDefault(t *testing.T) {
	cases := []struct {
		name      string
		image     string
		annots    map[string]string
		flags     []string
		wantGMU   float64
		wantBlock int64
	}{
		{name: "v0.26", image: imageV026, wantGMU: 0.92, wantBlock: 16},
		{name: "v0.27.1", image: imageV0271, wantGMU: 0.92, wantBlock: 16},
		{name: "v0.32", image: imageV032, wantGMU: 0.92, wantBlock: 16},
		{name: "v0.34", image: imageV034, wantGMU: 1.0, wantBlock: 64},
		{name: "v0.34 with NIXL keeps 0.92", image: imageV034,
			flags:   []string{"--kv-transfer-config", `{"kv_connector":"NixlConnector","kv_role":"kv_both"}`},
			wantGMU: 0.92, wantBlock: 64},
		{name: "v0.34 with a fixed KV size keeps 0.92", image: imageV034,
			flags: []string{"--kv-cache-memory-bytes", "40000000000"}, wantGMU: 0.92, wantBlock: 64},
		{name: "v0.34 extensible cache turned off", image: imageV034,
			flags: []string{"--no-enable-extensible-kv-cache"}, wantGMU: 0.92, wantBlock: 64},
		{name: "unknown", image: imageUnknown, wantGMU: 0.9, wantBlock: 16},
		{name: "unknown, annotated v0.34", image: imageUnknown,
			annots: map[string]string{versionAnnotation: "0.34"}, wantGMU: 1.0, wantBlock: 64},
		{name: "unknown, annotated v0.27.1", image: imageUnknown,
			annots: map[string]string{versionAnnotation: "v0.27.1"}, wantGMU: 0.92, wantBlock: 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filedParams(t, acceleratorH, versionedDeployment(tc.image, tc.annots, tc.flags...))
			if p.GpuMemoryUtilization != tc.wantGMU {
				t.Errorf("gpu_memory_utilization = %v, want %v", p.GpuMemoryUtilization, tc.wantGMU)
			}
			if p.BlockSize != tc.wantBlock {
				t.Errorf("block_size = %d, want %d", p.BlockSize, tc.wantBlock)
			}
		})
	}
}

// A flag the engine is given wins over every release's default.
func TestStatedFlagsWinOverEveryRelease(t *testing.T) {
	for _, image := range []string{imageV026, imageV0271, imageV032, imageV034, imageUnknown} {
		t.Run(image, func(t *testing.T) {
			p := filedParams(t, acceleratorB, versionedDeployment(image, nil,
				"--gpu-memory-utilization", "0.85", "--block-size", "32", "--max-num-batched-tokens", "4096"))
			if p.GpuMemoryUtilization != 0.85 || p.BlockSize != 32 || p.EffectiveMaxBatchedTokens != 4096 {
				t.Errorf("got gmu %v, block %d, budget %d; want the flags' 0.85, 32, 4096",
					p.GpuMemoryUtilization, p.BlockSize, p.EffectiveMaxBatchedTokens)
			}
		})
	}
}

// The step budget an engine runs without --max-num-batched-tokens depends on
// the release and the accelerator. From v0.28 a 160 GiB device gets 16384;
// below 70 GiB, or on an A100, every release gets 2048.
func TestUnsetBudgetFollowsReleaseAndAccelerator(t *testing.T) {
	cases := []struct {
		image, accelerator string
		annots             map[string]string
		want               int64
	}{
		{image: imageV026, accelerator: acceleratorH, want: 8192},
		{image: imageV0271, accelerator: acceleratorH, want: 8192},
		{image: imageV032, accelerator: acceleratorH, want: 8192},
		{image: imageV034, accelerator: acceleratorH, want: 8192},
		{image: imageUnknown, accelerator: acceleratorH, want: 8192},

		{image: imageV026, accelerator: acceleratorB, want: 8192},
		{image: imageV0271, accelerator: acceleratorB, want: 8192},
		{image: imageV032, accelerator: acceleratorB, want: 16384},
		{image: imageV034, accelerator: acceleratorB, want: 16384},
		{image: imageV034, accelerator: acceleratorGB, want: 16384},
		{image: imageUnknown, accelerator: acceleratorB, want: 8192},
		{image: imageUnknown, accelerator: acceleratorGB, annots: map[string]string{versionAnnotation: "0.34"}, want: 16384},

		{image: imageV026, accelerator: acceleratorA, want: 2048},
		{image: imageV0271, accelerator: acceleratorA, want: 2048},
		{image: imageV032, accelerator: acceleratorA, want: 2048},
		{image: imageV034, accelerator: acceleratorA, want: 2048},
		{image: imageUnknown, accelerator: acceleratorA, want: 2048},
		{image: imageV034, accelerator: acceleratorL, want: 2048},

		{image: imageV034, accelerator: "", want: 8192},
		{image: imageV034, accelerator: "some-new-accelerator", want: 8192},
	}
	for _, tc := range cases {
		t.Run(tc.image+" on "+tc.accelerator, func(t *testing.T) {
			p := filedParams(t, tc.accelerator, versionedDeployment(tc.image, tc.annots))
			if p.EffectiveMaxBatchedTokens != tc.want {
				t.Errorf("budget = %d, want %d", p.EffectiveMaxBatchedTokens, tc.want)
			}
		})
	}
}

// Two engines with the same flags on releases whose defaults differ run
// different engines, and must not price each other. Before releases were
// read, both resolved to the same defaults and compared equal.
func TestReleasesWithDifferentDefaultsAreNotCapacityCompatible(t *testing.T) {
	cases := []struct {
		name, a, b, accelerator string
		flags                   []string
		compatible              bool
	}{
		{name: "v0.27.1 vs v0.34 on H100", a: imageV0271, b: imageV034, accelerator: acceleratorH},
		{name: "v0.27.1 vs v0.32 on B200", a: imageV0271, b: imageV032, accelerator: acceleratorB},
		{name: "v0.26 vs v0.27.1 on H100", a: imageV026, b: imageV0271, accelerator: acceleratorH, compatible: true},
		{name: "v0.32 vs v0.34 with every default stated", a: imageV032, b: imageV034, accelerator: acceleratorH,
			flags: []string{"--gpu-memory-utilization", "0.9", "--block-size", "16"}, compatible: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := filedParams(t, tc.accelerator, versionedDeployment(tc.a, nil, tc.flags...))
			b := filedParams(t, tc.accelerator, versionedDeployment(tc.b, nil, tc.flags...))
			if got := a.IsCapacityCompatible(b); got != tc.compatible {
				t.Errorf("IsCapacityCompatible = %v, want %v (a: gmu %v block %d budget %d; b: gmu %v block %d budget %d)",
					got, tc.compatible,
					a.GpuMemoryUtilization, a.BlockSize, a.EffectiveMaxBatchedTokens,
					b.GpuMemoryUtilization, b.BlockSize, b.EffectiveMaxBatchedTokens)
			}
		})
	}
}
