# Read the planner's answer

This page walks one real answer, the one the [quick start](README.md) asks
for, part by part:

```bash
bin/capacity-planner at-replicas --recording recordings/ttft-staircase-b-on.jsonl.gz
```

The sample is 72 minutes of a test fleet of two prefill and two decode
replicas, under load that rises in steps. Every output below was printed by
the planner at v0.12.3 on that sample; only the annotations are added. The
page's **Judge the fleet** answers with the same report, in panels.

## The answer, from the top

### 1. The self-check: predicted against measured

```text
Self-check: tested on the last 36 min of this recording, each part predicted from a fit on what came before it, mean TPOT within 20% in 6 of 6 steps at 2 load levels (median ratio 0.92). It tested 35.76 to 41.55 r/s at 2 prefill + 2 decode: an answer at another rate or fleet is not covered by it. [weak]
  why: few or loose agreements: mean TPOT: it leans low (median ratio 0.92, under 0.95); within 20% in 100% of 6 steps; 2 independent load levels, fewer than 4
```

Before it answers, the planner tests itself on your recording. It learns
from the first part, predicts the TTFT and TPOT of the rest from its
traffic and replica counts alone, and compares each five-minute step with
what the fleet measured.

- `within 20% in 6 of 6 steps`: every prediction landed within 20% of the
  measured TPOT.
- `median ratio 0.92`: predictions read 8% under measured, on the middle
  step. Above 1 reads slow (the safe side for a latency), under 1 fast.
- TTFT is missing: this sample learned no prefill speed, so the planner
  predicts no TTFT for it.
- `It tested 35.76 to 41.55 r/s at 2 prefill + 2 decode`: the check covers
  only those rates and that fleet.
