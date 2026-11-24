# Predicted latency and capacity in the controller

**Status:** proposed; layer skeleton built (issue #440, refs #169). Built and
switched off: the models' inputs as collector signals, the shared per-cycle
input builders in the signals layer, the v2 latency learners registered by
name in the slo analyzer, and the slo analyzer's coverage series. The fits and
predictions wait for the models' freeze (planned for 2026-12-06).

**Audience:** contributors to the controller, its analyzers and the capacity
planner.

## What and why

The capacity planner (`cmd/capacity-planner`) predicts a fleet's TTFT and TPOT
and its knees from a recording. Operators want the same figures live, next to
what was measured. Issue #440 asks the controller for them from the planner's
own model code; #169 sets the rule that a planner fix is carried into the
controller's learners and analyzers, not copied.

So the planner's models are not added beside the engine. Each quantity is
**owned by the analyzer that owns it today**, learned by **the learner that
learns it today** (extended with a mode where the planner's model differs),
from **ordinary collector signals**, and the planner's replay
(`learnreplay`) runs the same learners and signal builders, so the two agree
by construction. Three rules from the issue hold throughout:

1. **One model, two callers.** Controller and planner call the same packages.
2. **Missing is reported, never estimated.** Per model, under the owning
   analyzer's metric family.
3. **Shadow until validated.** Every addition is behind a switch in the
   analyzers' existing switch registry (`SetSwitchesForReplay`), off from the
   constructor, and no sizing reads it until its evidence flips the default.

## The engine's layers

`collector -> signals -> learners -> analyzers -> engine`
(`docs/developer-guide/signal-dataflow.md`; import order in
`hack/check-import-direction.sh`: collector 20, signals 30-32, analyzers 40).
Learners follow the shared rules of [analyzer-learner](analyzer-learner.md):
observe, admit or refuse, fit, predict with a source and an age, validate,
keep, refit, hold or forget; each fills a `learn.Report` and is reached by the
forget annotation through `learn.Learners`.

## Mapping, per quantity

| Planner quantity | Signals (collector -> signals layer) | Learner | Owning analyzer | Series (existing / new) | Switch | After the freeze |
| --- | --- | --- | --- | --- | --- | --- |
| TTFT, default model (mean, objective percentile) | scaling + TTFT-target queries; prefill observation (`signals/prefill`) | existing: `V_P`, `a`, `f` (`signals/prefill`), decode term, router hop | slo | existing `llmsm_slo_predicted_ttft_seconds{statistic}`, `llmsm_slo_ttft_reason`, `_refused`, `_flag`, `_vp_source` | existing TTFT switches | publish `mean`, `p90`, `p95` for every fleet shape the model prices (today: the objective's percentile, prefill variants of P/D) |
| TTFT, v2 rank mixture | five per-pod queries + router TTFT mean (below); `whatif.MixtureCycleOf` | **v2 mode of the TTFT learner**: `learn.TTFTMixture` (registered by name, learns nothing yet) | slo | existing `llmsm_slo_predicted_ttft_seconds` under a model choice; new `llmsm_slo_model_coverage`, `llmsm_slo_model_input_missing` | `slo-v2-inputs` (built, off) | fit (admit steady steps, robust fit, held-out validate, hold/forget), price, persist the fit in the learned-state snapshot; a second switch for the model choice |
| TPOT, default model (mean, `R_q` percentile) | scaling queries | existing: the slo analyzer's ITL line (`learn.SLO`), the TPOT percentile ratio | slo | existing `llmsm_slo_predicted_itl_seconds`, `llmsm_slo_fill`; new `llmsm_slo_predicted_tpot_seconds{statistic}` (registered, unset) | existing | publish mean, p90, p95 |
| TPOT, v2 renewal lognormal | output-length histogram, ITL, running, accepted drafts, router TPOT histogram; `whatif.DecodeCycleOf` | **v2 mode of the TPOT ratio**: `learn.TPOTDistribution` (registered by name) | slo | as above, same coverage series | `slo-v2-inputs` | fit `C` on the router's buckets, price p90/p95 around the default mean, persist `C` |
| Decode knee (KV, batch, fill; ITL line) | scaling queries | existing: k2, ITL line, mu (`learn.K2`, `ITL`, `Mu`) | saturation | existing `llmsm_learned_state_k2_*`, `llmsm_itl_line_*`, `llmsm_itl_seconds_at_pricing_k`, `llmsm_pricing_k`, `llmsm_service_rate_per_replica`, `llmsm_kv_cache_tokens_capacity` | existing | where the planner's knee differs (#169 items 1, 3), a mode of the existing learner, behind a saturation switch |
| Prefill knee (`V_P` and its sources) | prefill observation | existing: `V_P` (`signals/prefill`) | slo (TTFT) and saturation (prefill floor) | existing `llmsm_slo_ttft_vp_source`, `llmsm_slo_ttft_replica_requests_per_second` | existing | `f` per pod count (#169 item 5) as a mode of the existing learner |
| Fleet knee and operating point | the above | the above | saturation (floor), throughput (service rate) | existing `llmsm_required_capacity`, `llmsm_spare_capacity`, `llmsm_saturation_utilization`, `llmsm_analyzer_demand` | existing | the planner's fleet knee read from the same learners; any new figure is a label of an existing family |

Nothing new is planned for knees and capacity: the planner's figures there
come from the learners the saturation, throughput and slo analyzers already
run (the planner replays them), so the work after the freeze is the #169
modes, each in its owning learner.

## Signals

What the v2 latency models read, whether the controller collected it before
this change, and over which window. "Scaling" is a query issued every cycle;
"TTFT target" one issued in a namespace whose policy prices TTFT; "record-only"
one issued only while a learner recording is on.

### TTFT v2 (rank mixture), built per cycle by `whatif.MixtureCycleOf`

| Signal (`signal` label) | Field | Role | Collected before | Window |
| --- | --- | --- | --- | --- |
| `prefill-pods` | a serving row of a prefill variant | prefill | scaling | cycle |
| `prefill-request-rate` | `RequestRate` | prefill | scaling | 1m |
| `prefill-ttft-mean` | `AvgTTFT` | prefill | scaling | 1m |
| `prefill-time-mean` | `AvgPrefillTime` (vLLM only) | prefill | **record-only** | 1m |
| `queue-time-mean` | `AvgQueueTime` | prefill | **record-only** | 1m |
| `computed-token-rate` | `PrefillComputedTokenRate` | prefill | scaling | 1m |
| `computed-tokens-histogram` | `ComputedTokensBuckets` (vLLM only) | prefill | TTFT target | 1m |
| `prompt-tokens-histogram` | `PromptTokensBuckets` | prefill | **record-only** | 5m |
| `queue-time-histogram` | `QueueTimeBuckets` | prefill | **record-only** | 1m |
| `decode-ttft-mean` | `AvgTTFT` | decode | scaling | 1m |
| `router-ttft-histogram` | `RouterLatency.TTFTBuckets` | router | slo analyzer | 5m |
| `router-ttft-mean` | `RouterLatency.TTFTSumRate`/`TTFTCountRate` | router | TTFT target | 5m |
| (engine flags) | prefill `--max-num-seqs`, chunked prefill | variant | discovered (`llmsm_engine_config`) | start |

### TPOT v2 (renewal lognormal), built per cycle by `whatif.DecodeCycleOf`

| Signal | Field | Role | Collected before | Window |
| --- | --- | --- | --- | --- |
| `decode-pods` | a serving decode row | decode | scaling | cycle |
| `decode-request-rate` | `RequestRate` | decode | scaling | 1m |
| `itl-mean` | `AvgITL` | decode | scaling | 1m |
| `output-tokens-histogram` | `OutputTokensBuckets` | decode | scaling | 5m |
| `running-requests` | `RunningRequests` | decode | scaling | instant |
| `spec-accepted-per-step` (optional) | `SpecAcceptedPerStep` | decode | scaling | 1m |
| `router-tpot-histogram` | `RouterLatency.TPOTBuckets` | router | **record-only** | 5m |
| `router-tpot-mean` | `RouterLatency.TPOTSumRate`/`TPOTCountRate` | router | slo analyzer | 5m |
| (mean TPOT) | the default TPOT model's mean | learner | the slo analyzer's ITL line | - |

The default models need no new collection: their inputs are scaling or
TTFT-target queries, and their learned state is the analyzers' own.

### Collection, and the proof that no decision moves

With `slo-v2-inputs` on, the engine names every namespace with a model
(`collector.ReadPredictionInputsIn`), and the collector issues the five
per-pod queries of `predictionReadReplicaQueries` as ordinary replica
queries: same registration, same per-cycle memo (once per namespace), same
failure counting (`query_type="slo_model_inputs"`, which
`LLMSMMetricsCollectionFailing` leaves out). Each is an existing record-only
or TTFT-target query; the switch adds a third condition under which it is
issued. The router's TTFT mean and TPOT histogram come from the same router
queries through `CollectPredictionLatency`.

The values these queries fill are read today by other analyzers when present
(`AvgQueueTime` and `AvgPrefillTime` by the saturation analyzer's busy-share
guard and the prefill signals), so the collector keeps the rows carrying
them for the slo analyzer's v2 models (`PredictionRows`) and hands the
analyzers the rows a collection without the switch builds: each field only
a prediction query filled is cleared (`clearPredictionOnly`).
`TestPredictionRows_AnalyzersReadTheRowsOfACollectionWithoutIt` compares the
two through the real Prometheus parse path, with and without a TTFT target and
with a recording; with the clearing disabled it fails. A recording is
unaffected: it issues every query already, and nothing is cleared.

### Missing signals

`whatif.ModelInputs` lists every input with its objective and role;
`whatif.MissingModelInputs` reports, per objective, the ones no serving row of
the role (or the router) read this cycle. It lives beside the models, so the
planner can report the same names for a recording. The slo analyzer publishes
`llmsm_slo_model_input_missing{objective,signal}` and the one-hot
`llmsm_slo_model_coverage{objective,reason}` (`priced`, `abstained`,
`signal-missing`, `window-short`, `not-built`). A failed query reads the same
as a series not exported: missing, never zero.

## Packages and import direction

| Code | Layer | Status |
| --- | --- | --- |
| model math (rank mixture, TPOT distribution, queues, inversions) | `internal/signals/whatif` (32) | shared already |
| per-cycle builders `MixtureCycleOf`, `DecodeCycleOf` | `internal/signals/whatif` | **moved** verbatim from `learnreplay`; the planner's series call them |
| input registry `ModelInputs`, `MissingModelInputs` | `internal/signals/whatif` | **new**, beside the builders |
| learner names `learn.TTFTMixture`, `learn.TPOTDistribution` | `internal/signals/learn` | **new**, on neither `Learners` nor `Reporters` until each learns and forgets |
| v2 learner registration, `ObserveModelInputs`, `SwitchV2Inputs` | `internal/engines/analyzers/slo` (40) | **new**, switched off |
| fits (`fitRankMixture`, `fitTPOTDist`, `fitMainMixture`, `fitMainTPOT`) | `internal/whatifapi` | after the freeze: move the fit cores into `signals/whatif` as learner fits the slo analyzer and `whatifapi` both call |
| default models' fleet (`servedState`, `servedFleet`, `ttft.go`) | `cmd/capacity-planner` (package `main`) | after the freeze: read from the analyzers' learners through read-only views ([what-if capacity, Packaging](slo-capacity-planner.md#packaging)); nothing is copied |

`whatifapi` imports no analyzer and must stay so; after the move the slo
analyzer (40) imports `signals/whatif` (32), never `whatifapi`.

## Per-cycle flow

Built (switched off):

1. `beginCollectorCycle` asks the slo analyzer whether it reads the v2
   inputs (`ReadsModelInputs`) and, if so, names every namespace with a model.
2. Each model's collection keeps its full rows; the analyzers get the cleared
   ones.
3. After `prepareModelData`, the engine hands the slo analyzer the kept rows
   (cut to the pods the analyzers read), the variant states and the router
   reading (`ObserveModelInputs`, an optional interface as
   `TakeLearnerReports` is).
4. The analyzer reports missing inputs per objective and publishes coverage;
   with every input present the reason is `not-built`.
5. A model no longer reconciled has its series deleted (the engine's prune,
   and `metrics.DeleteSLOModel`).

After the freeze, inside the two learners:

- **Observe and admit.** Each cycle's `MixtureCycle` / `DecodeCycle` is a
  reading; a step (5 minutes, `whatifapi.DefaultBacktestStep`) is admitted
  when stationary (the planner's backlog test) and settled
  ([settled replicas](analyzer-learner.md#4-settled-replicas)).
- **Fit.** Refit once per step boundary on the window of admitted steps,
  untrimmed after exclusion by state ([analyzer-learner, 5](analyzer-learner.md#5-fits-exclude-by-state-then-fit-untrimmed));
  price every cycle with the last fit. Budget: 200 ms CPU per refit per
  model, 5 ms per pricing; a refit over budget is abandoned and the reason
  reads `abstained`.
- **Validate.** The planner's held-out folds (the TPOT model's
  bucket-inside score, the TTFT model's within-20% share) run on the window;
  a fit that fails them is held, not used.
- **Keep, hold, forget.** Keys by prefill/decode pair; forgotten on the
  annotation and the variant timeout like every learner; each joins
  `learn.Learners` and `Reporters` then.
- **Persist.** The fitted parameters join the learned-state snapshot under
  their learner names, so a restart prices at once; until a window refills,
  `window-short`.

## Parity with the planner

By construction: the planner's replay (`learnreplay`) builds its cycles with
the same `whatif` builders, and after the freeze runs the same learners
through the slo analyzer it already replays. The test:

1. Replay a golden recording through `learnreplay` with the v2 switches on.
2. At cycle `N`, read the slo analyzer's v2 figures.
3. Run `capacity-planner at-replicas --recording R --cycle N
   --experimental-ttft=v2 --experimental-tpot=v2`; compare equal to the last
   bit.
4. Negative control: drop one input on the controller side only (the router's
   TPOT histogram); the test fails.

## Turn-on rule

Each switch stays off until, on held-out recordings scored by
`capacity-planner backtest`, the model passes the planner's own validation
bar ([calibration envelope](slo-capacity-planner.md#calibration-envelope-as-built)),
the parity test passes on every golden, and the cost budget holds on the
largest. Turning `slo-v2-inputs` on publishes coverage only; exporting v2
predictions is the model-choice switch; sizing with them is a third, later
decision with its own replay and GPU validation, as #169 requires.

## What waits for the Dec 6 freeze

- The two learners' fits, validation, hold/forget and snapshot keys, and
  their joining `learn.Learners`.
- The model-choice switch and the predicted figures (`mean`, `p90`, `p95`)
  on `llmsm_slo_predicted_ttft_seconds` and `llmsm_slo_predicted_tpot_seconds`.
- Moving the fit cores from `whatifapi` into `signals/whatif`.
- The #169 modes of the k2, ITL, mu and `V_P` learners for the knees.
- The final input list: a signal a frozen model adds or drops changes
  `whatif.ModelInputs` and, for a new per-pod series,
  `predictionReadReplicaQueries`, in the same change.
- The parity test and the held-out turn-on run.

## Entities added

| Name | Kind | Why an existing one could not carry it |
| --- | --- | --- |
| `slo.SwitchV2Inputs` (`slo-v2-inputs`) | switch, in the existing slo switch registry | the collection must be off by default and flippable on evidence; no existing switch gates these inputs. A controller flag was tried first and dropped in favour of the registry |
| `whatif.MixtureCycleOf`, `whatif.DecodeCycleOf` | functions (moved, not new) | existed unexported in `learnreplay`, which the controller cannot import below the analyzers; moved verbatim to the signals layer |
| `whatif.ModelInputs`, `MissingModelInputs` | signal registry | presence bits say whether one field was read; nothing lists which fields a model needs, and the planner needs the same list |
| `learn.TTFTMixture`, `learn.TPOTDistribution` | learner names | the v2 fits differ from the TTFT (`V_P`) and TPOT-ratio learners in what they fit; they are modes of those, but each needs its own report and forget key |
| `predictionReadReplicaQueries`, `PredictionRows`, `CollectPredictionLatency`, `ReadPredictionInputsIn` | collector query class and accessors | the existing classes (scaling, TTFT target, record-only) are each issued under one condition; the v2 inputs are a union of two of them under a third. The kept rows exist because clearing is the only way to issue them without moving a decision |
| `query_type="slo_model_inputs"` | label value of an existing counter | a failure must not raise the collection alert, as `ttft_inputs` and `learner_recording` do not |
| `llmsm_slo_model_coverage`, `llmsm_slo_model_input_missing` | series in the slo family | `llmsm_slo_ttft_reason` is per prefill variant and the default model's; coverage is per model and objective, and covers TPOT |
| `llmsm_slo_predicted_tpot_seconds` | series in the slo family | asked for by #440 beside `llmsm_slo_predicted_ttft_seconds`; `llmsm_slo_predicted_itl_seconds` is ITL at the priced fill, not a TPOT statistic |
| `modelInputsObserver` | optional interface in the engine | the engine holds the slo analyzer as `sloStateKeeper`, whose test doubles would all have to grow the methods; an optional interface is how `TakeLearnerReports` is taken |
