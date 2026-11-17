# Workloads that bring their own JIT cache

**Status:** proposal, 2026-11-16; nothing built. Depends on: PR #146 (FlashInfer
cache fix, not merged).

**The task.** Some clusters run an external cache manager, usually a sidecar,
that keeps the engine's compiled caches in a shared store. On those workloads
`make workload-patch` should prepare the image and the weights, leave the
compile caches to the manager, and say so. Today it reports the workload as
compiling from nothing, and writes a patch that, if applied, takes the caches
away from the manager.

## What we rely on

Only this behaviour, from no particular implementation:

- **Restore before start.** Before the engine starts, the manager restores the
  caches for the pod's key from the store into a pod-local directory (an
  `emptyDir` it shares with the engine).
- **Publish after serving.** Once the engine has served for a while, the
  manager publishes that directory back to the store.
- **First wins per key.** The first copy published for a key is the one every
  later pod restores.

The key comes from the engine build, the GPU type and the model's parallel
layout. The manager does not hold the image on the node or put the weights on
the node's disk; those stay ours.

## The engine-cache variables

"The set" below means the engine-cache variables of
`deploy/lib/scaledobject.sh`, listed once in `so_workload_patch_trim` and the
LeaderWorkerSet `half()`:

- today: `VLLM_CACHE_ROOT`, `TRITON_CACHE_DIR`, `PYTHONPYCACHEPREFIX`;
- after PR #146: `FLASHINFER_WORKSPACE_BASE` (keyed by image), with its own
  FlashInfer half;
