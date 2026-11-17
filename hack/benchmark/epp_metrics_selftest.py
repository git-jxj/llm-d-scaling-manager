#!/usr/bin/env python3
"""Self-test for the benchmark scripts that parse raw EPP /metrics scrapes.

Router v1.0.0 exports only the llm_d_epp_* names; routers before it export
those and the legacy inference_extension_* / inference_pool_* /
inference_objective_* names side by side. Each parser must read the current
name, fall back to the legacy one, and never add the two together.

Covers dump_epp_throughput.parse_one, dump_capacity_demand_estimate's and
plot_two_variant_pipeline's parse_epp_log. Exits non-zero on any failure.
"""
import importlib
import sys
import tempfile
import types
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

# plot_two_variant_pipeline exits at import without matplotlib; only its
# parsers are under test, so stand in for the plotting modules.
for name, attrs in {
    "matplotlib": {},
    "matplotlib.dates": {},
    "matplotlib.pyplot": {},
    "matplotlib.ticker": {"MaxNLocator": object},
    "matplotlib.transforms": {"blended_transform_factory": lambda *a, **k: None},
}.items():
    if name not in sys.modules:
        mod = types.ModuleType(name)
        for k, v in attrs.items():
            setattr(mod, k, v)
        sys.modules[name] = mod

throughput = importlib.import_module("dump_epp_throughput")
capacity = importlib.import_module("dump_capacity_demand_estimate")
plot = importlib.import_module("plot_two_variant_pipeline")

FAILED = 0
PASSED = 0


def check(name, got, want):
    global FAILED, PASSED
    if got == want:
        PASSED += 1
        print("ok   " + name)
    else:
        FAILED += 1
        print("FAIL %s: got %r, want %r" % (name, got, want))


TMP = Path(tempfile.mkdtemp())


def scrape(text):
    p = TMP / ("s%d.log" % (len(list(TMP.iterdir()))))
    p.write_text(text)
    return p


NEW = """\
llm_d_epp_request_total{fairness_id="d",model_name="m",priority="0",target_model_name="m"} 10
llm_d_epp_request_error_total{error_code="x",fairness_id="d",model_name="m",priority="0",target_model_name="m"} 1
llm_d_epp_request_input_tokens_count{fairness_id="d",model_name="m",priority="0",target_model_name="m"} 10
llm_d_epp_request_input_tokens_sum{fairness_id="d",model_name="m",priority="0",target_model_name="m"} 5000
llm_d_epp_request_output_tokens_count{fairness_id="d",model_name="m",priority="0",target_model_name="m"} 10
llm_d_epp_request_output_tokens_sum{fairness_id="d",model_name="m",priority="0",target_model_name="m"} 2000
llm_d_epp_scheduler_attempts_total{endpoint_name="p",namespace="n",port="8000",status="success",target_model_name="m"} 12
llm_d_epp_flow_control_queue_size{fairness_id="d",inference_pool="pool",model_name="m",priority="0",target_model_name="m"} 7
llm_d_epp_average_queue_size{name="pool"} 3
llm_d_epp_per_endpoint_queue_size{model_server_endpoint="m-decode-abc",name="pool"} 4
"""

LEGACY = """\
inference_objective_request_total{model_name="m",priority="0",target_model_name="m"} 6
inference_objective_request_error_total{error_code="x",model_name="m",target_model_name="m"} 2
inference_objective_input_tokens_count{model_name="m",target_model_name="m"} 6
inference_objective_input_tokens_sum{model_name="m",target_model_name="m"} 900
inference_objective_output_tokens_count{model_name="m",target_model_name="m"} 6
inference_objective_output_tokens_sum{model_name="m",target_model_name="m"} 300
inference_extension_scheduler_attempts_total{namespace="n",pod_name="p",port="8000",status="success",target_model_name="m"} 8
inference_extension_flow_control_queue_size{fairness_id="d",inference_pool="pool",model_name="m",priority="0",target_model_name="m"} 5
inference_pool_average_queue_size{name="pool"} 2
inference_pool_per_pod_queue_size{model_server_pod="m-prefill-xyz",name="pool"} 9
"""

# --- dump_epp_throughput.parse_one ---------------------------------------
want_new = {"request_total": 10.0, "request_error_total": 1.0, "input_tokens_sum": 5000.0,
            "output_tokens_sum": 2000.0, "sched_attempts_total": 12.0}
want_legacy = {"request_total": 6.0, "request_error_total": 2.0, "input_tokens_sum": 900.0,
               "output_tokens_sum": 300.0, "sched_attempts_total": 8.0}
check("throughput, v1.0.0 names only: token totals are the _sum, not the _count",
      throughput.parse_one(scrape(NEW)), want_new)
check("throughput, legacy names only", throughput.parse_one(scrape(LEGACY)), want_legacy)
check("throughput, both names: the new name's value, not the sum",
      throughput.parse_one(scrape(LEGACY + NEW)), want_new)

# --- dump_capacity_demand_estimate.parse_epp_log -------------------------
check("capacity, llm_d_epp_flow_control_queue_size",
      capacity.parse_epp_log(scrape(NEW)), {"eppQueueSize": 7.0})
check("capacity, inference_extension_flow_control_queue_size",
      capacity.parse_epp_log(scrape(LEGACY)), {"eppQueueSize": 5.0})

# --- plot_two_variant_pipeline.parse_epp_log -----------------------------
check("plot, v1.0.0 names: queue, pool average and per-endpoint",
      plot.parse_epp_log(scrape(NEW)), {"fc_queue": 7.0, "pool_avg": 3.0, "per_pod": {"m-decode-abc": 4.0}})
check("plot, legacy names: queue, pool average and per-pod",
      plot.parse_epp_log(scrape(LEGACY)), {"fc_queue": 5.0, "pool_avg": 2.0, "per_pod": {"m-prefill-xyz": 9.0}})
check("plot, both names: per_endpoint wins, per_pod is not added",
      plot.parse_epp_log(scrape(NEW + LEGACY))["per_pod"], {"m-decode-abc": 4.0})

print("epp metrics parsers: %d passed, %d failed" % (PASSED, FAILED))
sys.exit(1 if FAILED else 0)
