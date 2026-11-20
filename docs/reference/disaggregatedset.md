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
   switch). Installed with `smt` or the release files, see
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
  (`Deferred`, the default). A replica that starts while the set rolls out is
  not used to learn the role's start time: its start is the rollout's.
- **A refused scale-down.** A lower count counts as done only once the
  set's child LeaderWorkerSets run it. See
  [When the cluster refuses a scale-down](#when-the-cluster-refuses-a-scale-down).
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

## When the cluster refuses a scale-down

Some clusters remove a group only after it is drained: an admission policy or
webhook denies any lower `replicas` until a drain tool has taken the group out
of the router and waited for it to go idle. The role writer does not drain.
Pick how scale-down works on such a cluster:

- **Let the drain tool act on the writer's count.** Works when the policy
  guards the child LeaderWorkerSets, not the set: the set then keeps the lower
  count as a request. Your drain tool drains the groups above it and lets the
  decrease through; the set controller applies it. Scale-up works as usual.
  A drain that takes longer than 2 minutes shows as `RoleScaleDownRefused`
  until it ends; that is expected here.
- **Leave the set to your own tooling.** Remove the
  `scaling-manager.llmd.ai/role-writer` annotation.

What the writer does when a lower count is refused:

- **Refused on the set itself.** The write fails and the set keeps its count.
  The writer records `RoleScaleDownRefused` with the API server's message and
  keeps the role at its count. Changes to other roles are written without it
  on the next pass. It
  asks again after 5 minutes, then 10, 20, and so on, at most once an hour,
  until the cluster accepts or the engines want the count back.
- **Refused on the set's child LeaderWorkerSets.** The set takes the lower
  count, but its controller cannot apply it, so the groups keep running. If
  the children still run more groups 2 minutes after the write, the writer
  records `RoleScaleDownRefused` once and does not lower the role again until
  the children follow, the count goes up, or someone else changes it. While a
  rollout runs, the 2 minutes do not count.

To see it: `kubectl get events --field-selector reason=RoleScaleDownRefused -n <namespace>`,
or the controller log line `The cluster refused a role scale-down`.

The writer keeps this state in memory. After a restart it compares each
role's children with the set once, so a refusal on the children from before
is reported again 2 minutes later. A refusal on the set itself is found again
at the next write.

It reads the child LeaderWorkerSets with the `list` on `leaderworkersets`
that the controller's manager role has. If that read fails, the writer logs
it once, reports nothing new, and keeps holding a role it already reported.

## Events

On the DisaggregatedSet. A warning is recorded once, and again only after a
pass that went right. A scale-down refused on the set itself is recorded at
each try.

| reason | type | meaning |
| --- | --- | --- |
| `RoleReplicasWritten` | Normal | the writer changed these roles' replicas |
| `RoleReplicasConflict` | Warning | another field manager owns `replicas`; see [Ownership](#ownership-of-replicas) |
| `RoleWriterRefused` | Warning | the set is not scaled: no `model-id`, `spec.slices` above 1, or a bad `min-replicas` or `max-replicas` |
| `RoleReplicasWriteFailed` | Warning | the write failed for another reason, such as the set's validation |
| `RoleScaleDownRefused` | Warning | the cluster refused a lower count; see [When the cluster refuses a scale-down](#when-the-cluster-refuses-a-scale-down) |

## Turn it off

1. Remove the `scaling-manager.llmd.ai/role-writer` annotation from each set.
   The writer stops writing it at once, and its variants expire.
2. Set each Static role's `replicas` back to the count you want. The writer
   leaves what it last wrote, so a model it parked stays at zero.
3. Re-install without `DSET_ROLE_WRITER=true`. That removes the grant and the
   switch; for a split install, run `make setup-prereqs` and then
   `make deploy-scaling-manager INSTALL_PHASE=scaling-manager`, both without it.
   With `smt`, run `smt install` and then `smt admin-setup` with
   `--no-dset-role-writer`. The uninstall removes the grant always.

The writer's field ownership stays in the set's `managedFields` until another
manager writes the field; it does nothing on its own.

## Installed from the release files

With the tools image, add `--dset-role-writer`
([flags](tools-install.md#flags)). Namespace-scoped install: the cluster
admin grants it, then the namespace owner turns it on:

```bash
smt admin-setup -n <namespace> --dset-role-writer
smt install -n <namespace> --prometheus-url <url> --dset-role-writer
```

With `--model-namespace`, `admin-setup` puts the grant in each model
namespace, where the controller lists the sets. `install` stops with exit 3
until `admin-setup` has granted it. Cluster-wide install, one command:

```bash
smt install --scope cluster --keda-namespace <keda> --prometheus-url <url> --model-namespace <ns> --dset-role-writer
```

A re-run without the flag keeps the grant and the switch, as it keeps the
model namespaces. `--no-dset-role-writer` removes them.

With the release files and `kubectl` only, apply the grant file and set the
switch. `<ctl>` is the controller's namespace:

```bash
# Namespace-scoped install: a Role in <ctl>.
sed "s/__NAMESPACE__/<ctl>/g" scaling-manager-dset-role-writer.yaml | kubectl apply -f -
# Cluster-wide install, in scaling-manager-system: a ClusterRole.
kubectl apply -f scaling-manager-dset-role-writer-cluster.yaml

kubectl set env deployment/scaling-manager-controller-manager -n <ctl> DSET_ROLE_WRITER=true
```

A namespaced controller that manages other namespaces needs the grant in each
of them: use `smt admin-setup --model-namespace <ns> --dset-role-writer`,
which writes it there.

Upgrading from a grant made by hand (the `kubectl create` commands of earlier
releases): those objects carry no `app.kubernetes.io/instance` label, so `smt` does not
remove them with `--no-dset-role-writer`. Delete them yourself once `smt` has
made its own.

To remove it, turn it off as above, then:

```bash
kubectl set env deployment/scaling-manager-controller-manager -n <ctl> DSET_ROLE_WRITER-
sed "s/__NAMESPACE__/<ctl>/g" scaling-manager-dset-role-writer.yaml | kubectl delete -f -   # namespace scope
kubectl delete -f scaling-manager-dset-role-writer-cluster.yaml                              # cluster scope
```

Passive mode holds either way: the writer writes no replicas for a passive
model, grant or not ([Passive mode](#passive-mode)).
