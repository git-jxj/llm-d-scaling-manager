#!/usr/bin/env python3
"""Check that warm-pool sizing subtracts the host RAM serving engines hold.

A level-1 sleeper lives in host RAM, and so does a CPU KV-offload tier. Sizing
that reads a node's RAM and ignores the tier recommends a pool that is Pending
on those nodes, or evicted under memory pressure with every model in it.

The first cases call only recommend(), which exists on every revision, so on a
revision that ignores held RAM they fail on the number rather than on an
import. Every case reports once, and the run must reach the end.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "deploy" / "lib"))
import warmpool_sizing as ws  # noqa: E402

EXPECT_CASES = 58
CASES = []


def check(name, want, got):
    CASES.append(name)
    if want != got:
        print(f"FAIL  {name}\n        want {want!r}\n        got  {got!r}", file=sys.stderr)
        sys.exit(1)
    print(f"ok    {name}")


def sleep_line(node):
    lines, _ = ws.recommend(node, 8, "bf16", 0.25, 1.8, 5.2, 0.75)
    return next(text for tag, text in lines if tag == "SLEEP")


H200 = {"name": "h200", "gpus": 8, "free": 8, "per_gpu_gib": 141, "product": "H200",
        "ram_gib": 2016, "disk_gib": 0}

# --- recommend() ------------------------------------------------------------
# One 8B bf16 sleeper is 2.6 + 1.4 x 14.9 = 23.5 GiB. Usable RAM is the smaller
# of 0.75 x RAM and 0.95 x RAM minus what is held.
check("a node with nothing held fits the 16-instance cap",
      True, "capped at 16" in sleep_line(dict(H200)))
check("1950 GiB held by a CPU KV tier leaves no room for one 8B sleeper",
      True, "IMPOSSIBLE" in sleep_line(dict(H200, reserved_gib=1950.0)))
check("1800 GiB held leaves 115 GiB: 4 sleepers, not 16",
      True, "level 1 fits 4 model(s)" in sleep_line(dict(H200, reserved_gib=1800.0)))
check("100 GiB of requests is inside the 25 % margin, not taken off on top of it",
      True, "capped at 16" in sleep_line(dict(H200, reserved_gib=100.0)))
lines, verdict = ws.recommend(dict(H200, reserved_gib=1950.0, reserved_what="a CPU KV tier",
                                   reserved_by=["deployment/prefill"]), 8, "bf16", 0.25, 1.8, 5.2, 0.75)
check("the RAM line names what holds it and who",
      True, any(t == "RAM" and "a CPU KV tier: deployment/prefill" in x for t, x in lines))
check("the verdict names the held term, not always a tier",
      True, any("(a CPU KV tier)" in v for v in verdict))

# --- quantities -------------------------------------------------------------
GIB = 1024 ** 3
check("1Gi", 1.0, ws._quantity_gib("1Gi"))
check("1024Mi", 1.0, ws._quantity_gib("1024Mi"))
check("bytes", 1.0, ws._quantity_gib(str(GIB)))
check("decimal 1G", round(1e9 / GIB, 6), round(ws._quantity_gib("1G"), 6))
check("milli", 64.0, ws._quantity_gib("68719476736000m"))

# --- pod_host_ram() ---------------------------------------------------------


def container(**kw):
    c = {"name": "vllm", "resources": {"requests": {"memory": "64Gi"},
                                       "limits": {"nvidia.com/gpu": str(kw.pop("gpus", 1))}}}
    c.update(kw)
    return c


def tier_of(**kw):
    return ws.pod_host_ram({"containers": [container(**kw)]})[1]["cpu_tier"]


offload = '{"kv_connector":"OffloadingConnector","kv_connector_extra_config":{"cpu_bytes_to_use":%s}}'
held, parts, unread = ws.pod_host_ram({"containers": [container(
    gpus=2, args=["--kv-transfer-config", offload % (512 * GIB)])]})
check("cpu_bytes_to_use is the server's total, not per rank", 512.0, parts["cpu_tier"])
check("the Pod holds the largest term", 512.0, held)
check("nothing unread in a plain JSON arg", [], unread)
check("cpu_bytes_to_use_per_rank counts per rank",
      400.0, tier_of(gpus=4, args=["--kv-transfer-config",
                                   '{"kv_connector_extra_config":{"cpu_bytes_to_use_per_rank":%d}}' % (100 * GIB)]))
check("human-readable 200G is binary", 200.0, tier_of(args=["--kv-transfer-config", offload % "200G"]))
check("human-readable 200g is decimal", round(200e9 / GIB, 3),
      round(tier_of(args=["--kv-transfer-config", offload % "200g"]), 3))
check("--kv-offloading-size N is GiB for the server", 300.0, tier_of(gpus=8, args=["--kv-offloading-size", "300"]))
check("--kv-offloading-size=N", 300.0, tier_of(args=["--kv-offloading-size=300"]))
check("--kv_offloading_size, the underscore spelling", 300.0, tier_of(args=["--kv_offloading_size", "300"]))
check("an exponent is read, not cut at the e", round(1e11 / GIB, 3),
      round(tier_of(args=["--kv-transfer-config", offload % "1e11"]), 3))
check("LMCache's size in kv_connector_extra_config, per rank", 200.0,
      tier_of(gpus=2, args=["--kv-transfer-config",
                            '{"kv_connector_extra_config":{"lmcache.max_local_cpu_size": 100}}']))
check("LMCACHE_LOCAL_CPU=False holds no CPU tier", 0.0,
      tier_of(gpus=8, env=[{"name": "LMCACHE_MAX_LOCAL_CPU_SIZE", "value": "100"},
                           {"name": "LMCACHE_LOCAL_CPU", "value": "False"}]))
_, _, unread = ws.pod_host_ram({"containers": [container(
    env=[{"name": "LMCACHE_CONFIG_FILE", "value": "/cfg/lmcache.yaml"}])]})
check("an LMCache config file is reported, not read", ["vllm: LMCache CPU size in LMCACHE_CONFIG_FILE /cfg/lmcache.yaml"], unread)
shell = 'exec vllm serve m --kv-transfer-config "{\\"kv_connector_extra_config\\": {\\"cpu_bytes_to_use\\": %d}}"'
check("a tier inside a shell string with escaped quotes", 100.0,
      tier_of(command=["sh", "-c", shell % (100 * GIB)]))
check("LMCache's CPU size counts per rank: 100 GiB x 4", 400.0,
      tier_of(gpus=4, env=[{"name": "LMCACHE_MAX_LOCAL_CPU_SIZE", "value": "100"}]))
_, parts, unread = ws.pod_host_ram({"containers": [container(
    gpus=2, command=["sh", "-c", "export LMCACHE_MAX_LOCAL_CPU_SIZE=${LMCACHE_MAX_LOCAL_CPU_SIZE:-50}"])]})
check("a shell default ${X:-50} is read", 100.0, parts["cpu_tier"])
check("... and is not reported unread", [], unread)
_, parts, unread = ws.pod_host_ram({"containers": [container(command=["sh", "-c", "echo cpu_bytes_to_use: disabled"])]})
check("a word after the key is neither a tier nor a note", (0.0, []), (parts["cpu_tier"], unread))

held, parts, _ = ws.pod_host_ram({
    "containers": [container(resources={"requests": {"memory": "200Gi"}})],
    "volumes": [{"name": "dshm", "emptyDir": {"medium": "Memory", "sizeLimit": "1250Gi"}}]})
check("a memory-backed /dev/shm sizeLimit counts", 1250.0, parts["shm"])
check("requests are read", 200.0, parts["requests"])
check("the shm term wins over a smaller request", 1250.0, held)
_, parts, _ = ws.pod_host_ram({"containers": [container(resources={"limits": {"memory": "512G"}})]})
check("a decimal limit stands in for a missing request", round(512e9 / GIB, 3), round(parts["requests"], 3))
_, parts, unread = ws.pod_host_ram({"containers": [container(resources={"requests": {"memory": "lots"}})]})
check("a quantity that cannot be read is a note, not a crash", ["vllm: memory lots"], unread)

_, _, unread = ws.pod_host_ram({
    "containers": [container(args=["--kv-transfer-config", '{"cpu_bytes_to_use": $TIER}'],
                             env=[{"name": "LMCACHE_MAX_LOCAL_CPU_SIZE",
                                   "valueFrom": {"configMapKeyRef": {"name": "c", "key": "k"}}}])],
    "volumes": [{"name": "dshm", "emptyDir": {"medium": "Memory"}}]})
check("what is not a number is reported, not guessed",
      ["vllm: cpu_bytes_to_use=$TIER", "vllm: LMCACHE_MAX_LOCAL_CPU_SIZE from valueFrom",
       "memory-backed emptyDir 'dshm' has no sizeLimit"], unread)

# --- can_schedule() ---------------------------------------------------------
pinned = {"nodeSelector": {"role": "prefill"}}
check("nodeSelector matches", True, ws.can_schedule(pinned, {"role": "prefill"}))
check("nodeSelector excludes", False, ws.can_schedule(pinned, {"role": "decode"}))


def aff(*exprs):
    return {"affinity": {"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {
        "nodeSelectorTerms": [{"matchExpressions": list(exprs)}]}}}}


in_dne = aff({"key": "gpu", "operator": "In", "values": ["H200"]}, {"key": "tier", "operator": "DoesNotExist"})
check("In + DoesNotExist matches", True, ws.can_schedule(in_dne, {"gpu": "H200"}))
check("In + DoesNotExist excludes", False, ws.can_schedule(in_dne, {"gpu": "H200", "tier": "x"}))
notin = aff({"key": "role", "operator": "NotIn", "values": ["decode"]})
check("NotIn excludes", False, ws.can_schedule(notin, {"role": "decode"}))
check("NotIn admits a node without the label", True, ws.can_schedule(notin, {}))
check("Exists", (True, False), (ws.can_schedule(aff({"key": "nvme", "operator": "Exists"}), {"nvme": ""}),
                                ws.can_schedule(aff({"key": "nvme", "operator": "Exists"}), {})))
check("Gt on a missing label does not match", False,
      ws.can_schedule(aff({"key": "ram", "operator": "Gt", "values": ["1"]}), {}))

# --- reserved_by_node() -----------------------------------------------------
tier_spec = {"nodeSelector": {"role": "prefill"},
             "containers": [container(gpus=8, args=["--kv-transfer-config", offload % (160 * GIB)])]}
nodes = [{"name": "a", "labels": {"role": "prefill"}},
         {"name": "b", "labels": {"role": "prefill"}},
         {"name": "c", "labels": {"role": "decode"}}]


def pod(name, node, spec, labels=None):
    return {"metadata": {"name": name, "labels": labels or {}}, "spec": dict(spec, nodeName=node),
            "status": {"phase": "Running"}}


pods = [
    pod("prefill-0", "a", tier_spec), pod("prefill-1", "a", tier_spec),
    pod("pool-0", "c", tier_spec, {"llm-d.ai/warm-pool": "p"}),
    pod("cpu-only", "c", {"containers": [{"name": "x", "resources": {"requests": {"memory": "900Gi"}}}]}),
]
workloads = [{"kind": "Deployment", "metadata": {"name": "prefill"},
              "spec": {"template": {"spec": tier_spec}}}]
held, _ = ws.reserved_by_node(pods, workloads, nodes)
check("two tier Pods on one node: what they hold now beats one more",
      (320.0, ["pod/prefill-0", "pod/prefill-1"], "a CPU KV tier", 320.0), held["a"])
check("a node the workload can schedule to holds one Pod's worth",
      (160.0, ["deployment/prefill"], "a CPU KV tier", 0.0), held["b"])
check("pool Pods, Pods with no GPU and nodes the workload cannot reach hold nothing", False, "c" in held)
lws = [{"kind": "LeaderWorkerSet", "metadata": {"name": "glm"}, "spec": {"leaderWorkerTemplate": {
    "leaderTemplate": {"spec": {"containers": [{"name": "proxy"}]}},
    "workerTemplate": {"spec": dict(tier_spec, nodeSelector={})}}}}]
held, _ = ws.reserved_by_node([], lws, nodes)
check("an LWS worker template counts; a leader with no GPU does not",
      (160.0, ["leaderworkerset/glm (worker)"], "a CPU KV tier", 0.0), held["c"])

# --- read_reserved(): what the cluster refuses ------------------------------


def fake(answers):
    def items(args):
        return answers[args[1]]
    return items


ws._kubectl_items = fake({"pods": (None, "Error from server (Forbidden): pods is forbidden")})
ns_nodes = [dict(n) for n in nodes]
notes = ws.read_reserved("tenant", ns_nodes)
check("pods Forbidden: a note says nothing was subtracted",
      True, len(notes) == 1 and "could not read pods" in notes[0] and "NOT subtracted" in notes[0])
check("pods Forbidden: no node is given a held figure", False, any("reserved_gib" in n for n in ns_nodes))
lws_missing = (None, 'error: the server doesn\'t have a resource type "leaderworkersets"')
ws._kubectl_items = fake({"pods": (pods, ""), "deployments": (workloads, ""), "statefulsets": ([], ""),
                          "leaderworkersets": lws_missing})
notes = ws.read_reserved("tenant", ns_nodes)
check("no LeaderWorkerSet CRD is not a problem", [], notes)
check("... and the figures land on the nodes", (320.0, 160.0, 0.0),
      tuple(n["reserved_gib"] for n in ns_nodes))
check("... with what runs there now beside them", (320.0, 0.0, 0.0),
      tuple(n["held_now_gib"] for n in ns_nodes))
unbounded = {"containers": [container()], "volumes": [{"name": "dshm", "emptyDir": {"medium": "Memory"}}]}
ws._kubectl_items = fake({"pods": ([pod("p%d" % i, "a", unbounded) for i in range(5)], ""),
                          "deployments": ([], ""), "statefulsets": ([], ""), "leaderworkersets": ([], "")})
notes = ws.read_reserved("tenant", [dict(n) for n in nodes])
check("five unread values are summarised after three", (4, "... and 2 more values not read"),
      (len(notes), notes[-1]))

# --- check_pool() -----------------------------------------------------------
fits, lines = ws.check_pool([{"name": "n", "ram_gib": 2016, "alloc_ram_gib": 2000,
                              "reserved_gib": 1950.0, "reserved_by": ["deployment/prefill"]}], 100)
check("a pool that fits no node is refused", 0, fits)
check("the refusal names the numbers", True,
      "2000 GiB allocatable - 1950 GiB held" in lines[0] and "50 GiB for the pool" in lines[0])
fits, _ = ws.check_pool([{"name": "n", "ram_gib": 2016, "reserved_gib": 1950.0},
                         {"name": "m", "ram_gib": 2016}], 100)
check("one node with room is enough", 1, fits)
fits, lines = ws.check_pool([{"name": "idle", "ram_gib": 2016, "alloc_ram_gib": 2000, "reserved_gib": 1950.0,
                              "held_now_gib": 0.0, "reserved_by": ["deployment/prefill"]}], 100)
check("a replica that does not exist yet is not refused on", 1, fits)
check("... but it is printed", True, any("would hold up to 1950 GiB more" in x for x in lines))

if len(CASES) != EXPECT_CASES:
    print(f"FAIL  ran {len(CASES)} cases, expected {EXPECT_CASES}", file=sys.stderr)
    sys.exit(1)
print(f"all {len(CASES)} cases passed")