- added by the cache PR (see [Order](#order)): `DG_JIT_CACHE_DIR`,
  `TRTLLM_DG_CACHE_DIR`, `TORCHINDUCTOR_CACHE_DIR`, `CUDA_CACHE_PATH`,
  `XDG_CACHE_HOME`.

Every rule on this page applies to the whole set, so a variable added later
needs no edit here.

## Where the two collide today

| | ours today (after PR #146) | with a cache manager | effect |
| --- | --- | --- | --- |
| Patch report | an engine whose `VLLM_CACHE_ROOT` sits on an `emptyDir` is "COMPILES FROM NOTHING" | that `emptyDir` is the manager's restore target | a false alarm on every such workload |
| Emitted patch | sets the set under `/engine-cache`, and `FLASHINFER_WORKSPACE_BASE=/engine-cache/flashinfer/<image key>` | the same names already point at the manager's directory | applied, it moves the caches off that directory: the manager publishes copies without them, and later pods restore "warm" and compile anyway |
| FlashInfer warning | "BUILDS ITS FLASHINFER KERNELS AGAIN" for a workload that sets `FLASHINFER_WORKSPACE_DIR` | the manager may set either name | a warning about a cache that is not ours to fix |
| LeaderWorkerSet apply | replaces an env entry of the same name in place | the manager's entries are those entries | the same, on every group |
| `engine-cache keys` / `verify` | look for the key on the node directory | the key is in the `emptyDir`, never on the node | every node reported missing the key |
| Start-time estimate | one estimate per variant | starts are warm (copy found) or cold (first pod per key, or after an image change) | see [Start time](#7-start-time) |

## Shapes

- **`emptyDir` plus a manager.** This page.
- **`VLLM_CACHE_ROOT` on the workload's own claim.** Classified `volume:` and
  left alone today. No change.
- **Cache baked into the image.** Reported as "COMPILES FROM NOTHING" today.
  The same annotation covers it.

## What changes

### 1. An annotation decides

On the pod template:

```yaml
metadata:
  annotations:
    scaling-manager.llmd.ai/engine-cache-owner: external   # or: ours
```

- `external`: the rules in 2-4 apply.
- `ours`: today's behaviour, and the suggestion below is not printed. This is
  the opt-out when the suggestion is wrong.
- absent: today's behaviour.

It is a new key. The existing `scaling-manager.llmd.ai/engine-cache` label
holds a claim name and is not reused.

A LeaderWorkerSet is decided per template. If only one template is annotated,
that template gets external handling, the other gets ours, and the report says
so.

**Suggestion only.** When there is no annotation and an `emptyDir` that holds a
variable of the set is also mounted by another container that mounts durable
storage (a claim, hostPath, NFS, a CSI inline volume or a generic ephemeral
volume), the report prints one line suggesting the annotation. It never changes
the patch. The same shape matches a weights downloader that mounts a scratch
`emptyDir`, and a guess must not silence a real warning. It also misses
managers that mount the store inside the container (FUSE) and cache variables
set from a ConfigMap.

### 2. External: per variable

- **A variable of the set that the workload sets** is left alone: no variable,
  mount or `/engine-cache` volume of ours for it.
- **A variable the workload does not set** gets ours, as today, and the report
  names it, with the manager's directory as the alternative that would put it
  in the shared copy.
- PR #146's FlashInfer half and its warning follow the same rule: suppressed
  when the workload sets `FLASHINFER_WORKSPACE_BASE`, ours when it does not.
- The drain half, the weights half and the `prepull` check do not change.
- `--apply-engine-cache` applies only the uncovered variables. If every
  variable is covered, it is refused with a non-zero exit and the reason.
- `engine-cache keys` and `verify` say the caches are managed elsewhere.

What the operator sees:

```
deployment/qwen-decode: engine caches owned externally (scaling-manager.llmd.ai/engine-cache-owner=external)
  store: claim jit-store, mounted read-write by cache-sync: anyone who can mount it read-write can change the code this engine runs
  TORCHINDUCTOR_CACHE_DIR: not set by the owner; emitted under /engine-cache
```

and in the patch file:

```yaml
# engine caches: owned externally; only TORCHINDUCTOR_CACHE_DIR is ours (see the report)
```

### 3. Already patched workloads

A workload that was patched before it was annotated has our `engine-cache`
volume next to the manager, and its `VLLM_CACHE_ROOT` now classifies as
`volume:`, so today it is silent. With the annotation (or the suggestion's
shape), our volume next to a manager is reported as a conflict:

- the revert: remove our volume, its mounts, and the variables of the set that
  point under `/engine-cache`;
- delete the store's copies written while the patch was applied. They lack the
  moved caches and, first wins, are restored forever;
- if our half points at a shared ReadWriteMany claim, say so: several pods
  autotuning into one claim lost files, and a pod stayed Running and never
  Ready.

### 4. Guard and trust

**Guard.** Still offered in external mode, as text the owner pastes, including
PR #146's whole-tree check for FlashInfer. A manager that restores as root and
keeps file owners, or restores a copy made by another UID, leaves files a
non-root engine cannot write. FlashInfer then fails instead of compiling.

**Trust.** Triton and FlashInfer libraries and cubins run as native code, and
torch.compile artifacts include Python the engine executes. Whoever can write
the store, or publish first for a key, runs code in every later pod of that
key. Deferring does not create this risk; it is the same as for our cache
("trust it as you would the engine image"). The report:

- names the store and how it is mounted (example above);
- recommends a read-only mount for pods that only restore, where the manager
  allows it;
- warns when the image is not pinned by digest: a key built on a moved tag
  restores old kernels into a new engine.

The 1777 / 1770 question from PR #146 does not apply here: external mode
creates no node directory of ours. The `emptyDir` is pod-local; the store's
access control is what matters.

### 5. Python bytecode: images before vLLM v0.32 only

From v0.32 on, vLLM images ship their bytecode and the existing guard unsets
`PYTHONPYCACHEPREFIX`, so there is nothing to offer. For older images the report
offers, without applying, `PYTHONPYCACHEPREFIX=<manager dir>/pycache/<image key>`,
so the bytecode rides in the shared copy (about 11 s per start, measured on
vLLM 0.26). It widens the trust surface: a `.pyc` is checked only against its
source's mtime and size.

### 6. vLLM v0.34

- `vllm download-kernels` (v0.32 on) fetches FlashInfer's precompiled cubins.
  In external mode they land under
  `FLASHINFER_WORKSPACE_BASE/.cache/flashinfer/cubins` and ride in the shared
  copy. The preparer still mentions it, report only.
- The shared Triton compile pool (v0.33) must inherit `TRITON_CACHE_DIR`, or its
  workers compile outside the manager's directory. Confirm before building.
- Every cache key changes once at the v0.34 bump (vLLM #65652). Expect one cold
  start per key after the upgrade.

### 7. Start time

- `replicaStartSeconds` is only a prior: once three Pods are measured, the
  measured estimate rules, and sizing never reads a start shorter than one
  already measured.
- The first start of every key is cold by construction.
- **Decision:** recommend seeding the cold start for the first start of each
  key, or no seed. Seeding the warm start protects nothing.
- **Decision:** the 3600 s cap on a credible start does not change here. A
  group worker can wait up to 30 min for its leader and then compile cold; that
  passes 3600 s only if the cold compile takes over 30 min, and raising the cap
  weakens the readiness-blip guard. The better fix, later, is to measure a
  worker from its leader's decision.

Warm and cold figures such as "3 min warm, 20 min cold" are illustrative, not
measured on a given version.

Later, if the manager publishes its warm/cold decision as a per-pod metric, the
learner keeps two estimates per variant: the next replica is warm if the last
Pod of this variant and image was warm. That needs no read of the store. The
metric is scraped like any pod metric, one series per pod.

### 8. Our engine-cache backend stays

We do not ship the manager as a backend of `make engine-cache`. Ours needs no
change to the engine container and no shared store at run time; a manager needs
a store and a key someone maintains, and fits where nodes are short-lived or a
group must start all warm or all cold. The guide names both.

## Upgrade note

For v0.12.0 in `docs/reference/upgrade-and-uninstall.md`: a workload annotated
`scaling-manager.llmd.ai/engine-cache-owner: external` stops reporting
"COMPILES FROM NOTHING" and gets no engine-cache patch for the variables it
sets; annotate `ours` to keep today's report. Expect one cold start per cache
key after the vLLM v0.34 bump.

## Order

After PR #146 merges, one cache PR builds the shared variable set with the five
new variables, the annotation, and the report changes in 1-6. Start time (7) is
separate.

## Not doing

- Reading the manager's logs, files or store from the controller.
- Changing the manager's pod spec, or emitting its manifests.
- Letting the suggestion change the patch.
- Any change to `make prepull` or `make weights`, or to the 3600 s cap.

## Tests

`hack/check-workload-gaps.sh` fixtures, each run against the parent commit
first, where it must fail:

- Deployment, `external`, every variable covered: no engine-cache half, no
  FlashInfer half or warning, no "COMPILES FROM NOTHING"; the guard text is
  still offered.
- The same with one variable uncovered: ours emitted for it, and reported.
- `ours`: today's output, no suggestion.
- The suggestion's shape, no annotation: today's patch plus the suggestion.
- The downloader shape (scratch `emptyDir` plus a weights claim): today's
  warning still printed, patch unchanged.
- LeaderWorkerSet with the annotation on one template only.
- Already patched: conflict reported, with the revert.
- `--apply-engine-cache` with every variable covered: refused, non-zero exit.

And a start-learner unit test with warm and cold samples from one variant.
