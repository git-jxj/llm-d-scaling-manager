package main

import (
	"fmt"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// What the engine figures were read from: the engine's flag, or the default
// its release runs (docs/reference/vllm-versions.md).
const (
	engineSetByFlag = "flag"
	engineDefault   = "default"
)

// wireEngine is the engine a variant was priced as: which one, for vLLM the
// release and where it was read from, and the figures that release's
// defaults decide where the engine's flags set none.
type wireEngine struct {
	Engine string `json:"engine"`
	// VLLMVersion is the release ("0.34"), absent when unknown, and
	// VLLMVersionFrom where it was read: annotation, image tag,
	// --vllm-version or --engine-image.
	VLLMVersion     string `json:"vllmVersion,omitempty"`
	VLLMVersionFrom string `json:"vllmVersionFrom,omitempty"`
	// StepBudget is max_num_batched_tokens per step, GPUMemoryUtilization
	// the memory share and BlockSize the KV block, each with its source:
	// flag or default.
	StepBudget                 int64   `json:"stepBudget"`
	StepBudgetSource           string  `json:"stepBudgetSource"`
	GPUMemoryUtilization       float64 `json:"gpuMemoryUtilization"`
	GPUMemoryUtilizationSource string  `json:"gpuMemoryUtilizationSource"`
	BlockSize                  int64   `json:"blockSize"`
	BlockSizeSource            string  `json:"blockSizeSource"`
}

// engineOf is the wire's account of the engine p describes; nil with none.
func engineOf(p *capacity.EngineParams) *wireEngine {
	if p == nil {
		return nil
	}
	source := func(set bool) string {
		if set {
			return engineSetByFlag
		}
		return engineDefault
	}
	return &wireEngine{Engine: p.Engine.String(),
		VLLMVersion: p.VLLMVersion.String(), VLLMVersionFrom: p.VLLMVersionFrom,
		StepBudget: p.EffectiveMaxBatchedTokens, StepBudgetSource: source(p.MaxNumBatchedTokens > 0),
		GPUMemoryUtilization: p.GpuMemoryUtilization, GPUMemoryUtilizationSource: source(p.GpuMemoryUtilizationSet),
		BlockSize: p.BlockSize, BlockSizeSource: source(p.BlockSizeSet)}
}

// variantParams is the engine parameters the state holds for the named
// variant; nil with none.
func variantParams(st *learnreplay.State, variant string) *capacity.EngineParams {
	for i := range st.Variants {
		if st.Variants[i].Variant == variant {
			return st.Variants[i].Params
		}
	}
	return nil
}

// variantEngine is the wire's account of the named variant's engine; nil
// where the state holds no parameters for it.
func variantEngine(st *learnreplay.State, variant string) *wireEngine {
	return engineOf(variantParams(st, variant))
}

// engineInfo is the API's account of the engine p describes, the CLI's
// figures with engineText as its summary; nil with none.
func engineInfo(p *capacity.EngineParams) *whatifapi.EngineInfo {
	w := engineOf(p)
	if w == nil {
		return nil
	}
	return &whatifapi.EngineInfo{Engine: w.Engine, Summary: engineText(p),
		VLLMVersion: w.VLLMVersion, VLLMVersionFrom: w.VLLMVersionFrom,
		StepBudget: w.StepBudget, StepBudgetSource: w.StepBudgetSource,
		GPUMemoryUtilization: w.GPUMemoryUtilization, GPUMemoryUtilizationSource: w.GPUMemoryUtilizationSource,
		BlockSize: w.BlockSize, BlockSizeSource: w.BlockSizeSource}
}

// engineText names the engine p describes and, for vLLM, the release and
// where it was read from: "vLLM 0.34 (from image tag)". An unknown release
// says so: the defaults are then an unknown release's.
func engineText(p *capacity.EngineParams) string {
	switch {
	case p == nil:
		return "engine not recorded"
	case p.Engine == inferenceengine.EngineSGLang:
		return "SGLang"
	case !p.VLLMVersion.Known():
		return "vLLM release unknown (an unknown release's defaults)"
	case p.VLLMVersionFrom == "":
		return "vLLM " + p.VLLMVersion.String()
	}
	return fmt.Sprintf("vLLM %s (from %s)", p.VLLMVersion, p.VLLMVersionFrom)
}

// engineRowText is engineText with the figures the release decides where
// the flags do not: the step budget, the memory share and the block size.
func engineRowText(p *capacity.EngineParams) string {
	w := engineOf(p)
	if w == nil {
		return engineText(p)
	}
	return fmt.Sprintf("%s: step budget %d (%s), GPU memory %s (%s), block size %d (%s)", engineText(p),
		w.StepBudget, w.StepBudgetSource, num(w.GPUMemoryUtilization), w.GPUMemoryUtilizationSource,
		w.BlockSize, w.BlockSizeSource)
}
