#!/usr/bin/env bash
#
# Executes the DisaggregatedSet paths in deploy/lib/scaledobject.sh against a
# fixture set: discovery (one ScaledObject plan entry per role, targeting the
# role's DisaggregatedSetRoleScaler), the reasons a role cannot be targeted
# yet, the workload report, and the namespace scan. Also the install's
# LeaderWorkerSet report (llmsm_report_lws_dset in deploy/lib/prereqs.sh):
# a LeaderWorkerSet without the role scaler's /scale is warned about.
#
# Every case has a POSITIVE assertion: a function that does not exist must
# fail it (see the note at the top of hack/check-workload-gaps.sh).
#
# kubectl is stubbed; nothing here touches a cluster.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Sourced through a CR-stripped copy, as check-workload-gaps.sh does: a
# Windows checkout writes these CRLF.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh scaledobject.sh prereqs.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
BLUE='' GREEN='' YELLOW='' RED='' NC=''
# shellcheck source=/dev/null
source "$WORK/common.sh"
# shellcheck source=/dev/null
source "$WORK/scaledobject.sh"
# shellcheck source=/dev/null
source "$WORK/prereqs.sh"

command -v jq >/dev/null 2>&1 || { printf 'FATAL: jq is required to run these checks.\n' >&2; exit 1; }

FAILED=0
RAN=0
CASE=""
EXPECT_CASES="$(grep -c '^CASE="[^"]' "${BASH_SOURCE[0]}")"

fail() {
    printf 'FAIL  %s\n      %s\n' "$CASE" "$1" >&2
    FAILED=$((FAILED + 1))
}
ok() { RAN=$((RAN + 1)); printf 'ok    %s\n' "$CASE"; }
assert_contains() {
    case "$1" in
        *"$2"*) : ;;
        *) fail "expected to contain: $2" ;;
    esac
}
assert_not_contains() {
    case "$1" in
        *"$2"*) fail "expected NOT to contain: $2" ;;
        *) : ;;
    esac
}
assert_eq() { [ "$1" = "$2" ] || fail "${3:-value}: want '$2', got '$1'"; }

# --- fixture ----------------------------------------------------------------

# A P/D set as LWS v1.0.0 stores it: prefill External with its own preStop,
# decode still Static (the shape a rollout-managed deployment starts from),
# neither with a leader template, so both lead from the worker template. The
# engine is the SECOND container: a routing sidecar comes first.
# The hooked role's grace period covers its 30 s sleep and the 15 s margin.
role() {
    local name="$1" mode="$2" prestop="$3"
    jq -n --arg name "$name" --arg mode "$mode" --argjson prestop "$prestop" '
        { name: $name,
          spec: { replicas: 7,
                  leaderWorkerTemplate: { size: 4,
                    workerTemplate: {
                      metadata: { labels: { "llm-d.ai/role": $name, "llm-d.ai/model": "glm" } },
                      spec: { terminationGracePeriodSeconds: (if $prestop then 60 else 30 end),
                              containers: [
                        { name: "routing-proxy", image: "registry.example/router:v1" },
                        ( { name: "vllm", image: "registry.example/llm-d-cuda:v1",
                            command: ["/bin/bash", "-c"],
                            args: ["exec vllm serve /model-cache/models/org/GLM \\\n  --served-model-name org/GLM --port 8200"] }
                          + (if $prestop then { lifecycle: { preStop: { exec: { command: ["sleep", "30"] } } } } else {} end) )
                      ] } } } } }
        + (if $mode == "" then {} else { scaling: { mode: $mode } } end)'
}

dset() {
    local slices="$1" prefill_mode="$2" decode_mode="$3"
    jq -n --argjson slices "$slices" --argjson p "$(role prefill "$prefill_mode" true)" \
          --argjson d "$(role decode "$decode_mode" false)" '
        { apiVersion: "disaggregatedset.x-k8s.io/v1", kind: "DisaggregatedSet",
          metadata: { name: "glm", namespace: "serving" },
          spec: ({ roles: [ $p, $d ] } + (if $slices == 0 then {} else { slices: $slices } end)) }'
}

