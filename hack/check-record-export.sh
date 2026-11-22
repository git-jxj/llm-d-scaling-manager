#!/usr/bin/env bash
# Runs the fetch and check steps of the capacity planner's manual recording
# (docs/guides/record-for-capacity-planner/guide.yaml, manual.fetch and
# manual.check) against a fake Prometheus (hack/testdata/record-export/fakeprom.py),
# then converts what they saved.
#
# What it guards exits 0 when wrong: a cache-tier series dropped from
# CACHE_METRICS (the export then never holds it), a series the fleet does not
# export read as a failed query (an older engine's export refused), a failed
# query read as an absent series (a window that lost data without a word),
# and the LMCache server's series looked up in the engines' namespace when it
# runs in another.
#
# Needs bash, curl 7.76 or later, jq, yq and python3.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GUIDE="$ROOT/docs/guides/record-for-capacity-planner/guide.yaml"
FAKE="$ROOT/hack/testdata/record-export/fakeprom.py"
CONVERT="$ROOT/hack/promjson-to-openmetrics.py"
TMP=$(mktemp -d)
PID=
cleanup() {
  [ -n "$PID" ] && kill "$PID" 2>/dev/null
  rm -rf "$TMP"
}
trap cleanup EXIT
fail=0

FETCH=$(yq -r ".manual.fetch.run" "$GUIDE")
CHECK=$(yq -r ".manual.check.run" "$GUIDE")
# Both extractions must hold the steps: two empty scripts run clean.
case "$FETCH" in *"CACHE_METRICS="*) ;; *)
  echo "FAIL: manual.fetch in $GUIDE has no CACHE_METRICS list" >&2; exit 1 ;; esac
case "$CHECK" in *"bad="*) ;; *)
  echo "FAIL: manual.check in $GUIDE did not extract" >&2; exit 1 ;; esac

# serve starts the fake Prometheus with its arguments; PROM_URL is its URL.
serve() {
  [ -n "$PID" ] && kill "$PID" 2>/dev/null && wait "$PID" 2>/dev/null
  rm -f "$TMP/port"
  python3 "$FAKE" --namespace llm --lmcache-namespace lmc --port-file "$TMP/port" "$@" &
  PID=$!
  for _ in $(seq 1 50); do [ -s "$TMP/port" ] && break; sleep 0.1; done
  [ -s "$TMP/port" ] || { echo "FAIL: the fake Prometheus did not start" >&2; exit 1; }
  PROM_URL="http://127.0.0.1:$(cat "$TMP/port")"
}

# record runs fetch then check in a fresh directory, as the guide's reader
# runs them in one shell; its output is in $TMP/$1.out.
record() {
  local dir="$TMP/$1"
  shift
  mkdir -p "$dir"
  (
    cd "$dir" || exit 1
    export PROM_URL NAMESPACE=llm FROM=1700000000 TO=1700000600 CHUNK=300 CURL_OPTS=""
    for kv in "$@"; do export "${kv?}"; done
    bash -c "$FETCH
$CHECK"
  ) > "$dir.out" 2>&1
}

PRESENT="vllm:kv_cache_usage_perc vllm:external_prefix_cache_hits_total vllm:prompt_tokens_cached_by_source_total lmcache_mp_lookup_hit_tokens_total"

# 1. Some cache-tier series present, the rest absent: nothing fails, every
# CACHE_METRICS name has its two chunks, and the check names which are there.
serve --present "$PRESENT"
record ok LMCACHE_NAMESPACE=lmc
out="$TMP/ok.out"
grep -qx "failed=0" "$out" || { echo "FAIL: an absent series failed the fetch:" >&2; cat "$out" >&2; fail=1; }
grep -qx "bad=0" "$out" || { echo "FAIL: an absent series failed the check:" >&2; cat "$out" >&2; fail=1; }
cache=$(printf '%s\n' "$FETCH" | sed -n '/^export CACHE_METRICS="/,/^"$/p' | sed 's/export CACHE_METRICS=//' | tr -d '"')
n=0
for m in $cache; do
  n=$((n + 1))
  have=$(ls "$TMP/ok/planner-export/${m//:/_}".*.json 2>/dev/null | wc -l)
  [ "$have" -eq 2 ] || { echo "FAIL: $m has $have of 2 chunks" >&2; fail=1; }
done
[ "$n" -ge 40 ] || { echo "FAIL: CACHE_METRICS holds $n names, fewer than the 40 the export list needs" >&2; fail=1; }
for m in vllm:external_prefix_cache_hits_total vllm:prompt_tokens_cached_by_source_total lmcache_mp_lookup_hit_tokens_total; do
  grep -qx "present: $m" "$out" || { echo "FAIL: the check did not report $m present" >&2; fail=1; }
done
for m in vllm:kv_offload_load_time_total sglang:prefill_effective_tokens_total llm_d_epp_request_cached_tokens_bucket; do
  grep -qx "absent: $m" "$out" || { echo "FAIL: the check did not report $m absent" >&2; fail=1; }
done
if grep -q "^present: vllm:kv_cache_usage_perc" "$out"; then
  echo "FAIL: the check reported a series outside CACHE_METRICS" >&2; fail=1
fi

# 1b. What was saved converts: the present cache-tier series reach the
# OpenMetrics file, and the empty answers convert to nothing.
if ! python3 "$CONVERT" -o "$TMP/ok.om" "$TMP"/ok/planner-export/*.json 2>"$TMP/conv.err"; then
  echo "FAIL: the converter refused the export: $(cat "$TMP/conv.err")" >&2; fail=1
else
  for m in vllm:external_prefix_cache_hits_total lmcache_mp_lookup_hit_tokens_total; do
    grep -q "^$m{" "$TMP/ok.om" || { echo "FAIL: $m did not reach the converted file" >&2; fail=1; }
  done
fi

# 2. Without LMCACHE_NAMESPACE the LMCache server's series is looked up in
# the engines' namespace, where this fake does not hold it: absent, not failed.
record nolmc
grep -qx "absent: lmcache_mp_lookup_hit_tokens_total" "$TMP/nolmc.out" || {
  echo "FAIL: LMCACHE_NAMESPACE is not what finds the LMCache server's series" >&2; fail=1; }

# 3. A cache-tier query that fails is a failure, not an absence.
serve --present "$PRESENT" --fail "vllm:kv_offload_load_bytes_total"
record failing LMCACHE_NAMESPACE=lmc
out="$TMP/failing.out"
grep -qx "failed=1" "$out" || { echo "FAIL: a failed cache-tier query did not fail the fetch" >&2; cat "$out" >&2; fail=1; }
grep -q "^FAILED: vllm:kv_offload_load_bytes_total at " "$out" || { echo "FAIL: the fetch did not name the failed series" >&2; fail=1; }
grep -qx "bad=1" "$out" || { echo "FAIL: a failed cache-tier query passed the check" >&2; fail=1; }

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "OK: the manual recording saves $n cache-tier series, reads absent ones as absent and failed ones as failed"
