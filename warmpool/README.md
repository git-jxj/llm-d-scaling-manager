# Warm pool

A small population of GPU-**holding** Pods that keep several models resident but
asleep, so capacity for any of them arrives in seconds instead of ~41 s.

Design: [`docs/proposals/fast-model-loading.md`](../docs/proposals/fast-model-loading.md)
and [the implementation design](../docs/proposals/fast-model-loading-implementation.md).

**Status: built, and running on a test cluster.** A pool Pod holding two models
has served real EPP traffic on an H100 OpenShift test cluster, switching between them in ~437 ms. No
pool exists until you create one: see the
[warm pool guide](../docs/guides/warm-pool/) (`deploy/warmpool.sh`). The proxy
image in `config/warmpool` is pinned by digest under the project's registry
namespace; read the note beside it before replacing it.

## Why this exists rather than using Fast Model Actuation

FMA solves a neighbouring problem and most of it is genuinely reusable — the
launcher here is FMA's, copied rather than rewritten. One thing is not reusable,
and it is the thing that decides whether a warm pool works:

**In FMA the launcher requests no GPU; the *requester* holds it.** When the
requester goes away the device allocation is released while the sleeping engine's
`cumem` handles still refer to memory on that device, which another tenant then
takes. Measured on an H100 OpenShift test cluster: two of three unbound sleepers **could not be woken at
all** —

```
pfw8b: HTTP 500  CUDA error: invalid argument   cumem_allocator.cpp:145
v6fdd: HTTP 500  CUDA error: out of memory      cumem_allocator.cpp:139
```

A warm copy that does not own its device is not slower, it is unreliable. So here
**the pool Pod holds the GPUs for as long as it holds models**, which is the one
structural change from FMA and the reason this is a separate component rather
than a fork.

The second reason is scope: FMA's Kubernetes half (dual-pods controller,
launcher-populator, `LauncherPopulationPolicy`, the requester/SPI/proxy path)
exists to pair transient requester Pods with launchers. A pool needs none of it:
the scaling manager already runs a control loop, knows the InferencePools, and decides what
should be warm.

## Which launcher image to use

A pool's engines run the launcher image's vLLM, not the workload's. Pick the
image whose vLLM matches the workload:

| workload's vLLM | pool | launcher image |
| --- | --- | --- |
| v0.26 | pool of Pods | FMA's `launcher`, pinned in `config/warmpool/warmpool-deployment.yaml` (the default; nothing to pass) |
| v0.26 | pool of groups | `ghcr.io/ev-shindin/fma-launcher:v0.6.4-headless`, with `--launcher-image` ([groups](../docs/guides/warm-pool/groups.md)) |
| v0.34.0 | either | `ghcr.io/llm-d-incubation/llm-d-scaling-manager/warmpool-launcher`, from the release that carries it, with `--launcher-image`. Not yet run on a GPU |
| any other | either | none published; build one from FMA's launcher on that vLLM |

## What is copied, and from where

Upstream base: Fast Model Actuation **v0.8.0**, commit
**`3f67d2c4c5c51baa6016fc24630222decd94971f`**, directory
`inference_server/launcher/`. Three changes on top (plus one comment whose
issue number became a full link):

1. **The follower-rank fix**: a rank > 0 of a multi-node engine runs vLLM's
   headless executor instead of an API server, as vLLM's own `run_headless`
   does. First carried in fork commit `65ba31b` of
   `ev-shindin/llm-d-fast-model-actuation`; not upstream.
2. **The vLLM v0.34 import paths**: v0.34 moved `make_arg_parser`,
   `validate_parsed_serve_args`, `cli_env_setup` and `run_server`.