DSET_OBJ=""
CRD_RC=0
LIST_RC=0
SO_OBJS=""
# What discovery serves for disaggregatedset.x-k8s.io/v1, as LeaderWorkerSet
# v0.9.0 (sets only) and v0.10.0 (the role scaler and its /scale) answer it.
LWS_V09='{"kind":"APIResourceList","groupVersion":"disaggregatedset.x-k8s.io/v1","resources":[{"name":"disaggregatedsets"},{"name":"disaggregatedsets/status"}]}'
LWS_V010='{"kind":"APIResourceList","groupVersion":"disaggregatedset.x-k8s.io/v1","resources":[{"name":"disaggregatedsetrolescalers"},{"name":"disaggregatedsetrolescalers/scale"},{"name":"disaggregatedsetrolescalers/status"},{"name":"disaggregatedsets"},{"name":"disaggregatedsets/status"}]}'
RAW_OUT=""
RAW_RC=0
RAW_ERR=""
reset() { DSET_OBJ="$(dset 0 External "")"; CRD_RC=0; LIST_RC=0; SO_OBJS=""; RAW_OUT="$LWS_V010"; RAW_RC=0; RAW_ERR=""; }

kubectl() {
    case "${1:-}" in
        get)
            case "${2:-}" in
                --raw)
                    [ "${3:-}" = "/apis/disaggregatedset.x-k8s.io/v1" ] || return 1
                    [ "$RAW_RC" -eq 0 ] || { printf '%s\n' "$RAW_ERR" >&2; return "$RAW_RC"; }
                    printf '%s' "$RAW_OUT" ;;
                crd) return "$CRD_RC" ;;
                disaggregatedsets)
                    [ "$LIST_RC" -eq 0 ] || return "$LIST_RC"
                    case "${3:-}" in
                        -n|-A) printf '%s' "$DSET_OBJ" | jq -c '{ items: [ . ] }' ;;
                        *) printf '%s' "$DSET_OBJ" ;;
                    esac ;;
                scaledobject) printf '%s' "${SO_OBJS:-{\"items\":[]}}" ;;
                *) printf '{"items":[]}' ;;
            esac ;;
        *) return 0 ;;
    esac
}

# --- cases --------------------------------------------------------------------

CASE="role rows: one record per role, mode defaulting to Static, slices to 1"
reset
before=$FAILED
rows="$(printf '%s' "$DSET_OBJ" | jq -c '{ items: [ . ] }' | so_dset_role_rows)"
assert_eq "$(printf '%s\n' "$rows" | wc -l | tr -d ' ')" "2" "records"
IFS=$'\037' read -r set idx r mode slices subroles labels _ <<< "$(printf '%s\n' "$rows" | sed -n 1p)"
assert_eq "$set/$idx/$r/$mode/$slices/$subroles" "glm/0/prefill/External/1/0" "prefill record"
assert_contains "$labels" "llm-d.ai/role=prefill"
IFS=$'\037' read -r set idx r mode _ <<< "$(printf '%s\n' "$rows" | sed -n 2p)"
assert_eq "$idx/$r/$mode" "1/decode/Static" "decode record"
[ "$FAILED" -eq "$before" ] && ok

CASE="role rows: the engine container, not the sidecar listed first, gives the model"
reset
before=$FAILED
rows="$(printf '%s' "$DSET_OBJ" | jq -c '{ items: [ . ] }' | so_dset_role_rows)"
IFS=$'\037' read -r _ _ _ _ _ _ _ _ _ args <<< "$(printf '%s\n' "$rows" | sed -n 1p)"
assert_eq "$(so_model_id "$args")" "org/GLM" "modelID"
[ "$FAILED" -eq "$before" ] && ok

