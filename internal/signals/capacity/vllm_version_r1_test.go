package capacity

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Like vllm_version_defaults_test.go, this file uses only what existed before
// releases were read, so it runs against the parent commit as a negative
// control.

// v0.34 keeps the extensible KV cache, and its 1.0 default, when the engine
// asks for it explicitly, connector or not. It turns it off -- 0.92 -- for
// elastic EP and for a fixed number of GPU blocks.
func TestV034MemoryShareFollowsTheExtensibleCache(t *testing.T) {
	nixl := []string{"--kv-transfer-config", `{"kv_connector":"NixlConnector","kv_role":"kv_both"}`}
	cases := []struct {
		name  string
		flags []string
		want  float64
	}{
		{"explicit extensible cache with a connector", append([]string{"--enable-extensible-kv-cache"}, nixl...), 1.0},
		{"elastic EP", []string{"--enable-elastic-ep"}, 0.92},
		{"a fixed number of GPU blocks", []string{"--num-gpu-blocks-override", "1000"}, 0.92},
		{"the device-memory alias", []string{"--device-memory-utilization", "0.8"}, 0.8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filedParams(t, acceleratorH, versionedDeployment(imageV034, nil, tc.flags...))
			if p.GpuMemoryUtilization != tc.want {
				t.Errorf("gpu_memory_utilization = %v, want %v", p.GpuMemoryUtilization, tc.want)
			}
		})
	}
}

// --num-gpu-blocks-override is the one path where the block-size default
// moves k1: the total is the blocks times the block size, 64 from v0.34.
func TestBlockOverrideTotalUsesTheReleasesBlockSize(t *testing.T) {
	for _, tc := range []struct {
		image string
		want  int64
	}{
		{imageV0271, 1000 * 16}, {imageV034, 1000 * 64}, {imageUnknown, 1000 * 16},
	} {
		s := NewStore()
		s.LoadFromScaleTarget("ns", "m", "v", acceleratorH, 1,
			versionedDeployment(tc.image, nil, "--num-gpu-blocks-override", "1000"), time.Now())
		rec := s.Get("ns", "m", "v")
		if rec == nil || rec.TotalKvCapacityTokens != tc.want {
			t.Errorf("%s: total KV %v, want %d", tc.image, rec, tc.want)
		}
	}
}

// A wide-EP engine with a batched all-to-all and more than one DP rank runs
// vLLM's batched-DP default of 256 tokens, in every release and on every
// accelerator, when it does not set the budget.
func TestBatchedDPMoEBudget(t *testing.T) {
	wide := []string{"--enable-expert-parallel", "--data-parallel-size", "4"}
	cases := []struct {
		name, image, accelerator string
		flags                    []string
		env                      []corev1.EnvVar
		want                     int64
	}{
		{"deepep_low_latency on B200, v0.34", imageV034, acceleratorB,
			append(wide, "--all2all-backend", "deepep_low_latency"), nil, 256},
		{"nixl_ep on H100, v0.27.1", imageV0271, acceleratorH,
			append(wide, "--all2all-backend", "nixl_ep"), nil, 256},
		{"the backend from the environment, unknown image", imageUnknown, acceleratorA,
			wide, []corev1.EnvVar{{Name: "VLLM_ALL2ALL_BACKEND", Value: "deepep_low_latency"}}, 256},
		{"the flag wins over the environment", imageV034, acceleratorB,
			append(wide, "--all2all-backend", "deepep_high_throughput"),
			[]corev1.EnvVar{{Name: "VLLM_ALL2ALL_BACKEND", Value: "deepep_low_latency"}}, 16384},
		{"one DP rank", imageV034, acceleratorB,
			[]string{"--enable-expert-parallel", "--all2all-backend", "deepep_low_latency"}, nil, 16384},
		{"no expert parallelism", imageV034, acceleratorB,
			[]string{"--data-parallel-size", "4", "--all2all-backend", "deepep_low_latency"}, nil, 16384},
		{"a stated budget wins", imageV034, acceleratorB,
			append(wide, "--all2all-backend", "deepep_low_latency", "--max-num-batched-tokens", "512"), nil, 512},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := versionedDeployment(tc.image, nil, tc.flags...)
			st.GetLeaderPodTemplateSpec().Spec.Containers[0].Env = tc.env
			p := filedParams(t, tc.accelerator, st)
			if p.EffectiveMaxBatchedTokens != tc.want {
				t.Errorf("budget = %d, want %d", p.EffectiveMaxBatchedTokens, tc.want)
			}
		})
	}
}

// The step budget depends on the accelerator, and the arguments digest
// hashes the budget. A variant whose accelerator reads empty for a cycle must
// keep its arguments: a different digest would read as a rollout.
func TestAnEmptyAcceleratorReadingKeepsTheArguments(t *testing.T) {
	s := NewStore()
	st := versionedDeployment(imageUnknown, nil)
	now := time.Now()
	s.LoadFromScaleTarget("ns", "m", "v", acceleratorA, 1, st, now)
	first := s.Get("ns", "m", "v").EngineParams
	tag, budget := first.ArgsTag(), first.EffectiveMaxBatchedTokens

	s.LoadFromScaleTarget("ns", "m", "v", "", 1, st, now.Add(time.Second))
	again := s.Get("ns", "m", "v").EngineParams
	if again.ArgsTag() != tag || again.EffectiveMaxBatchedTokens != budget {
		t.Errorf("an empty accelerator reading moved the arguments: budget %d -> %d, tag %s -> %s",
			budget, again.EffectiveMaxBatchedTokens, tag, again.ArgsTag())
	}

	// Another variant never seen on an accelerator keeps the unsized default.
	s.LoadFromScaleTarget("ns", "m", "w", "", 1, st, now)
	if got := s.Get("ns", "m", "w").EngineParams.EffectiveMaxBatchedTokens; got != 8192 {
		t.Errorf("a variant never seen on an accelerator: budget %d, want 8192", got)
	}
}