- `[weak]`: the grade. See [Trust levels](#trust-levels-ok-weak-inconsistent).

### 2. The fleet and the load

```text
ns-01/model-02  P/D 2 prefill (variant-02) + 2 decode (variant-01)  window 2026-11-06 02:18-03:30Z, cycle 289 of 289
decode engine   vLLM release not recorded
prefill engine  vLLM release not recorded
load judged     41.70 r/s, the p95 of the 282 of 289 cycles with traffic
load seen       per cycle (1-minute rates): mean 31.59, p95 41.70, peak 43.24 r/s; p95 less counting noise 41.66; busiest 2m0s (8 cycles): 42.13 r/s
SLO             TTFT p95 190.0 ms (policy)
```

- The model, its fleet, and the window the recording covers.
- `load judged`: the fleet is judged at the p95 of the request rate, its
  busy spells, not at the mean. The mean is 31.6 r/s and the peak 43.2.
- `SLO`: the targets, here the policy's. `--ttft 2s@p90 --tpot 30ms` sets
  your own.

### 3. The summary and the verdict

```text
summary     50%              80%              100%            seen
  measured  -/3.2m @18.49 ?  -/6.8m @29.59 ?  -/- @36.98 SAT  -/- @41.70 SAT
verdicts
  measured  UNDER-PROVISIONED (decode only): at the p95 load of 41.7 r/s, 1.1x the fleet knee of 37.0 r/s, the fleet saturates; needs at least 3 decode (has 2), 6 decode if the decode knee reads 99% high.
            self-check: not covered by the self-check: it tested 35.76 to 41.55 r/s at 2 prefill + 2 decode; this verdict is at 41.70 r/s at 2 prefill + 2 decode.
```

- **Summary.** One column per load: 50%, 80% and 100% of the knee, and the
  load seen. A cell is TTFT / TPOT in ms at that rate, then a mark: `ok`
  every target met, `MISS` missed, `SAT` saturated, `?` not judged. `-`
  is a figure the planner refused (no prefill speed, so no TTFT); `m` marks
  a TPOT mean where the percentile is refused.
- **Verdict.** One sentence: what the fleet is, why, and the fleet it
  needs. Here: the busy load, 41.7 r/s, is 1.1 times the knee, so the fleet
  saturates. [Verdicts](#verdicts-under-over-right-sized-not-judged) lists
  the four words.
- `needs at least 3 decode (has 2)`: see ["At least"](#at-least-and-lower-bounds).
- `6 decode if the decode knee reads 99% high`: the same need if the knee
  is as far off as the worst error measured outside the validated region
  ([below](#outside-validated-region)).
- `not covered by the self-check`: the verdict's rate, 41.70 r/s, is just
  past what the self-check tested (up to 41.55).

### 4. The shape and how busy each role is

```text
shape measured: IL 8003 OL 200, prefix hit 0.00 (measured, cycles 269-288)
  the load's cycles (the p95 and above: 15, cycles 247-283): IL 8001 OL 200, prefix hit 0.00
utilization at the load judged
  decode        KV fill saturated (band top 0.8), slots saturated of 222, wait p saturated
  prefill       utilization pooled no prefill speed learned, worst pod no prefill speed learned
```

- `IL 8003 OL 200`: the request shape priced, 8003 prompt tokens in and 200
  out, read from the last cycles. The busy cycles ran the same shape. When
  they differ by more than 20%, the answer says `SHAPE/LOAD MISMATCH`.
- Decode at the load: its KV cache, its batch slots and the chance a
  request waits are all past saturation. Prefill is not priced.

### 5. The knee: how much the fleet carries

```text
capacity            req/s     out tok/s   bound by
  prefill knee      no prefill speed learned            P1 refused V_P of variant-02: no prefill speed learned; pass --assume-vp for a flagged answer
  decode knee       36.98     7396        decode band top at fill 0.798
                    OUTSIDE VALIDATED REGION (arrivals not measured (Poisson assumed), measured at Ca^2 0.8 to 1.25)
  fleet knee        36.98     7396        decode (prefill not priced)
  operating (0.85)  31.44     6287        0.85 x fleet knee (scaleUpThreshold)
  max at SLO        no prefill speed learned            no fleet rate: the prefill side is not priced, and decode alone does not bound the fleet (slo-to-rate gives none either): P1 refused V_P of variant-02: no prefill speed learned; pass --assume-vp for a flagged answer
```

The **knee** is the rate at which a role saturates: past it, queues grow
without bound and latency with them. Each role has one; the fleet's is the
lower of the two, and `bound by` says what sets it. Here decode saturates
at 36.98 r/s, when its KV cache fills to 0.8.

- `operating (0.85)`: the operating point, the knee times the policy's
  headroom (its `scaleUpThreshold`). A fleet run above it has no room for a
  burst.
- `max at SLO`: the highest rate that still meets every target. It needs
  TTFT, so it is refused here.
- `prefill knee ... refused`: this sample has no measurement of prefill
  speed (V_P), so prefill is not priced. `--assume-vp` states one, and
  every figure on it is flagged as assumed.

### 6. The SLO against load, and the checks

```text
SLO vs load         r/s    TTFT p50/90 ms            TPOT mean/p90 ms           e2e mean (sum)
  50% of knee       18.49  no prefill speed learned  3.15/TPOT bucket too wide  no prefill speed learned
  80% of knee       29.59  no prefill speed learned  6.81/TPOT bucket too wide  no prefill speed learned
  100% of knee      36.98  no prefill speed learned  saturated                  no prefill speed learned
  load seen (113%)  41.70  no prefill speed learned  saturated                  no prefill speed learned
checks
  2 prefill + 2 decode  does not hold: 41.70 r/s is 1.1x the fleet knee of 36.98 r/s; prefill not priced (no prefill speed learned); TTFT p95 not judged (no prefill speed learned)
```

The latency at each load, and each fleet the planner tried. `TPOT bucket
too wide`: the router's histogram buckets are too coarse to read a p90,
so only the mean is given.

### 7. Flags and what is not validated

```text
flags: DecodeOnly; ExtrapolatedFill; BelowFittedBand; CoarseTPOTBucket; HostBoundTPOTTail

not validated:
  - decode knee: outside its validated region (see its label); inside the region the q32 staircase measured (1000-token prompts, 1000 out, Ca^2 1) it read at most +3%, and outside it, on the 0.6B goldens, 43% to 99% high (docs/proposals/slo-capacity-planner.md, Measured on the goldens)
  - e2e mean is a sum, TTFT mean + (OL - 1) x TPOT mean, not a percentile
```

Every flag names something an answer rests on that was assumed, stretched
or read loosely. [Troubleshooting](troubleshooting.md#refused-figures) and
the page's **Flags** drawer say what each means and what clears it. `not
validated` lists the figures no measured run has checked.

The command exits 0: it answered. It exits 3 when a verdict is NOT JUDGED,
and 4 when the self-check found a verdict's figures inconsistent.

## Verdicts: UNDER, OVER, RIGHT-SIZED, NOT JUDGED

The same sample, with a prefill speed and targets stated, at three fleets.
`--assume-vp 200000 --assume-a 20ms` states prefill's speed and fixed
latency; `--ttft 2s@p90 --tpot 30ms` sets targets the sample can meet:

```bash
bin/capacity-planner at-replicas --recording recordings/ttft-staircase-b-on.jsonl.gz \
  --assume-vp 200000 --assume-a 20ms --ttft 2s@p90 --tpot 30ms --prefill 2 --decode 2
```

```text
  measured  UNDER-PROVISIONED: at the p95 load of 41.7 r/s, 1.1x the fleet knee of 37.0 r/s, the fleet saturates; needs 3 prefill + 3 decode (has 2 + 2), 6 decode if the decode knee reads 99% high; the same at f 1 to 1.5 (prefill f 1 assumed at 3 pods).
```

With `--prefill 3 --decode 3`:

```text
  measured  RIGHT-SIZED: at the p95 load of 41.7 r/s, the fleet holds every target with headroom; decode: no spare (11.6% under its operating point); prefill: no spare (34.5% under its operating point) (prefill f 1 assumed at 3 pods).
```

With `--prefill 4 --decode 4`:

```text
  measured  OVER-PROVISIONED: at the p95 load of 41.7 r/s, decode: one fewer holds even at the peak 43.2 r/s; prefill: one fewer holds even at the peak 43.2 r/s; needs 3 prefill + 3 decode (has 4 + 4), 6 decode if the decode knee reads 99% high; at f 1 needs 3 + 3, at f 1.5 needs 4 + 3 (decode knee outside its validated region; prefill f 1 assumed at 4 pods; prefill f 1 assumed at 3 pods).
```

And with no prefill speed: the first command, with `--decode 3`:

```text
  measured  NOT JUDGED: at the p95 load of 41.7 r/s, TTFT p95 cannot be priced (the recording learned no prefill speed; --assume-vp states one).
```

| Verdict | Means | What to do |
| --- | --- | --- |
| UNDER-PROVISIONED | The busy load runs past headroom times the knee, or misses a target. | Scale to the fleet it `needs`. Size each role's `maxReplicaCount` at least that high. |
| RIGHT-SIZED | Every target holds with headroom, and no role has a replica to spare. | Nothing. `no spare (11.6% under its operating point)` says how close each role is. |
| OVER-PROVISIONED | A role has a replica to spare: one fewer still holds every target at the busy load, and at the peak. | Remove the spare, or lower `minReplicaCount`. It never names a cut that fails at the peak. |
| NOT JUDGED | A figure the verdict needs is refused, the load and the shape priced are different traffic, or the window contradicts the model. The sentence says which. | Do what it names: `--assume-vp` for a missing prefill speed, `--shape load` for a shape mismatch, or a longer recording. |

The first three verdicts above rest on the stated prefill speed, which this
sample's self-check finds wrong for TTFT: each also prints `self-check: not
trusted` under it, and the command exits 4
([trust levels](#trust-levels-ok-weak-inconsistent)).

`f` is how unevenly the router spreads prompts over the prefill pods; with
no run to learn it at a pod count, the planner gives the need at both ends
of the range. [Check a fleet at N replicas](page-and-commands.md#check-a-fleet-at-n-replicas)
gives every rule.

## "At least" and lower bounds

`needs at least 3 decode (has 2)` and `(decode only)` mean prefill was not
priced, so the verdict rests on decode alone. The fleet needs at least
that; prefill could need more. Such a verdict is never RIGHT-SIZED or
OVER-PROVISIONED, which is why the same sample at `--decode 3` is NOT
JUDGED. To get a full answer, state a prefill speed with `--assume-vp`, or
record a window where prefill ran busy enough to learn it.

The day plan writes the same as `>=` in its tables. A prefill speed learned
on a fleet that never went quiet is itself a lower bound, and every TTFT
answer on it says so ([when a term is refused](page-and-commands.md#when-a-term-is-refused)).

## Trust levels: ok, weak, inconsistent

The self-check's grade, in brackets at the end of its line and as a badge
on the page:

| Grade | Means | What to do |
| --- | --- | --- |
| `ok` (held on its own steps) | 12 steps or more at 4 load levels or more, 70% within 20% of measured, a median ratio from 0.95 to 1.25. | Size from it, at the rates and fleet it tested. |
| `weak` | It held, but on fewer steps or load levels, fewer within 20%, or leaning low (median ratio 0.80 to 0.95). This sample: 6 steps at 2 levels, ratio 0.92. | Size with care. A longer recording with more load levels firms it up. |
| `inconsistent` | The planner and the recording disagree. The line says which figures not to trust. | Do not size from those figures. Record a longer window with busy minutes early, and check again. |
| `notRun` | The recording is under an hour, or holds too few steps to score. | No evidence either way. |

On the sample with a stated prefill speed, the check also scores TTFT, and
the stated speed does not match this fleet:

```text
Self-check, replayed with the stated --assume-vp 200000, --assume-a 20ms (flagged as stated, outside the validated region): tested on the last 36 min of this recording, each part predicted from a fit on what came before it, p90 TTFT within 20% in 0 of 6 steps at 2 load levels (median ratio 3.79); mean TPOT within 20% in 6 of 6 steps at 2 load levels (median ratio 0.92). It tested 35.76 to 41.55 r/s at 2 prefill + 2 decode: an answer at another rate or fleet is not covered by it. [inconsistent]
  why: the planner and this recording disagree: p90 TTFT: median ratio 3.79, want 0.80 to 1.25; within 20% in 0% of the steps, want 50% or more; at 0.8 to 1.0 of the knee the worst tenth of 6 steps read 12.13 of measured, want 0.67 to 2.00
  Do not trust prefill sizing and the TTFT figures from this recording; decode sizing and the TPOT figures (loosely) held.
```

Every verdict that rests on the TTFT figures then carries:

```text
            self-check: not trusted: the self-check found this recording's TTFT figures inconsistent with it, and this verdict rests on them.
```

and the command exits 4. The verdict is still printed. An inconsistent
answer also prints a link to file an issue with its diagnostics, which
hold no names and no recording. [The self-check](check.md#the-self-check-beside-every-answer)
has every rule; [Check the planner](check.md) runs a longer check yourself.

## OUTSIDE VALIDATED REGION

```text
  decode knee       36.98     7396        decode band top at fill 0.798
                    OUTSIDE VALIDATED REGION (arrivals not measured (Poisson assumed), measured at Ca^2 0.8 to 1.25)
```

Each figure says whether a measured run covered conditions like yours.
Inside one, it carries `validated in-sample: error ...` with the error
measured there. Outside, it says `OUTSIDE VALIDATED REGION` and, in
brackets, the first condition that differs: here, the recording did not
measure how bursty arrivals were, so Poisson arrivals are assumed.

It does not mean the figure is wrong. It means no run has checked it. The
`not validated` lines give the error seen elsewhere (the decode knee read
43% to 99% high outside the region), and the verdict gives the need at that
error beside its point answer (`6 decode if the decode knee reads 99%
high`). Size between the two by how much a miss costs you.
[How accurate each figure is](accuracy.md) has what was measured.