CASE="role block: External at slices 1 is targetable; Static, slices and sub-roles say why not"
before=$FAILED
assert_eq "$(so_dset_role_block glm decode External 1 0)" "" "External, slices 1"
out="$(so_dset_role_block glm decode Static 1 0)"
assert_contains "$out" "scaling.mode: External"
assert_contains "$out" "DisaggregatedSetRoleScaler glm-decode"
# ...and the second path, for a role that cannot take the first.
assert_contains "$out" "scaling-manager.llmd.ai/role-writer=true"
assert_contains "$(so_dset_role_block glm decode External 2 0)" "slices: 1"
assert_contains "$(so_dset_role_block glm decode External 1 2)" "sub-roles"
[ "$FAILED" -eq "$before" ] && ok

CASE="plan: each role targets its role scaler <set>-<role>, never a child LeaderWorkerSet"
reset
before=$FAILED
out="$(so_discover_dsets serving 2>/dev/null)"
assert_contains "$out" "kind: DisaggregatedSetRoleScaler"
assert_contains "$out" "name: glm-prefill"
assert_contains "$out" "name: glm-decode"
assert_contains "$out" 'modelID: "org/GLM"'
assert_not_contains "$out" "kind: LeaderWorkerSet"
# prefill (External) is applied; decode (Static) is not, with the reason.
# Entries are written in role order: prefill first.
assert_contains "$(printf '%s\n' "$out" | grep 'apply:' | sed -n 1p)" 'apply: "yes"'
assert_contains "$(printf '%s\n' "$out" | grep 'apply:' | sed -n 2p)" 'apply: "no"'
assert_contains "$out" 'is in scaling.mode Static'
assert_eq "$(printf '%s\n' "$out" | grep -c 'apply: "no"')" "1" "entries written apply: no"
[ "$FAILED" -eq "$before" ] && ok

CASE="plan: notes read the role template, falling back to the worker template for the leader"
reset
before=$FAILED
out="$(so_discover_dsets serving 2>/dev/null)"
# decode has no preStop on its engine: the drain note names it. prefill has
# one, so exactly one drain note is written.
assert_contains "$out" "Scale-down will cut in-flight requests: vllm declares no preStop hook"
assert_eq "$(printf '%s\n' "$out" | grep -c 'declares no preStop hook')" "1" "drain notes"
[ "$FAILED" -eq "$before" ] && ok

CASE="plan: slices above 1 keeps an External role out, with the reason"
reset
DSET_OBJ="$(dset 2 External External)"
before=$FAILED
out="$(so_discover_dsets serving 2>/dev/null)"
assert_eq "$(printf '%s\n' "$out" | grep -c 'apply: "no"')" "2" "entries written apply: no"
assert_contains "$out" "spec.slices 2"
[ "$FAILED" -eq "$before" ] && ok

CASE="plan: no CRD, no entries and no error"
reset
CRD_RC=1
before=$FAILED
rc=0
out="$(so_discover_dsets serving 2>&1)" || rc=$?
assert_eq "$rc" "0" "return code"
assert_eq "$out" "" "output"
CRD_RC=0
out="$(so_discover_dsets serving 2>/dev/null)"
assert_contains "$out" "name: glm-decode"
[ "$FAILED" -eq "$before" ] && ok

CASE="scaledobject: a role scaler renders with the disaggregatedset apiVersion"
before=$FAILED
assert_eq "$(so_api_version DisaggregatedSetRoleScaler)" "disaggregatedset.x-k8s.io/v1" "apiVersion"
assert_eq "$(so_api_version LeaderWorkerSet)" "leaderworkerset.x-k8s.io/v1" "LWS apiVersion"
assert_eq "$(so_api_version Deployment)" "apps/v1" "Deployment apiVersion"
out="$(render_default_scaledobject serving DisaggregatedSetRoleScaler glm-decode org/GLM scaler:9090 1 4 2>/dev/null)"
assert_contains "$out" "apiVersion: disaggregatedset.x-k8s.io/v1"
assert_contains "$out" "kind: DisaggregatedSetRoleScaler"
assert_contains "$out" "name: glm-decode"
[ "$FAILED" -eq "$before" ] && ok

