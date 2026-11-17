"""Replay one staircase step's arrivals through a model of the EPP's
flow-control gate and the chunked-prefill engines, with the gate on and off.

This is the evidence for the mechanism the capacity planner's router stage
prices (internal/signals/whatif/ttft_gate.go; docs/proposals/
slo-capacity-planner-ttft.md, "As built: the router stage"). It checks the
mechanism, not the size of the effect. The result swings with the pods'
capacity: on the Qwen3-32B staircase, moving V by 9 % either way flips the
step at 3.24 r/s from no wait to a collapse.

Usage, on a staircase run directory (the layout hack/benchmark/staircase.sh
writes: phases/<phase>/sweep.log and phases/<phase>/requests.csv.gz):

    python3 hack/benchmark/replay_fc_gate.py <run dir> <phase> <step> [key=value ...]
    python3 hack/benchmark/replay_fc_gate.py ~/stair-q32j C-on-r1 4
    python3 hack/benchmark/replay_fc_gate.py ~/stair-q32j C-on-r1 4 route=roundrobin V=18500

Keys, with their defaults (the Qwen3-32B staircase's fleet):
  V=17046        prefill tokens per second per pod (the learned V_P)
  s0=0.009       fixed time per engine step, seconds
  B=65536        engine token budget per step (--max-num-batched-tokens)
  Q=5 KV=0.8     the utilization detector's thresholds
  scrape=0.05    the EPP's metrics poll, seconds
  cycle=0.001    the dispatch cycle, seconds
  ttl=30         the flow-control request TTL, seconds
  decode=0.25    a constant added for the decode side, seconds
  pods=2         prefill pods
  route=score    score: the prefill scorers on the stale poll; roundrobin
  gauge=post     post: the waiting count a step leaves published when it
                 ends (arrivals during a step are not counted until the
                 next); end: everything waiting when the step ends

What it models (llm-d-router v0.11.0):
  - The gate dispatches one queued request each cycle unless the mean over
    the prefill pods of max(waiting/Q, kv/KV), read from the last poll, is
    at least 1. Within one poll the score does not move, so an open gate
    empties its queue. The decode stage's score is left out.
  - The engine's waiting count moves once a step.
  - Routing: queue-scorer (2) and kv-cache-utilization-scorer (2) on the
    same stale poll; the prefix-cache scorer ties on unshared prompts. The
    max-score picker breaks only exact ties, round-robin. So one poll's
    releases go to one pod.
  - Engine: chunked prefill. Running prompts first, then waiting ones,
    up to B tokens a step; the step takes s0 + tokens/V.
"""
import csv
import datetime
import gzip
import heapq
import re
import sys

D, PH, LV = sys.argv[1], sys.argv[2], sys.argv[3]
P = dict(V=17046.0, s0=0.009, B=65536, Q=5.0, KV=0.8, scrape=0.05, cycle=0.001, ttl=30.0,
         decode=0.25, pods=2, route="score", gauge="post")
for a in sys.argv[4:]:
    k, v = a.split("=", 1)
    if k not in P:
        sys.exit(f"unknown key {k}; keys: {', '.join(P)}")
    P[k] = v if isinstance(P[k], str) else type(P[k])(v)


def ts(s):
    return datetime.datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc).timestamp()


arr, meas = [], []
with gzip.open(f"{D}/phases/{PH}/requests.csv.gz", "rt") as f:
    for r in csv.DictReader(f):
        if r["level"] != LV:
            continue
        arr.append((float(r["sent"]), float(r["prompt_tokens"] or 0)))
        if r["status"] == "200" and r["first"]:
            meas.append(float(r["first"]) - float(r["sent"]))
if not arr:
    sys.exit(f"no requests at step {LV} of {PH}")
arr.sort()
t0 = arr[0][0]
arr = [(t - t0, i) for t, i in arr]
gaps = [b[0] - a[0] for a, b in zip(arr, arr[1:])]
mg = sum(gaps) / len(gaps)
ca2 = sum((g - mg) ** 2 for g in gaps) / len(gaps) / mg / mg


