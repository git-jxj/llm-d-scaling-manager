# Predicted latency in the controller

**Status:** proposed; part built (issue #440, refs #169). The collection of
the models' inputs, the shared per-cycle input builders, and a stub
component behind `--predicted-latency` (off by default) are built. The
pricing waits for the models to be frozen (planned for 2026-12-06).

**Audience:** contributors to the controller and the capacity planner.

## What and why

The capacity planner (`cmd/capacity-planner`) predicts a fleet's TTFT and TPOT
from a recording. Operators want the same figures live: predicted next to
measured, per model, and the headroom left before a target is missed. Issue
#440 asks the controller to publish the planner's predicted TTFT and TPOT
(mean, p90, p95) at the current load and replicas, computed by the planner's
own model code, not a copy.

Three rules come from the issue:

1. **One model, two callers.** The controller calls the packages the
   planner calls. A fix to a model lands in both (the #169 backport problem
   does not come back).
2. **Missing is reported, never estimated.** A signal the deployment does not
   export shows as missing, per model.
3. **Shadow until validated.** Off by default. No scaling decision reads a
   prediction until a later switch says so.

## The models

| Objective | Default model (planner, no flag) | Experimental model v2 (`--experimental-*=v2`) |
| --- | --- | --- |
| TTFT | the queueing model of [what-if capacity: TTFT](slo-capacity-planner-ttft.md): prefill `V_P`, `a`, `f`, decode term, router hop, over the replayed learned state | rank mixture (`whatif.QueueRankMixture`): processor sharing per prefill rank, mixed over the ranks' traffic shares, a fitted KV-deferral wait and a per-prompt-token cost, fitted on a window |
| TPOT | the ITL line at the fleet's fill, percentile from `R_q` ([what-if capacity](slo-capacity-planner.md#tpot-as-a-percentile-over-requests)) | renewal lognormal (`whatif.TPOTRenewalLognormal`): lognormal around the default model's mean TPOT, its spread from the decode groups plus a renewal term over output lengths, fitted by interval-censored likelihood on the router's TPOT buckets |

The v2 models are still moving. Their form is not settled until the freeze;
their inputs mostly are, and that is what this step builds on.

## Signals

What each model reads, whether the controller collected it before this
change, and how. "Scaling" is a query issued every cycle for every
namespace; "TTFT target" one issued in a namespace whose policy prices TTFT
(`collector.ReadTTFTInputsIn`); "record-only" one issued only while a learner
recording is on (`collector.BeginRecordingCycle`). Windows are the PromQL
range of the query (`internal/collector/registration`). Every per-pod
figure is per pod, ranks merged (`collector/pod_collapse.go`), and per
engine for a LeaderWorkerSet group (`domain.EngineReading`).

### TTFT v2 (rank mixture)

Built per cycle by `whatif.MixtureCycleOf`; fitted per step on the router.

| Signal (`signal` label) | Field | Role | Collected before | Window |
| --- | --- | --- | --- | --- |
| `prefill-pods` | a serving row of a prefill variant | prefill | scaling | cycle |
| `prefill-request-rate` | `RequestRate` | prefill | scaling | 1m |
| `prefill-ttft-mean` | `AvgTTFT` | prefill | scaling | 1m |
| `prefill-time-mean` | `AvgPrefillTime` (vLLM only) | prefill | **record-only** | 1m |
| `queue-time-mean` | `AvgQueueTime` | prefill | **record-only** | 1m |
| `computed-token-rate` | `PrefillComputedTokenRate` | prefill | scaling | 1m |
| `computed-tokens-histogram` | `ComputedTokensBuckets` (vLLM only) | prefill | TTFT target | 1m |
| `prompt-tokens-histogram` | `PromptTokensBuckets` (SGLang behind `--collect-tokens-histogram`) | prefill | **record-only** | 5m |
| `queue-time-histogram` | `QueueTimeBuckets` | prefill | **record-only** | 1m |
| `decode-ttft-mean` | `AvgTTFT` | decode | scaling | 1m |
| `router-ttft-histogram` | `RouterLatency.TTFTBuckets` | router | slo analyzer listed | 5m |
| `router-ttft-mean` | `RouterLatency.TTFTSumRate`/`TTFTCountRate` | router | TTFT target | 5m |
| (engine flags) | prefill `--max-num-seqs`, chunked prefill | variant | discovered (`llmsm_engine_config`) | start |
| (series) | router arrival rate, engine completion rate | model | scaling (`CollectModelArrivalRate`) | 1m |

### TPOT v2 (renewal lognormal)

Built per cycle by `whatif.DecodeCycleOf`; on a fleet with no decode role
the serving rows that are not prefill stand in.

| Signal | Field | Role | Collected before | Window |
| --- | --- | --- | --- | --- |
| `decode-pods` | a serving decode row | decode | scaling | cycle |
| `decode-request-rate` | `RequestRate` | decode | scaling | 1m |
| `itl-mean` | `AvgITL` | decode | scaling | 1m |
| `output-tokens-histogram` | `OutputTokensBuckets` | decode | scaling | 5m |
| `running-requests` | `RunningRequests` | decode | scaling | instant |
| `spec-accepted-per-step` (optional, never missing) | `SpecAcceptedPerStep` | decode | scaling | 1m |
| `router-tpot-histogram` | `RouterLatency.TPOTBuckets` | router | **record-only** | 5m |
| `router-tpot-mean` | `RouterLatency.TPOTSumRate`/`TPOTCountRate` | router | slo analyzer listed | 5m |
| (engine flags) | decode `--max-num-seqs`, `num_speculative_tokens` | variant | discovered | start |
| (mean TPOT) | the default TPOT model's mean | learned state | see below | - |

### Default models

The default models price from learned state, not from one cycle: the ITL
line (`internal/signals/itl`), prefill `V_P`, `a`, `f`
(`internal/signals/prefill`), `k2`, the decode intercept `B`, and the
traffic's lengths and prefix hit rate. The planner gets that state by
replaying a recording through the analyzers' own learners
(`learnreplay.StateAt`); the controller's slo and saturation analyzers hold
the same state live, and the learned-state snapshot
([learned-state snapshot](learned-state-snapshot.md)) persists it. Every
input is a scaling query or a TTFT-target one, so the default models need no
new collection. What they need is the state exposed from the analyzers in
the shape `learnreplay.State` has (see "Package boundary").

### What was missing, and what this change adds

Missing in a controller that records nothing: `prefill-time-mean`,
`queue-time-mean`, `prompt-tokens-histogram`, `queue-time-histogram` (all
record-only), `computed-tokens-histogram` (TTFT target only),
`router-tpot-histogram` (record-only) and `router-ttft-mean` (TTFT target
only). `--predicted-latency` adds them:

- per pod, the five queries of `collector.predictionReadReplicaQueries`,
  issued through the cycle's memo once per namespace (so a second model in
  the namespace costs nothing), for every namespace with a model;