CASE="workload report: counts roles, reports the gap, writes no patch"
reset
before=$FAILED
err="$WORK/report.err"
out="$(so_workload_report_dsets serving 2>"$err")"
assert_eq "$out" "2 2" "roles checked, roles with a gap"
assert_contains "$(cat "$err")" "DisaggregatedSet serving/glm, role decode (spec.roles[1]): not patched"
assert_contains "$(cat "$err")" "drain: Scale-down will cut in-flight requests"
# Both roles keep their compile caches in /tmp; only decode lacks a preStop.
assert_contains "$(cat "$err")" "role prefill (spec.roles[0]): not patched"
assert_eq "$(grep -c 'drain: ' "$err")" "1" "drain findings"
assert_eq "$(grep -c 'caches: ' "$err")" "2" "cache findings"
[ "$FAILED" -eq "$before" ] && ok

CASE="workload report: a role whose engine caches are owned externally is judged on the variables its template leaves unset"
reset
DSET_OBJ="$(printf '%s' "$DSET_OBJ" | jq -c '
    .spec.roles[1].spec.leaderWorkerTemplate.workerTemplate.metadata.annotations = {"scaling-manager.llmd.ai/engine-cache-owner": "external"}
    | (.spec.roles[1].spec.leaderWorkerTemplate.workerTemplate.spec.containers[] | select(.name == "vllm") | .env) =
        [ {name: "VLLM_CACHE_ROOT", value: "/var/cache/jit/vllm"},
          {name: "FLASHINFER_WORKSPACE_BASE", value: "/var/cache/jit/flashinfer"},
          {name: "TRITON_CACHE_DIR", value: "/var/cache/jit/triton"},
          {name: "PYTHONPYCACHEPREFIX", value: "/var/cache/jit/pycache"} ]')"
before=$FAILED
err="$WORK/report-ext.err"
out="$(so_workload_report_dsets serving 2>"$err")"
# decode still lacks a preStop, so it is still a role with a gap -- but not
# a cache one: every variable is the owner's. prefill is as before.
assert_eq "$out" "2 2" "roles checked, roles with a gap"
assert_eq "$(grep -c 'caches: ' "$err")" "1" "cache findings"
assert_contains "$(grep -A3 'role prefill' "$err")" "caches: Every start compiles from nothing"
# One variable left unset: named, as the owner's to set.
DSET_OBJ="$(printf '%s' "$DSET_OBJ" | jq -c '(.spec.roles[1].spec.leaderWorkerTemplate.workerTemplate.spec.containers[] | select(.name == "vllm") | .env) |= map(select(.name != "TRITON_CACHE_DIR"))')"
out="$(so_workload_report_dsets serving 2>"$err")"
assert_eq "$(grep -c 'caches: ' "$err")" "2" "cache findings, one variable unset"
assert_contains "$(cat "$err")" "caches: Engine caches owned externally (scaling-manager.llmd.ai/engine-cache-owner=external), but the pod spec does not set TRITON_CACHE_DIR"
[ "$FAILED" -eq "$before" ] && ok

CASE="workload report: an unreadable listing is not a clean one"
reset
LIST_RC=1
before=$FAILED
rc=0
out="$(so_workload_report_dsets serving 2>/dev/null)" || rc=$?
assert_eq "$rc" "1" "return code"
LIST_RC=0
assert_eq "$(so_workload_report_dsets serving 2>/dev/null)" "2 2" "readable again"
[ "$FAILED" -eq "$before" ] && ok

CASE="namespaces: a set with a serving role is a namespace with model servers, counted once"
reset
before=$FAILED
assert_eq "$(so_namespaces_of_dsets)" "serving" "namespace"
# The namespace count for preflight. Deployments and LWS list nothing here.
assert_eq "$(llmsm_model_server_count serving)" "1" "model servers"
DSET_OBJ="$(printf '%s' "$DSET_OBJ" | jq '.spec.roles[].spec.leaderWorkerTemplate.workerTemplate.metadata.labels = {"app": "other"}')"
assert_eq "$(so_namespaces_of_dsets)" "" "namespace of a set with no serving role"
assert_eq "$(llmsm_model_server_count serving)" "0" "model servers of a set with no serving role"
[ "$FAILED" -eq "$before" ] && ok

