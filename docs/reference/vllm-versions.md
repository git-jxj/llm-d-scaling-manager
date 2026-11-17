# vLLM versions

The scaling manager supports **vLLM v0.26 through v0.34 and recent main
builds**. Variants on different versions can run side by side in one cluster.
Each variant is read on its own.

## What to do

1. **Pass the flags that size the engine.** When the engine is given
   `--max-num-batched-tokens`, `--gpu-memory-utilization` and `--block-size`,
   the scaling manager uses them, and the version no longer matters.
2. **Otherwise, let the scaling manager know the version.** A stock image
   (`vllm/vllm-openai:v0.34.0`, or a mirror that keeps the name `vllm-openai`)
   is read from its tag. For any other image, such as a private build, a digest
   or an image built on top of vLLM, annotate the Deployment or
   LeaderWorkerSet itself, not its pod template, so nothing restarts:

   ```bash
   kubectl annotate deployment <name> -n <namespace> llm-d.ai/vllm-version=0.34
   ```

   The annotation wins over the tag. A value that is not a version is ignored.
3. **Do nothing, and the version is unknown.** The scaling manager then uses
   the defaults below marked *unknown*. It never assumes the newest release.

## Defaults that change between versions

These apply only when the engine is **not** given the flag. From vLLM's
source at each release tag:

| Engine default | v0.26 to v0.27 | v0.28 to v0.33 | v0.34 | Unknown version |
|---|---|---|---|---|
| `--gpu-memory-utilization` | 0.92 | 0.92 | 1.0, or 0.92 with a KV connector or a fixed KV size | 0.9 |
| `--block-size` | 16 | 16 | 64 | 16 |
| `--max-num-batched-tokens`, 160 GiB or more (B200, GB200, B300, MI300X) | 8192 | 16384 | 16384 | 8192 |
| `--max-num-batched-tokens`, 70 GiB or more (H100, H200), not an A100 | 8192 | 8192 | 8192 | 8192 |
| `--max-num-batched-tokens`, below 70 GiB, or an A100 | 2048 | 2048 | 2048 | 2048 |

- **The accelerator is read from the variant's accelerator name.** A name the
  scaling manager does not recognise keeps 8192.
- **The unknown column is the cautious one.** On a recognised accelerator,
  each value is at or below what any release would use. An unknown engine is
  priced with less KV cache and a smaller step budget, which orders more
  replicas, not fewer.
- **v0.34 with a KV connector.** v0.34 keeps 0.92 when a connector such as
  NIXL runs without an IMEX channel. A pod spec does not show the IMEX
  channel, so any connector counts as 0.92.
- **Builds from vLLM main** take whichever defaults they were cut with. A main
  build from early November 2026 has the v0.34 memory default but still uses
  block size 16. Annotate such a build with the release it is closest to, or
  better, pass the flags.

These defaults matter before a replica reports. That covers a variant at
zero replicas, and the first estimate after a rollout. Once a replica runs,
the KV cache size comes from the engine itself (below).

## What a running engine reports

| Signal | v0.26 | v0.27 | v0.34 | Used for |
|---|---|---|---|---|
| `vllm:cache_config_info` labels `block_size`, `num_gpu_blocks` | yes | yes | yes | KV cache size, read live |
| `vllm:cache_config_info` label `enable_extensible_kv_cache` | no | no | yes | nothing: a main build has it before the v0.34 block size |
| A version or build-info metric | no | no | no | not available |
| The step budget (`max_num_batched_tokens`) as a metric | no | no | no | not available; flag or default only |

So the KV cache size of a running replica is always right, whatever the
version. The step budget is not reported by any version, so pass
`--max-num-batched-tokens` when the default is not what you want.

## Other version-dependent behaviour

- **Metrics.** Every `vllm:*` series the scaling manager reads exists in
  v0.26 through v0.34 with the same name and labels.
  `vllm:num_requests_kv_fetch_by_stage` exists from v0.32. Without it, the P/D
  decode queue falls back to `num_requests_waiting`.
- **Warm pool wake.** From v0.32, `/wake_up` answers with a status. An answer
  of `sleeping` (part of the engine stayed asleep) is treated as a failed wake.
  Before v0.32 the answer is empty, and a 200 is a wake.
- **Python bytecode.** Images from v0.32 ship their bytecode. The engine-cache
  guard sees that at container start and unsets `PYTHONPYCACHEPREFIX`. See
  [Python bytecode](workload-preparation.md#python-bytecode).
- **Compile caches** are keyed by the vLLM build. Each prepared cache misses
  once after a version change.

## Changing a variant's version

A new image changes the defaults of every flag the engine is not given. If a
default changes, the scaling manager treats it as new engine arguments: it
learns the variant's capacity again instead of carrying the old figure. With
the flags passed explicitly, an image change keeps what was learned.

See also [preparing a workload](workload-preparation.md#depends-on-the-vllm-version)
for the start-time options that depend on the version.