- for the router, `collector.CollectPredictionLatency`: the slo analyzer's
  three queries plus the TTFT `_sum`/`_count` and the TPOT histogram, again
  through the memo.

Cost: at most five per-pod range queries per namespace and one router query
per namespace a cycle, fewer where a TTFT target or a recording issues them
already. The engine TTFT and TPOT histograms (`TTFTBuckets`, `TPOTBuckets`
per pod) are not added: no v2 fit reads them (both fit on the router).

**No scaling decision moves.** The collector keeps the rows with these
fields for the component (`collector.PredictionRows`) and hands the
analyzers the rows a collection without the switch would have built: each
field only a prediction query filled is cleared again
(`collector.clearPredictionOnly`). `TestPredictionRows_AnalyzersReadTheRowsOfACollectionWithoutIt`
compares the two through the real Prometheus parse path, with and without a
TTFT target; with the clearing disabled it fails. This matters because
`AvgQueueTime` and `AvgPrefillTime` are read by the saturation analyzer's
busy-share guard and the prefill signals when present. The router reading
is a separate struct the analyzers never see.

### How missing signals are reported

`whatif.ModelInputs` lists every signal with its objective and role, and
`whatif.MissingModelInputs` reports, per objective, the ones no serving row
of the role (or the router) read this cycle. It sits beside the models, so
the planner can report the same names for a recording. The controller
publishes them as `llmsm_predicted_latency_missing_signal{objective,signal}`
and the objective's `llmsm_predicted_latency_coverage{reason="signal-missing"}`.
A query that failed reads the same as a series not exported (unknown, never
zero); the failure itself is counted under
`llmsm_metrics_collection_errors_total{query_type="predicted_latency"}`,
which `LLMSMMetricsCollectionFailing` leaves out.

## Package boundary

`hack/check-import-direction.sh` orders the pipeline: engine (70) > policy
(60) > plan (50) > analyzers (40) > `signals/whatif` (32) > `signals/floor`
(31) > measuring signals (30) > collect (20) > decision (10) > domain (0).
`internal/whatifapi`, `internal/whatifsource`, `cmd/*` and the new
`internal/predictedlatency` are outside the layers.