def pct(xs, q):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(q * len(xs)))] if xs else float("nan")


def sim(gate):
    n = P["pods"]
    waiting = [[] for _ in range(n)]  # [rid, tokens left]
    running = [[] for _ in range(n)]
    busy = [False] * n
    gauge = [0] * n  # the waiting count the engine last published
    seen = [0] * n  # the waiting count the router last polled
    rq = []  # the router's queue
    done, disp, evicted = {}, {}, set()
    ev = []
    seq = [0]
    rr = [0]

    def push(t, kind, data):
        seq[0] += 1
        heapq.heappush(ev, (t, seq[0], kind, data))

    for rid, (t, _) in enumerate(arr):
        push(t, "arr", rid)
    t, end = 0.0, arr[-1][0] + 600
    while t < end:
        push(t, "poll", None)
        t += P["scrape"]

    def pick():
        rr[0] += 1
        if P["route"] == "roundrobin":
            return rr[0] % n
        lo, hi = min(seen), max(seen)
        scores = [1.0 if hi == lo else (hi - w) / (hi - lo) for w in seen]
        best = [i for i in range(n) if scores[i] == max(scores)]
        return best[rr[0] % len(best)]

    def start_step(i, now):
        budget, tok, sched = P["B"], 0, []
        for r in running[i]:
            c = min(r[1], budget)
            if c <= 0:
                break
            sched.append((r, c))
            budget -= c
            tok += c
        while waiting[i] and budget > 0:
            r = waiting[i].pop(0)
            running[i].append(r)
            c = min(r[1], budget)
            sched.append((r, c))
            budget -= c
            tok += c
        busy[i] = bool(sched)
        if sched:
            push(now + P["s0"] + tok / P["V"], "step", (i, sched, len(waiting[i])))

    def dispatch(rid, now):
        i = pick()
        disp[rid] = now
        waiting[i].append([rid, arr[rid][1]])
        if not busy[i]:
            start_step(i, now)

    def try_gate(now):
        k = 0
        while rq:
            if gate and sum(max(w / P["Q"], 0) for w in seen) / n >= 1:
                return
            dispatch(rq.pop(0), now + k * P["cycle"])
            k += 1

    while ev:
        now, _, kind, data = heapq.heappop(ev)
        if kind == "arr":
            rq.append(data)
            try_gate(now)
        elif kind == "poll":
            seen[:] = gauge
            if gate:
                expired = [rid for rid in rq if now - arr[rid][0] > P["ttl"]]
                evicted.update(expired)
                rq[:] = [rid for rid in rq if rid not in evicted]
            try_gate(now)
            if len(done) + len(evicted) == len(arr):
                break
        else:
            i, sched, pending = data
            for r, c in sched:
                r[1] -= c
            for r in running[i]:
                if r[1] <= 0:
                    done[r[0]] = now
            running[i] = [r for r in running[i] if r[1] > 0]
            gauge[i] = pending if P["gauge"] == "post" else len(waiting[i])
            start_step(i, now)
    ttft = [done[r] - arr[r][0] + P["decode"] for r in done]
    wait = [disp[r] - arr[r][0] for r in disp]
    return ttft, wait, len(evicted)


print(f"{PH} step {LV}: {len(arr)} requests, {len(arr) / arr[-1][0]:.3f} r/s, Ca2 sent {ca2:.2f}; {P}")
if meas:
    print(f"  measured client: mean {sum(meas) / len(meas):.2f} p90 {pct(meas, .9):.2f} p95 {pct(meas, .95):.2f}")
for gate in (False, True):
    x, w, e = sim(gate)
    cdf = " ".join(f"{le}:{sum(1 for v in w if v <= le) / len(w):.3f}" for le in (0.01, 0.1, 1, 2.5, 5, 10, 30))
    print(f"  gate {'on ' if gate else 'off'}: mean {sum(x) / len(x):.2f} p90 {pct(x, .9):.2f} p95 {pct(x, .95):.2f}"
          f"  router wait mean {sum(w) / len(w):.3f} p95 {pct(w, .95):.2f}  evicted {e}  wait cdf {cdf}")
