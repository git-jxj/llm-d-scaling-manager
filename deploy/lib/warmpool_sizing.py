"""Answer "should this model be warmed on this cluster?" from the cluster's own nodes.

Every recommendation about warming a model is really four questions, and all
four are decided by cluster facts rather than by preference:

  1. Does the model fit one node?          -> if not, the engine spans Pods (LWS)
  2. Can host RAM hold a level-1 sleeper?  -> if not, there is no warm copy
  3. Can it hold MORE THAN ONE?            -> if not, a pool IS an idle replica,
                                              and an idle replica at least serves
  4. Is the cold start weight-bound or fixed-cost bound?
                                           -> decides whether faster storage,
                                              a warm process, or neither helps

The thresholds move with the hardware, which is why this reads the nodes instead
of quoting numbers from one fleet. The COEFFICIENTS below are measured, and are
the part to distrust first when a prediction disagrees with reality.
"""

import argparse
import json
import os
import re
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import accelerator_labels  # noqa: E402  (path set above; these run standalone)

# MUST MATCH internal/warmpool/pool/shape.go. The controller is what admits a
# model; a tool that is more optimistic than the admission logic sizes a Pod the
# controller then refuses to fill, which is worse than being wrong in the open.
#
# shape.go fits 2.6 GiB + 1.4x weights to 0.6B->4.1 GiB and 8B->23.4 GiB.
# An independent run on 2026-08-27 measured 0.6B->2.91 GiB and 8B->22.05 GiB,
# fitting 1.41 + 1.352. The SLOPES agree within 3.5%; the intercepts do not, and
# the difference is presumably engine configuration (KV budget, max-model-len)
# charged to the same cgroup. Both are two-point fits.
#
# Follow the controller's, which is the higher one. Over-estimating declines an
# admission that would have fitted and costs a cold start; under-estimating
# OOM-kills the Pod and takes every model resident in it with it.
SLEEPER_FIXED_GIB = 2.6
SLEEPER_FACTOR = 1.4

# Cold start to serving, cached image, 0.6B: ~100 s, of which the weight read was
# 0.56 s. So this is the part that does NOT scale with the model.
FIXED_STARTUP_S = 100.0

# vLLM's startup weight loader, ONE rank, page cache warm: "Loading weights took
# 1.11 seconds" for 1.4 GiB. This is the rate that matters for a cold start --
# reload_weights runs a little faster (1.5-2.7 GB/s) but is a different path.
#
# It is FIVE TIMES slower than a warm cached read (6.3 GB/s), so a single rank is
# bound by parse and host-to-device, not by storage. Ranks parse concurrently, so
# the fleet's aggregate is this x TP, and storage only binds above that.
PARSE_RATE_GBPS = 1.08
# How aggregate load scales with tensor parallelism. FITTED TO TWO POINTS
# (TP=1 and TP=2 on one 8B model); extrapolating it to TP=16 is the weakest
# number in this file. Linear would be 1.0.
PARSE_TP_EXPONENT = 0.34
# Host-to-device: what a level-1 wake actually costs. MEASURED, not assumed --
# Qwen3-8B woke in 0.42 s from 15.27 GiB, i.e. ~39 GB/s. An earlier guess of
# 20 GB/s made every published wake time about twice as pessimistic as reality.
PCIE_GBPS = 39.0

# A pool Pod can hold at most this many engines -- a port-range constant in the
# supervisor, not a tuning knob. Node RAM can imply far more than this, and the
# implication is wrong.
MAX_INSTANCES_PER_POD = 16

# Host RAM left to the system when serving workloads hold part of it.
SYSTEM_RAM_FRAC = 0.05

BYTES_PER_PARAM = {"bf16": 2.0, "fp16": 2.0, "fp8": 1.0, "int8": 1.0, "fp4": 0.5, "int4": 0.5}


def gpu_requests_by_node():
    """GPUs already requested per node, which allocatable does not tell you."""
    out = subprocess.run(
        ["kubectl", "get", "pods", "--all-namespaces", "--field-selector",
         "status.phase=Running", "-o", "json"],
        capture_output=True, text=True, encoding="utf-8",
        errors="replace", timeout=120)
    if out.returncode != 0:
        return None
    used = {}
    for pod in json.loads(out.stdout).get("items", []):
        node = (pod.get("spec") or {}).get("nodeName")
        if not node:
            continue
        n = 0
        for c in (pod["spec"].get("containers") or []):
            res = (c.get("resources") or {}).get("requests") or {}
            lim = (c.get("resources") or {}).get("limits") or {}
            g = lim.get("nvidia.com/gpu") or res.get("nvidia.com/gpu")
            if g:
                n += int(g)
        used[node] = used.get(node, 0) + n
    return used