| Code | Where today | Where it goes | Status |
| --- | --- | --- | --- |
| model math: rank mixture, TPOT distribution, queues, inversions | `internal/signals/whatif` | stays | shared already |
| per-cycle input builders (`MixtureCycleOf`, `DecodeCycleOf`) | were `learnreplay` (unexported) | `internal/signals/whatif/cycle_inputs.go` | **moved in this change**, `learnreplay` calls them |
| input registry, missing-signal report | - | `internal/signals/whatif/model_inputs.go` | **new** |
| fits and pricing entry points (`fitMainMixture`, `fitMainTPOT`, `ApplyTTFTModel`, `ApplyTPOTModel`, `fitRankMixture`, `fitTPOTDist`) | `internal/whatifapi` | stay; a series-level entry point is added (below) | after freeze |
| `whatifapi.Resolved` / `inputsEntry` assembly | `internal/whatifapi`, from a recording | an `Inputs` built from the controller's ring of cycles | after freeze |
| engine flags (`prefillSlotsOf`, `decodeEngineOf`) | `cmd/capacity-planner` (read `servedState`) | `whatifapi.EngineSlotsOf(prefill, decode *capacity.EngineParams)` | small; after freeze, with the first caller |
| default models' fleet (`servedState`, `servedFleet`, `ttft.go`) | `cmd/capacity-planner`, package `main` (`serve_price.go`, `ttft.go`, `engine.go` and their helpers) | `internal/whatifapi/served` (new), reading a state interface both `learnreplay.State` and the live analyzers satisfy | after freeze; the largest move |
| live learned state in `learnreplay.State` shape | analyzers (in memory) | read-only views on the slo and saturation analyzers (the accessors listed in [what-if capacity, Packaging](slo-capacity-planner.md#packaging)) | after freeze |

Why not move the planner's entry points now: `whatifapi`'s fits take an
`inputsEntry` built from a recording, and the v2 fits change shape weekly
until the freeze. Moving them now would be two moves. The pieces moved here
are the ones whose form is fixed by the collector, not by the models, and
they move byte for byte (the planner goldens and the learnreplay goldens are
unchanged, below).

Import directions after the full move: `predictedlatency` imports
`whatifapi`, `signals/whatif`, `domain` and `metrics`; the engine (70)
imports `predictedlatency`; nothing in the layers imports
`predictedlatency`. `whatifapi` must not import `learnreplay` (it does not
today) so the controller does not link the replay engine.

## Per-cycle flow

Built (stub):

1. `beginCollectorCycle` names every namespace with a model
   (`ReadPredictionInputsIn`) when the switch is on.
2. Each model's collection keeps its full rows; the analyzers get the
   cleared ones.
3. After `prepareModelData`, `observePredictedLatency` reads the kept rows,
   cut to the pods the analyzers read (routing drops unselected pods), the
   variant states, and `CollectPredictionLatency`, and calls
   `Predictor.Observe`.
4. `Observe` reports missing signals per objective and publishes coverage.
   With every input present the reason is `not-built`.
5. A model no longer reconciled has its series deleted
   (`pruneAnalyzerSeries`, `evictAllAnalyzerSeries`).

After the freeze:

- **Ring of cycles.** Per model, the last `W` cycles as `MixtureCycle`,
  `DecodeCycle`, the series sample and the router reading: the fit window.
  `W` is the planner's fit window for the v2 models, in steps of
  `whatifapi.DefaultBacktestStep` (5 minutes); a cycle every 15 s is 20
  cycles a step. Memory: a `MixtureCycle` is a few hundred bytes plus one
  rank per prefill engine; 24 h at 15 s is 5,760 cycles, under 10 MB for a
  large P/D model. Until the ring holds the minimum steps the planner's fit
  needs, the reason is `window-short`.
- **Refit cadence.** Refit once per step boundary (every 5 minutes), not
  every cycle. Between refits, price every cycle at the cycle's load with
  the last fit: pricing is a closed form or a short search, a fit is an
  optimisation.
- **Cost budget.** Budget a refit at 200 ms CPU per model and the pricing
  at 5 ms per model per cycle; measure on the planner's largest golden
  before turning anything on. A refit over budget is abandoned and the
  reason reads `abstained` with the planner's own words, rather than stall
  the optimize loop. Fits run on the optimize goroutine after the
  decision, not before it.
- **Variants.** The planner prices one prefill and one decode variant. A
  model with several gets one prediction per pair, labelled by variant,
  where the pairing is unambiguous; otherwise `abstained`.
- **Learned state.** The fitted parameters (`C` of the TPOT distribution,
  the rank mixture's wait and per-token cost) join the learned-state
  snapshot under their own keys, so a restart prices at once and refits at
  the next step. The ring itself is not persisted: after a restart the
  window refills, and the reason says `window-short` until then, unless a
  persisted fit is younger than one window.

## Metrics

Built: registration, lifecycle and the coverage series.

| Metric | Labels | Value |
| --- | --- | --- |
| `llmsm_predicted_ttft_seconds` | `namespace`, `model_name`, `statistic` (`mean`, `p90`, `p95`) | predicted TTFT; present only while coverage is `priced` |
| `llmsm_predicted_tpot_seconds` | same | predicted TPOT; same |
| `llmsm_predicted_latency_coverage` | `namespace`, `model_name`, `objective`, `reason` | one-hot: `priced`, `abstained`, `signal-missing`, `window-short`, `not-built` |
| `llmsm_predicted_latency_missing_signal` | `namespace`, `model_name`, `objective`, `signal` | 1 per missing signal; absent when read |

All carry `controller_instance` when set. Lifecycle: absent while the
switch is off; set every cycle a model is collected; a signal read again has
its series deleted the same cycle; a model's series go when it stops being
reconciled, every series when a cycle reconciles none. A figure is deleted
the cycle its reason leaves `priced`, so a stale prediction never stands.
The reference: [Predicted latency metrics](../reference/prometheus.md#predicted-latency-metrics).

After the freeze, per figure: `llmsm_predicted_latency_extrapolated` (1 when
the load is past the highest the fit window served, as the planner marks
`Figure.Extrapolated`) and the held-out score of the current fit.

## Parity test

The controller and the planner must give the same figures on the same
recording. The test, after the freeze:

1. Take a recording the planner goldens use (`cmd/capacity-planner/testdata`).
2. Feed its Analyze events, in order, to the component as the engine would:
   each event's `AnalyzerInput` and router latency into `Observe`, with the
   cycle clock at the event's time.
3. At cycle `N`, read the component's TTFT and TPOT figures.
4. Run `capacity-planner at-replicas --recording R --cycle N
   --experimental-ttft=v2 --experimental-tpot=v2` (the same code path, via
   `whatifapi`) and compare: equal to the last bit, not within a tolerance,
   because both call one function on one input.
5. Negative control: change one model input on the controller side only
   (drop the router's TPOT histogram); the test must fail.

The pieces that make this possible exist now: the controller builds its
cycles with the builders the planner's series use
(`TestMixtureCycleOf_*`, `TestDecodeCycleOf_*` cover both callers), and a
recording's `AnalyzerInput` is what the collector hands the analyzers. A
recording's rows always carry every prediction input (a recording issues
every record-only query, and nothing is cleared while it is on), so the
test feeds the component the same rows the planner reads.

## Turn-on rule

Default off. The metrics turn on (the switch is documented for operators)
when, on held-out recordings the planner's backtest scores
(`capacity-planner backtest`, `whatifapi.Backtest` with the v2 models):

- each objective's held-out score passes the planner's own validation bar
  for the model ([calibration envelope](slo-capacity-planner.md#calibration-envelope-as-built)):
  the TPOT model's p90 and p95 in the measured router bucket on the share
  of held-out steps the planner requires, the TTFT model within
  `whatifapi.BacktestTolerance` (20%) on the planner's share of steps;
- the parity test passes on every golden recording;
- the cost budget above holds on the largest golden.

No scaling decision reads a prediction in this issue. Using one in a
decision is a separate switch, with its own replay and GPU validation as
#169 requires.

## What waits for the freeze

- The fits and pricing in the controller, and the ring of cycles.
- The series-level entry point in `whatifapi` (fit and price from cycles,
  not from an `inputsEntry`), and the `EngineSlotsOf` helper.
- The move of the default models' fleet out of `cmd/capacity-planner`, and
  the analyzers' read-only state views.
- The final input list: a signal a v2 model starts or stops reading changes
  `whatif.ModelInputs` and, if it is a new per-pod series,
  `collector.predictionReadReplicaQueries`, in the same change as the model.
- The parity test and the held-out turn-on run.
- Persisting fitted parameters in the learned-state snapshot.

## Code map (built)

- `internal/signals/whatif/cycle_inputs.go`: `MixtureCycleOf`, `DecodeCycleOf`.
- `internal/signals/whatif/model_inputs.go`: `ModelInputs`, `MissingModelInputs`.
- `internal/collector/prediction_inputs.go`: the queries, the clearing,
  `ReadPredictionInputsIn`, `PredictionRows`.
- `internal/collector/router_latency.go`: `CollectPredictionLatency`.
- `internal/predictedlatency`: the stub component.
- `internal/metrics/predicted_latency_metrics.go`, `internal/constants/predicted_latency_metrics.go`.
- `internal/engines/steadystate/predicted_latency.go`: the wiring.
- `cmd/main.go`: `--predicted-latency`.
