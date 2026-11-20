# Scaling a DisaggregatedSet

**Status:** built: discovery, the ScaledObject plan, the workload report, RBAC,
the role writer for Static roles, and the prefill-with-decode wake. The
follow-ups are at the end.

## What a DisaggregatedSet is

`DisaggregatedSet` (`disaggregatedset.x-k8s.io/v1`) is the LeaderWorkerSet
project's object for prefill/decode serving. One object lists the roles
(`prefill`, `decode`, sometimes `encode`). Each role has its own LeaderWorkerSet
template and its own replica count. The controller ships with LWS and creates one
child LeaderWorkerSet per role and per revision, named
`<set>-<slice>-<revision>-<role>`. It rolls a template change across the roles
together, so the router never pairs a new prefill with an old decode.

The child LeaderWorkerSets are an implementation detail. Their names change on
every rollout. Select them, and their pods, by label:

| label | on | value |
| --- | --- | --- |
| `disaggregatedset.x-k8s.io/name` | child LWS, pods | the DisaggregatedSet |
| `disaggregatedset.x-k8s.io/role` | child LWS, pods | the role |
| `disaggregatedset.x-k8s.io/slice` | child LWS, pods | the slice index, `0` when `slices` is 1 |
| `disaggregatedset.x-k8s.io/revision` | child LWS, pods | the template revision hash |

The pods also carry the usual LeaderWorkerSet labels
(`leaderworkerset.sigs.k8s.io/name`, `group-index`, `worker-index`).

Upstream references, all at LWS **v1.0.0** (stable):

