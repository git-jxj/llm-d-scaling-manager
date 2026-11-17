package main

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// The vLLM release a fleet runs decides the defaults of the flags its engines
// are not given (docs/reference/vllm-versions.md), and the planner prices
// each variant with them: the step budget, the memory share and the block
// size the answer's inputs name are the release's, as the controller's are.
// A capture whose stand-in names no release reads every fleet as unknown.

// engineOfAnswer is the decode variant's engine, as replicas-to-slo's
// priced inputs name it, for the sweep's window read with extra flags.
func engineOfAnswer(t *testing.T, srv *promtest.Server, extra ...string) wireEngine {
	t.Helper()
	out, code := runCmd(t, slices.Concat([]string{"replicas-to-slo"}, sweepWindow(srv.URL), extra, []string{"-o", "json"})...)
	w := decodeOutput(t, out)
	require.Equalf(t, 0, code, "replicas-to-slo: exit %d\n%s", code, out)
	require.NotEmpty(t, w.Roles, out)
	require.NotNil(t, w.Roles[0].Inputs, out)
	require.NotNil(t, w.Roles[0].Inputs.Engine, "no engine in the priced inputs:\n%s", out)
	return *w.Roles[0].Inputs.Engine
}

func TestEngine_TheReleaseSetsThePricedDefaults(t *testing.T) {
	srv := sweepServer(t)
	cases := []struct {
		name  string
		flags []string
		want  wireEngine
	}{
		{"v0.34 on an H200", []string{"--accelerator", "NVIDIA-H200", "--vllm-version", "0.34"},
			wireEngine{VLLMVersion: "0.34", VLLMVersionFrom: whatifsource.VLLMVersionFromFlag,
				StepBudget: 8192, GPUMemoryUtilization: 1.0, BlockSize: 64}},
		{"v0.27.1 on an H200, from the image", []string{"--accelerator", "NVIDIA-H200",
			"--engine-image", "quay.io/vllm/vllm-openai:v0.27.1"},
			wireEngine{VLLMVersion: "0.27", VLLMVersionFrom: whatifsource.VLLMVersionFromEngineImage,
				StepBudget: 8192, GPUMemoryUtilization: 0.92, BlockSize: 16}},
		{"unknown on an H200", []string{"--accelerator", "NVIDIA-H200"},
			wireEngine{StepBudget: 8192, GPUMemoryUtilization: 0.9, BlockSize: 16}},
		{"an image whose tag is not vLLM's", []string{"--accelerator", "NVIDIA-H200",
			"--engine-image", "ghcr.io/llm-d/llm-d-cuda:v0.4.0"},
			wireEngine{StepBudget: 8192, GPUMemoryUtilization: 0.9, BlockSize: 16}},
		// The budget is where the release moves the estimate: from v0.28 an
		// accelerator of 160 GiB or more steps 16384 tokens, and an unknown
		// release gets the smaller of the two.
		{"v0.34 on a B200", []string{"--accelerator", "NVIDIA-B200", "--vllm-version", "0.34"},
			wireEngine{VLLMVersion: "0.34", VLLMVersionFrom: whatifsource.VLLMVersionFromFlag,
				StepBudget: 16384, GPUMemoryUtilization: 1.0, BlockSize: 64}},
		{"unknown on a B200", []string{"--accelerator", "NVIDIA-B200"},
			wireEngine{StepBudget: 8192, GPUMemoryUtilization: 0.9, BlockSize: 16}},
		// A flag the engine is given wins over every release's default.
		{"v0.34 with the flags given", []string{"--accelerator", "NVIDIA-B200", "--vllm-version", "0.34",
			"--engine-args", "--max-num-batched-tokens=4096 --gpu-memory-utilization=0.85 --block-size=32"},
			wireEngine{VLLMVersion: "0.34", VLLMVersionFrom: whatifsource.VLLMVersionFromFlag,
				StepBudget: 4096, StepBudgetSource: engineSetByFlag, GPUMemoryUtilization: 0.85,
				GPUMemoryUtilizationSource: engineSetByFlag, BlockSize: 32, BlockSizeSource: engineSetByFlag}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := c.want
			want.Engine = "vllm"
			for _, s := range []*string{&want.StepBudgetSource, &want.GPUMemoryUtilizationSource, &want.BlockSizeSource} {
				if *s == "" {
					*s = engineDefault
				}
			}
			assert.Equal(t, want, engineOfAnswer(t, srv, c.flags...))
		})
	}
}