def nodes_from_cluster(selector, count_gpus=True):
    cmd = ["kubectl", "get", "nodes", "-o", "json"]
    if selector:
        cmd += ["-l", selector]
    out = subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8",
                         errors="replace", timeout=60)
    if out.returncode != 0:
        raise SystemExit("could not read nodes: %s" % out.stderr.strip())
    nodes = []
    for n in json.loads(out.stdout).get("items", []):
        cap, alloc = n["status"]["capacity"], n["status"]["allocatable"]
        # CAPACITY, not allocatable: a node's SHAPE is how many devices it has,
        # and what is free right now is a scheduling question. Grouping by free
        # GPUs invents node types -- a box with 4 of 8 idle reads as a 4-GPU
        # machine and gets sized as one.
        gpus = cap.get("nvidia.com/gpu") or cap.get("amd.com/gpu")
        if not gpus or int(gpus) == 0:
            continue
        # ALLOCATABLE is capacity minus what the node cannot schedule -- it is
        # NOT capacity minus what is already requested. Reading it as "free"
        # overstates availability, sometimes badly: this fleet reported 4-7
        # allocatable per node while the scheduler answered "15 Insufficient
        # nvidia.com/gpu" for a 4-GPU pod. Real availability needs the Pods.
        free = alloc.get("nvidia.com/gpu") or alloc.get("amd.com/gpu") or "0"
        labels = n["metadata"].get("labels") or {}
        mib = accelerator_labels.per_gpu_mib(labels)
        nodes.append({
            "name": n["metadata"]["name"],
            "gpus": int(gpus),
            "free": int(free),
            "requested": 0,  # filled in below when the Pod list is readable
            "per_gpu_gib": (mib / 1024.0) if mib else None,
            "product": accelerator_labels.product_of(labels),
            "ram_gib": _quantity_gib(cap.get("memory", "0")),
            "alloc_ram_gib": _quantity_gib(alloc.get("memory") or cap.get("memory", "0")),
            "labels": labels,
            "disk_gib": _quantity_gib(cap.get("ephemeral-storage", "0")),
        })
    # Every Pod in the cluster: skipped where only RAM is asked (create's check),
    # which never reads it.
    used = gpu_requests_by_node() if count_gpus else None
    if used is not None:
        for n in nodes:
            n["requested"] = used.get(n["name"], 0)
            n["free"] = max(0, n["free"] - n["requested"])
    return nodes


_QUANTITY_SUFFIXES = (
    ("Ki", 1024.0), ("Mi", 1024.0 ** 2), ("Gi", 1024.0 ** 3), ("Ti", 1024.0 ** 4),
    ("Pi", 1024.0 ** 5), ("Ei", 1024.0 ** 6),
    ("k", 1e3), ("M", 1e6), ("G", 1e9), ("T", 1e12), ("P", 1e15), ("E", 1e18),
    ("m", 1e-3),
)


def _quantity_gib(q):
    """A Kubernetes memory quantity (binary, decimal or milli suffix, or bytes) in GiB.

    Raises ValueError on anything else; callers that read a cluster turn that
    into an unread note rather than a traceback.
    """
    q = str(q).strip()
    for suffix, mult in _QUANTITY_SUFFIXES:
        if q.endswith(suffix):
            return float(q[: -len(suffix)]) * mult / (1024.0 ** 3)
    return float(q) / (1024.0 ** 3)


# --- host RAM the serving engines already hold --------------------------------
#
# A level-1 sleeper lives in host RAM. So does a CPU KV-offload tier, usually in
# /dev/shm. On a node whose engines pin a large tier, the RAM a node "has" is
# not the RAM a pool can use, and a pool sized from capacity is Pending or,
# worse, evicted under memory pressure with every resident model in it.
#
# How each tier is sized, from the engines' own sources:
#   vLLM `cpu_bytes_to_use` (in --kv-transfer-config) and `--kv-offloading-size`
#   (GiB) are the total for the whole SERVER: the connector divides them by the
#   world size. Counted once per Pod; for an engine spanning Pods that is an
#   upper bound.
#   `cpu_bytes_to_use_per_rank` and LMCache's LMCACHE_MAX_LOCAL_CPU_SIZE (GiB)
#   are per rank: times the container's GPUs.

# A number in vLLM's human-readable form: k/m/g/t decimal, K/M/G/T binary.
# A shell variable with a default, ${X:-50}, reads as its default.
_NUM = (r"""([0-9][0-9.]*(?:[eE][+-]?[0-9]+)?[kKmMgGtT]?"""
        r"""|\$\{[A-Za-z_][A-Za-z0-9_]*:-[0-9][0-9.]*[kKmMgGtT]?\}"""
        r"""|\$(?:\{[A-Za-z_][A-Za-z0-9_]*\}|[A-Za-z_][A-Za-z0-9_]*))""")
_SHELL_DEFAULT_RE = re.compile(r"\$\{[A-Za-z_][A-Za-z0-9_]*:-([^}]*)\}")
# Inside --kv-transfer-config, in a plain arg or a shell string with escaped quotes.
_CPU_BYTES_RE = re.compile(r"""cpu_bytes_to_use(_per_rank)?[\\'"]*\s*:\s*[\\'"]*""" + _NUM)
# vLLM accepts both spellings of a flag.
_OFFLOAD_SIZE_RE = re.compile(r"""--kv[-_]offloading[-_]size[=\s]+['"]?""" + _NUM)
LMCACHE_CPU_ENV = "LMCACHE_MAX_LOCAL_CPU_SIZE"
# `X=50`, `X: 50`, a shell default `${X:-50}`, or the extra-config key
# `lmcache.max_local_cpu_size` / `max_local_cpu_size` in --kv-transfer-config.
_LMCACHE_RE = re.compile(r"""(?:""" + LMCACHE_CPU_ENV + r"""|max_local_cpu_size[\\'"]*)"""
                         r"""(?:=|:-|\s*:\s*)[\\'"]*""" + _NUM)