- [Roles](https://lws.sigs.k8s.io/docs/concepts/disaggregatedset/roles/) and
  [Slices](https://lws.sigs.k8s.io/docs/concepts/disaggregatedset/slices/), concept pages
- [API reference, disaggregatedset.v1](https://lws.sigs.k8s.io/docs/reference/disaggregatedset.v1/)
- source: [`api/disaggregatedset/v1`](https://github.com/kubernetes-sigs/lws/tree/v1.0.0/api/disaggregatedset/v1),
  [KEP-766 DisaggregatedSet](https://github.com/kubernetes-sigs/lws/tree/v1.0.0/keps/766-DisaggregatedSet),
  [KEP-846 slices](https://github.com/kubernetes-sigs/lws/tree/v1.0.0/keps/846-disaggregatedset-slices)
- llm-d's [DisaggregatedSet page](https://llm-d.ai/docs/architecture/model-server-orchestration/disaggregatedset)

## How DisaggregatedSet scales

Each fact is tagged **[source]** when it was checked against the v1.0.0 source,
or **[docs]** when it comes from the concept pages only. File paths are in the
LWS repository at `v1.0.0`.

- **Roles scale independently. There is no ratio.** Each role has its own
  `spec.replicas`, a count of LWS groups, and each role is its own child
  LeaderWorkerSet. Nothing in steady state pairs or rebalances the roles; 3
  prefill and 7 decode groups is a valid set. **[source]**
  `disaggregatedset_types.go` (`DisaggregatedRoleSpec`, one
  `LeaderWorkerSetTemplateSpec` per role); `pkg/utils/disaggregatedset/utils.go`
  (`GenerateName`, one LWS per role). The docs say the count is per slice. **[docs]**
- **One cross-role rule: all zero or all non-zero.** A CEL rule on the spec
  requires every Static role to have `replicas > 0`, or every Static role to have
  0. Roles in External mode are exempt from it. **[source]**
  `disaggregatedset_types.go` lines 220-224.
- **The scale target is `DisaggregatedSetRoleScaler`.** A role opts in with
  `scaling.mode: External` (the default is `Static`). The controller then creates
  one `DisaggregatedSetRoleScaler` per role, named `<set>-<role>`, and reads the
  role's replica count from it, ignoring the role's `spec.replicas`. It has a
  `/scale` subresource: specpath `.spec.replicas`, statuspath `.status.replicas`,
  selectorpath `.status.selector`. Its name does not change across rollouts. Its
  status counts groups across all revisions, and its selector matches one leader
  pod per group. When an existing role switches to External, the scaler is seeded
  with the role's current group count, so the role does not drain. **[source]**
  `disaggregatedsetrolescaler_types.go` lines 28-90; `disaggregatedset_types.go`
  line 93. The DisaggregatedSet itself has no `/scale` subresource, only
  `/status` (line 402). **[source]**
- **External mode needs `slices: 1`.** The webhook rejects `slices > 1` while any
  role is External. **[source]** `pkg/webhooks/disaggregatedset/disaggregatedset_webhook.go`
  lines 157-160.
- **A slice is a whole copy of the topology.** `spec.slices: N` runs N
  independent copies of every role, each with that role's replica count, each
  rolling out on its own. A slice is **not** one prefill group plus one decode
  group, and not a group of ranks. Changing `slices` does not create a revision.
  **[source]** `disaggregatedset_types.go` line 237; KEP-846. (A group of ranks
  is an LWS group: `leaderWorkerTemplate.size` pods with one `group-index`.)
- **Rollouts move the roles in fractional lockstep.** During a template rollout
  the controller keeps the roles' progress within one replica-fraction of each
  other: `largestReplicaFraction` is 1 / (replicas of the smallest role). For
  8 prefill and 4 decode, at most 1/4 apart. It bounds rollout progress, not the
  replica ratio. **[source]** KEP-766, "fractional lockstep", line 190.
- **A replica change does not create a revision.** The revision hash covers each
  role's name and LWS template, not its replica count. **[source]**
  `pkg/utils/disaggregatedset/utils.go` `ComputeRevision`; KEP-846.
- **Scale-down waits for a rollout to finish by default.**
  `scalingPolicy.scaleDownDuringRollout` defaults to `Deferred`: a lower target
  is applied after the old revision has drained. **[source]**
  `disaggregatedset_types.go` lines 141-147.
- **Per-role status.** `status.roleStatuses[]` has `desiredReplicas`,
  `replicas`, `readyReplicas` and `updatedReplicas` per role, all counting groups
  summed across slices. Sub-roles report in `subRoleStatuses[]`; do not add them
  to the parent. **[source]** `disaggregatedset_types.go` lines 288-343. Newer
  `main` adds `availableReplicas` and `minAvailableReplicas`; v1.0.0 does not have
  them.
- **Version.** DisaggregatedSet first shipped in LWS v0.9.0, without slices and
  without `DisaggregatedSetRoleScaler`. Both arrived in v0.10.0. **[source]**
  (the file `disaggregatedsetrolescaler_types.go` is absent at the `v0.9.0` tag).

## What this means for us

- **The optimizer stays per role.** Prefill and decode are already separate
  variants of one model, sized independently. A DisaggregatedSet adds no ratio to
  honour, so there is no joint P:D decision to build.
- **Scale-to-zero zeroes both roles and wakes them together.** For Static roles
  the API requires it. For External roles it does not, but a prefill with no
  decode serves nothing. Our scale-from-zero wakes a model's roles as one group
  (`internal/engines/scalefromzero/model_group.go`). For a model served by a
  DisaggregatedSet, `requirePrefill` defaults to true, so prefill wakes with
  decode; a policy can still set it to false. Other models keep the
  decode-only default.
- **A scale-down can be held through a rollout.** With `Deferred`, KEDA writes a
  lower count to the role scaler and the groups stay until the rollout ends.
  The actuator must read the scaler's `status.replicas`, not assume its write
  took effect, and the start-time learner must not read the delay as a slow
  start. A rollout of the set reads as "rolling out" for every role.
- **One ScaledObject per role, targeting the role scaler.** Never target a child
  LeaderWorkerSet: the next rollout replaces it, and the DisaggregatedSet
  controller sets its replicas anyway.

  ```yaml
  scaleTargetRef:
    apiVersion: disaggregatedset.x-k8s.io/v1
    kind: DisaggregatedSetRoleScaler
    name: <set>-decode
  ```

- **Switching a role to External is an operator step.** A production P/D
  deployment typically runs both roles in Static mode at equal fixed counts, and
  rolls them out by hand with drain tooling. To let us scale it, the operator
  sets `scaling.mode: External` on each role, removes `spec.replicas` from it,
  and keeps `slices: 1`. This needs LWS v0.10.0 or newer. The scaler is seeded
  with the current count, so nothing drains at the switch. `make scaledobjects-plan`
  says this per role and writes `apply: no` until it is done. A set that cannot
  switch (LWS v0.9.0, or a role that must stay Static) uses the second path
  below.

## How each part of ours maps

| ours | Deployment / LeaderWorkerSet | DisaggregatedSet role |
| --- | --- | --- |
| scale target (`scaleTargetRef`) | the workload | `DisaggregatedSetRoleScaler` `<set>-<role>` |
| variant | one workload | one role |
| role (`llm-d.ai/role`) | pod template label | the same label on the role's template |
| replica | a pod, or an LWS group | an LWS group (`leaderWorkerTemplate.size` pods) |
| pod template | the workload's | `spec.roles[].spec.leaderWorkerTemplate` |
| pods of the variant | `leaderworkerset.sigs.k8s.io/name=<lws>` | `disaggregatedset.x-k8s.io/name=<set>`, `.../role=<role>` |
| pod to variant (collector) | owner walk ends at the LWS | owner walk passes the LWS to the DisaggregatedSet, then the pod's role label names the scaler |
| desired / current replicas | `spec.replicas` / `status.replicas` | the scaler's `spec.replicas` / `status.replicas` |
| rolling out | LWS `UpdateInProgress` | the role's `updatedReplicas < replicas`, or the set's status is behind its generation |
| start time | per LWS group | per LWS group; the group key includes the child LWS, so revisions do not mix |
| scale-in | highest group index first, whole group | the same, per child LWS; held through a rollout by `Deferred` |
| workload preparation | patch the workload | report per role; never patched in place, because any template change rolls every group of every role |

## The design

1. **Discovery.** `make scaledobjects-plan` lists DisaggregatedSets beside
   Deployments and LeaderWorkerSets. Each serving role is one plan entry,
   kind `DisaggregatedSetRoleScaler`, name `<set>-<role>`. An entry is
   `apply: no`, with the reason, when the role is Static, when `slices` is above
   1, or when the role has sub-roles.
2. **Attribution.** The collector's owner walk goes pod, StatefulSet, LWS. When
   that LWS is controlled by a DisaggregatedSet, the scale target becomes the
   role scaler named from the pod's `disaggregatedset.x-k8s.io/role` label.
3. **Scale target reads.** `scaletarget.FetchScaleTarget` accepts
   `DisaggregatedSetRoleScaler`. It reads the scaler, follows its controller
   reference to the DisaggregatedSet, and builds the target from the role's
   template. It behaves as a LeaderWorkerSet everywhere a group matters (group
   size, GPUs per group, whole-group scale-in), and lists its pods by the set and
   role labels.
4. **Actuation** is KEDA's, unchanged: the scaler's `/scale` subresource. The
   scaling manager reads the subresource the same way for every kind.
5. **Workload report.** `make workload-patch` reports drain, weights and engine
   cache gaps per role. It writes no patch for a DisaggregatedSet: the fix
   belongs in the role template, and changing the template rolls every group.
6. **RBAC.** `get` on `disaggregatedsets` and `disaggregatedsetrolescalers`, and
   on `disaggregatedsetrolescalers/scale`, beside the LeaderWorkerSet rules.

The objects are read as unstructured. The repository stays on its current LWS
module, and a cluster without the CRD is not affected.

## The second path: writing a Static role's replicas

A Static role has no `/scale`, and LWS v0.9.0 has no role scaler at all, so
KEDA cannot target the role and never calls us about it. For such a set the
controller actuates the role itself: the role writer (`internal/dsetwriter`).
The codebase had no direct actuator to reuse (KEDA actuates everything); the
writer follows the KEDA HPA repair's shape instead: off by default, leader
only, uncached reads, its own opt-in grant (`DSET_ROLE_WRITER`).

**Which path.** External role on LWS v0.10.0 or newer: KEDA plus the role
scaler, always. The writer never writes an External role (the set controller
ignores its `spec.replicas`). Static role: the writer, if the operator opts in;
otherwise the role is not scaled. `make scaledobjects-plan` writes a Static role
`apply: no` and names both options; it creates nothing for the writer path.
`make workload-patch` reports the role the same on both paths.

How to turn it on and off, what it writes, its safety rules and its events
are in [DisaggregatedSet roles](../reference/disaggregatedset.md). The design
points, in short:

- It registers each Static role of an annotated set in the registry, as a
  KEDA call registers a ScaledObject, under the name a role scaler would have
  (`<set>-<role>`), so attribution and the engines are the same on both paths.
- It applies the engines' decisions with one server-side apply per set,
  naming only `spec.roles[name].spec.replicas`, in the set's own role order.
- It does what KEDA and the HPA do on the other path: it asks the external
  scaler's passive resolver and writes nothing for a passive model, applies
  the external scaler's staleness rule, and holds a scale-down for the HPA's
  default 5-minute stabilization window.
- It keeps the set's all-zero-or-all-non-zero rule, and refuses sets it
  cannot size (`slices` above 1, no model, bad bounds) with an event.
- The installer grants it with `DSET_ROLE_WRITER=true`, like
  `KEDA_HPA_REPAIR`: a ClusterRole or a namespaced Role, renamed per
  install, copied into each managed namespace, revoked when the variable is
  off and by the uninstall.

## Rollout and scaling are different jobs

A rollout tool keeps replica counts fixed and moves the template, group by
group, behind a release gate. A scaler keeps the template fixed and moves the
count. On a DisaggregatedSet they meet in three places:

- **During a rollout** the controller owns the pace. A scale-up still lands, a
  scale-down waits (`Deferred`). Do not fight it.
- **A release gate label.** Some deployments keep a Ready pod out of the
  InferencePool with a label (for example `<prefix>/production-released=false`)
  while it drains or before it is released. Such a group is Ready and serves
  nothing. Pool-selected supply ([pool-selected-supply.md](pool-selected-supply.md),
  on by default) leaves it out: the role's template is selected, so the role is
  not held, and each relabelled group is counted out through its leader, during
  a rollout too. `internal/engines/variantmeta/routing_dset_test.go` and
  `internal/engines/steadystate/pool_routing_dset_test.go` cover it.
- **Drain on scale-in.** The DisaggregatedSet controller deletes groups directly
  on scale-down. The engine must drain in its own `preStop`, on the leader and
  every worker, as for a LeaderWorkerSet
  ([workload preparation](../reference/workload-preparation.md)).

## Out of scope

- `slices > 1`. Upstream rejects it with External roles, and the role writer
  refuses it.
- Sub-roles (`roles[].subRoles`). Their scalers are `<set>-<role>-<sub-role>`,
  and a group's sub-role label can change after assignment. Their pods are not
  attributed.
- A kind e2e. The CRD installs from the LWS release manifests, but our e2e
  images and fixtures would need a P/D model on it; left for later.

## Follow-ups

1. **Release-gated pods as supply** (decided: its own PR). The collector will
   count only pods the variant's InferencePool selects. It is the right answer
   for any gated deployment, not only this one.
2. **Group keys during a rollout** (accepted for now). Two revisions of one role
   each have a group 0. Code that keys groups by `group-index` alone merges them
   while a rollout runs. Readings are held while rolling out; the held-GPU and
   placement counts are not. Fix: key by child LWS and group index everywhere.
3. **Drain timing under `Deferred`** (accepted for now). A lower count waits for
   the rollout to finish. The actuator reads the live count, but the start-time
   learner does not yet tell that wait from a slow start.