3. **`_wait_procs`**: psutil 7.2, which the vLLM v0.34 image ships, re-raises
   `EINVAL` from its pidfd wait for a process being reaped
   ([giampaolo/psutil#2715](https://github.com/giampaolo/psutil/issues/2715)),
   so a DELETE that had to SIGKILL failed with HTTP 500. The wrapper treats
   `EINVAL` as gone, re-checks the rest, and keeps waiting. Upstream FMA v0.8.0
   has the same bug.

| file | origin | why it earns its place |
| --- | --- | --- |
| `supervisor/launcher.py` | `inference_server/launcher/launcher.py`, with the three changes | spawns and supervises vLLM engines inside one container. Proven: every 2-3 s wake measured on the test cluster came from an instance it created |
| `supervisor/gputranslator.py` | `inference_server/launcher/gputranslator.py`, unchanged | GPU UUID ↔ index mapping via pynvml, with a mock mode for tests |
| `supervisor/tests/` | `inference_server/launcher/tests/`, with the vLLM stubs moved to the v0.34 paths; `test_follower_rank.py` is ours | the launcher's unit tests |

v0.8.0 over the old v0.6.4 base brings two fixes the pool wants: deleting an
instance stops its whole process group, so lingering engine children no longer
keep GPU memory; and a delete no longer blocks the launcher's API while it
waits.

To move to a newer upstream: merge the new upstream `launcher.py` with this
one (`git merge-file`, the old upstream as base), keep the three changes, and
update the base commit here and in the header of `launcher.py`.

### The image built from it

`Dockerfile.warmpool-launcher` builds
`ghcr.io/llm-d-incubation/llm-d-scaling-manager/warmpool-launcher` from this
copy:

- **Base:** vLLM **v0.34.0**, the vLLM project's own build, pinned by digest. It
  is pulled from the registry vLLM's release pipeline pushes to
  (`public.ecr.aws/q9t5s3a7/vllm-release-repo`), not Docker Hub; the digest is
  the same as `vllm/vllm-openai:v0.34.0`.
- **On top:** `nvidia-ml-py` and `kubernetes` at pinned versions, and the two
  files above.
- **Checked at build time:** the build imports the launcher against the real
  vLLM, and checks the vLLM version. A vLLM module the launcher imports that
  has moved fails the build, not a pool Pod. The unit tests cannot catch that:
  they stub vLLM.
- **Released** with every scaling-manager release, under the same tag, for
  `linux/amd64` and `linux/arm64`, signed with cosign.
- **One vLLM only.** A pool's engines run the launcher image's vLLM, not the
  workload's, so this image is for workloads on vLLM v0.34.0. This copy also
  imports the v0.34 module layout only; it does not run on v0.26 to v0.33. For
  workloads on older vLLM, use FMA's images.

Build it locally with `make docker-build-warmpool-launcher` (it pulls about
10 GB). Run the unit tests with `make test-warmpool-launcher`; CI runs both on
any PR that touches this directory.

> **Not the default yet, and not yet run on a GPU.** Pools still run FMA's
> images: a SINGLE-POD pool runs upstream `launcher:v0.8.0` (vLLM v0.26.0),
> pinned by digest in `config/warmpool/warmpool-deployment.yaml`, and a GROUP
> pool runs `ghcr.io/ev-shindin/fma-launcher:v0.6.4-headless` given with
> `--launcher-image`. That image's `launcher.py` is the old fork copy (v0.6.4
> plus the follower fix, without v0.8.0's eviction fixes), not this one. To try this image, pass it with `--launcher-image`. Moving the default
> waits for GPU checks on vLLM v0.34: switch time, output across sleep and
> wake, eviction, sleeper host memory, and `--gpu-memory-utilization`.

## The split of responsibilities

The launcher has **no sleep or wake endpoint**, and that is correct rather than
missing: in FMA the *controller* calls vLLM's own HTTP API directly
(`inference-server.go` calls `/wake_up`, `/sleep` and `/is_sleeping` on the
instance). The scaling manager takes that role.

| responsibility | where |
| --- | --- |
| spawn / list / delete engines in a Pod | supervisor (this directory) |
| `/sleep`, `/wake_up`, `/is_sleeping` | **the scaling manager**, over HTTP to the engine's port |
| which models are warm, which wakes, which is evicted | **the scaling manager** — it is an allocation problem, which is what the scaling manager is for |
| joining a woken model to its InferencePool | **the scaling manager** — label membership; readiness comes from an ordinary probe against the proxy's `/readyz`, not a Pod readiness gate |

No policy lives in the Pod. That is deliberate: the cache policy is the part most
likely to change, and it must be changeable without touching the data plane.

## What had to change, and where it changed

All four are done. Note where: three of them turned out to be decisions the scaling manager
makes when CALLING the launcher, not modifications to the launcher itself, which
is why the copy here carries only the three changes listed above.

1. **The Pod holds the GPUs.** Done in the manifest. The pool Deployment requests `nvidia.com/gpu`, and
   the supervisor allocates the container's own devices among instances rather
   than being handed a device by a requester.
2. **Instance identity keyed by model**, not by a hash over GPU UUIDs. Done in
   the scaling manager (`pool.InstanceID`): the launcher takes the ID from its caller. The hash is
   correct *for FMA* — `CUDA_VISIBLE_DEVICES` is fixed at process start, so a
   sleeper is not portable between GPUs — but our reuse question is "is this model
   resident in this Pod", which the model name answers.
3. **Ports assigned from a local range**, not derived from an
   InferenceServerConfig. Done in the scaling manager (`pool.freePort`): the launcher takes the
   port from its caller too.
   FMA's ISC-derived port is what made two instances of one model collide, and the
   port-conflict fix in `aa072ef` is a workaround for it.
4. **Drop the launcher-notifier sidecar** — never copied, so nothing to do:
   it maintains the `dual-pods.llm-d.ai/sleeping` label for the dual-pods
   controller. The scaling manager reads instance state from the supervisor instead, which the
   measurements showed is the reliable source — the Pod label is per-Pod and flips
   for reasons unrelated to whether a given model is asleep.