# LMCache with its local CPU backend switched off holds no CPU tier; a config
# file holds its size where this cannot read it.
_LMCACHE_CPU_OFF_RE = re.compile(r"""LMCACHE_LOCAL_CPU[=:]\s*['"]?(?:false|False|FALSE|0)\b""")
LMCACHE_CONFIG_ENV = "LMCACHE_CONFIG_FILE"
GPU_KEYS = ("nvidia.com/gpu", "amd.com/gpu")
_HUMAN = {"k": 1e3, "m": 1e6, "g": 1e9, "t": 1e12,
          "K": 1024.0, "M": 1024.0 ** 2, "G": 1024.0 ** 3, "T": 1024.0 ** 4}


def _gpus_of(container):
    res = container.get("resources") or {}
    for part in ("limits", "requests"):
        vals = res.get(part) or {}
        for key in GPU_KEYS:
            if vals.get(key):
                return int(vals[key])
    return 0


def _human(text):
    """vLLM's human-readable number, or None for a variable or anything else."""
    mult = 1.0
    default = _SHELL_DEFAULT_RE.fullmatch(text)
    if default:
        text = default.group(1)
    if text and text[-1] in _HUMAN:
        text, mult = text[:-1], _HUMAN[text[-1]]
    try:
        v = float(text) * mult
    except ValueError:
        return None
    return v if v >= 0 else None


def pod_host_ram(spec):
    """Return (gib, parts, unread): host RAM one Pod of this spec holds.

    parts names each term in GiB: `requests` (memory requests, or limits where
    no request is set), `cpu_tier` (a CPU KV tier, sized as described above),
    `shm` (memory-backed emptyDir sizeLimits). The Pod holds the LARGEST of the
    three, not their sum: a tier in /dev/shm is usually inside the memory
    request too, and counting it twice refuses pools that fit. unread lists
    what was found but could not be read as a number.
    """
    requests = tier = shm = 0.0
    unread = []
    for c in spec.get("containers") or []:
        cname = c.get("name", "?")
        res = c.get("resources") or {}
        mem = ((res.get("requests") or {}).get("memory")
               or (res.get("limits") or {}).get("memory"))
        if mem:
            try:
                requests += _quantity_gib(mem)
            except ValueError:
                unread.append("%s: memory %s" % (cname, mem))
        ranks = max(1, _gpus_of(c))
        text = "\n".join(str(x) for x in (c.get("command") or []) + (c.get("args") or []))
        server = per_rank = 0.0
        for m in _CPU_BYTES_RE.finditer(text):
            v = _human(m.group(2))
            if v is None:
                unread.append("%s: cpu_bytes_to_use%s=%s" % (cname, m.group(1) or "", m.group(2)))
            elif m.group(1):
                per_rank = max(per_rank, v / (1024.0 ** 3))
            else:
                server = max(server, v / (1024.0 ** 3))
        for m in _OFFLOAD_SIZE_RE.finditer(text):
            v = _human(m.group(1))
            if v is None:
                unread.append("%s: --kv-offloading-size %s" % (cname, m.group(1)))
            else:
                server = max(server, v)
        lmcache = [m.group(1) for m in _LMCACHE_RE.finditer(text)]
        env_text = "\n".join("%s=%s" % (e.get("name"), e.get("value", "")) for e in c.get("env") or [])
        for env in c.get("env") or []:
            if env.get("name") == LMCACHE_CPU_ENV:
                if "value" in env:
                    lmcache.append(str(env["value"]))
                else:
                    unread.append("%s: %s from %s" % (cname, LMCACHE_CPU_ENV,
                                                     ", ".join(sorted(k for k in env if k != "name"))))
            if env.get("name") == LMCACHE_CONFIG_ENV:
                unread.append("%s: LMCache CPU size in %s %s" % (cname, LMCACHE_CONFIG_ENV,
                                                                env.get("value", "(from valueFrom)")))
        if _LMCACHE_CPU_OFF_RE.search(text + "\n" + env_text):
            lmcache = []
        for raw in lmcache:
            v = _human(raw)
            if v is None:
                unread.append("%s: %s=%s" % (cname, LMCACHE_CPU_ENV, raw))
            else:
                per_rank = max(per_rank, v)
        tier += max(server, per_rank * ranks)
    for vol in spec.get("volumes") or []:
        ed = vol.get("emptyDir")
        if ed is None or ed.get("medium") != "Memory":
            continue
        if ed.get("sizeLimit"):
            try:
                shm += _quantity_gib(ed["sizeLimit"])
            except ValueError:
                unread.append("emptyDir '%s' sizeLimit %s" % (vol.get("name", "?"), ed["sizeLimit"]))
        else:
            unread.append("memory-backed emptyDir '%s' has no sizeLimit" % vol.get("name", "?"))
    parts = {"requests": requests, "cpu_tier": tier, "shm": shm}
    return max(parts.values()), parts, unread


