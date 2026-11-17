# Count only the replicas the router can reach

**Status:** built. The decision on a variant outside every pool is final:
it is excluded and held. Hold means the controller never moves the variant
and never counts it.

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

1. **Reads the pools once per namespace per cycle.** An uncached `List` of
   InferencePools in the configured group (`POOL_GROUP`), shared by every
   model of the namespace (`Engine.poolLister`). Not the datastore: its
   informer cannot tell "no pool" from "could not read".
2. **Decides which variants are selected.** A variant is selected when a pool
   selects its pod template, with the labels its controller adds at creation
   (an LWS leader's set name and worker index 0; a selector on a Deployment's
   `pod-template-hash` is taken to match), or any of its live Pods (labels an
   FMA or other controller adds). Its Pods are the scale target's own, found
   by ownership. A model is routed when some variant is selected.
3. **Holds a variant no pool selects.** In a routed model, a variant that is
   not selected (a canary with its own ScaledObject, a role whose labels are
   wrong) gets no router traffic at any size. It is held:
   - its rows are dropped, so it adds no demand and no supply;
   - its per-replica capacity is 0, so the optimizers leave it at its count;
   - the scale-to-zero enforcer does not park it, and the minimum-replica
     floor and the keep-the-cheapest-at-1 rule do not count it, so the
     routed variants keep their minimum;
   - scale from zero counts only a variant a pool selects as covering a
     routed model, and never wakes a held one, so a model whose routed
     variants are all at zero still wakes (`decision.DefaultHeld`, keyed by
     model);
   - a utilization-share plan does not include a model with a held variant,
     and a Kueue lender release does not lower a held variant.

   A model counts as routed by its parked variants too, by template: once
   scale-to-zero parks the routed variant, the canary stays held, and the
   parked variant is the one scale from zero wakes. The same holds when the
   controller starts with the routed variant already at zero. A model with
   every variant parked has its routing published each cycle as well.
4. **Drops single Pods no pool selects.** In a selected variant, a live Pod
   whose own labels are not selected (a relabelled Pod) has its rows dropped
   before the analyzers run. After them (`applyRouting`) it is taken out of
   `ReplicaCount` when that came from status, and out of `PendingReplicas`,
   where it would otherwise come back through `CurrentReplicas -
   ownReplicas`. A stuck Pod is already subtracted as stuck and is not
   subtracted again. An LWS group counts once, by its leader.
5. **Keeps targets in scale-target units.** `CurrentReplicas` is not changed,
   so a scale-up of one still adds one Pod, and that Pod is selected.

## A read that fails

If the pools, a variant's scale target or its Pods cannot be read, Route
establishes no verdict for the variants that read decides. The engine keeps
each variant's last verdict for **5 minutes** (`routingVerdictTTL`, ten cycles
at the default interval): a held variant stays held and is not sized, and
unselected Pods stay excluded. After 5 minutes without a good read, the
verdict lapses and every replica is counted as before, the same as an install
whose pools were never readable. The model's selected and held lists, which
scale from zero reads, keep the time of the last good read too and lapse at the
same moment. A lapse is logged once; it is not a release.
A variant never seen before is counted as before at once.

A failed pool listing is counted once per namespace per cycle in
`llmsm_metrics_collection_errors_total{query_type="pool_list"}` (reason
`forbidden`, `unavailable` or `other`) and logged when the namespace's state
changes, naming the permission it needs. Forbidden is not absent. A pool kind
the API server does not serve is no pool, not a failure. If the parked
variants cannot be listed, a model that looks not routed is unknown rather
than released.

## How an operator sees it, and releases a held variant

- `llmsm_unrouted_replicas{namespace, model_name, variant_name}`: replicas not
  counted as supply. For a held variant, all its live, Ready or starting
  replicas, counted as for a selected variant's unselected Pods.
- A `HeldNotInInferencePool` Warning Event on the held variant: recorded when
  it becomes held and every 30 minutes after, outside the one-Event-per-cycle
  budget the scaling Events use. It says the variant is held, why, and how to
  release it.
