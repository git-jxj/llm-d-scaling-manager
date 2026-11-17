package capacity

import (
	"strconv"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/annotations"
)

// vLLM changes its own defaults between releases, and a fleet can run several
// releases side by side. When a flag is on the command line, the parser reads
// it and nothing here applies. When it is absent, the engine runs its
// release's default, and this file says which one that is.
//
// The defaults, from vLLM's own source at each release tag:
//
//	--gpu-memory-utilization  0.9 before v0.20; 0.92 from v0.20; from v0.34,
//	                          1.0 with the extensible KV cache, else 0.92
//	--block-size              16 before v0.34; 64 from v0.34
//	--max-num-batched-tokens  2048 below 70 GiB or on an A100; 8192 at 70 GiB
//	                          or more; from v0.28, 16384 at 160 GiB or more
//
// The version comes from the scaling-manager.llmd.ai/vllm-version annotation, else from the
// image tag. vLLM exports no version metric, so a running engine cannot be
// asked. An unknown version never gets the newest defaults: it keeps the
// values used before releases were read. The memory share (0.9) and block size
// (16) are at or below every release's, and on a large accelerator the budget
// is the smaller of the two it could be, so an unknown engine is priced with
// less, never more.

// VLLMVersion is a vLLM release as major and minor. The zero value is unknown.
type VLLMVersion struct {
	Major int
	Minor int
}

// Known reports whether v names a release.
func (v VLLMVersion) Known() bool {
	return v.Major > 0 || v.Minor > 0
}

// AtLeast reports whether v is known and is major.minor or later.
func (v VLLMVersion) AtLeast(major, minor int) bool {
	if !v.Known() {
		return false
	}
	return v.Major > major || (v.Major == major && v.Minor >= minor)
}

// String renders v as "0.27", or "" when unknown.
func (v VLLMVersion) String() string {
	if !v.Known() {
		return ""
	}
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor)
}

// ParseVLLMVersion reads a release from text such as "0.27", "v0.27.1",
// "0.34.0rc1", "0.33.0+g87ecf7d" or "v0.34.0-cu130". Anything else, including
// "latest", "nightly" and a bare digest, is unknown.
func ParseVLLMVersion(s string) VLLMVersion {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	major, rest, ok := leadingInt(s)
	if !ok || !strings.HasPrefix(rest, ".") {
		return VLLMVersion{}
	}
	minor, _, ok := leadingInt(rest[1:])
	if !ok {
		return VLLMVersion{}
	}
	v := VLLMVersion{Major: major, Minor: minor}
	if !v.Known() {
		return VLLMVersion{}
	}
	return v
}

// leadingInt splits s into the decimal number it starts with and the rest.
func leadingInt(s string) (int, string, bool) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 || end > 4 {
		return 0, s, false
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0, s, false
	}
	return n, s[end:], true
}

// VLLMVersionFromImage reads the release from an image reference whose
// repository is vLLM's own (vllm/vllm-openai, vllm-openai-rocm, and mirrors
// that keep the name). Other repositories are unknown: an llm-d or private
// image's tag is that project's version, not vLLM's. A digest without a tag
// is unknown. A comma-joined list (engineImage, when several containers
// qualify) is known only when every entry names the same release.
func VLLMVersionFromImage(image string) VLLMVersion {
	var found VLLMVersion
	for _, ref := range strings.Split(image, ",") {
		v := vllmVersionOfRef(strings.TrimSpace(ref))
		if !v.Known() || (found.Known() && v != found) {
			return VLLMVersion{}
		}
		found = v
	}
	return found
}

// vllmVersionOfRef reads one image reference.
func vllmVersionOfRef(ref string) VLLMVersion {
	ref, _, _ = strings.Cut(ref, "@")
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash {
		return VLLMVersion{}
	}
	repo := ref[slash+1 : colon]
	if repo != "vllm" && !strings.HasPrefix(repo, "vllm-openai") {
		return VLLMVersion{}
	}
	return ParseVLLMVersion(ref[colon+1:])
}

// vllmVersionOf is the release a scale target states with the
// scaling-manager.llmd.ai/vllm-version annotation, else the one its image tag names.
func vllmVersionOf(annots map[string]string, image string) VLLMVersion {
	if v := ParseVLLMVersion(annots[annotations.VLLMVersion]); v.Known() {
		return v
	}
	return VLLMVersionFromImage(image)
}

// The defaults this package used before it read versions. They stay the
// values for an unknown release.
const (
	legacyGPUMemoryUtilization = 0.9
	legacyBlockSize            = 16
	legacyMaxNumBatchedTokens  = 8192
)