CASE="plan: an External role already on the scaling manager is adopted; one on another scaler is left alone"
reset
before=$FAILED
SO_OBJS="$(jq -n '{ items: [
    { metadata: { name: "glm-prefill-so" },
      spec: { scaleTargetRef: { name: "glm-prefill" }, minReplicaCount: 2, maxReplicaCount: 6,
              triggers: [ { type: "external", metadata: { scalerAddress: "scaling-manager-external-scaler.llmsm.svc:9090", modelID: "org/GLM" } } ] } } ] }')"
out="$(so_discover_dsets serving 2>/dev/null)"
assert_contains "$(printf '%s\n' "$out" | grep -e '- apply:' | sed -n 1p)" 'apply: "adopt"'
assert_contains "$out" "Already targets the scaling manager (ScaledObject glm-prefill-so, min 2, max 6)"
SO_OBJS="$(jq -n '{ items: [
    { metadata: { name: "glm-prefill-cpu" },
      spec: { scaleTargetRef: { name: "glm-prefill" }, minReplicaCount: 1, maxReplicaCount: 4,
              triggers: [ { type: "cpu", metadata: { value: "80" } } ] } } ] }')"
out="$(so_discover_dsets serving 2>/dev/null)"
assert_contains "$(printf '%s\n' "$out" | grep -e '- apply:' | sed -n 1p)" 'apply: "no"'
assert_contains "$out" "Already scaled by ScaledObject glm-prefill-cpu"
assert_eq "$(printf '%s\n' "$out" | grep -c -e '- apply: "no"')" "2" "entries written apply: no"
SO_OBJS=""
[ "$FAILED" -eq "$before" ] && ok

CASE="plan: a model named by an env reference resolves from the role's env, and is refused without it"
reset
before=$FAILED
with_ref() {
    printf '%s' "$DSET_OBJ" | jq --argjson env "$1" '
        .spec.roles[0].spec.leaderWorkerTemplate.workerTemplate.spec.containers[1].args = ["exec vllm serve /m --served-model-name $(MODEL_NAME) --port 8200"]
        | .spec.roles[0].spec.leaderWorkerTemplate.workerTemplate.spec.containers[1].env = $env'
}
DSET_OBJ="$(with_ref '[{"name": "MODEL_NAME", "value": "org/FromEnv"}]')"
out="$(so_discover_dsets serving 2>/dev/null)"
assert_contains "$out" 'modelID: "org/FromEnv"'
assert_contains "$(printf '%s\n' "$out" | grep -e '- apply:' | sed -n 1p)" 'apply: "yes"'
reset
DSET_OBJ="$(with_ref '[]')"
out="$(so_discover_dsets serving 2>/dev/null)"
assert_contains "$out" 'the model is written as $(MODEL_NAME), a reference the engine container env does not answer'
assert_contains "$(printf '%s\n' "$out" | grep -e '- apply:' | sed -n 1p)" 'apply: "no"'
[ "$FAILED" -eq "$before" ] && ok

CASE="plan: a role with sub-roles is written apply: no, with the reason"
reset
before=$FAILED
DSET_OBJ="$(printf '%s' "$DSET_OBJ" | jq '.spec.roles[0].subRoles = [{"name": "a"}, {"name": "b"}]')"
out="$(so_discover_dsets serving 2>/dev/null)"
assert_contains "$(printf '%s\n' "$out" | grep -e '- apply:' | sed -n 1p)" 'apply: "no"'
assert_contains "$out" "sub-roles"
[ "$FAILED" -eq "$before" ] && ok

