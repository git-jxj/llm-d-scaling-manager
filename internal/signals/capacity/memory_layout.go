package capacity

import (
	"encoding/json"
	"strconv"
	"strings"
)

// layoutShape is what the value of one memory-layout flag
// (EngineParams.MemoryLayout) is.
type layoutShape int

const (
	// layoutBytes is a size: a whole number, or a number and one of vLLM's
	// human_readable_int suffixes ("40G", humanIntForm).
	layoutBytes layoutShape = iota + 1
	// layoutCount is a whole number.
	layoutCount
	// layoutSwitch is a flag that takes no value, recorded as "true".
	layoutSwitch
	// layoutVLLMAll2All is a vLLM all-to-all backend (vllmAll2AllBackends).
	layoutVLLMAll2All
	// layoutSGLangA2A is an SGLang MoE all-to-all backend
	// (sglangA2ABackends).
	layoutSGLangA2A
	// layoutEPLBConfig is vLLM's --eplb-config, as canonicalJSON writes it.
	layoutEPLBConfig
)

// memoryLayoutFlags is every key MemoryLayout records, and what its value
// is. A flag added to MemoryLayout (addLayoutPart) is added here, and a test
// holds every addLayoutPart call to this table.
var memoryLayoutFlags = map[string]layoutShape{
	"kv_cache_memory_bytes": layoutBytes,
	"num_redundant_experts": layoutCount,
	"enable_eplb":           layoutSwitch,
	"eplb_config":           layoutEPLBConfig,
	"all2all_backend":       layoutVLLMAll2All,
	envAll2AllKey:           layoutVLLMAll2All,
	"enable_dp_attention":   layoutSwitch,
	"moe_a2a_backend":       layoutSGLangA2A,
}

// vllmAll2AllBackends are the backends vLLM's --all2all-backend and
// VLLM_ALL2ALL_BACKEND take (vllm/config/parallel.py, All2AllBackend).
var vllmAll2AllBackends = map[string]bool{
	"naive": true, "pplx": true, "deepep_high_throughput": true, "deepep_low_latency": true,
	"allgather_reducescatter": true, "flashinfer_all2allv": true, "mori": true, "nixl_ep": true,
}

// sglangA2ABackends are the backends SGLang's --moe-a2a-backend takes
// (sglang/srt/server_args.py, MOE_A2A_BACKEND_CHOICES).
var sglangA2ABackends = map[string]bool{
	"none": true, "deepep": true, "mooncake": true, "nixl": true, "mori": true, "ascend_fuseep": true,
	"flashinfer": true,
}

// eplbConfigKeys are the fields of vLLM's EPLBConfig (vllm/config/parallel.py)
// that take a number or a boolean.
var eplbConfigKeys = map[string]bool{
	"window_size": true, "step_interval": true, "num_redundant_experts": true,
	"log_balancedness": true, "log_balancedness_interval": true, "use_async": true,
}

// KnownMemoryLayout reports whether every part of layout, a MemoryLayout, is
// a flag of memoryLayoutFlags with a value of its own shape: a number, a
// switch's "true", a backend the engine itself defines, or an EPLB
// configuration of known fields holding numbers and booleans. A value that
// could be any other text -- a backend the engines do not define, a string
// in the EPLB configuration -- is not known, so a reader that must not pass
// a name on (the recording export) can refuse it.
func KnownMemoryLayout(layout string) bool {
	if layout == "" {
		return true
	}
	for _, part := range strings.Split(layout, ";") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return false
		}
		kind, ok := memoryLayoutFlags[key]
		if !ok || !kind.holds(value) {
			return false
		}
	}
	return true
}

// holds reports whether v is a value of this kind.
func (k layoutShape) holds(v string) bool {
	switch k {
	case layoutBytes:
		return isWholeNumber(v) || humanIntForm.MatchString(v)
	case layoutCount:
		return isWholeNumber(v)
	case layoutSwitch:
		return v == "true"
	case layoutVLLMAll2All:
		return vllmAll2AllBackends[v]
	case layoutSGLangA2A:
		return sglangA2ABackends[v]
	case layoutEPLBConfig:
		return knownEPLBConfig(v)
	}
	return false
}

// isWholeNumber reports a run of decimal digits.
func isWholeNumber(v string) bool {
	_, err := strconv.ParseUint(v, 10, 64)
	return err == nil
}

// knownEPLBConfig reports a JSON object whose every key is an EPLBConfig
// field of eplbConfigKeys and whose every value is a number or a boolean.
func knownEPLBConfig(v string) bool {
	var cfg map[string]any
	if err := json.Unmarshal([]byte(v), &cfg); err != nil || cfg == nil {
		return false
	}
	for key, value := range cfg {
		if !eplbConfigKeys[key] {
			return false
		}
		switch value.(type) {
		case float64, bool:
		default:
			return false
		}
	}
	return true
}
