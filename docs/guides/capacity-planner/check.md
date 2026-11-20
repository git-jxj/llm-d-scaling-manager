# Check the planner against a period it did not see

Run this before you trust the planner with your fleet. It fits the planner on
one part of your own recording, predicts the TTFT and TPOT of the rest from
that part's traffic and fleet alone, and sets each prediction beside what your
fleet measured.

## What you need

- [ ] The planner, as in [Get the planner](README.md#get-the-planner).
- [ ] A recording of a few hours of your fleet, from
  [Record a window](../record-for-capacity-planner/). Longer is better: the
  part the planner is fitted on must hold busy minutes, or it learns nothing
  to predict from. The window must hold traffic you care about, at the
  replica counts you run.

## Run it

One command, on one recording:

```bash
capacity-planner backtest --recording run.jsonl.gz --split 60%
```

It fits on the first 60% of the recording and tests on the last 40%. To cut
at a time instead, use `--fit-until 2026-11-20T12:00:00Z`. To fit on one
recording and test on another of the same configuration, use
`--fit day1.jsonl.gz --test day2.jsonl.gz`.

On the page: load the recording in Source, open **Check the planner**, set the
slider, and press **Run**. To test on another recording, pick **another
recording** and choose it.

## Read the answer

The first line is the answer:

```text
Over the 2 h it did not see, the planner's p90 TTFT was within 20% of what the fleet measured in 73% of the
15 5-minute steps it priced (median ratio 1.05, on target), and gave no figure for 7 it judged past the knee; ...
Judged 22 of 23 5-minute steps; not judged: 1 warm-up.
```

- **Within 20%**: the share of the steps the planner priced where its figure
  was within 20% of the measured one. It counts only the steps it priced:
  read it with the count beside it.
- **Median ratio**: the planner's figure over the measured one, at the middle
  step. Above 1 it reads high, on the safe side. Below 1 it reads low, the
  side that can cost an SLO.
- **Gave no figure past the knee**: steps whose rate was at or past what the
  planner thinks the fleet can carry. It answers "saturated" there, not a
  number. If the fleet served those minutes well, the planner's knee is too
  low for your fleet.
- **Not judged**: the first minutes of the test, and minutes near a change of
  replicas, are left out. So are minutes with no traffic.

Below it, the table gives each step, then the score for the mean, p50, p90
and p95 of TTFT and TPOT. The score is given over every judged step, then
over the steps inside the planner's validated region, then by load: below
0.8 of the knee, 0.8 to 1.0, and past it. The chart on the page shows the
same steps over time: the predicted line, the measured dots, and the
not-judged minutes in grey.

What it predicts from: each step's request rate, its prompt and output
lengths, its prompt spread, and the replicas each role ran. It never reads
the test's latency to predict. It assumes Poisson arrivals unless you give
`--arrival-cv2`. What it measures against: the router's TTFT and TPOT
histograms in the same recording, else the engines' own (labelled; on a P/D
fleet the engines' TTFT leaves out the router and the KV transfer).

## If the result is bad

- **No figure in any step** (`refused: NoPrefillCapacity`, `PinnedDefaultB`,
  `NoFixedLatency`): the fitted part did not hold the minutes the planner
  learns from. Fit on more: a larger `--split`, or a longer recording with
  busy minutes. [Troubleshooting](troubleshooting.md) has each refusal.
- **Refused: the test ran another configuration**: the two parts differ in a
  field the recording carries, such as the accelerator, the parallelism, an
  engine flag or the flow-control series. A score across configurations
  measures the change, not the planner. Test on a window of the same
  configuration. `--allow-config-mismatch` prices it anyway and prints the
  difference as a caveat.
- **Reads high near the knee**: expected. The planner's tail rises faster
  than most fleets' near saturation, so near the knee it errs on the safe
  side. Judge it by the steps below 0.8 of the knee.
- **Reads low**: tell us, with the recording. That is the side that costs an
  SLO.

## Reference

| Flag | Default | Notes |
| --- | --- | --- |
| `--recording` with `--split` or `--fit-until` | `--split 60%` | One recording, cut in two. |
| `--fit`, `--test` | | Two recordings. |
| `--step` | `5m` | A step, from `1m` to `1h`. |
| `--settle` | `2m` | How long after a change of replicas, and after the test's start, a step is not judged. |
| `--arrival-cv2` | Poisson | The test's arrivals' Ca², when you know it. |
| `--ttft-target` | | Judge each step against a target too: `500ms@p90`. The summary counts the steps predicted to meet it that measured a miss. |
| `--measured` | `auto` | `router`, `engine`, or `auto` (the router's where recorded). |
| `--assume-vp`, `--assume-a` | | Terms the fitted part did not learn, flagged. |
| `--allow-config-mismatch` | off | Price a test of another configuration, with the difference as a caveat. |
| `-o json` | text | The same answer as `POST /v1/backtest` ([the API](api.md#the-backtest)). |

The command exits 0 when it judged at least one step, 3 when it judged none,
and 2 on a usage error or a refused configuration.
