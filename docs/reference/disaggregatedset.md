# DisaggregatedSet roles

A DisaggregatedSet (LeaderWorkerSet v0.9.0 and later) runs each role of a
model, prefill and decode say, as LeaderWorkerSet groups. The scaling manager
sizes each role as its own variant. How a role's count is changed depends on
its `scaling.mode`:

| role | how it is scaled | what you set up |
| --- | --- | --- |
| `External` (LWS v0.10.0 and later) | KEDA, through the role's `DisaggregatedSetRoleScaler` | a ScaledObject per role: `make scaledobjects-plan` writes them, like any other workload |
| `Static` (the default), or any role on LWS before v0.10.0 | the controller's role writer, which writes `spec.roles[].spec.replicas` on the set | the role writer, below |

Prefer `External` where the set allows it. The role writer is for sets that
cannot have a role scaler. Not supported on either path: sets with
`spec.slices` above 1, and sub-roles. How the scaling manager reads a set is in
the [proposal](../proposals/disaggregatedset.md).

## Passive mode

For a [passive](scaling-policy.md) model the scaling manager changes no
replica count of any role, on either path:

- **KEDA (`External` roles).** The external scaler answers each role's
  ScaledObject with the role's current count, read from its
  `DisaggregatedSetRoleScaler`, so the HPA holds it. A role at zero stays at
  zero. If that read fails, the scaler answers an error, not a number, and
  KEDA and the HPA change nothing. (With other triggers on the ScaledObject it
  answers 0 and lets those triggers scale, as for any workload.)
- **Role writer (`Static` roles).** It writes nothing to the set: no
  scale-up, no scale-down, no wake from zero, and no lifting of a role parked
  alone.

The engines still size the roles and publish the counts they would set, so
you can compare before turning passive off.

## Turn on the role writer