- One log line when a variant becomes held, when it is released, when the
  number of unselected Pods changes, and when a namespace has pools but none
  selects a model (the router may have no endpoint for it).

**To release a held variant**, add labels to its pod template that an
InferencePool in the namespace selects, or create an InferencePool that
selects it. The next cycle counts it and sizes it again.

**To turn the feature off** for every model, set
`LLMSM_POOL_SELECTED_SUPPLY=false` (environment, or the settings ConfigMap
key of the same name; read at start). Off, every replica counts and nothing
is held, as before v0.12.0.

## Edge cases

- **The namespace has no pool, or the pool kind is not served.** As before.
- **A model no pool selects.** As before, with the log line above: it is
  served without a router, or every variant is mislabelled.
- **A Pod the pool selects that the target does not own.** Unchanged: it
  resolves to no scale target and is skipped as a mapping miss. Pool
  membership never adds supply.
- **P/D roles.** llm-d selects both roles with one pool (on `llm-d.ai/guide`)
  and splits them by `llm-d.ai/role` in the picker. Membership is checked
  against the pool selector only, so a role move does not change supply. A
  role the pool does not select is held: a prefill reached through a fixed
  Service instead of the pool does not scale with decode.
- **DisaggregatedSet roles.** A role is a variant whose replicas are
  LeaderWorkerSet groups. Its Pods are found by the set and role labels, and
  counted by their leaders. Its template is matched with those labels and worker
  index 0 added, so a pool that selects the set by its name label selects a
  parked role too. A role no pool selects is held like any variant. A selector
  on the child LeaderWorkerSet's name, which changes with every rollout, matches
  only through running Pods: a parked role so selected reads as held.
- **Warm pool.** A lent Pod carries the pool's selector labels by design, so
  it stays selected; it is demand and not supply, as before. A held variant is
  never short, so the pool does not lend to it.
- **Short windows where scale from zero reads as before.** Until the first
  steady-state cycle after a restart publishes the routing, and while only the
  parked-variant listing fails, scale from zero has no routing for a model and
  treats it as before the feature: a running held canary covers it, and a
  parked held variant could be woken. Both last about one cycle. A variant
  created between cycles in a routed model is in neither list and does not
  cover it until the next cycle: at worst one extra wake.
- **A blocked Kueue release is logged.** A release that would lower a held
  variant leaves it alone and logs that once every 10 minutes, naming the
  variant, so a quota a lender cannot get back is visible.
- **Not covered yet.** The warm pool's return rule reads rows before they are
  filtered, and an external analyzer sums its own PromQL series, which never
  pass through this filter. Both are follow-ups.

## Tests

- `internal/engines/variantmeta/routing_test.go`: pools select every Pod, a
  subset, none of a variant (held); unreadable, absent, not served, another
  model's pool; a selector on a Deployment-added label; selection by live
  Pods; a failed Pod list (unknown, not held); LWS leaders only; starting and
  stuck Pods; the `pool_list` counter by reason.
- `internal/engines/steadystate/pool_routing_test.go` and
  `pool_routing_cycles_test.go`: the supply arithmetic, the dropped rows, a
  held variant through the capacity build and the optimizer, hold across a
  failed read and its lapse, the Event cadence, one pool listing per
  namespace per cycle, and the feature off.
- `internal/engines/allocation/held_variant_test.go`: scale-to-zero leaves a
  held variant at its count; the minimum-replica floor and cheapest-at-1 rule
  skip it.
- `internal/engines/scalefromzero/held_test.go`: a held variant neither covers
  its model nor wakes.
- `internal/utils/pool/selectors_test.go`: both pool API groups, an unknown
  group, and the selector match (an empty selector matches every Pod).

The negative control turns each new rule back into what it replaced, with the
tests left as they are, and runs them. The PR lists the exact changes and the
tests that fail.
