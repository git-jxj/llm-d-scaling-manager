package registration

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
)

// The per-pod series that say what a cached prompt token from outside the
// GPU costs to load: the bytes a pod moved a second, and the seconds it
// spent moving them a second, for each way KV reaches its GPU. Beside the
// cached tokens by tier (cache_tiers.go), they price a host, disk or peer
// hit in the capacity planner (slo.SwitchTTFTTierLoad): the seconds a
// second over the tokens a second is the load time per token. Recorded
// only, issued only while recording, vLLM only, as the cache tiers are; an
// engine that does not publish one returns no series, and the row reads it
// as unknown.
const (
	// QueryKVOffloadLoadByteRate and QueryKVOffloadLoadSecondsRate are the
	// bytes, and the seconds of transfer, the OffloadingConnector loaded
	// from its offload medium into the GPU per pod, over [1m]
	// (vllm:kv_offload_load_bytes and _load_time, v0.24.0 and later).
	QueryKVOffloadLoadByteRate    = "kv_offload_load_byte_rate"
	QueryKVOffloadLoadSecondsRate = "kv_offload_load_seconds_rate"

	// QueryKVTierReadByteRate and QueryKVTierReadSecondsRate are the bytes,
	// and the seconds, a tiering offload spec read from its secondary tiers
	// (a peer, a file system) into its primary CPU tier per pod, over [1m]
	// (vllm:kv_offload_tiering_read_bytes and _read_time, v0.28.0 and
	// later): the step before the load into the GPU.
	QueryKVTierReadByteRate    = "kv_tier_read_byte_rate"
	QueryKVTierReadSecondsRate = "kv_tier_read_seconds_rate"

	// QueryNIXLByteRate and QueryNIXLSecondsRate are the bytes, and the
	// seconds of transfer, a pod's NixlConnector moved per second, over
	// [1m] (vllm:nixl_bytes_transferred and nixl_xfer_time_seconds, the
	// histograms' sums).
	QueryNIXLByteRate    = "nixl_byte_rate"
	QueryNIXLSecondsRate = "nixl_seconds_rate"
)

// registerKVTransferQueries registers the queries above.
func registerKVTransferQueries(registry *source.QueryList) {
	for _, q := range []struct{ name, counter, what string }{
		{QueryKVOffloadLoadByteRate, "vllm:kv_offload_load_bytes_total", "Bytes the KV offloading connector loaded into the GPU per pod (bytes/s, 1m)"},
		{QueryKVOffloadLoadSecondsRate, "vllm:kv_offload_load_time_total", "Seconds of KV offload loads into the GPU per pod (s/s, 1m)"},
		{QueryKVTierReadByteRate, "vllm:kv_offload_tiering_read_bytes_total", "Bytes a tiering offload read from its secondary tiers per pod (bytes/s, 1m)"},
		{QueryKVTierReadSecondsRate, "vllm:kv_offload_tiering_read_time_total", "Seconds of tiering offload reads from secondary tiers per pod (s/s, 1m)"},
		{QueryNIXLByteRate, "vllm:nixl_bytes_transferred_sum", "Bytes NIXL transferred per pod (bytes/s, 1m)"},
		{QueryNIXLSecondsRate, "vllm:nixl_xfer_time_seconds_sum", "Seconds of NIXL transfers per pod (s/s, 1m)"},
	} {
		registry.MustRegister(source.QueryTemplate{
			Name:        q.name,
			Type:        source.QueryTypePromQL,
			Template:    perPodRate(q.counter, TTFTInputWindow),
			Params:      []string{source.ParamNamespace},
			Description: q.what + " (vLLM only)",
		})
	}
}