1. Install with `DSET_ROLE_WRITER=true`, with the variables you installed with:

   ```bash
   DSET_ROLE_WRITER=true make deploy-scaling-manager NAMESPACE=<namespace> SCOPE=<namespace|cluster>
   ```

   This grants the controller `get`, `list` and `patch` on
   `disaggregatedsets` and turns the writer on. `patch` reaches every field of
   every set in scope, so it is off by default. A cluster-scoped install gets
   the ClusterRole `scaling-manager-dset-role-writer-role-<suffix>`; a
   namespace-scoped one gets the Role `scaling-manager-dset-role-writer-role`
   in its namespace, copied into each namespace it manages. Each install gets
   its own names, so two installs do not share a binding.

   For a split install, the cluster admin runs `make setup-prereqs` with
   `DSET_ROLE_WRITER=true` (the grant), then the namespace owner runs
   `make deploy-scaling-manager INSTALL_PHASE=scaling-manager` with it (the
   switch). Installed from the release files, see
   [below](#installed-from-the-release-files).

2. Annotate each set the writer should scale:

   ```yaml
   metadata:
     annotations:
       scaling-manager.llmd.ai/role-writer: "true"   # or "force", see Ownership
       scaling-manager.llmd.ai/model-id: org/Model   # the served model name; required
       scaling-manager.llmd.ai/min-replicas: "1"     # groups per role; default 1
       scaling-manager.llmd.ai/max-replicas: "8"     # default 10
       scaling-manager.llmd.ai/scaling-policy: standard   # optional tier
   ```

   `min-replicas` is an integer of 0 or more, `max-replicas` an integer of 1
   or more and not below the minimum. Any other value refuses the set with a
   `RoleWriterRefused` event; nothing is scaled until it is fixed.

3. Check it is on: the controller logs `DisaggregatedSet role writer is on`
   at start, and the set gets a `RoleReplicasWritten` event at its first change.

Setting `DSET_ROLE_WRITER=true` on the Deployment by hand, or in the
[settings ConfigMap](configuration-advanced.md), turns the writer on without
the grant: it logs once that it may not list DisaggregatedSets, and scales
nothing.

## What it does

Every 15 seconds, on the leader, for each annotated set in the namespaces the
controller manages:

- It registers every Static role as a variant, as KEDA's calls register a
  ScaledObject: entry `<set>-<role>-llmsm`, scale target
  `DisaggregatedSetRoleScaler` `<set>-<role>`, the name a role scaler would
  have.
- It reads the count the engines decided for each role. A positive decision
  older than 3 minutes is ignored, as the external scaler ignores it; a
  decision to park at zero does not expire.
- When a count changes, it writes every Static role in one server-side
  apply, as field manager `scaling-manager-role-writer`, naming only
  `spec.roles[name].spec.replicas`. External roles are never written.

It holds what KEDA and the HPA hold on the other path:

- **Passive.** For a [passive](scaling-policy.md) model it registers the roles
  but writes nothing; the would-be counts are still published. See
  [Passive mode](#passive-mode).
- **Scale-down window.** A lower count waits 5 minutes, the HPA's default
  `scaleDown.stabilizationWindowSeconds`: the writer applies the highest count
  planned in the last 5 minutes. A higher count is written at once. Parking at
  zero waits the same 5 minutes. After a controller restart the window starts
  again from the set's current counts.
- **All zero or none.** The set forbids one Static role at zero while another
  runs. The writer parks the Static roles only together; a role decided to zero
  alone keeps `max(min-replicas, 1)` groups.
- **During a rollout.** A replica change does not start a roll. A scale-up
  lands in the rollout's pace; a scale-down waits for the old revision to drain
  (`Deferred`, the default).
- **Role order.** The write lists the roles in the set's own order. LWS before
  v1.0.0 hashes `spec.roles` in order into the revision, so a reordered list
  would roll every group.

## Ownership of `replicas`

If another field manager owns a role's `replicas`, the write conflicts: the
writer writes nothing and records `RoleReplicasConflict` on the set. Who owns
the field depends on how the set is applied:

- **Server-side apply** (`kubectl apply --server-side`, Argo CD with
  `ServerSideApply=true`, Flux): the source owns only the fields in its
  manifest. Remove `replicas` from each Static role there, and `"true"` works.
- **Create or client-side apply** (`kubectl create`, plain `kubectl apply`,
  Helm, Argo CD by default): the source owns every field of the stored set,
  including the `replicas` the API defaulted to 1. Use `"force"`, which takes
  the field over, or switch the source to server-side apply.

With `"force"`, stop the source from setting the field back. In Argo CD,
ignore it and keep the ignore on sync:

```yaml
spec:
  ignoreDifferences:
  - group: disaggregatedset.x-k8s.io
    kind: DisaggregatedSet
    jqPathExpressions:
    - .spec.roles[].spec.replicas
  syncPolicy:
    syncOptions:
    - RespectIgnoreDifferences=true
```

`jsonPointers` take no wildcard, so they cannot name every role.

## Events

On the DisaggregatedSet. A warning is recorded once, and again only after a
pass that went right.

| reason | type | meaning |
| --- | --- | --- |
| `RoleReplicasWritten` | Normal | the writer changed these roles' replicas |
| `RoleReplicasConflict` | Warning | another field manager owns `replicas`; see [Ownership](#ownership-of-replicas) |
| `RoleWriterRefused` | Warning | the set is not scaled: no `model-id`, `spec.slices` above 1, or a bad `min-replicas` or `max-replicas` |
| `RoleReplicasWriteFailed` | Warning | the write failed for another reason, such as the set's validation |

## Turn it off

1. Remove the `scaling-manager.llmd.ai/role-writer` annotation from each set.
   The writer stops writing it at once, and its variants expire.
2. Set each Static role's `replicas` back to the count you want. The writer
   leaves what it last wrote, so a model it parked stays at zero.
3. Re-install without `DSET_ROLE_WRITER=true`. That removes the grant and the
   switch; for a split install, run `make setup-prereqs` and then
   `make deploy-scaling-manager INSTALL_PHASE=scaling-manager`, both without it.
   The uninstall removes the grant always.

The writer's field ownership stays in the set's `managedFields` until another
manager writes the field; it does nothing on its own.

## Installed from the release files

The release files (`kubectl apply`, `smt install`) carry neither the grant
nor the switch. Grant both by hand, named after the controller's namespace so
two installs never share a binding. `<ctl>` is the controller's namespace.

Cluster-scoped install:

```bash
kubectl create clusterrole scaling-manager-dset-role-writer-role-<ctl> \
  --verb=get,list,patch --resource=disaggregatedsets.disaggregatedset.x-k8s.io
kubectl create clusterrolebinding scaling-manager-dset-role-writer-rolebinding-<ctl> \
  --clusterrole=scaling-manager-dset-role-writer-role-<ctl> \
  --serviceaccount=<ctl>:scaling-manager-controller-manager
kubectl set env deployment/scaling-manager-controller-manager -n <ctl> DSET_ROLE_WRITER=true
```

Namespace-scoped install, for each namespace the controller manages,
`<namespace>`:

```bash
kubectl create role scaling-manager-dset-role-writer-role -n <namespace> \
  --verb=get,list,patch --resource=disaggregatedsets.disaggregatedset.x-k8s.io
kubectl create rolebinding scaling-manager-dset-role-writer-rolebinding-<ctl> -n <namespace> \
  --role=scaling-manager-dset-role-writer-role \
  --serviceaccount=<ctl>:scaling-manager-controller-manager
```

then the same `kubectl set env`.

To remove it, turn it off as above, then:

```bash
kubectl set env deployment/scaling-manager-controller-manager -n <ctl> DSET_ROLE_WRITER-
kubectl delete clusterrolebinding scaling-manager-dset-role-writer-rolebinding-<ctl>   # cluster scope
kubectl delete clusterrole scaling-manager-dset-role-writer-role-<ctl>
kubectl delete rolebinding scaling-manager-dset-role-writer-rolebinding-<ctl> -n <namespace>   # namespace scope
kubectl delete role scaling-manager-dset-role-writer-role -n <namespace>
```