# What dominates a Pod's held RAM, in words for the report.
TERM_WORDS = {"requests": "memory requests", "cpu_tier": "a CPU KV tier",
              "shm": "a memory-backed /dev/shm"}


def _dominant(parts):
    return TERM_WORDS[max(parts, key=lambda k: parts[k])]


def _match_expression(expr, labels):
    key, op = expr.get("key"), expr.get("operator")
    values = expr.get("values") or []
    have = key in labels
    if op == "In":
        return have and labels[key] in values
    if op == "NotIn":
        return not have or labels[key] not in values
    if op == "Exists":
        return have
    if op == "DoesNotExist":
        return not have
    if op in ("Gt", "Lt"):
        if not have or not values:
            return False
        try:
            a, b = int(labels[key]), int(values[0])
        except ValueError:
            return False
        return a > b if op == "Gt" else a < b
    return True  # an operator this does not know: assume it can land here


def can_schedule(spec, labels):
    """Whether a Pod of this spec may land on a node with these labels.

    Reads nodeSelector and REQUIRED node affinity only. Taints and preferred
    affinity are not read, so the answer errs towards "it can", which counts
    RAM as held rather than as free.
    """
    for k, v in (spec.get("nodeSelector") or {}).items():
        if labels.get(k) != v:
            return False
    req = (((spec.get("affinity") or {}).get("nodeAffinity") or {})
           .get("requiredDuringSchedulingIgnoredDuringExecution") or {})
    terms = req.get("nodeSelectorTerms") or []
    if not terms:
        return True
    return any(all(_match_expression(e, labels) for e in (t.get("matchExpressions") or []))
               for t in terms)


def _is_pool(meta):
    labels = (meta or {}).get("labels") or {}
    return ("llm-d.ai/warm-pool" in labels
            or labels.get("app.kubernetes.io/component") == "warm-pool")


def _template_specs(obj):
    """(name, pod labels, pod spec) for every pod template a workload runs."""
    kind = obj.get("kind", "")
    name = "%s/%s" % (kind.lower(), obj["metadata"]["name"])
    spec = obj.get("spec") or {}
    if kind == "LeaderWorkerSet":
        lwt = spec.get("leaderWorkerTemplate") or {}
        out = []
        for role in ("leaderTemplate", "workerTemplate"):
            t = lwt.get(role)
            if t:
                out.append(("%s (%s)" % (name, role[:-len("Template")]), t.get("metadata"),
                            t.get("spec") or {}))
        return out
    t = spec.get("template") or {}
    return [(name, t.get("metadata"), t.get("spec") or {})]


def reserved_by_node(pods, workloads, nodes):
    """Host RAM the namespace's model workloads hold, per node name.

    Returns {node: (gib, [who], what, now_gib)} and a list of unread notes,
    where what names the term that dominates. A node's figure is the larger of
    what the Pods running there hold now (now_gib), and what ONE Pod of any
    workload that can schedule there would hold: the next replica may land on
    a node that has none today. sizing plans with the figure; create refuses
    only on now_gib. Pools and Pods with no GPU are left out.
    """
    now, who_now, what_now, maybe, who_maybe, what_maybe, unread = {}, {}, {}, {}, {}, {}, []
    for pod in pods:
        spec = pod.get("spec") or {}
        node = spec.get("nodeName")
        if not node or _is_pool(pod.get("metadata")):
            continue
        if (pod.get("status") or {}).get("phase") in ("Succeeded", "Failed"):
            continue
        if not any(_gpus_of(c) for c in spec.get("containers") or []):
            continue
        gib, parts, notes = pod_host_ram(spec)
        unread += ["pod/%s: %s" % (pod["metadata"]["name"], n) for n in notes]
        now[node] = now.get(node, 0.0) + gib
        who_now.setdefault(node, []).append("pod/%s" % pod["metadata"]["name"])
        what_now.setdefault(node, set()).add(_dominant(parts))
    for obj in workloads:
        if _is_pool(obj.get("metadata")):
            continue
        for name, meta, spec in _template_specs(obj):
            if _is_pool(meta) or not any(_gpus_of(c) for c in spec.get("containers") or []):
                continue
            gib, parts, notes = pod_host_ram(spec)
            unread += ["%s: %s" % (name, n) for n in notes]
            for n in nodes:
                if can_schedule(spec, n.get("labels") or {}) and gib > maybe.get(n["name"], 0.0):
                    maybe[n["name"]] = gib
                    who_maybe[n["name"]] = [name]
                    what_maybe[n["name"]] = _dominant(parts)
    out = {}
    for node in set(now) | set(maybe):
        if now.get(node, 0.0) >= maybe.get(node, 0.0):
            out[node] = (now[node], who_now[node], " and ".join(sorted(what_now[node])), now[node])
        else:
            out[node] = (maybe[node], who_maybe[node], what_maybe[node], now.get(node, 0.0))
    return out, sorted(set(unread))


