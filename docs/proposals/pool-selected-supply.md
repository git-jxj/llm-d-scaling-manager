# Count only the replicas the router can reach

**Status:** built. The decision on a variant outside every pool is final:
it is excluded and held.

The router sends traffic only to Pods its InferencePool selects. The scaling
manager used to count supply from the scale target and from every Pod that
reported metrics. A Pod outside the pool was counted as capacity but took no
router traffic, so the model looked better supplied than it was and was scaled
too low.

## Where supply was counted

| Step | Code | What it counted |
| --- | --- | --- |
| Replica rows | `internal/collector/attribute.go:44` (`buildInstanceKey`) | every Pod whose series resolves to a managed scale target. Deleted and terminating Pods are dropped at line 62 (`podIsGone`); pool membership was not checked. |
| Replica state | `internal/engines/variantmeta/discovery.go:86` | `CurrentReplicas`, `ReadyReplicas`, `PendingReplicas` from the scale target's status; stuck Pods from a Pod list at line 112. |
| Supply | `internal/engines/analyzers/saturation/aggregate.go:171` | `ReplicaCount` = own rows; with no rows, the scale target's ready count (lines 74, 86). |
| Arriving supply | `aggregate.go:198` | `PendingReplicas` = `CurrentReplicas - ownReplicas`: every Pod of the target that did not report. |
| Clamp | `internal/engines/steadystate/engine_v2.go:1673` | `ReplicaCount` capped at `CurrentReplicas`. |

Supply is `ReplicaCount x PerReplicaCapacity`; anticipated supply adds
`PendingReplicas - StuckReplicas`. A Pod outside the pool reached both.

## What the scaling manager does now

1. **Reads the pools each cycle.** One uncached `List` of InferencePools per
   namespace, in the configured group (`POOL_GROUP`), from
   `variantmeta.Route` (`internal/engines/variantmeta/routing.go`). Not the
   datastore: its informer cannot tell "no pool" from "could not read".
2. **Decides per model whether it is routed.** A model is routed when some
   pool in its namespace selects the pod template of at least one of its
   variants.
3. **Holds a variant no pool selects.** In a routed model, a variant whose pod
   template no pool selects (a canary with its own ScaledObject, a role whose
   labels are wrong) gets no router traffic at any size. Its rows are dropped,
   so it adds no demand and no supply, and its per-replica capacity is set to
   0, which every optimizer reads as "do not size this variant": it stays at
   its current replica count. Sizing it instead would add replicas the router
   never reaches, without end if it is the cheapest variant.
4. **Drops single Pods no pool selects.** In a variant whose template is
   selected, a live Pod whose own labels are not (a relabelled Pod) is found
   among the scale target's own Pods (by ownership, not by label). Its rows
   are dropped before the analyzers run, and after them
   (`applyRouting`, `internal/engines/steadystate/pool_routing.go`) it is
   taken out of `ReplicaCount` when that came from status, and out of
   `PendingReplicas`, where it would otherwise come back through
   `CurrentReplicas - ownReplicas`. A stuck Pod is already subtracted as stuck
   and is not subtracted again.
5. **Keeps targets in scale-target units.** `CurrentReplicas` is not changed,
   so a scale-up of one still adds one Pod, and that Pod is selected.

## How an operator sees it, and releases a held variant

- `llmsm_unrouted_replicas{namespace, model_name, variant_name}`: replicas
  not counted as supply. For a held variant, all of them.
- A `HeldNotInInferencePool` Warning Event on the held variant, every cycle it
  is held: it is held at N replicas, no InferencePool selects its pod template
  while the named variants are routed, so the router sends it nothing.
- One log line when a variant becomes held (`variant held: no InferencePool
  selects its pod template`), one when it is released, and one whenever the
  number of unselected Pods changes.

**To release a held variant**, add labels to its pod template that an
InferencePool in the namespace selects, or create an InferencePool that
selects it. The next cycle counts it and sizes it again.

## Edge cases

- **Pools cannot be read** (Forbidden, API error). Supply is counted as before,
  never as zero, and nothing is held. The failure is counted in
  `llmsm_metrics_collection_errors_total{query_type="pool_list"}` (reason
  `forbidden`, `unavailable` or `other`) and logged. Forbidden is not absent.
- **The pool kind is not served, or the namespace has no pool.** As before:
  no router, nothing to check, nothing counted as an error.
- **A model no pool selects.** As before: it is served without a router, or
  every variant is mislabelled, and the two cannot be told apart.
- **A Pod the pool selects that the target does not own.** Unchanged: it
  resolves to no scale target and is skipped as a mapping miss. Pool
  membership never adds supply.
- **P/D roles.** llm-d selects both roles with one pool (on `llm-d.ai/guide`)
  and splits them by `llm-d.ai/role` in the picker. Membership is checked
  against the pool selector only, so a role move does not change supply.
- **LeaderWorkerSet.** A group counts once, by its leader Pod.
- **Warm pool.** A lent Pod carries the pool's selector labels by design, so
  it stays selected; it is demand and not supply, as before. Bridges lent to a
  held variant are dropped with its rows.
- **Not covered yet.** Scale from zero still reads the scale target's ready
  count (`internal/engines/scalefromzero/wake_signal.go:251`), the warm pool's
  return rule reads rows before they are filtered, and an external analyzer
  sums its own PromQL series, which never pass through this filter. Each is a
  follow-up.

## Tests

- `internal/engines/variantmeta/routing_test.go`: the pool selects every Pod,
  a subset, none of a variant (held), cannot be read, does not exist, the kind
  is not served, and selects another model only; plus starting and stuck Pods.
- `internal/engines/steadystate/pool_routing_test.go`: the supply arithmetic,
  the dropped rows, a held variant through the capacity build and the default
  optimizer (it stays at its count while the routed variant scales up), and the
  Event text.

With the new logic switched off, 4 of 10 variantmeta results and 8 of 9
steadystate results fail; the ones that pass are the cases that must not
change. The steady-state envtest suite installs no InferencePool CRD, so unit
tests carry this path.
