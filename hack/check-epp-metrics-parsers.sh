#!/usr/bin/env bash
# Runs the benchmark EPP-scrape parsers against fixtures in both metric
# namings (router v1.0.0's llm_d_epp_* and the legacy names older routers also
# export). See hack/benchmark/epp_metrics_selftest.py for the cases.
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if python3 "$ROOT/hack/benchmark/epp_metrics_selftest.py"; then
    echo "EPP metrics parser check OK"
else
    echo "EPP metrics parser check FAILED"
    exit 1
fi
