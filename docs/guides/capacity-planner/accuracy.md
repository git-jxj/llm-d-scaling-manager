# Capacity planner: how accurate each figure is

Every figure the [capacity planner](README.md) prints says whether a real run
checked it, and how far off it read there. **Validated means measured, not
accurate: read the range.** A validated figure can be twice the real value.

## Read the label

Each figure carries one of two labels.

- **`validated: error +18..+108% (p10-p90, min-max +15..+156%, n=12; q32 staircase)`.**
  Your fleet and traffic sit inside what a measured run covered. On that
  run the planner's figure read 18% to 108% over what the client measured
  for 8 in 10 of its 12 readings, and 15% to 156% over at the extremes.
- **`OUTSIDE VALIDATED REGION (prompt spread not measured (cv 2 assumed), ...)`.**
  No run checked a figure like this one. The bracket names the first thing
  that differs. The figure may still be right; nobody has measured it.

The label always gives the count (`n=`) and what kind of range it is:

| Range | Means |
| --- | --- |
| `p10-p90` with `min-max` | 5 or more readings: the middle 8 in 10, then the extremes |
| `min-max` | fewer than 5 readings: the lowest and the highest |
| `single reading, between two ladder steps` | one measurement, known only to lie between two steps of the run |
| `one-sided ... at most -6%` | the run never reached the point: the planner read at least 6% low, and how much lower is unknown |

The error is the planner's figure over the measured one, less 1.

| Figure | `+` means | Safe side |
| --- | --- | --- |
| TTFT, TPOT | the planner reads slower than measured | `+` |
| Knee, onset, rate at a target | the planner promises more than the fleet held | `-` |

Where to find it:

- **CLI text:** after each figure, one label a line.
- **CLI JSON (`-o json`):** `validation` on each answer. It holds
  `validated`, `label`, `errorLo` and `errorHi` (shares, not percent),
  `readings` and `outside`. A refused figure, and the operating rate, which
  is an input, have none.
- **API (`POST /v1/plan`):** `validation` on the knees, the TTFT at the rate,
  the rate at the target, the TPOT's two points and the limits. Curve points
  and the marginal steps carry none.

## What was measured

One run so far: Qwen3-32B at tensor parallelism 2 on H200, 2 prefill and 2
decode Pods. It climbed a ladder of 6 load levels, from 0.40 to 0.90 of the
knee, each judged over 7 to 22 minutes. Each level ran twice, once with the router's
flow control off and once on. The client timed every request.

| Figure | 1000 tokens in, 1000 out, Poisson | 8000 in, 200 out, Poisson | 8000 in, 200 out, bursty (Ca² 4) |
| --- | --- | --- | --- |
| Mean TTFT | -4..+18% (n=12) | +29..+95% (n=12) | +30..+93% (n=9) |
| TTFT p90 | +0..+14% (n=12) | +18..+108% (n=12) | +45..+131% (n=9) |
| TTFT p95 | -3..+12% (n=12) | +18..+102% (n=12) | +44..+145% (n=9) |
| Mean TPOT | +5..+18% (n=12) | +36..+45% (n=12) | +34..+43% (n=10) |
| Knee, flow control off | at most +0% (n=1) | at most +0% (n=1) | at most +0% (n=1) |
| Flow-control onset, on | not reached | -4..+2% (n=1) | -7..-4% (n=1) |
| Rate at a TTFT p90 or p95 target | at most -6% (n=1) | at most -6% (n=1) | at most +0% (p90), at most +2% (p95) (n=2) |

The TTFT and TPOT ranges are p10-p90. The full extremes are in each label.

What this says:

- **Short prompts:** the planner is close. TTFT reads up to about 18% slow.
- **Long prompts:** TTFT and TPOT read 20% to 150% slow, and more so near the
  knee. Sizing for TTFT therefore errs on the side of more Pods than needed.
- **Rates and knees:** the fleet held every rate and knee the planner named.
  The run stopped at the knee, so how much more it could have held is not
  known.
- **Flow control on:** the router started holding requests near the onset the
  planner names, a few percent before it. For bursty traffic it was 4% to 7%
  before.

A figure counts as inside only if all of these hold:

- The prompts' length spread was measured or stated, and is close to the
  run's (cv 0.046 to 0.072). The default, cv 2 with no histogram, is outside.
- The arrivals were measured or stated (`--arrival-cv2`), with Ca² within
  1.25x of the run's. Poisson assumed by default is outside.
- The router's flow-control state is known. Without its queue series, state
  it with `--flow-control on|off`.
- V_P (the prefill throughput) and a (the fixed prefill time) were learned
  from the recording, not typed in.
- 2 prefill and 2 decode Pods, on H200.
- Prompt and output lengths are within 1.25x of a measured shape.
- Statistic: the mean, p90 or p95.
- The load is within the levels the run measured:
  - TTFT: 0.40 to 0.90 of the knee (0.85 for bursty traffic).
  - Mean TPOT on long prompts: 0.10 to 0.21 of decode's knee.
  - A rate: 0.825 to 0.875 of the knee.
- Flow control below its onset. A figure flagged `AboveFlowControlOnset` is
  outside: the router's wait there is not priced.

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
- p99, and any statistic but the mean, p90 and p95.
- Any prefill count but 2.
- Any accelerator but H200.

## Adding a run

The regions are data, in `internal/signals/whatif/envelope.json`. A new run
adds rows with its own `source`. `TestStaircaseQ32_TheErrorsAreFrozen` writes
the q32 rows (`go test ./cmd/capacity-planner -run TestStaircaseQ32 -update`).
It prices every step of the run and writes the per-step table to
`cmd/capacity-planner/testdata/staircase-q32-validation.golden`.
[The proposal's "Calibration envelope, as built"](../../proposals/slo-capacity-planner.md#calibration-envelope-as-built)
explains how the rows are drawn.