// vllmDefaultGPUMemoryUtilization is the share of GPU memory a vLLM engine
// takes when --gpu-memory-utilization is absent.
//
// 0.9 before v0.20, and for an unknown release; 0.92 from v0.20. v0.34 turned
// the extensible KV cache on by default and, with it, 1.0. It stays off --
// and the default 0.92 -- when the cache's size is fixed by a flag, and when
// a KV connector exports memory without an IMEX channel. A pod's IMEX channel
// is invisible from its spec, so any KV connector reads as off: the lower
// value.
func vllmDefaultGPUMemoryUtilization(v VLLMVersion, p *EngineParams) float64 {
	switch {
	case !v.AtLeast(0, 20):
		return legacyGPUMemoryUtilization
	case !v.AtLeast(0, 34):
		return 0.92
	case p.ExtensibleKVCache > 0:
		return 1.0
	case p.ExtensibleKVCache < 0, p.NumGpuBlocksOverride > 0, p.KVConnector != "",
		strings.Contains(p.MemoryLayout, "kv_cache_memory_bytes="):
		return 0.92
	default:
		return 1.0
	}
}

// vllmDefaultBlockSize is the KV block size when --block-size is absent:
// 64 from v0.34, 16 before. A model can still pick its own (attention
// backends with fixed page sizes do); the running engine reports the real
// value in vllm:cache_config_info, which the live path reads.
func vllmDefaultBlockSize(v VLLMVersion) int64 {
	if v.AtLeast(0, 34) {
		return 64
	}
	return legacyBlockSize
}

// vllmDefaultMaxNumBatchedTokens is the OpenAI server's per-step token budget
// when --max-num-batched-tokens is absent, for one accelerator.
//
// From v0.28, accelerators with 160 GiB or more (B200, B300, MI300X) get
// 16384. Below that the rule is the same in every supported release: 8192 at
// 70 GiB or more unless the device is an A100, 2048 otherwise. An accelerator
// this package cannot size keeps 8192, as before; an unknown release on a
// large accelerator gets 8192 too, the smaller of its two possible budgets.
func vllmDefaultMaxNumBatchedTokens(v VLLMVersion, accelerator string) int64 {
	gib, a100, ok := acceleratorMemoryGiB(accelerator)
	switch {
	case !ok:
		return legacyMaxNumBatchedTokens
	case gib >= 160:
		if v.AtLeast(0, 28) {
			return 16384
		}
		return 8192
	case gib >= 70 && !a100:
		return 8192
	default:
		return 2048
	}
}

// acceleratorMemory maps a fragment of an accelerator name, as nodes label it
// (NVIDIA-H100-80GB-HBM3, nvidia-h100-80gb, H200, A100_NVLINK_80GB), to its
// memory in GiB. Order matters: a longer name is tried before one it
// contains (h200 before h20, a100 before a10, l40 before l4).
var acceleratorMemory = []struct {
	fragment string
	gib      float64
}{
	{"gb300", 279}, {"b300", 268},
	{"gb200", 185}, {"b200", 179},
	{"mi355", 288}, {"mi325", 256}, {"mi300x", 192},
	{"gh200", 95}, {"h200", 140}, {"h100", 79}, {"h800", 79}, {"h20", 95},
	{"a100", 80}, {"a800", 80},
	{"l40", 45}, {"l4", 22}, {"a10", 22}, {"a40", 45}, {"t4", 15}, {"v100", 32},
}

// acceleratorMemoryGiB is an accelerator's memory as vLLM sees it, whether
// vLLM treats it as an A100 (its name contains "a100"), and whether the name
// was recognised at all.
func acceleratorMemoryGiB(accelerator string) (gib float64, a100 bool, ok bool) {
	name := strings.ToLower(accelerator)
	if name == "" {
		return 0, false, false
	}
	for _, m := range acceleratorMemory {
		if strings.Contains(name, m.fragment) {
			return m.gib, strings.Contains(name, "a100"), true
		}
	}
	return 0, false, false
}

// applyVLLMDefaults replaces the defaults the vLLM parser left in place with
// the ones the engine's release runs on accelerator. A flag the parser read is
// never replaced.
func applyVLLMDefaults(p *EngineParams, accelerator string) {
	v := p.VLLMVersion
	if !p.GpuMemoryUtilizationSet {
		p.GpuMemoryUtilization = vllmDefaultGPUMemoryUtilization(v, p)
	}
	if !p.BlockSizeSet {
		p.BlockSize = vllmDefaultBlockSize(v)
	}
	if p.MaxNumBatchedTokens <= 0 && p.IsV1Engine && p.ChunkedPrefillEnabled {
		p.EffectiveMaxBatchedTokens = vllmDefaultMaxNumBatchedTokens(v, accelerator)
	}
}
