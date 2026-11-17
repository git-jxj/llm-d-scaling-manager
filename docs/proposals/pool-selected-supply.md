# Count only the replicas the router can reach

**Status:** proposed. One decision is open (see [Open question](#open-question));
nothing is built.

The router sends traffic only to Pods its InferencePool selects. The scaling
manager counts supply from the scale target and from every Pod that reports
metrics. A Pod outside the pool is counted as capacity but takes no router
traffic, so the model looks better supplied than it is and is scaled too low.

## Where supply is counted today

| Step | Code | What it counts |
| --- | --- | --- |
| Replica rows | `internal/collector/attribute.go:44` (`buildInstanceKey`) | every Pod whose series resolves to a managed scale target. Deleted and terminating Pods are dropped at line 62 (`podIsGone`); pool membership is not checked. |
| Pod state | `internal/collector/pod_serving_state.go:98` (`podStates`) | one uncached Pod `List` per namespace per cycle. A failed list fails open. |
| Replica state | `internal/engines/variantmeta/discovery.go:86` | `CurrentReplicas`, `ReadyReplicas`, `PendingReplicas` from the scale target's status; stuck Pods from a Pod list at line 112. |
| Supply | `internal/engines/analyzers/saturation/aggregate.go:171` | `ReplicaCount` = own rows; with no rows, the scale target's ready count (line 74, 86). |
| Arriving supply | `aggregate.go:198` | `PendingReplicas` = `CurrentReplicas - ownReplicas`: every Pod of the target that did not report. |
| Clamp | `internal/engines/steadystate/engine_v2.go:1673` | `ReplicaCount` capped at `CurrentReplicas`. |

Supply is `ReplicaCount x PerReplicaCapacity`, and anticipated supply adds
`PendingReplicas - StuckReplicas`. A Pod outside the pool reaches both.

Pools are already read: `internal/datastore/datastore.go:183`
(`PoolGetFromLabels`) matches labels against each pool's selector, fed by the
InferencePool informer. RBAC already grants `get, list, watch` on
`inferencepools` in both API groups (`internal/controller/rbac.go:120`,
`docs/reference/security-model.md`), and preflight checks `list`.

## What changes

1. **Read the pools per cycle.** One uncached `List` of InferencePools per
   namespace per cycle, memoised beside the Pod list, in the configured pool
   group (`POOL_GROUP`; `inference.networking.k8s.io` v1 or the x-k8s v1alpha2).
   Not the datastore: its informer cannot tell "no pool" from "could not read",
   and a missing CRD leaves it empty.
2. **Decide per model whether it is routed.** A model in a namespace is routed
   when some pool there selects the pod template of at least one of its
   variants. A model no pool selects is served some other way (a plain Service)
   and keeps today's count.
3. **For a routed model, drop Pods the pool does not select.** In
   `buildInstanceKey`, beside `podIsGone`: such a Pod's series are dropped, so
   neither its demand nor its capacity reaches an analyzer.
4. **Take them out of arriving supply too.** Discovery counts, per variant, the
   live Pods of the scale target that no pool selects (`UnroutedReplicas`, in
   replica units; an LWS counts its leader Pod). They are subtracted from
   `PendingReplicas` and, when a variant has no rows, from the ready count.
   Otherwise a dropped Pod returns as "pending" through
   `CurrentReplicas - ownReplicas` and still holds a scale-up back.
5. **Targets stay in scale-target units.** `CurrentReplicas` is not changed:
   the optimizer adds or removes replicas from the real count, so a scale-up of
   one still adds one routed Pod.
6. **Say what was excluded.** A gauge
   `llmsm_unrouted_replicas{namespace,variant}` and one log line per variant per
   change, naming the pool selectors that were checked.

## Edge cases

- **Pools cannot be read** (Forbidden, CRD not served, API error). Fall back to
  today's count, never to zero. Count the failure in
  `llmsm_metrics_collection_errors_total{query_type="pool_list"}` with the
  same reasons as the Pod list (`forbidden`, `unavailable`, `other`) and log it
  once per namespace per cycle. Forbidden is not absent.
- **The namespace has no pool.** Today's count. No router, nothing to check.
- **A model no pool selects.** Today's count, plus the log line: either it is
  served without a router, or every variant is mislabelled; the scaling
  manager cannot tell which.
- **A Pod the pool selects that the target does not own.** Unchanged: it
  resolves to no scale target and is already skipped as a mapping miss. Pool
  membership never adds supply.
- **P/D roles.** The pool selects both roles (llm-d selects on
  `llm-d.ai/guide`; the picker splits by `llm-d.ai/role`). Membership is
  checked against the pool selector only, not the role, so a role label move
  does not change supply.
- **Scale from zero.** Unchanged in this proposal. The wake signal reads the
  scale target's ready count (`internal/engines/scalefromzero/wake_signal.go:251`);
  a model whose only ready Pods are outside the pool is not woken. Listed as a
  follow-up.
- **Warm pool.** A lent Pod carries the pool's selector labels by design
  (`internal/warmpool/demand.go`, `poolLabels`), so it stays selected; it is
  still demand and never supply, as today. A pool Pod that is awake and not lent
  resolves to the pool's own target and never reaches a model.
- **External analyzers.** Their series do not pass through `buildInstanceKey`.
  `SeriesPodIsGone` is extended to drop unselected Pods the same way, so every
  analyzer sees the same population.

## Open question

**What happens to a variant whose pod template no pool selects, in a model
that is routed?** For example, a canary Deployment with its own ScaledObject and
no pool label. There is no safe default:

- **A. Keep counting it** (today). Its idle capacity keeps inflating the model's
  supply, so the routed variants are scaled too low. This is the bug.
- **B. Exclude it and hold it.** Its supply and demand leave the model, and it
  is held at its current replica count (`PerReplicaCapacity = 0`, which the
  optimizers already skip: `internal/engines/allocation/cost_aware_optimizer.go:90,120`).
  Fixes the bug, but a canary served by another route (an HTTPRoute to a plain
  Service) stops autoscaling.
- **C. Exclude it without holding it.** Not acceptable: if it is the cheapest
  variant, the optimizer keeps adding replicas that never add supply, up to its
  `maxReplicas`.

A data-based variant of B is possible: hold only when the variant also reports
no traffic of its own, and count it as today when it does. That adds a second
rule to explain.

Recommendation: **B**, with the gauge and log line, because a variant outside
every pool of a routed model is far more often a mistake than a second route.
Pods the pool does not select inside a variant the pool does select are
excluded under any choice.

## Tests

Table tests over the pool listing: selects all Pods, a subset, none (whole
variant), unreadable (Forbidden, error), absent (no pool in the namespace, CRD
not served). Each asserts the replica count, pending count and the rows kept,
and each is run against the parent commit to show it failing there. The
envtest suites do not install InferencePool CRDs on this path, so unit tests
carry it.