CASE="plan: a set with no serving role writes no entries"
reset
before=$FAILED
DSET_OBJ="$(printf '%s' "$DSET_OBJ" | jq '.spec.roles[].spec.leaderWorkerTemplate.workerTemplate.metadata.labels = {"app": "other"}')"
out="$(so_discover_dsets serving 2>/dev/null)"
assert_eq "$(printf '%s\n' "$out" | grep -c -e '- apply:')" "0" "entries"
assert_not_contains "$out" "glm-"
[ "$FAILED" -eq "$before" ] && ok

# Pinned as it is: the plan skips a namespace whose sets it cannot list, as it
# skips one without the CRD, while the workload report, which gates on a clean
# result, fails (case above).
CASE="plan: an unreadable listing writes nothing and returns 0"
reset
LIST_RC=1
before=$FAILED
rc=0
out="$(so_discover_dsets serving 2>/dev/null)" || rc=$?
assert_eq "$rc" "0" "return code"
assert_eq "$out" "" "output"
[ "$FAILED" -eq "$before" ] && ok

CASE="lws: v0.9.x serves sets without the role scaler: warn, name the release and the CRD-first upgrade"
reset
RAW_OUT="$LWS_V09"
before=$FAILED
rc=0
out="$(llmsm_report_lws_dset 2>&1)" || rc=$?
assert_eq "$rc" "0" "return code"
assert_contains "$out" "[WARNING]"
assert_contains "$out" "serves DisaggregatedSet but not DisaggregatedSetRoleScaler"
assert_contains "$out" "KEDA cannot scale DisaggregatedSet roles"
# The same words as scaletarget.RoleScalerUnservedMessage and its fix: in
# passive mode the roles are not observed, and the role writer is the way out.
assert_contains "$out" "In passive mode this means the roles are not observed at all"
assert_contains "$out" "for a passive model it reads them and writes nothing"
assert_contains "$out" "either turn on the role writer for the Static roles"
assert_contains "$out" "v0.10.0 or later"
assert_contains "$out" "kubectl apply --server-side --force-conflicts -f lws/crds"
assert_contains "$out" "--version 0.10.0"
assert_contains "$out" "LWS_CHART_VERSION"
assert_contains "$out" "the way it was installed (on OpenShift, through its operator)"
assert_contains "$out" "If it was installed with the upstream Helm chart"
[ "$FAILED" -eq "$before" ] && ok

CASE="lws: v0.10.0 serves the role scaler /scale: no warning"
reset
before=$FAILED
out="$(llmsm_report_lws_dset 2>&1)"
assert_not_contains "$out" "[WARNING]"
assert_contains "$out" "KEDA can scale DisaggregatedSet roles"
assert_contains "$out" "The API serves"
assert_contains "$out" "controller is v0.10.0 or later"
[ "$FAILED" -eq "$before" ] && ok

CASE="lws: no DisaggregatedSet API (LWS before v0.9.0, or none): nothing said"
reset
RAW_RC=1
RAW_ERR="Error from server (NotFound): the server could not find the requested resource"
before=$FAILED
out="$(llmsm_report_lws_dset 2>&1)"
assert_eq "$out" "" "output"
[ "$FAILED" -eq "$before" ] && ok

CASE="lws: discovery refused: says it was not checked, never that all is well"
reset
RAW_RC=1
RAW_ERR="Error from server (Forbidden): forbidden"
before=$FAILED
out="$(llmsm_report_lws_dset 2>&1)"
assert_contains "$out" "was not checked"
assert_not_contains "$out" "[INFO]"
[ "$FAILED" -eq "$before" ] && ok

# ----------------------------------------------------------------------------

if [ "$FAILED" -gt 0 ]; then
    printf '\n%d DisaggregatedSet assertion(s) failed.\n' "$FAILED" >&2
    exit 1
fi
if [ "$RAN" -ne "$EXPECT_CASES" ]; then
    printf '\n%d case(s) reported ok, %d expected -- the run did not finish.\n' \
        "$RAN" "$EXPECT_CASES" >&2
    exit 1
fi
printf '\nDisaggregatedSet checks passed (%d cases).\n' "$RAN"
