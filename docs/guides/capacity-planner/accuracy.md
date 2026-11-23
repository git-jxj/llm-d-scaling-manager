# Capacity planner: how accurate each figure is

Every figure the [capacity planner](README.md) prints says whether a real run
checked it, and how far off it read there. **Validated means measured, not
accurate: read the range.** A validated figure can be twice the real value.
And every region so far is **in-sample**: the one run that measured it also
tuned the planner (see [What was measured](#what-was-measured)).

## Read the label

Each figure carries one of two labels.

- **`validated in-sample: error +3..+152% (min-max, n=6 load levels, 12 readings; q32 staircase)`.**
  Your fleet and traffic sit inside what a measured run covered. On that
  run the planner's figure read 3% to 152% over what the client measured,
  lowest to highest, over 6 independent load levels (12 readings: each level
  read twice). `in-sample` says the run also tuned the planner; a run kept
  out of tuning would print `validated on a holdout`.
- **`OUTSIDE VALIDATED REGION (prompt spread not measured (cv 2 assumed), ...)`.**
  No run checked a figure like this one. The bracket names the first thing
  that differs. The figure may still be right; nobody has measured it.

The label always gives the count (`n=`, the independent load levels first)
and what kind of range it is:

| Range | Means |
| --- | --- |
| `p10-p90` with `min-max` | 10 or more independent load levels: the middle 8 in 10, then the extremes |
| `min-max` | fewer than 10 independent load levels: the lowest and the highest |
| `single reading, between two ladder steps` | one measurement, known only to lie between two steps of the run |
| `one-sided ... not crossed within the steps run` | the run never reached the point. For a rate, `at most -6%`: the planner read at least 6% low, and how much lower is unknown. For a knee, `at most +3%`: the fleet held the planner's knee to within the run's 3% hold rule |

The error is the planner's figure over the measured one, less 1.

| Figure | `+` means | Safe side |
| --- | --- | --- |
| TTFT, TPOT | the planner reads slower than measured | `+` |
| Knee, onset, rate at a target | the planner promises more than the fleet held | `-` |

Where to find it:

- **CLI text:** after each figure, one label a line.
- **CLI JSON (`-o json`):** `validation` on each answer. It holds
  `validated`, `label`, `errorLo` and `errorHi` (shares, not percent),
  `readings` and `outside`; inside a measured row also `source`, `shape`,
  `spread` (the kind of range: `min-max`, `p10-p90`, `bracket` or
  `one-sided`), `levels` (the independent load levels among the readings)
  and `holdout` (true only for a row whose run never tuned the planner,
  false for an in-sample one). A refused figure, and the operating rate,
  which is an input, have none.
- **API (`POST /v1/plan`):** `validation` on the knees, the TTFT at the rate,
  the rate at the target, the TPOT's two points and the limits. Curve points
  and the marginal steps carry none.

## What was measured

One run so far: Qwen3-32B at tensor parallelism 2 on H200, 2 prefill and 2
decode Pods. It climbed a ladder of 6 load levels, from 0.40 to 0.90 of the
knee, each judged over 7 to 22 minutes. Each level ran twice, once with the
router's flow control off and once on. The client timed every request.

So "n=6 load levels, 12 readings" is 6 load levels, each read twice. The
planner prices both phases the same below the onset, so the two readings of
a level are one reading of the planner, and the count is 6. Over 6 values a
10th and a 90th percentile are just the second-lowest and the second-highest,
so under 10 independent levels the label gives the lowest and the highest.
The error grows with load: on long prompts the mean TTFT reads about +7% at
0.40 of the knee and +58% to +141% at 0.90. The range on a label covers the
whole band of load it names.

**In-sample.** This run is also one of the planner's goldens
(one of its test fixtures), and constants the planner ships were read from it. Its
errors are how well the planner fits the run it was tuned on, so treat them
as a floor on the error at another fleet, not a bound. No run kept out of
tuning has been measured yet; when one is, its rows will print `validated on
a holdout`.

**The decode knee.** Inside its region (the row below) the decode knee held
to within the run's 3% hold rule. Outside it, on the Qwen3-0.6B goldens, three
knees read 43% to 99% high past the table's bound and one 42% low; the
planner says the same 43% to 99%. `at-replicas` prints the label under the decode knee
in its capacity table, says the 0.6B range under `not validated` only when
the knee is outside, and sizes decode at the label's upper error inside the
region, or at 99.1% outside it, beside the point answer
(`needed.decodeAtErrorBound`, at most the search's `maxReplicas`). An
OVER-PROVISIONED verdict never cuts decode below that count: decode has a
replica to spare only where one fewer still meets it. The
label's `n=` gives the independent load levels first, then the raw
readings.

This table is generated from the rows the planner ships:

<!-- envelope-table: begin (go test ./cmd/capacity-planner -run TestStaircaseQ32 -update) -->
| Figure | Shape | Error | Range | Readings | Knee share | Flow control |
| --- | --- | --- | --- | --- | --- | --- |
| decode-knee | 1000-token prompts, 1000 out, Ca^2 1 | at most +3% | one-sided | 1 |  | off |
| flow-control-onset | 8000-token prompts, 200 out, Ca^2 1 | -4..+2% | bracket | 1 |  | on |
| flow-control-onset | 8000-token prompts, 200 out, Ca^2 4 | -7..-4% | bracket | 1 |  | on |
| mean TPOT | 1000-token prompts, 1000 out, Ca^2 1 | +1..+18% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | any |
| mean TPOT | 8000-token prompts, 200 out, Ca^2 1 | -3..+1% | min-max | 6 load levels (12 readings) | 0.096 to 0.215 | any |
| mean TPOT | 8000-token prompts, 200 out, Ca^2 4 | -5..-1% | min-max | 6 load levels (10 readings) | 0.096 to 0.203 | any |
| prefill-knee | 8000-token prompts, 200 out, Ca^2 1 | at most +3% | one-sided | 1 |  | off |
| prefill-knee | 8000-token prompts, 200 out, Ca^2 4 | at most +3% | one-sided | 1 |  | off |
| rate-at-ttft p90 | 1000-token prompts, 1000 out, Ca^2 1 | -6..+0% | bracket | 1 | 0.825 to 0.875 | off, on |
| rate-at-ttft p95 | 1000-token prompts, 1000 out, Ca^2 1 | -6..+0% | bracket | 1 | 0.825 to 0.875 | off, on |
| rate-at-ttft p90 | 8000-token prompts, 200 out, Ca^2 1 | at most -6% | one-sided | 1 | 0.825 to 0.875 | off, on |
| rate-at-ttft p95 | 8000-token prompts, 200 out, Ca^2 1 | at most -6% | one-sided | 1 | 0.825 to 0.875 | off, on |
| rate-at-ttft p90 | 8000-token prompts, 200 out, Ca^2 4 | at most +0% | one-sided | 2 | 0.740 to 0.875 | off, on |
| rate-at-ttft p95 | 8000-token prompts, 200 out, Ca^2 4 | at most +2% | one-sided | 2 | 0.740 to 0.875 | off, on |
| ttft mean | 1000-token prompts, 1000 out, Ca^2 1 | -15..+11% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft p90 | 1000-token prompts, 1000 out, Ca^2 1 | -11..+11% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft p95 | 1000-token prompts, 1000 out, Ca^2 1 | -11..+10% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft mean | 8000-token prompts, 200 out, Ca^2 1 | +7..+141% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft p90 | 8000-token prompts, 200 out, Ca^2 1 | +4..+152% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft p95 | 8000-token prompts, 200 out, Ca^2 1 | -4..+171% | min-max | 6 load levels (12 readings) | 0.400 to 0.900 | off, on |
| ttft mean | 8000-token prompts, 200 out, Ca^2 4 | +9..+122% | min-max | 6 load levels (9 readings) | 0.400 to 0.850 | off, on |
| ttft p90 | 8000-token prompts, 200 out, Ca^2 4 | +25..+164% | min-max | 6 load levels (9 readings) | 0.400 to 0.850 | off, on |
| ttft p95 | 8000-token prompts, 200 out, Ca^2 4 | +31..+184% | min-max | 6 load levels (9 readings) | 0.400 to 0.850 | off, on |
<!-- envelope-table: end -->

What this says:

- **Short prompts:** the planner is close. TTFT reads from about 18% fast
  (the mean, at the lightest step) to 9% slow, and mean TPOT up to 18% slow.
  A figure that reads fast promises a TTFT the fleet will not deliver. At
  light load (0.4 of the knee) the p90 read 13% fast and the mean 18% fast:
  add that much before you compare a short-prompt TTFT with your SLO.
- **Mean TPOT on long prompts:** within a few percent, from 5% fast to 1% slow.
- **Long prompts:** TTFT reads from 4% fast to 184% slow. It is mostly slow,
  and more so near the knee.
  Sizing for TTFT therefore errs on the side of more Pods than needed.
- **Knees, flow control off:** every phase held at the knee the planner
  names. The run stopped there, and its hold rule cannot see a fleet short
  by less than 3%, so the knee is "not exceeded within the steps run": at
  most about 3% high.
- **Flow control on:** the fleet stops at the router's onset, not at the
  knee: 5% to 25% under it in this run. The knee is outside with flow control
  on; read the onset's label instead. For bursty traffic the router started
  holding requests 4% to 7% before the onset the planner names.
- **Rates:** the fleet held every rate the planner named at its target. For
  short prompts the true rate lies between the planner's and 6% above it. For
  long prompts the run's steps did not go far enough to say how much more.

A figure counts as inside only if every input that priced it is one the run
priced:

- The prompts' length spread was measured or stated, and is close to the
  run's (cv 0.046 to 0.072). The default, cv 2 with no histogram, is outside.
- The arrivals were measured or stated (`--arrival-cv2`), with Ca² within
  1.25x of the run's. Poisson assumed by default is outside.
- The router's flow-control state is known. Without its queue series, state
  it with `--flow-control on|off`. A knee needs it off.
- Every term was learned from the recording. A flag that types a term in
  (`--assume-vp`, `--assume-a`, `--assume-router-hop`,
  `--assume-transfer-base`, `--batch-cap`, `--assume-tpot-ratio`, the
  experimental pricing flags, the capture's engine overrides) puts every
  figure outside. So does any flag on a figure that the run's own figures did
  not carry.
- The model is Qwen3-32B, at tensor parallelism 2 for prefill and decode, on
  H200, with 2 prefill and 2 decode Pods.
- Prompt and output lengths are within 1.25x of a measured shape, and the
  prompts share little prefix (hit rate up to 0.05).
- Statistic: the mean, p90 or p95.
- The load is within the levels the run measured; the table gives each
  row's band of knee shares.
- Flow control below its onset. A figure flagged `AboveFlowControlOnset` is
  outside: the router's wait there is not priced.

Past the onset on router v1.0.0, `--router-wait-in-prefill` takes the
router's wait as part of the prefill time, so the figure stands and a rate
answer can pass the onset (up to 0.9 of the prefill knee). It is on by
default. On a router v1.0.0 staircase (Qwen3-32B, 2 prefill and 2 decode,
three seeds), the figure read 1.3x to 3.4x the router's measured TTFT at 15
steps past the onset, never under, and no step up to 0.9 of the knee
collapsed. One more step, disturbed when both prefill pods were replaced, read
0.78x at the p95; the backtest excludes it as warm-up, a choice made after the
run. The figure errs high: it overstates the wait rather than claiming an SLO
the fleet misses.

**On router v0.11.0, set `--router-wait-in-prefill=false`** (the API's
`assume.routerWait: none`). There the router's gate collapsed on its TTL at 15
of 41 steps past the onset, and this figure reads short at every one of them.
Off, a figure past the onset is flagged `AboveFlowControlOnset`, a miss, and
the rate answer stops at the onset. The planner cannot read the router's
version, so this is the operator's call.

To ask the CLI the way the run was priced, for example:

```bash
capacity-planner replicas-to-slo --recording run.jsonl.gz --prefill 2 --decode 2 \
  --assume-input-tokens 8000 --assume-output-tokens 200 --ttft-percentile 90 \
  --assume-prompt-cv 0.0577 --arrival-cv2 1 --flow-control off
```

## Never validated

These always read `OUTSIDE VALIDATED REGION`:

- Aggregated variants (one Pod does prefill and decode).
- Prefill's own latency. The client times TTFT end to end only.
- TPOT percentiles, and decode's rate at a TPOT target.
- A rate the operating-point cap set. Read the knee's label instead.
- A knee with flow control on or not known. Read the onset's label instead.
- Any figure priced with a flag that types a term in.
- Any prefill or TTFT figure priced with `--experimental-ttft`
  (`ExperimentalTTFT`) or from the occupancy plateau (`VPFromPlateau`).
- p99, and any statistic but the mean, p90 and p95.
- Any model but Qwen3-32B, any tensor parallelism but 2, and any Pod count
  but 2 prefill and 2 decode.
- Any accelerator but H200.
- Any TTFT from `backtest --experimental-ttft=v2` (below).
- Any TPOT percentile from `backtest --experimental-tpot=v2` (below).

## Experimental: the rank-mixture TTFT model

`capacity-planner backtest --experimental-ttft=v2` prices TTFT with a second
model, for a P/D fleet whose prefill engines run chunked prefill on several
data-parallel ranks. It is off by default, and every TTFT it prices is flagged
`RankMixtureTTFT` and reads `OUTSIDE VALIDATED REGION`. TPOT, the exclusions and
the scoring are the planner's own.

What it prices, per request: `TTFT = Z + q + P`.

| part | model | where it comes from | assumption, and the check on each step |
|---|---|---|---|
| `P`, prefill | limited processor sharing on the rank the request lands on, `K` = `--max-num-seqs` prompts at once (1 without chunked prefill). With probability `1 - rho^K` the request starts at once and takes the PS sojourn `S(x)/(1 - rho)`, spread by the planner's PS rule; otherwise it first waits an exponential admission wait. The mean is Zhang and Zwart's, `E[S]/(1 - rho) * ((1 - rho^K) + rho^K (1 + cs^2)/2)`. `S(x) = s0 + x/V`, `x` drawn from the computed-token histogram, which also gives `cs^2` | `s0` and `V` by inverting each rank's mean prefill time, `E[S] = E[T]/(1 + lambda E[T])`, then a line in its mean computed prompt. Where the prompts do not spread (a staircase at one length), the line is not identified: the service is taken proportional to the prompt (`serviceProportional`), right at the prompt it was read at | the approximation is a heavy-traffic one: a rank above `rho` 0.9 is flagged `LimitedPSHeavyTraffic`. Against a simulated rank at `cs^2` 4.9 its mean read within 8 % at `rho` 0.3 to 0.85, and plain PS 17 % to 46 % low from 0.6. The answer reports the share of requests that wait for admission and the busiest rank's tail index (Nair, Wierman and Zwart) |
| the ranks | each rank its own queue at its share of the traffic; the population is the mixture | the ranks' measured request shares, busiest first | the shares hold over the fit (`shareCv` in the answer) |
| `q`, KV wait | none with probability `1 - p_d`, else exponential with mean `m_d`; both lines in the ranks' utilization. Fitted, not derived | the prefill engines' queue-time histogram at 0.3 s and 1 s | priced inside the utilizations it was fitted over, else flagged `KVDeferralOutside` |
| `Z`, the rest | `a0 + s * beta * L`, `L` drawn from the prompt-length histogram | `beta`: the slopes of the engine's pre-queue overhead, the decode engine's first token and the router's remainder against the prompt. A slope is fitted only where its prompts spread (cv 0.03 or more); at one prompt length it is 0 and named in `betaUnidentified`. `s` is a calibration, fitted on the fit window's self-check folds so that their median p90 ratio is 1; p95 is not fitted. On a fit window under 12 h, `s` is 1, not fitted (`spreadDefaulted`); a fitted `s` under 1 is priced at 1 (`spreadFloored`) | `s` near 1 means `beta` holds the prompt's whole effect; its distance from 1 is what `beta` misses. Fitted `s` follows the fit window's own p90-over-mean, so it is not yet known to carry from one day to the next |
| each step | priced as if its rate held (pointwise stationary) | | the busiest rank's relaxation, `E[S]/(1 - rho)^2`, well under the step; else the step is not scored (`unsettled`). A step whose arrivals (the router's rate `A`) ran ahead of its completions (the engine rows' rate `D`) by `(A - D)/D` over 0.1 left a backlog adding over a tenth of the step to the wait: it is not stationary. The fit leaves it out (`fitStepsBacklogged`), and the model refuses it as a test step |

The model prices only what its inputs identify. It abstains on the whole backtest where:
- a prompt slope is not identified;
- the spread is not calibrated (fit window under 12 h);
- the spread is not identified: it, or one self-check fold's, ends at its upper bound 8, so no spread
  brings the fold's p90 ratio to 1;
- `a0` is negative. `a0` is refitted at each spread to hold the mean, so a spread that outgrows `beta`
  pushes it down; a negative one is that offset, not a time;
- the service line is not identified (one prompt length);
- the ranks' own rates or `--max-num-seqs` are not read;
- or the fit fails.

Every TTFT is then the planner's own, flagged `RankMixtureAbstained`, and `rankMixture.abstained` says why. On
the public staircases (about an hour each, at one prompt length) it abstains.

The answer's `rankMixture` block names every fitted figure, and each step's
`rankMixture` block its checks. The model needs a recording made by this version
or later. It reads each prefill rank's own request rate, prefill time, queue
time, TTFT and prompt from a LeaderWorkerSet group's engines
(`GroupEngines`). In an older recording, a row's ranks share its rate evenly,
and the answer says so.

## Experimental: the per-request TPOT distribution

`capacity-planner backtest --experimental-tpot=v2` prices the TPOT percentiles
from a model of each request's TPOT, in place of the ratio `R_q` read from the
histogram's bucket edges. It is off by default. Every percentile it prices is
flagged `TPOTDistribution` and reads `OUTSIDE VALIDATED REGION`. The mean TPOT
is the planner's own, and the distribution is placed around it.

Why a ratio read at a bucket edge runs wide: the router's TPOT buckets are
coarse (10 ms to 20 ms, then 20 ms to 40 ms). Where the measured p90 falls in
the 10 ms to 20 ms bucket, the edge reading prices it at 20 ms whatever the
spread inside is.

What it prices, per request: `log TPOT ~ Normal(log m - sigma^2/2, sigma^2)`,
with `m` the planner's mean and `sigma^2 = g^2 + C E[1/(L-1)]`.

| part | model | where it comes from |
|---|---|---|
| the server | a decode replica's data-parallel ranks step in lockstep. Below `--max-num-seqs` no sequence waits for a slot: the step time, not a queue, sets a token's time | the share that would wait is Erlang C at the fit window's highest mean running sequences per rank (`slotWait`). Over 1 % the model abstains |
| `g^2`, the replicas | the spread of the replicas' time per token, weighted by their request rates | each replica's step time over `1 +` the drafts it accepts a step |
| `C E[1/(L-1)]`, the request | with speculative decoding a step emits `1 + A` tokens. The steps a request of `L` tokens needs is a first passage of that renewal process. By the renewal central limit theorem its squared CV is about `Var(1+A)/(1+E A)` over `L - 1`: a long request sits near the mean, a short one spreads | `E[1/(L-1)]` from the decode pods' output-length histogram over the step. `C` is fitted by interval-censored likelihood over every bucket of the fit window's router TPOT histogram, floored at the renewal value for the engine's `num_speculative_tokens` (`renewal`) |

`C` is the only fitted figure, and no percentile is fitted, so p90 and p95 are
both held out. The fit window's two self-check folds (its first half, its first
three quarters) each fit `C` alone (`foldC`) and are scored on the steps after
their cut (`fold`).

Each step is also scored at the histogram's resolution: the largest gap between
the predicted and the measured share of requests under a bucket edge (`ks`), and
whether the predicted p90 and p95 lie in the bucket the measured ones fell in.
`shapeKs` is the same gap with the distribution placed at the measured mean, so
it shows the shape's miss without the mean's. A note under the answer gives
these figures beside the planner's own ratio.

The model abstains on the whole backtest where `--max-num-seqs` is not read,
where a slot queue would form, where under two hours of fit steps read the
router's TPOT histogram and the output lengths, or where `C` ends at its bound.
Every percentile is then the planner's own, flagged `TPOTDistributionAbstained`.

## Adding a run

The regions are data, in `internal/signals/whatif/envelope.json`. A new run
adds rows with its own `source`. `TestStaircaseQ32_TheErrorsAreFrozen` writes
the q32 rows (`go test ./cmd/capacity-planner -run TestStaircaseQ32 -update`).
It prices every step of the run and writes the per-step table to
`cmd/capacity-planner/testdata/staircase-q32-validation.golden`.
[The proposal's "Calibration envelope, as built"](../../proposals/slo-capacity-planner.md#calibration-envelope-as-built)
explains how the rows are drawn.