def _kubectl_items(args):
    try:
        out = subprocess.run(["kubectl"] + args + ["-o", "json"], capture_output=True, text=True,
                             encoding="utf-8", errors="replace", timeout=120)
    except (OSError, subprocess.TimeoutExpired) as e:
        return None, str(e)
    if out.returncode != 0:
        return None, out.stderr.strip()
    try:
        return json.loads(out.stdout).get("items", []), ""
    except ValueError as e:
        return None, "unreadable answer: %s" % e


# More unread notes than this are summarised: one workload with an unbounded
# /dev/shm would otherwise print a line per Pod on every create.
MAX_UNREAD_NOTES = 3


def read_reserved(namespace, nodes):
    """Fill each node's reserved_gib, reserved_by and reserved_what from the namespace.

    Returns notes. Every note means held RAM was NOT counted, so callers show
    them as warnings.
    """
    notes = []
    pods, err = _kubectl_items(["get", "pods", "-n", namespace])
    if pods is None:
        return ["could not read pods in %s (%s): host RAM they hold is NOT subtracted; "
                "state it with --reserved-ram-gib" % (namespace, err)]
    workloads = []
    for kind in ("deployments", "statefulsets", "leaderworkersets"):
        items, err = _kubectl_items(["get", kind, "-n", namespace])
        if items is None:
            if kind == "leaderworkersets" and "doesn't have a resource type" in err:
                continue
            notes.append("could not read %s in %s (%s): what they hold is NOT subtracted"
                         % (kind, namespace, err))
            continue
        workloads += items
    held, unread = reserved_by_node(pods, workloads, nodes)
    for n in nodes:
        (n["reserved_gib"], n["reserved_by"], n["reserved_what"],
         n["held_now_gib"]) = held.get(n["name"], (0.0, [], "", 0.0))
    shown = unread[:MAX_UNREAD_NOTES]
    for u in shown:
        notes.append("not read, so not subtracted: %s. State it with --reserved-ram-gib" % u)
    if len(unread) > len(shown):
        notes.append("... and %d more values not read" % (len(unread) - len(shown)))
    return notes