// A wide-EP engine with a batched all-to-all and more than one DP rank steps
// 256 tokens in every release, as vLLM's own default: the capture's window
// line, which the generated run files every scale target from, holds it.
func TestEngine_BatchedDPStepsTwoHundredFiftySixTokens(t *testing.T) {
	srv := sweepServer(t)
	for _, version := range []string{"0.34", "0.27"} {
		out := t.TempDir() + "/c.gz"
		text, err := runCLI(slices.Concat([]string{"capture"}, sweepWindow(srv.URL), []string{"--accelerator", "NVIDIA-H200",
			"--vllm-version", version, "--engine-args",
			"--enable-expert-parallel --data-parallel-size=2 --all2all-backend=deepep_low_latency", "-o", out})...)
		require.NoError(t, err)
		params := capturedParams(t, out)
		assert.Equalf(t, int64(256), params.EffectiveMaxBatchedTokens, "v%s: step budget", version)
		assert.Contains(t, text, fmt.Sprintf("vLLM %s (from --vllm-version)", version))
	}
}

// What kube-state-metrics shows the workload running wins over the flags:
// the engine container's vllm-openai image, and over it the Deployment's
// scaling-manager.llmd.ai/vllm-version annotation, as the controller reads
// them. A sidecar's image names no release and is passed over.
func TestEngine_TheLiveWorkloadWinsOverTheFlags(t *testing.T) {
	k := whatifsource.KSMQueries(sweepNS)
	withKSM := func(annotation string) *promtest.Server {
		srv := sweepServer(t)
		for j := range sweepPods {
			pod := fmt.Sprintf("m-dec-7c9d-%d", j)
			sweepKSM(srv, pod, false)
			for container, image := range map[string]string{"vllm": "quay.io/vllm/vllm-openai:v0.34.0",
				"routing-proxy": "ghcr.io/llm-d/llm-d-routing-sidecar:v0.4.0"} {
				srv.Add(k.PodContainers, promtest.Const(map[string]string{"namespace": sweepNS, "pod": pod,
					"container": container, "image_spec": image, "image": image}, 1))
			}
		}
		if annotation != "" {
			srv.Add(k.DeploymentAnnotations, promtest.Const(map[string]string{"namespace": sweepNS, "deployment": "m-dec",
				"annotation_scaling_manager_llmd_ai_vllm_version": annotation}, 1))
		}
		return srv
	}
	got := engineOfAnswer(t, withKSM(""), "--kube-state-metrics", "--vllm-version", "0.27")
	assert.Equal(t, "0.34", got.VLLMVersion)
	assert.Equal(t, "image tag", got.VLLMVersionFrom)
	assert.Equal(t, int64(64), got.BlockSize)

	got = engineOfAnswer(t, withKSM("0.27"), "--kube-state-metrics")
	assert.Equal(t, "0.27", got.VLLMVersion)
	assert.Equal(t, "annotation", got.VLLMVersionFrom)
	assert.Equal(t, int64(16), got.BlockSize)

	text, code := runCmd(t, slices.Concat([]string{"replicas-to-slo"}, sweepWindow(withKSM("").URL), []string{"--kube-state-metrics"})...)
	require.Equal(t, 0, code, text)
	assert.Contains(t, text, "vLLM 0.34 (from image tag): step budget 8192 (default), GPU memory 1 (default), block size 64 (default)")
}

// --vllm-version takes a release, and refuses anything else rather than
// read the fleet as unknown.
func TestEngine_TheVersionFlagIsARelease(t *testing.T) {
	srv := sweepServer(t)
	_, err := runCLI(slices.Concat([]string{"replicas-to-slo"}, sweepWindow(srv.URL), []string{"--vllm-version", "latest"})...)
	require.Error(t, err)
	assert.Equal(t, 2, exitCode(err))
	assert.Contains(t, err.Error(), `--vllm-version "latest" is not a vLLM release`)
}

// capturedParams is the engine parameters of a capture's one variant, as its
// window line holds them.
func capturedParams(t *testing.T, path string) capacity.EngineParams {
	t.Helper()
	b, err := readMaybeGzipped(path)
	require.NoError(t, err)
	read, err := learnrec.Read(b)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(read.Events), 2)
	var window learnreplay.CaptureWindow
	require.NoError(t, read.Events[1].DecodeData(&window))
	require.Len(t, window.Variants, 1)
	return window.Variants[0].Params
}
