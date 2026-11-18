package saturation

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// Reading the vLLM release changes which default an unset flag resolves to,
// and the resolved value is what is hashed, so FingerprintVersion stays 4. A
// snapshot written before the change restores into the controller after it:
// as the variant's own where the resolved values did not move, and as a
// lower-only prior where they did. Nothing else relearns.
//
// before is what the parser resolved before releases were read: memory share
// 0.9, block size 16 and budget 8192 for every engine that left them unset.
func TestVLLMDefaultsMoveOnlyTheVariantsTheyChange(t *testing.T) {
	deployment := func(image string) scaletarget.ScaleTargetAccessor {
		return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "vllm", Image: image, Command: []string{"vllm", "serve", "m"}}},
			}}},
		})
	}
	before := func(p capacity.EngineParams) *capacity.EngineParams {
		p.GpuMemoryUtilization, p.BlockSize, p.EffectiveMaxBatchedTokens = 0.9, 16, 8192
		return &p
	}
	const unknownImage = "registry.example.com/team/engine:build-1"
	for name, tc := range map[string]struct {
		image, accelerator string
		own                bool
	}{
		"unknown image on an H100: nothing moved":           {unknownImage, "NVIDIA-H100-80GB-HBM3", true},
		"unknown image on an A100: budget 8192 to 2048":     {unknownImage, "NVIDIA-A100-SXM4-80GB", false},
		"vllm-openai v0.27.1 on an H100: share 0.9 to 0.92": {"vllm/vllm-openai:v0.27.1", "NVIDIA-H100-80GB-HBM3", false},
	} {
		t.Run(name, func(t *testing.T) {
			after := capacity.ParseEngineArgsOn(inferenceengine.EngineVLLM, deployment(tc.image), tc.accelerator)
			old := before(after)
			if same := old.ArgsFingerprint() == after.ArgsFingerprint(); same != tc.own {
				t.Fatalf("precondition: arguments digest unchanged = %v, want %v (after: share %v, block %d, budget %d)",
					same, tc.own, after.GpuMemoryUtilization, after.BlockSize, after.EffectiveMaxBatchedTokens)
			}

			a, _, clock := waProcess(old)
			waRun(t, a, clock, 4000, 6)
			saved := a.ExportLearnedState()

			second, _, _ := waProcess(&after)
			second.RestoreLearnedState(saved)
			r, done := second.installRestored("test-ns", "test-model", "variant-a")
			if !done {
				t.Fatal("precondition: the restore was not checked")
			}
			switch {
			case tc.own && (!r.Installed || r.Outcome != RestoredWhole):
				t.Errorf("defaults did not move: installed=%v outcome=%s, want its own state back (%s)", r.Installed, r.Outcome, RestoredWhole)
			case !tc.own && (r.Installed || r.Outcome != RestoredPrior || r.Priors == 0):
				t.Errorf("defaults moved: installed=%v outcome=%s priors=%d, want a lower-only prior (%s)", r.Installed, r.Outcome, r.Priors, RestoredPrior)
			}
		})
	}
}
