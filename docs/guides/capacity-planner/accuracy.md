# Capacity planner: how accurate each figure is

Every figure the [capacity planner](README.md) prints says how far off it
read when it was checked against a real run, or that it was never checked
for a fleet like yours.

## Read the label

Each figure carries one of two labels.

- **`validated: error -4..+18% (q32 staircase)`.** Your fleet and traffic sit
  inside a region a measured run covered. On that run, the planner's figure
  was between 4% under and 18% over what the client measured, for 8 in 10 of
  its readings. The name in brackets is the run.
- **`OUTSIDE VALIDATED REGION (4 prefill pods, measured 2)`.** No measured
  run covered this. The bracket says the first thing that differs. The figure
  may still be right; nobody has checked it.

The error is the planner's figure over the measured one, less 1.

| Figure | `+` means | Safe side |
| --- | --- | --- |
| TTFT, TPOT | the planner reads slower than measured | `+` |
| Knee, rate at a target | the planner promises more than the fleet held | `-` |

`at most -6%` means the run only bounded the figure: the fleet held at least
6% more than the planner said, and the run did not go far enough to say how
much more.

Where to find it:

- **CLI text:** after each figure, where `UNVALIDATED` used to be for TTFT.
- **CLI JSON (`-o json`):** `validation` on each answer: `validated`,
  `label`, `errorLo` and `errorHi` (shares, not percent), `readings`,
  `outside`.
- **API (`POST /v1/plan`):** `validation` on the knees, the TTFT at the rate,
  the rate at the target, the TPOT's two points and the limits. Curve points
  carry none.

## What was measured

One run so far: Qwen3-32B at tensor parallelism 2 on H200, 2 prefill and 2
decode Pods, flow control off and on, a staircase of rates up to the knee.
Each step sent one shape for 10 minutes; the client timed every request.

| Figure | 1000 tokens in, 1000 out, Poisson | 8000 in, 200 out, Poisson | 8000 in, 200 out, bursty (Ca² 4) |
| --- | --- | --- | --- |
| Mean TTFT | -4..+18% | +29..+95% | +30..+93% |
| TTFT p90 | +0..+14% | +18..+108% | +45..+131% |
| TTFT p95 | -3..+12% | +18..+102% | +44..+145% |
| Mean TPOT | +5..+18% | +36..+45% | +34..+43% |
| Prefill knee, flow control on | not crossed | +5..+11% | +21..+25% |
| Rate at a TTFT p90 or p95 target | at most -6% | at most -6% | at most +0% (p90), at most +2% (p95) |

In short: on short prompts the planner is close and a little slow. On long
prompts it reads TTFT and TPOT well above what the client saw, so a fleet it
sizes holds the target with room to spare. With flow control on, the fleet
stopped holding 5% to 25% before the knee the planner names.

A figure counts as inside only if all of these hold:

- P/D, 2 prefill and 2 decode Pods (mean TPOT: 2 decode Pods, any prefill).
- Accelerator H200.
- Prompt and output lengths within 1.25x of a measured shape, and the same
  arrival burstiness.
- Mean, p90 or p95. A p99 is outside.
- A rate up to the highest step measured: 0.90 of the knee for 1000 and 8000
  Poisson, 0.85 for bursty.
- Flow control below its onset. A figure flagged `AboveFlowControlOnset` is
  outside: the router's wait there is not priced.
- V_P and a learned. A figure on an assumed V_P or a is outside.

## Never validated

These always read `OUTSIDE VALIDATED REGION`:

- Aggregated variants (one Pod does prefill and decode).
- Prefill's own latency. The client times TTFT end to end only.
- TPOT percentiles, decode's knee, and decode's rate at a TPOT target.
- p99, and any statistic but the mean, p90 and p95.
- More than 2 prefill Pods, or any count but 2.
- Any accelerator but H200.

## Adding a run

The regions are data, in `internal/signals/whatif/envelope.json`. A new run
adds rows with its own `source`. The q32 rows are written by
`TestStaircaseQ32_TheErrorsAreFrozen` (`go test ./cmd/capacity-planner -run
TestStaircaseQ32 -update`), which prices every step of the run and writes the
per-step table to `cmd/capacity-planner/testdata/staircase-q32-validation.golden`.
How the rows are drawn:
[the proposal's "Calibration envelope, as built"](../../proposals/slo-capacity-planner.md#calibration-envelope-as-built).
