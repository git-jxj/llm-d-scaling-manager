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

EXPECT_CASES = 23
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
check("a node with nothing held fits the 16-instance cap",
      True, "capped at 16" in sleep_line(dict(H200)))
check("1500 GiB held by a CPU KV tier leaves no room for one 8B sleeper",
      True, "IMPOSSIBLE" in sleep_line(dict(H200, reserved_gib=1500.0)))
check("1400 GiB held leaves room for some, not 16",
      True, "level 1 fits 4 model(s)" in sleep_line(dict(H200, reserved_gib=1400.0)))

# --- pod_host_ram() ---------------------------------------------------------
GIB = 1024 ** 3


def container(**kw):
    c = {"name": "vllm", "resources": {"requests": {"memory": "64Gi"},
                                       "limits": {"nvidia.com/gpu": str(kw.pop("gpus", 1))}}}
    c.update(kw)
    return c


offload = '{"kv_connector":"OffloadingConnector","kv_connector_extra_config":{"cpu_bytes_to_use":%d}}'
held, parts, unread = ws.pod_host_ram({"containers": [container(
    gpus=2, args=["--kv-transfer-config", offload % (512 * GIB)])]})
check("cpu_bytes_to_use counts per rank: 512 GiB x 2 ranks", 1024.0, parts["cpu_tier"])
check("the Pod holds the largest term", 1024.0, held)
check("nothing unread in a plain JSON arg", [], unread)

shell = 'exec vllm serve m --kv-transfer-config "{\\"kv_connector_extra_config\\": {\\"cpu_bytes_to_use\\": %d}}"'
_, parts, _ = ws.pod_host_ram({"containers": [container(
    gpus=1, command=["sh", "-c", shell % (100 * GIB)])]})
check("a tier inside a shell string with escaped quotes", 100.0, parts["cpu_tier"])

_, parts, _ = ws.pod_host_ram({"containers": [container(
    gpus=4, env=[{"name": "LMCACHE_MAX_LOCAL_CPU_SIZE", "value": "100"}])]})
check("LMCache's CPU size counts per rank: 100 GiB x 4", 400.0, parts["cpu_tier"])

held, parts, _ = ws.pod_host_ram({
    "containers": [container(resources={"requests": {"memory": "200Gi"}})],
    "volumes": [{"name": "dshm", "emptyDir": {"medium": "Memory", "sizeLimit": "1250Gi"}}]})
check("a memory-backed /dev/shm sizeLimit counts", 1250.0, parts["shm"])
check("requests are read", 200.0, parts["requests"])
check("the shm term wins over a smaller request", 1250.0, held)

_, parts, _ = ws.pod_host_ram({"containers": [container(resources={"limits": {"memory": "512G"}})]})
check("a decimal limit stands in for a missing request", round(512e9 / GIB, 3), round(parts["requests"], 3))

_, _, unread = ws.pod_host_ram({
    "containers": [container(args=["--kv-transfer-config", '{"cpu_bytes_to_use": $TIER}'],
                             env=[{"name": "LMCACHE_MAX_LOCAL_CPU_SIZE",
                                   "valueFrom": {"configMapKeyRef": {"name": "c", "key": "k"}}}])],
    "volumes": [{"name": "dshm", "emptyDir": {"medium": "Memory"}}]})
check("a tier that is not a number is reported, not guessed", 3, len(unread))

# --- can_schedule() ---------------------------------------------------------
pinned = {"nodeSelector": {"role": "prefill"}}
check("nodeSelector matches", True, ws.can_schedule(pinned, {"role": "prefill"}))
check("nodeSelector excludes", False, ws.can_schedule(pinned, {"role": "decode"}))
aff = {"affinity": {"nodeAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": {
    "nodeSelectorTerms": [{"matchExpressions": [
        {"key": "gpu", "operator": "In", "values": ["H200"]},
        {"key": "tier", "operator": "DoesNotExist"}]}]}}}}
check("required affinity matches", True, ws.can_schedule(aff, {"gpu": "H200"}))
check("required affinity excludes", False, ws.can_schedule(aff, {"gpu": "H200", "tier": "x"}))

# --- reserved_by_node() -----------------------------------------------------
tier_spec = {"nodeSelector": {"role": "prefill"},
             "containers": [container(gpus=8, args=["--kv-transfer-config", offload % (160 * GIB)])]}
nodes = [{"name": "a", "labels": {"role": "prefill"}},
         {"name": "b", "labels": {"role": "prefill"}},
         {"name": "c", "labels": {"role": "decode"}}]
pods = [
    {"metadata": {"name": "prefill-0"}, "spec": dict(tier_spec, nodeName="a"), "status": {"phase": "Running"}},
    {"metadata": {"name": "pool-0", "labels": {"llm-d.ai/warm-pool": "p"}},
     "spec": dict(tier_spec, nodeName="c"), "status": {"phase": "Running"}},
    {"metadata": {"name": "cpu-only"}, "spec": {"nodeName": "c", "containers": [
        {"name": "x", "resources": {"requests": {"memory": "900Gi"}}}]}, "status": {"phase": "Running"}},
]
workloads = [{"kind": "Deployment", "metadata": {"name": "prefill"},
              "spec": {"template": {"spec": tier_spec}}}]
held, _ = ws.reserved_by_node(pods, workloads, nodes)
check("a node running a tier Pod holds it", 1280.0, held["a"][0])
check("a node the workload can schedule to holds one Pod's worth", (1280.0, ["deployment/prefill"]), held["b"])
check("pool Pods, Pods with no GPU and nodes the workload cannot reach hold nothing", False, "c" in held)

# --- check_pool() -----------------------------------------------------------
fits, lines = ws.check_pool([{"name": "n", "ram_gib": 2016, "alloc_ram_gib": 2000,
                              "reserved_gib": 1950.0, "reserved_by": ["deployment/prefill"]}], 100)
check("a pool that fits no node is refused", 0, fits)
check("the refusal names the numbers", True,
      "2000 GiB allocatable - 1950 GiB held" in lines[0] and "50 GiB for the pool" in lines[0])
fits, _ = ws.check_pool([{"name": "n", "ram_gib": 2016, "reserved_gib": 1950.0},
                         {"name": "m", "ram_gib": 2016}], 100)
check("one node with room is enough", 1, fits)

if len(CASES) != EXPECT_CASES:
    print(f"FAIL  ran {len(CASES)} cases, expected {EXPECT_CASES}", file=sys.stderr)
    sys.exit(1)
print(f"all {len(CASES)} cases passed")
