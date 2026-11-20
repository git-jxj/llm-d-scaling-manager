# Check the planner against a period it did not see

Run this before you trust the planner with your fleet. It learns from one
part of your own recording. It then predicts the TTFT and TPOT of the rest
from that period's traffic and replica counts only, never from its measured
latency, and compares each prediction with what your fleet measured.

## What you need

- [ ] The planner, as in [Get the planner](README.md#get-the-planner).
- [ ] A recording of a few hours of your fleet, from
  [Record a window](../record-for-capacity-planner/). Longer is better: the
  part the planner learns from must hold busy minutes, or it has nothing to
  learn. The recording must hold traffic you care about, at the replica
  counts you run.

## Run it

One command, on one recording:

```bash
capacity-planner backtest --recording run.jsonl.gz --split 60%
```

It learns from the first 60% of the recording and tests on the last 40%. To
cut at a time instead, use `--fit-until 2026-11-20T12:00:00Z`. To learn from
one recording and test on a later one of the same configuration, use
`--fit day1.jsonl.gz --test day2.jsonl.gz`.

On the page: load the recording in Source, open **Check the planner against
a period it did not see**, set the slider, and press **Run**. To test on
another recording, pick **another recording** and choose it.

## Read the answer

The first lines are the answer. For example:

```text
Over the 1 h 52 min the planner did not see:
p90 TTFT: within 20% of measured in 11 of the 15 five-minute steps scored (a figure, and enough measured requests);
it leans high (median ratio 1.09). For 7 more it gave no figure, calling the fleet overloaded; the fleet measured
p90 358 ms to 374 ms there.
p90 TPOT: within 20% of measured in 2 of the 15 five-minute steps scored (a figure, and enough measured requests);
its errors are wide; it leans high (median ratio 1.24); it read 5 more than 20% low. For 7 more it gave no figure, ...
None of these figures falls within the conditions our own accuracy benchmarks cover
(docs/guides/capacity-planner/accuracy.md): this check is the evidence for this fleet.
Judged 22 of 23 five-minute steps; not judged: 1 warm-up.
```

How to read it:

- **Within 20% in 11 of the 15 steps scored**: the count that matters. A
  step is scored when the planner gave a figure and the fleet measured
  enough requests to read the statistic (100 for a p90). The other steps,
  and why they were not scored, follow in the same line.
- **Median ratio**: the planner's figure over the measured one, at the
  middle step. Above 1 it leans high at the middle; below 1 it leans low. A
  median near 1 can still hide steps it read low, so check that count too.
- **Read more than 20% low**: the steps where it said the fleet was faster
  than it was. These are the costly ones.
- **Gave no figure, calling the fleet overloaded**: steps whose rate was at
  or past what the planner thinks the fleet can carry. If the fleet served
  those minutes well, the planner is too cautious for your fleet.
- **Within the conditions our benchmarks cover**: how many figures fall
  where our own accuracy runs measured the planner. For a real fleet this is
  often none, and then this check, not the benchmark, is your evidence.
- **Not judged**: steps whose measurement reaches into the first minutes of
  the test or over a change of replicas, and steps with no traffic.

Below that are each step and then the scores: the mean, p50, p90 and p95 of
TTFT and TPOT, then the same split by load. The page draws the same steps over
time: predicted lines, measured dots, steps it called overloaded marked in
red at the top, and steps not judged in grey.

The prediction uses each step's request rate, prompt and output lengths, the
spread of prompt lengths, and the replicas each role ran. Unless you give
`--arrival-cv2` (1 is random arrivals, higher is burstier), it assumes random
arrivals. It is compared with the router's TTFT and TPOT histograms in the
same recording, over the window its histogram covers (the router's rate
window, five minutes on a default router); the inputs are read over the
same window. Without
router histograms it uses the engines' own, and leaves out what they cannot
compare: on a P/D fleet the engines' TTFT covers prefill alone.

## Is it good enough?

Size from the planner when, below the load it thinks the fleet can carry:

- the p90 TTFT is within 20% in most of the steps it gave a figure for;
- the median ratio is between about 0.95 and 1.2 (leaning a little high
  costs replicas, leaning low costs the SLO);
- it read few steps more than 20% low;
- it gave no figure only where the fleet really was overloaded.

## If the result is bad

- **No figure in any step** (`refused: NoPrefillCapacity`, `PinnedDefaultB`,
  `NoFixedLatency`): the part it learned from did not hold the busy minutes
  it learns from. Give it more: a larger `--split`, or a longer recording.
  [Troubleshooting](troubleshooting.md) has each refusal.
- **Refused: the test ran another configuration**: the two parts differ in
  something the recording carries, such as the accelerator, the
  parallelism, an engine flag, the image or the flow-control series. A
  score across configurations measures the change, not the planner. Test on
  a period of the same configuration. `--allow-config-mismatch` scores it
  anyway and prints the difference.
- **Reads high near the knee**: expected. Close to the load it thinks the
  fleet can carry, the planner's tail rose faster than the fleets' on every
  run we measured, so it leans high there. Judge it by the steps below 0.8 of that load (the
  by-load lines).
- **Reads high everywhere**: check the `assumed:` lines. An assumed prompt
  spread or arrival pattern can move every figure.
- **Within 20% is low but the median ratio is near 1**: its errors are wide
  both ways. Do not size from single steps.
- **Reads low**: open an issue with the recording (or the output of
  `-o json`). That is the side that costs an SLO.

## Reference

| Flag | Default | Notes |
| --- | --- | --- |
| `--recording` with `--split` or `--fit-until` | `--split 60%` | One recording, cut in two. |
| `--fit`, `--test` | | Two recordings. They must not overlap in time. |
| `--step` | `5m` | A step, from `1m` to `1h`. A step shorter than the router's five-minute window shares requests with its neighbours. |
| `--settle` | `2m` | How long after a change of replicas, and after the test starts, a measurement is not judged. |
| `--arrival-cv2` | random | How bursty arrivals are, when you know it: 1 is random. |
| `--ttft-target` | | Judge each step against a target too: `500ms@p90`. The summary counts the steps predicted to meet it that measured a miss. |
| `--measured` | `auto` | `router`, `engine`, or `auto` (the router's where recorded). |
| `--assume-vp`, `--assume-a` | | A prefill pod's prompt tokens per second, and its fixed latency, where the part it learned from held none. Flagged. |
| `--allow-config-mismatch` | off | Score a test of another configuration, with the difference printed. |
| `-o json` | text | The same answer as `POST /v1/backtest` ([the API](api.md#the-backtest)). |

The command exits 0 when it judged at least one step, 3 when it judged none,
and 2 on a usage error or a refused configuration.