def recommend(node, params_b, dtype, kv_headroom, shared_bw, local_bw, ram_frac):
    weights_gb = params_b * BYTES_PER_PARAM[dtype]
    node_gpu_gib = (node["per_gpu_gib"] or 0) * node["gpus"]
    if node_gpu_gib <= 0:
        raise SystemExit(
            "node %s publishes its GPU memory under none of %s, so its capacity "
            "cannot be read. Pass --gpu-mem-gib to state it." % (
                node["name"],
                ", ".join(k for k, _ in accelerator_labels.MEMORY_KEYS_TO_MIB)))

    need_gib = weights_gb * (1.0 + kv_headroom) / 1.073741824
    nodes_needed = max(1, -(-int(need_gib * 1000) // int(node_gpu_gib * 1000)))
    per_node_weights_gb = weights_gb / nodes_needed

    # 1. shape
    #
    # TP is what the MODEL needs, not what the node has. Assuming a model spreads
    # over every GPU on its node overstates the aggregate parse rate for small
    # models by the node's GPU count, and parse aggregate is what decides whether
    # storage is worth spending on.
    lines = []
    per_gpu = node["per_gpu_gib"]
    if nodes_needed == 1:
        tp = max(1, -(-int(need_gib * 1000) // int(per_gpu * 1000)))
        tp = min(tp, node["gpus"])
    else:
        tp = nodes_needed * node["gpus"]
    if nodes_needed == 1:
        lines.append(("SHAPE", "fits one node: Deployment, TP=%d of %d GPUs"
                      % (tp, node["gpus"])))
    else:
        lines.append(("SHAPE", "does NOT fit one node (%.0f GiB needed vs %.0f GiB): "
                               "%d nodes, TP=%d, engine spans Pods -> LWS required"
                      % (need_gib, node_gpu_gib, nodes_needed, tp)))

    # 2 + 3. can host RAM hold sleepers, and how many
    #
    # What the serving engines on this node already hold (a CPU KV tier above
    # all) comes off the top: a sleeper and a pinned tier compete for the same
    # RAM, and the tier was there first.
    #
    # The ram_frac margin is for everything else on the node, serving Pods
    # included, so held RAM is not taken off on top of it: what is left is the
    # smaller of the margin's share and the RAM after what is held, less a 5 %
    # floor for the system.
    reserved = node.get("reserved_gib") or 0.0
    usable_ram = node["ram_gib"] * ram_frac
    if reserved > 0:
        usable_ram = max(0.0, min(usable_ram, node["ram_gib"] * (1.0 - SYSTEM_RAM_FRAC) - reserved))
        lines.append(("RAM", "%.0f GiB of host RAM held by serving workloads (%s: %s), subtracted"
                      % (reserved, node.get("reserved_what") or "stated by hand",
                         ", ".join(node.get("reserved_by") or ["--reserved-ram-gib"]))))
    # The fixed term is per RANK, not per model: each rank is a worker process
    # with its own CUDA context, and every rank on this node shares its RAM.
    # Measured on an H200 with Qwen3-8B awake, 3.16 GiB at TP=1 and 6.75 at
    # TP=2. Must match shape.go and warmpool.sh, which admit and size against
    # it -- three copies of one formula, and they have already drifted once.
    ranks_here = max(1, tp // nodes_needed)
    per_sleeper = (SLEEPER_FIXED_GIB * ranks_here
                   + SLEEPER_FACTOR * (per_node_weights_gb / 1.073741824))
    by_ram = int(usable_ram // per_sleeper) if per_sleeper > 0 else 0
    fits = min(by_ram, MAX_INSTANCES_PER_POD)
    capped = by_ram > MAX_INSTANCES_PER_POD
    if fits < 1:
        lines.append(("SLEEP", "level 1 IMPOSSIBLE: one sleeper needs %.0f GiB per node, "
                               "%.0f GiB usable. No warm copy on this hardware."
                      % (per_sleeper, usable_ram)))
    elif capped:
        lines.append(("SLEEP", "level 1: RAM would allow %d, capped at %d per Pod "
                               "(port range, not a knob). %.0f GiB each."
                      % (by_ram, MAX_INSTANCES_PER_POD, per_sleeper)))
    else:
        lines.append(("SLEEP", "level 1 fits %d model(s) per node: %.0f GiB each, %.0f GiB usable"
                      % (fits, per_sleeper, usable_ram)))

    # 4. what dominates the cold start
    #
    # Aggregate load rate grows SUB-LINEARLY with TP. Measured on Qwen3-8B:
    # 1.08 GB/s at TP=1 and 1.37 GB/s at TP=2 -- 1.27x for twice the ranks, and
    # only 8% of that came back when CPU was tripled, so it is not CPU contention.
    # Fitting gives tp**0.34.
    #
    # This replaces a linear `rate x tp` model that was badly optimistic: it put
    # TP=16 at 17 GB/s where the fit says 2.8, and made node-local storage look
    # 5.7x better than shared for a 744 GB model when the real figure is 1.55x.
    aggregate_parse = PARSE_RATE_GBPS * (tp ** PARSE_TP_EXPONENT)
    shared_s = weights_gb / min(shared_bw, aggregate_parse)
    local_s = weights_gb / min(local_bw * nodes_needed, aggregate_parse)
    wake_s = per_node_weights_gb / PCIE_GBPS
    lines.append(("COLD", "~%.0f s fixed + %.0f s weights (shared storage) = ~%.0f s"
                  % (FIXED_STARTUP_S, shared_s, FIXED_STARTUP_S + shared_s)))
    lines.append(("COLD", "~%.0f s fixed + %.0f s weights (node-local storage) = ~%.0f s"
                  % (FIXED_STARTUP_S, local_s, FIXED_STARTUP_S + local_s)))
    if fits >= 1:
        lines.append(("WAKE", "level-1 wake ~%.0f s (host-to-device, per node in parallel)" % wake_s))

    # The verdict, which is the only part anyone should act on.
    verdict = []
    if fits < 1:
        # Level 2 holds no host RAM at all, so it is the only warm option left --
        # and its wake is a reload, which means node-local storage is what decides
        # whether it is a bridge or a slow cold start.
        if reserved > 0:
            verdict.append(
                "Serving workloads hold %.0f GiB of this node's RAM (%s), and level-1 "
                "sleepers need the same RAM. Place the pool on nodes without them, or "
                "keep a replica up instead."
                % (reserved, node.get("reserved_what") or "stated by hand"))
        verdict.append(
            "Level 1 is IMPOSSIBLE here: host RAM cannot hold one sleeper. Level 2 "
            "holds no RAM, and its wake is a reload: ~%.0f s from node-local storage, "
            "~%.0f s from shared. That is the only warm option on this hardware."
            % (local_s, shared_s))
        verdict.append(
            "But level 2 SERVES GARBAGE unless reload_weights is called after every "
            "wake, and a failed reload poisons the engine until it restarts. Nothing "
            "implements that today. Pin minReplicaCount unless you build it "
            "(see warm-pool-sleep-levels.md).")
    elif fits == 1:
        verdict.append(
            "DO NOT pool this model: one sleeper per node means the pool holds exactly "
            "the accelerators a replica would, while serving nothing. Pin "
            "minReplicaCount >= 1 -- same GPU cost, and it answers requests.")
    else:
        # What a pool Pod HOLDS is the model's device count, not the node's.
        held = tp
        verdict.append(
            "POOL IT: %d models share %d GPU(s) instead of %d, switching in ~%.0f s "
            "against a ~%.0f s cold start. That multiplexing is the whole gain."
            % (fits, held, held * fits, wake_s, FIXED_STARTUP_S + local_s))

    if shared_s > FIXED_STARTUP_S and local_bw > shared_bw:
        verdict.append(
            "STAGE WEIGHTS ON NODE-LOCAL STORAGE: the weight read (%.0f s) exceeds the "
            "fixed startup (%.0f s), and local storage cuts it to %.0f s. Needs %.0f GiB "
            "per node, of %.0f GiB free. No code, no GPU -- price this before anything else."
            % (shared_s, FIXED_STARTUP_S, local_s, per_node_weights_gb / 1.073741824,
               node["disk_gib"]))
    elif shared_s < FIXED_STARTUP_S:
        verdict.append(
            "Faster storage will NOT help: the weight read (%.0f s) is already below the "
            "fixed startup cost (%.0f s). Only keeping a process alive removes that."
            % (shared_s, FIXED_STARTUP_S))

    if nodes_needed > 1 and node.get("whole_free") is not None:
        if node["whole_free"] < nodes_needed:
            verdict.append(
                "SCHEDULING: a group needs %d nodes with every GPU free, and only %d are. "
                "This will not schedule today without draining something."
                % (nodes_needed, node["whole_free"]))
    if nodes_needed > 1:
        verdict.append(
            "Multi-node sleep is DEMONSTRATED (both ranks release, no Ray -- vLLM rejects "
            "the ray backend for nnodes>1). But a group is all-or-nothing: one evicted "
            "worker destroys every warm model the group held.")
    return lines, verdict


def check_pool(nodes, pool_gib):
    """Whether a pool Pod with this memory limit fits a node after what is held there.

    Returns (fits, lines). A node's room is its allocatable memory (capacity
    for a described node) minus the host RAM its serving Pods hold NOW. One
    more replica that may land there is printed, not refused on: it does not
    exist yet, and an emptyDir sizeLimit is a cap, not memory in use. Only a
    pool that fits NO candidate node is refused; the scheduler decides the rest.
    """
    lines, fits = [], 0
    shapes = {}
    for n in nodes:
        alloc = round(n.get("alloc_ram_gib", n["ram_gib"]))
        held = round(n.get("held_now_gib", n.get("reserved_gib") or 0.0))
        more = round(n.get("reserved_gib") or 0.0) - held
        shapes.setdefault((alloc, held, max(0, more)), []).append(n)
    for (alloc, held, more), members in sorted(shapes.items()):
        room = alloc - held
        ok = room >= pool_gib
        if ok:
            fits += len(members)
        who = sorted({w for n in members for w in (n.get("reserved_by") or [])})
        lines.append("%d node(s): %d GiB allocatable - %d GiB held by serving workloads%s = "
                     "%d GiB for the pool, which needs %.0f GiB: %s"
                     % (len(members), alloc, held, (" (%s)" % ", ".join(who)) if who else "",
                        max(0, room), pool_gib, "fits" if ok else "does NOT fit"))
        if more > 0:
            lines.append("   one more replica there would hold up to %d GiB more (not refused on)" % more)
    return fits, lines


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--params",
                    help="total parameters, e.g. 744B or 8B. For MoE use TOTAL, not active: "
                         "every expert must be resident.")
    ap.add_argument("-n", "--namespace", default="",
                    help="the models' namespace: host RAM its GPU workloads hold (memory "
                         "requests, a CPU KV tier, a memory-backed /dev/shm) is subtracted "
                         "on the nodes they can schedule to")
    ap.add_argument("--reserved-ram-gib", type=float,
                    help="host RAM per node held by serving engines, stated by hand; "
                         "replaces reading the namespace")
    ap.add_argument("--check-pool-gib", type=float,
                    help="(used by create) exit 3 if a pool Pod with this memory limit fits "
                         "no node after what serving workloads hold")
    ap.add_argument("--dtype", default="fp8", choices=sorted(BYTES_PER_PARAM))
    ap.add_argument("--kv-headroom", type=float, default=0.25,
                    help="fraction of weight size to leave for KV cache and activations "
                         "(default 0.25). Long-context serving needs far more.")
    ap.add_argument("--shared-bw", type=float, default=1.8,
                    help="shared storage read GB/s (default 1.8, measured on Spectrum Scale)")
    ap.add_argument("--local-bw", type=float, default=5.2,
                    help="node-local storage read GB/s per node (default 5.2, measured NVMe)")
    ap.add_argument("--ram-fraction", type=float, default=0.75,
                    help="share of host RAM usable for sleepers (default 0.75)")
    ap.add_argument("-l", "--selector", default="", help="node label selector")
    # A cluster you cannot reach is still a cluster you have to decide about, and
    # it is the common case when choosing hardware rather than using it.
    ap.add_argument("--gpus-per-node", type=int,
                    help="describe a node instead of reading a cluster")
    ap.add_argument("--gpu-mem-gib", type=float, help="memory per GPU, e.g. 141 for H200")
    ap.add_argument("--ram-gib", type=float, help="host RAM per node")
    ap.add_argument("--disk-gib", type=float, default=0.0, help="node-local disk per node")
    ap.add_argument("--product", default="described node", help="name for the described node")
    args = ap.parse_args()

    # The held RAM: stated by hand, read from the namespace, or neither.
    def apply_reserved(nodes, described):
        if args.reserved_ram_gib is not None:
            for n in nodes:
                n["reserved_gib"], n["reserved_by"] = args.reserved_ram_gib, ["--reserved-ram-gib"]
                n["reserved_what"] = "stated by hand"
            return []
        if described:
            return ["host RAM held by serving engines is NOT subtracted on a described "
                    "node: state it with --reserved-ram-gib"]
        if not args.namespace:
            return ["host RAM held by serving engines is NOT subtracted: pass -n <models' "
                    "namespace> to read it, or --reserved-ram-gib to state it"]
        return read_reserved(args.namespace, nodes)

    if args.check_pool_gib is not None:
        if args.ram_gib is not None:
            nodes = [{"name": args.product, "ram_gib": args.ram_gib, "described": True}]
        else:
            try:
                nodes = nodes_from_cluster(args.selector, count_gpus=False)
            except SystemExit as e:
                print("cannot tell whether the pool fits: %s" % e)
                return 2
            if not nodes:
                print("cannot tell whether the pool fits: no GPU node matches '%s'" % args.selector)
                return 2
        notes = apply_reserved(nodes, args.ram_gib is not None)
        fits, lines = check_pool(nodes, args.check_pool_gib)
        for line in lines:
            print(line)
        for note in notes:
            print("NOTE: %s" % note)
        return 0 if fits else 3

    if not args.params:
        raise SystemExit("--params is required, e.g. --params 8B")
    text = args.params.strip().upper().rstrip("B")
    try:
        params_b = float(text)
    except ValueError:
        raise SystemExit("--params should look like 744B or 8B")

    if args.gpus_per_node:
        if not (args.gpu_mem_gib and args.ram_gib):
            raise SystemExit("--gpus-per-node needs --gpu-mem-gib and --ram-gib too")
        nodes = [{"name": args.product, "gpus": args.gpus_per_node,
                  "free": args.gpus_per_node, "per_gpu_gib": args.gpu_mem_gib,
                  "product": args.product, "ram_gib": args.ram_gib,
                  "disk_gib": args.disk_gib, "described": True}]
    else:
        nodes = nodes_from_cluster(args.selector)
        if not nodes:
            raise SystemExit("no GPU nodes found. Pass -l to select them, or describe "
                             "one with --gpus-per-node/--gpu-mem-gib/--ram-gib.")
    notes = apply_reserved(nodes, bool(args.gpus_per_node))

    # Group by shape: a fleet is only as capable as the node type the model lands on,
    # and mixed fleets give different answers per type. Held RAM is part of the
    # shape: nodes running a CPU KV tier are a different node for a pool.
    shapes = {}
    for n in nodes:
        key = (n["product"], n["gpus"], round(n["per_gpu_gib"] or 0), round(n["ram_gib"]),
               round(n.get("reserved_gib") or 0))
        shapes.setdefault(key, []).append(n)

    weights_gb = params_b * BYTES_PER_PARAM[args.dtype]
    print("Model: %s params at %s = %.0f GB of weights\n" % (args.params, args.dtype, weights_gb))
    for note in notes:
        print("NOTE: %s" % note)
    if notes:
        print("")

    for (product, gpus, per_gpu, ram, _), members in sorted(shapes.items()):
        described = members[0].get("described")
        whole = sum(1 for m in members if m["free"] == m["gpus"])
        if described:
            print("Node: %s  %dx%.0f GiB GPU, %.0f GiB RAM, %.0f GiB local disk"
                  % (product, gpus, per_gpu, ram, members[0]["disk_gib"]))
        else:
            print("%d node(s) of: %s  %dx%.0f GiB GPU, %.0f GiB RAM, %.0f GiB local disk"
                  % (len(members), product, gpus, per_gpu, ram, members[0]["disk_gib"]))
            print("   %d of them currently have every GPU free" % whole)
        try:
            shape = dict(members[0], whole_free=(None if described else whole))
            lines, verdict = recommend(shape, params_b, args.dtype, args.kv_headroom,
                                       args.shared_bw, args.local_bw, args.ram_fraction)
        except SystemExit as e:
            print("   %s\n" % e)
            continue
        for tag, line in lines:
            print("   %-6s %s" % (tag, line))
        print("")
        for v in verdict:
            print("   -> %s" % v)
        print("")

    print("The warm-set budget is the POD's memory LIMIT, not the node's RAM: a Pod is\n"
          "admitted against its own limit, so these counts are what the hardware permits,\n"
          "not what a Pod will hold until you set that limit to match.\n")
    print("Coefficients are MEASURED at 0.6B/8B and extrapolated: a sleeper costs\n"
          "%.1f GiB PER RANK + %.1fx weights of host RAM, the fixed startup is ~%.0f s, and the\n"
          "load path runs ~%.1f GB/s per rank. Distrust these first if a prediction\n"
          "disagrees with the cluster." % (SLEEPER_FIXED_GIB, SLEEPER_FACTOR,
                                           FIXED_STARTUP_S, PARSE_RATE_GBPS))
    return 0


if __name__ == "__main__":
    sys.exit(main())
