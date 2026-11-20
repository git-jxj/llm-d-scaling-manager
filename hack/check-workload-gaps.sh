#!/usr/bin/env bash
#
# Executes the workload-readiness path in deploy/lib/scaledobject.sh against
# fixture pod specs.
#
# It exists because `bash -n` is the only check these libraries had, and every
# defect it pins down parses cleanly: an engine chosen by name that landed on a
# routing proxy, a preStop check that counted hooks pod-wide, a "weights persist"
# test satisfied by a PVC mounted anywhere, and an unreadable object reported as
# healthy. None are syntax errors and all silently answer wrongly about a running
# workload.
#
# WRITING A CASE HERE: every case needs at least one POSITIVE assertion. An
# earlier version of this file was mutation-tested by deleting the library
# outright, and 10 of its 25 cases still printed `ok` -- `assert_not_contains`
# and `[ -z "$out" ]` are both satisfied by a function that does not exist. Where
# a case is about an absence, anchor it to something that must be PRESENT in the
# same output.
#
# kubectl is stubbed, so the assertions are about what the functions CONCLUDE.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Sourced through a CR-stripped copy. These files are LF in git, but a Windows
# checkout with core.autocrlf writes them CRLF, and bash then reports
# `$'\r': command not found` and stops at the first function definition -- which
# looks exactly like every function being missing. Same reason the repo verifies
# gofmt against an LF-normalised copy rather than the working tree.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh scaledobject.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
# common.sh's header says it plainly: "Requires vars: BLUE, GREEN, YELLOW, RED,
# NC." install.sh defines them; a harness that sources the library directly has
# to as well, or the first log_warning dies on an unbound variable under `set -u`
# -- which inside a command substitution kills only the subshell, so it surfaces
# as one test silently returning nothing rather than as an error.
BLUE='' GREEN='' YELLOW='' RED='' NC=''
# shellcheck source=/dev/null
source "$WORK/common.sh"
# shellcheck source=/dev/null
source "$WORK/scaledobject.sh"

# yq is not optional. The library cannot index or trim a patch document without
# it, and skipping those cases meant the run stayed green on a machine where the
# feature could not work at all.
for tool in jq yq; do
    command -v "$tool" >/dev/null 2>&1 || {
        printf 'FATAL: %s is required to run these checks.\n' "$tool" >&2
        exit 1
    }
done

FIXTURE=""        # JSON for `kubectl get <kind> <name>`
FIXTURE_RC=0      # or the failure it returns instead
LIST_FIXTURE=''   # JSON for `kubectl get <kind> -n <ns>`
LIST_RC=0
LIST_FIXTURE_LWS=''  # the leaderworkersets listing, when it differs from LIST_FIXTURE
CRD_RC=1          # no LeaderWorkerSet CRD unless a case says otherwise
PATCH_CALLS=0
PATCH_RC=0
PATCH_BODY=""     # the last --patch-file content kubectl was handed
PATCH_ARGS=""     # ...and the arguments it was called with
PVC_FIXTURE='{"items":[]}'   # JSON for `kubectl get pvc -n <ns>`
PVC_RC=0
DS_FIXTURE=""     # JSON for `kubectl get daemonset <name>`; empty: none
DS_RC=0           # or the failure the read returns instead
PODS_FIXTURE=''   # JSON for `kubectl get pods -n <ns> -l <sel>`; empty: none
PODS_RC=0
PODS_ERR=""
RS_FIXTURE=''     # JSON for `kubectl get replicasets -n <ns> -l <sel>`; empty: none
DSET_FIXTURE=''   # JSON for `kubectl get disaggregatedsets <name>`; empty: none.
                  # Not cleared by reset: the cases that set it clear it.
KUBECTL_TRACE=""  # a file the pods and logs reads are appended to: they run
                  # inside command substitutions, so a variable set there is lost
LOGS_FIXTURE=''   # the text `kubectl logs <pod>` prints
LOGS_RC=0
LOGS_ERR=""
# Per-NAME object fixtures, for the cases about a namespace holding more than
# one workload. A single FIXTURE cannot express "this one is readable and that
# one is not", nor tell two patch documents apart. "__RC1__" means the read
# fails for that name.
declare -A FIXTURE_MAP=()

kubectl() {
    local a f
    case "${1:-}" in
        get)
            case "${2:-}" in
                crd) return "$CRD_RC" ;;
                pvc|persistentvolumeclaims)
                    [ "$PVC_RC" -eq 0 ] || return "$PVC_RC"
                    # `get pvc -n <ns>` is the listing; `get pvc <name> -n <ns>`
                    # is one claim, and the real client answers NotFound (rc 1)
                    # for a name the listing does not carry. The stub used to
                    # print the listing for both, so "does this claim exist?"
                    # was yes for every name -- and a patch that asked it
                    # reported a claim that was never created as reused.
                    case "${3:-}" in ""|-*) : ;; *)
                        # ...and with -o json it is that one object, as the
                        # real client prints it, not the listing.
                        printf '%s' "$PVC_FIXTURE" \
                            | jq -e --arg n "$3" '[.items[]? | select(.metadata.name == $n)] | length > 0' >/dev/null 2>&1 \
                            || return 1
                        printf '%s' "$PVC_FIXTURE" | jq -c --arg n "$3" '[.items[] | select(.metadata.name == $n)] | first'
                        return 0 ;;
                    esac
                    printf '%s' "$PVC_FIXTURE"
                    ;;
                daemonset|daemonsets|ds)
                    # --ignore-not-found semantics: an absent object is empty
                    # output with rc 0; DS_RC is any other failure (Forbidden,
                    # a timeout), which the library must not read as absent.
                    [ "$DS_RC" -eq 0 ] || return "$DS_RC"
                    printf '%s' "$DS_FIXTURE"
                    ;;
                deployments|leaderworkersets)
                    # `get <kind> -n <ns>` is a listing; `get <kind> <name> …` is
                    # one object. The stub has to tell them apart or a case can
                    # pass on a response the real client would never give.
                    if [ "${3:-}" = "-n" ]; then
                        [ "$LIST_RC" -eq 0 ] || return "$LIST_RC"
                        if [ "$2" = "leaderworkersets" ] && [ -n "$LIST_FIXTURE_LWS" ]; then
                            printf '%s' "$LIST_FIXTURE_LWS"
                            return 0
                        fi
                        printf '%s' "$LIST_FIXTURE"
                    elif [ "${#FIXTURE_MAP[@]}" -gt 0 ] \
                         && [ -n "${FIXTURE_MAP[${3:-}]+set}" ]; then
                        [ "${FIXTURE_MAP[${3:-}]}" = "__RC1__" ] && return 1
                        printf '%s' "${FIXTURE_MAP[${3:-}]}"
                    else
                        [ "$FIXTURE_RC" -eq 0 ] || return "$FIXTURE_RC"
                        printf '%s' "$FIXTURE"
                    fi
                    ;;
                replicasets)
                    printf '%s' "$RS_FIXTURE"
                    ;;
                disaggregatedsets)
                    # DSET_FIXTURE: one set, read by name or listed; empty
                    # (the default) is a namespace with none.
                    [ -n "$DSET_FIXTURE" ] || return 0
                    case "${3:-}" in
                        -n|-A) printf '%s' "$DSET_FIXTURE" | jq -c '{ items: [ . ] }' ;;
                        *) printf '%s' "$DSET_FIXTURE" ;;
                    esac
                    ;;
                pods)
                    # `get pods -n <ns> -l <selector> -o json`: the replicas
                    # the KV-bytes proposal reads a log from.
                    [ -z "$KUBECTL_TRACE" ] || printf '%s\n' "$*" >> "$KUBECTL_TRACE"
                    [ "$PODS_RC" -eq 0 ] || { printf '%s\n' "$PODS_ERR" >&2; return "$PODS_RC"; }
                    printf '%s' "$PODS_FIXTURE"
                    ;;
                *) return 0 ;;
            esac
            ;;
        logs)
            [ -z "$KUBECTL_TRACE" ] || printf '%s\n' "$*" >> "$KUBECTL_TRACE"
            [ "$LOGS_RC" -eq 0 ] || { printf '%s\n' "$LOGS_ERR" >&2; return "$LOGS_RC"; }
            printf '%s' "$LOGS_FIXTURE"
            ;;
        patch)
            PATCH_CALLS=$((PATCH_CALLS + 1))
            PATCH_ARGS="$*"
            for a in "$@"; do
                case "$a" in
                    --patch-file=*) f="${a#--patch-file=}"
                                    PATCH_BODY="$(cat "$f" 2>/dev/null)" ;;
                esac
            done
            return "$PATCH_RC"
            ;;
        *) return 0 ;;
    esac
}

POD_DEPLOY='["spec","template"]'
POD_LWS='["spec","leaderWorkerTemplate","leaderTemplate"]'
FAILED=0
CASE=""
RAN=0
# Counted from the file rather than hand-maintained, so adding a case cannot
# leave the tally stale. It is the number of `CASE=` labels with a name; the
# tally at the bottom checks that every one of them reached an `ok`.
EXPECT_CASES="$(grep -c '^CASE="[^"]' "${BASH_SOURCE[0]}")"

fail() {
    printf 'FAIL  %s\n      %s\n' "$CASE" "$1" >&2
    FAILED=$((FAILED + 1))
}

ok() { RAN=$((RAN + 1)); printf 'ok    %s\n' "$CASE"; }
# skip REASON: the case checked nothing here. It is counted apart from ok,
# so a skipped case never reads as a passing one.
SKIPPED=0
# The places a case may skip, counted from the file like EXPECT_CASES.
SKIP_SITES="$(grep -c '^ *skip "' "${BASH_SOURCE[0]}")"
skip() { SKIPPED=$((SKIPPED + 1)); printf 'SKIP  %s: %s\n' "$CASE" "$1" >&2; }

# gaps_field reads one field the way PRODUCTION reads it: `IFS read`, not `cut`.
# The difference is the whole point of one of the cases below -- `cut` is
# line-oriented and re-emits a delimiterless line whole, so it cannot see the
# truncation that `read` suffers when a field still contains newlines.
gaps_field() {
    local idx="$1" gaps f1 f2 f3 f4 f5 f6 f7 f8 f9 f10
    gaps="$(so_workload_gaps ns deployments w "$POD_DEPLOY")" || return 1
    IFS=$'\037' read -r f1 f2 f3 f4 f5 f6 f7 f8 f9 f10 <<< "$gaps"
    case "$idx" in
        1) printf '%s' "$f1" ;; 2) printf '%s' "$f2" ;; 3) printf '%s' "$f3" ;;
        4) printf '%s' "$f4" ;; 5) printf '%s' "$f5" ;; 6) printf '%s' "$f6" ;;
        7) printf '%s' "$f7" ;; 8) printf '%s' "$f8" ;; 9) printf '%s' "$f9" ;;
        10) printf '%s' "$f10" ;;
    esac
}

assert_field() {
    local idx="$1" want="$2" got
    if ! got="$(gaps_field "$idx")"; then
        fail "so_workload_gaps returned non-zero"
        return
    fi
    [ "$got" = "$want" ] || fail "field $idx: want '$want', got '$got'"
}

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

# --- fixtures ---------------------------------------------------------------

f_vllm_plain() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

f_sglang_proxy_has_hook() {
    # The engine is called `server` and the container that HAS a preStop hook is
    # the proxy. A name regex picks the proxy; counting hooks pod-wide reports
    # this pod as already draining.
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "containers":[
          {"name":"routing-proxy","image":"quay.io/x/proxy:1",
           "lifecycle":{"preStop":{"exec":{"command":["sleep","10"]}}}},
          {"name":"server","image":"lmsysorg/sglang:latest",
           "args":["python3","-m","sglang.launch_server","--model-path","meta-llama/Llama-3.1-8B"]}]}}}}'
}

f_mount_without_hf_home() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                       "command":["/bin/sh","-c"],
                       "args":["vllm serve Qwen/Qwen3-0.6B --port 8000"]}]}}}}'
}

# Nothing wrong with it: drains, weights on the claim, and the engine caches
# on it too (a second cache half is the same claim under another directory,
# which is the layout the benchmark harness uses), Python bytecode included.
f_cached_properly() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"},
                              {"name":"VLLM_CACHE_ROOT","value":"/model-cache/engine-cache/vllm"},
                              {"name":"PYTHONPYCACHEPREFIX","value":"/model-cache/engine-cache/pycache/5f8fcd7456cd"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

# A PVC mounted at one path while the engine downloads to another. This is the
# misconfiguration the weights check exists to catch, and the one a `startswith`
# predicate that compared a value with itself reported as solved.
f_cache_mounted_elsewhere() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HOME","value":"/root/.cache/huggingface"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/workspace"}],
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

# --download-dir overrides HF_HOME, and HF_HUB_CACHE is read alongside it.
f_download_dir_on_volume() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HUB_CACHE","value":"/root/.cache"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                       "args":["vllm","serve","Qwen/Qwen3-0.6B","--download-dir","/model-cache/dl"]}]}}}}'
}

f_local_path() {
    FIXTURE='{"spec":{"template":{"spec":{
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "command":["/bin/sh","-c"],
                       "args":["vllm serve /models/qwen3-0.6b"]}]}}}}'
}

f_long_grace() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":300,
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

f_no_engine() {
    FIXTURE='{"spec":{"template":{"spec":{
        "containers":[{"name":"sidecar","image":"quay.io/x/proxy:1","args":["--listen",":80"]}]}}}}'
}

# An LWS-shaped object: the pod template lives under leaderWorkerTemplate. A
# Deployment body read through the LWS path yields {} and lands on the "no engine"
# branch, so a case using one proves nothing about the LWS skip.
f_lws() {
    FIXTURE='{"spec":{"leaderWorkerTemplate":{"leaderTemplate":{"spec":{
        "terminationGracePeriodSeconds":30,
        "containers":[{"name":"vllm","image":"vllm/vllm-openai:glm52",
                       "args":["vllm","serve","zai-org/GLM-4.5"]}]}}}}}'
}

# Copied from a running llm-d deployment on OpenShift (modelservice chart,
# uriProtocol: pvc). Two things about it broke assumptions the invented fixtures
# could not: the command is a multi-line block scalar, and the weights are served
# FROM the volume by local path while --served-model-name advertises a repo id.
f_llmd_real() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}},
                   {"name":"dshm"},{"name":"shared-config"}],
        "containers":[{"name":"vllm","image":"docker.io/vllm/vllm-openai:v0.26.0",
                       "env":[{"name":"HF_HOME","value":"/tmp/huggingface"}],
                       "volumeMounts":[{"name":"dshm","mountPath":"/dev/shm"},
                                       {"name":"shared-config","mountPath":"/shared-config"},
                                       {"name":"model-storage","mountPath":"/model-cache"}],
                       "command":["/bin/bash","-c"],
                       "args":["/bin/true ; vllm serve /model-cache/models/Qwen/Qwen3-0.6B \\\n  --host 0.0.0.0 \\\n  --served-model-name Qwen/Qwen3-0.6B \\\n  --port 8000\n"]}]}}}}'
}

# The same block-scalar shape, genuinely fetching from the Hub.
f_llmd_real_downloads() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"dshm"}],
        "containers":[{"name":"vllm","image":"docker.io/vllm/vllm-openai:v0.26.0",
                       "env":[{"name":"HF_HOME","value":"/tmp/huggingface"}],
                       "volumeMounts":[{"name":"dshm","mountPath":"/dev/shm"}],
                       "command":["/bin/bash","-c"],
                       "args":["/bin/true ; vllm serve Qwen/Qwen3-0.6B \\\n  --host 0.0.0.0 \\\n  --served-model-name my-alias \\\n  --port 8000\n"]}]}}}}'
}

f_unreadable() { FIXTURE=""; FIXTURE_RC=1; }

reset() {
    FIXTURE=""; FIXTURE_RC=0; LIST_FIXTURE=''; LIST_FIXTURE_LWS=''; LIST_RC=0; CRD_RC=1
    PATCH_CALLS=0; PATCH_RC=0; PATCH_BODY=""; PATCH_ARGS=""
    PVC_FIXTURE='{"items":[]}'; PVC_RC=0; DS_FIXTURE=""; DS_RC=0
    PODS_FIXTURE=''; PODS_RC=0; PODS_ERR=""; RS_FIXTURE=''; LOGS_FIXTURE=''; LOGS_RC=0; LOGS_ERR=""; KUBECTL_TRACE=""
    FIXTURE_MAP=()
}

# --- canary -----------------------------------------------------------------
#
# Proves the harness can report a failure at all. Without it, a bookkeeping
# mistake that suppressed every `fail` would look exactly like a clean run.
CASE="the harness itself reports failures (canary)"
before=$FAILED
# stderr is dropped for this one call: the assertion is MEANT to fail, and
# printing its FAIL line would train a reader to ignore FAIL lines.
assert_eq "left" "right" "canary" 2>/dev/null
if [ "$FAILED" -eq $((before + 1)) ]; then
    FAILED="$before"      # expected; un-count it
    ok
else
    FAILED=$((before + 1))
    printf 'FAIL  %s\n      assert_eq did not register a failure\n' "$CASE" >&2
fi

# --- detection --------------------------------------------------------------

CASE="plain vLLM: engine, no hook, default grace, nothing cached"
reset; f_vllm_plain
before=$FAILED
assert_field 1 "vllm"
assert_field 2 "0"
assert_field 3 "30"
assert_field 5 "0"
[ "$FAILED" -eq "$before" ] && ok

CASE="SGLang: engine is 'server' by image, not the proxy that has the hook"
reset; f_sglang_proxy_has_hook
before=$FAILED
assert_field 1 "server"
assert_field 2 "0"      # the ENGINE's hook count, which is what decides
[ "$FAILED" -eq "$before" ] && ok

CASE="llm-d inference simulator: an engine for the drain half, no cache halves"
reset
FIXTURE='{"spec":{"template":{"spec":{"containers":[
  {"name":"vllm","image":"ghcr.io/llm-d/llm-d-inference-sim:v0.9.0",
   "args":["--model","model-a","--port","8000"]}]}}}}'
before=$FAILED
assert_field 1 "vllm"   # it was skipped as no engine at all
assert_field 2 "0"
assert_field 6 ""       # no argv: it fetches no weights
assert_field 7 ""       # not vLLM: no compile caches
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "preStop"
assert_not_contains "$doc" "HF_HOME"
assert_not_contains "$doc" "VLLM_CACHE_ROOT"
[ "$FAILED" -eq "$before" ] && ok

CASE="a PVC mounted with no HF_HOME does not make weights persist"
reset; f_mount_without_hf_home
before=$FAILED
assert_field 4 "1"      # a PVC is present...
assert_field 5 "0"      # ...and the engine still downloads outside it
[ "$FAILED" -eq "$before" ] && ok

CASE="HF_HOME under a mounted volume counts as cached"
reset; f_cached_properly
before=$FAILED
assert_field 5 "1"
assert_field 2 "1"
[ "$FAILED" -eq "$before" ] && ok

CASE="a PVC mounted somewhere ELSE does not count as cached"
reset; f_cache_mounted_elsewhere
before=$FAILED
# The predicate must compare the download dir against each MOUNT. Comparing it
# with itself -- `$dldir | startswith(.)` -- is true for every mount, and made
# this exact misconfiguration report as solved.
assert_field 5 "0"
assert_field 4 "1"
[ "$FAILED" -eq "$before" ] && ok

CASE="--download-dir overrides HF_HUB_CACHE and is matched against the mounts"
reset; f_download_dir_on_volume
before=$FAILED
assert_field 5 "1"      # /model-cache/dl is under the /model-cache mount
[ "$FAILED" -eq "$before" ] && ok

CASE="unreadable object returns non-zero and prints nothing"
reset; f_unreadable
before=$FAILED
out="$(so_workload_gaps ns deployments w "$POD_DEPLOY")" && fail "expected non-zero"
[ -z "$out" ] || fail "expected no output, got: $out"
# Positive anchor: the SAME call on a readable object must produce a record, so
# a deleted function cannot satisfy this case.
reset; f_vllm_plain
assert_contains "$(so_workload_gaps ns deployments w "$POD_DEPLOY")" "vllm"
[ "$FAILED" -eq "$before" ] && ok

CASE="a multi-line block scalar keeps its later flags (read stops at newline 1)"
reset; f_llmd_real
before=$FAILED
argv="$(gaps_field 6)" || fail "so_workload_gaps returned non-zero"
assert_contains "$argv" "--served-model-name"
assert_contains "$argv" "--port"
[ "$FAILED" -eq "$before" ] && ok

# --- the model source -------------------------------------------------------

CASE="so_model_source reads the source, not the advertised name"
before=$FAILED
assert_eq "$(so_model_source 'vllm serve /model-cache/models/Qwen --served-model-name Qwen/Qwen3-0.6B')" \
          "/model-cache/models/Qwen" "served-model-name must not win"
assert_eq "$(so_model_source 'vllm serve Qwen/Qwen3-0.6B')" "Qwen/Qwen3-0.6B" "positional"
assert_eq "$(so_model_source 'vllm --model=Qwen/Qwen3-0.6B')" "Qwen/Qwen3-0.6B" "--model="
# SGLang. Omitting --model-path gave every SGLang engine a silent pass.
assert_eq "$(so_model_source 'python3 -m sglang.launch_server --model-path meta-llama/Llama-3.1-8B')" \
          "meta-llama/Llama-3.1-8B" "--model-path"
[ "$FAILED" -eq "$before" ] && ok

CASE="so_model_source does not glob against the working directory"
before=$FAILED
# `for tok in $args` splits AND globs. Run from a directory with files in it and
# a bare glob inside someone else's entrypoint becomes the answer.
tmpd="$(mktemp -d)"; : > "$tmpd/decoy.log"
assert_eq "$(cd "$tmpd" && so_model_source 'sh -c rm -f *.log ; vllm serve Qwen/Qwen3-0.6B')" \
          "Qwen/Qwen3-0.6B" "glob leaked into the parse"
rm -rf "$tmpd"
[ "$FAILED" -eq "$before" ] && ok

# --- notes ------------------------------------------------------------------

CASE="drain note fires for the proxy-has-a-hook pod"
reset; f_sglang_proxy_has_hook
before=$FAILED
assert_contains "$(so_drain_note ns deployments w "$POD_DEPLOY")" "server declares no preStop hook"
[ "$FAILED" -eq "$before" ] && ok

CASE="drain note is silent when the engine has a hook"
reset; f_cached_properly
before=$FAILED
note="$(so_drain_note ns deployments w "$POD_DEPLOY")"
[ -z "$note" ] || fail "expected silence, got: $note"
# Anchor: the same function must still speak for a pod that needs it.
reset; f_vllm_plain
assert_contains "$(so_drain_note ns deployments w "$POD_DEPLOY")" "no preStop hook"
[ "$FAILED" -eq "$before" ] && ok

CASE="weights note fires for the sh -c form (used to match on /bin/sh)"
reset; f_mount_without_hf_home
before=$FAILED
assert_contains "$(so_weights_note ns deployments w "$POD_DEPLOY")" "Weights do not persist"
[ "$FAILED" -eq "$before" ] && ok

CASE="weights note fires for an SGLang engine launched by --model-path"
reset; f_sglang_proxy_has_hook
before=$FAILED
assert_contains "$(so_weights_note ns deployments w "$POD_DEPLOY")" "meta-llama/Llama-3.1-8B"
[ "$FAILED" -eq "$before" ] && ok

CASE="weights note is silent for a local model path"
reset; f_local_path
before=$FAILED
note="$(so_weights_note ns deployments w "$POD_DEPLOY")"
[ -z "$note" ] || fail "expected silence, got: $note"
reset; f_mount_without_hf_home
assert_contains "$(so_weights_note ns deployments w "$POD_DEPLOY")" "Weights do not persist"
[ "$FAILED" -eq "$before" ] && ok

CASE="an unreadable object is reported as unknown, never as healthy"
reset; f_unreadable
before=$FAILED
note="$(so_drain_note ns deployments w "$POD_DEPLOY")"
assert_contains "$note" "unknown"
assert_not_contains "$note" "no preStop hook"
[ "$FAILED" -eq "$before" ] && ok

# --- emitted patch ----------------------------------------------------------

CASE="patch names the engine container, both halves, HF_HOME with the mount"
reset; f_mount_without_hf_home
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "- name: vllm"
assert_contains "$doc" "preStop"
assert_contains "$doc" "HF_HOME"
assert_contains "$doc" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="patch never lowers an existing longer grace period"
reset; f_long_grace
before=$FAILED
assert_contains "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" \
    "terminationGracePeriodSeconds: 300"
[ "$FAILED" -eq "$before" ] && ok

CASE="weights served BY LOCAL PATH need no cache, whatever name is advertised"
reset; f_llmd_real
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "preStop"          # the drain half is still owed
assert_not_contains "$doc" "claimName: model-pvc"    # ...but not a second cache
assert_not_contains "$doc" "HF_HOME"
assert_not_contains "$doc" "#   cache:"
[ "$FAILED" -eq "$before" ] && ok

CASE="a re-downloading workload is warned about ON THE CONSOLE, not just in the file"
reset; f_llmd_real_downloads
before=$FAILED
# The emitted file always carried this; the console said only "1 workload(s)
# need something". A cost that only shows up the moment a replica is added, and
# reads as the autoscaler being slow, has to be said where someone will see it.
warn="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1 >/dev/null)"
assert_contains "$warn" "RE-DOWNLOADS ITS WEIGHTS"
assert_contains "$warn" "Qwen/Qwen3-0.6B"
[ "$FAILED" -eq "$before" ] && ok

CASE="a workload whose weights DO persist is not warned about"
reset; f_cached_properly
before=$FAILED
warn="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1 >/dev/null)"
assert_not_contains "$warn" "RE-DOWNLOADS"
# Anchor, so a silenced warning cannot pass this pair by never firing at all.
reset; f_llmd_real_downloads
assert_contains "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1 >/dev/null)" "RE-DOWNLOADS"
[ "$FAILED" -eq "$before" ] && ok

CASE="the emitted file carries a PVC to fill in, and still parses"
reset; f_llmd_real_downloads
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "kind: PersistentVolumeClaim"
assert_contains "$doc" "ReadWriteMany"
# Commented, not a live document: a claim with a placeholder size would be
# rejected, and a claim we guessed could break scale-up outright.
d2="$(mktemp)"; printf '%s\n' "$doc" > "$d2"
yq eval '.' "$d2" >/dev/null 2>&1 || fail "the emitted file no longer parses as YAML"
assert_eq "$(yq eval-all '[.] | length' "$d2" 2>/dev/null | tail -1)" "1" "documents in the file"
rm -f "$d2"
[ "$FAILED" -eq "$before" ] && ok

CASE="the same shape genuinely downloading from the Hub does get a cache"
reset; f_llmd_real_downloads
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName"
assert_contains "$doc" "HF_HOME"
[ "$FAILED" -eq "$before" ] && ok

CASE="patch emits no cache half for a local model path"
reset; f_local_path
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "preStop"          # anchor: a document was produced
assert_not_contains "$doc" "claimName: model-pvc"
assert_not_contains "$doc" "HF_HOME"
assert_not_contains "$doc" "#   cache:"
[ "$FAILED" -eq "$before" ] && ok

CASE="nothing is emitted for a workload that is already fine"
reset; f_cached_properly
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
[ -z "$doc" ] || fail "expected no document, got: $doc"
reset; f_vllm_plain
assert_contains "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" "apiVersion"
[ "$FAILED" -eq "$before" ] && ok

CASE="no identifiable engine is skipped (rc 2), not patched at container 0"
reset; f_no_engine
before=$FAILED
doc_rc=0
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" || doc_rc=$?
assert_eq "$doc_rc" "2" "return code"
[ -z "$doc" ] || fail "expected no document, got: $doc"
[ "$FAILED" -eq "$before" ] && ok

CASE="LeaderWorkerSet: the cache halves are written, the drain half is not"
reset; f_lws
before=$FAILED
# The detector must be able to READ this object -- otherwise the case would pass
# through the "no engine" branch and prove nothing about the LWS path.
assert_contains "$(so_workload_gaps ns leaderworkersets w "$POD_LWS")" "vllm"
doc_rc=0
doc="$(so_workload_patch_doc ns leaderworkersets w "$POD_LWS" 2>"$WORK/lws.err")" || doc_rc=$?
assert_eq "$doc_rc" "0" "return code"
assert_contains "$doc" "kind: LeaderWorkerSet"
assert_contains "$doc" "- name: VLLM_CACHE_ROOT"
assert_not_contains "$doc" "lifecycle:"
assert_not_contains "$doc" "terminationGracePeriodSeconds:"
assert_contains "$(cat "$WORK/lws.err")" "no drain half"
[ "$FAILED" -eq "$before" ] && ok

CASE="an unreadable object emits nothing and returns rc 3"
reset; f_unreadable
before=$FAILED
doc_rc=0
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" || doc_rc=$?
assert_eq "$doc_rc" "3" "return code"
[ -z "$doc" ] || fail "expected no document, got: $doc"
[ "$FAILED" -eq "$before" ] && ok

# --- the patch index --------------------------------------------------------

CASE="the index lists every document as three readable fields"
before=$FAILED
multi="$(mktemp)"
cat > "$multi" <<'YAML'
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: a
  namespace: ns1
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: b
  namespace: ns2
YAML
# Counting lines is not enough. yq does not decode a  escape the way jq
# does, and emitted one unsplittable field per line -- two lines, correct count,
# and `read -r ns name` left the name EMPTY, so the apply loop ran zero times.
# The kind is the third field: a Deployment and a LeaderWorkerSet may share a
# name in one namespace.
pairs=""
while read -r i_ns i_name i_kind; do
    [ -n "$i_name" ] || fail "index line did not split into three fields"
    pairs="$pairs[$i_ns/$i_name/$i_kind]"
done < <(so_workload_patch_index "$multi")
assert_eq "$pairs" "[ns1/a/Deployment][ns2/b/Deployment]" "index pairs"
rm -f "$multi"
[ "$FAILED" -eq "$before" ] && ok

# --- dropping the weights half ----------------------------------------------

CASE="drop_cache keeps the drain half and removes the weights half"
before=$FAILED
d="$(mktemp)"
cat > "$d" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: w, namespace: ns1}
spec:
  template:
    spec:
      terminationGracePeriodSeconds: 120
      containers:
      - name: vllm
        lifecycle:
          preStop:
            exec:
              command: ["/bin/sh", "-c", "sleep 45"]
        env:
        - name: HF_HOME
          value: /model-cache/huggingface
        volumeMounts:
        - name: model-storage
          mountPath: /model-cache
      volumes:
      - name: model-storage
        persistentVolumeClaim:
          claimName: model-pvc
YAML
so_workload_patch_drop_cache "$d" || fail "expected the drain half to survive"
left="$(cat "$d")"
assert_contains "$left" "terminationGracePeriodSeconds: 120"
assert_contains "$left" "preStop"
assert_not_contains "$left" "claimName"
assert_not_contains "$left" "HF_HOME"
assert_not_contains "$left" "volumeMounts"
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

CASE="a cache-only document is skipped, not applied as an empty patch"
before=$FAILED
d="$(mktemp)"
cat > "$d" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: w, namespace: ns1}
spec:
  template:
    spec:
      containers:
      - name: vllm
        env:
        - name: HF_HOME
          value: /model-cache/huggingface
      volumes:
      - name: model-storage
        persistentVolumeClaim:
          claimName: model-pvc
YAML
drop_rc=0
so_workload_patch_drop_cache "$d" || drop_rc=$?
# EXACTLY 1, not merely non-zero. A function that does not exist returns 127,
# and this case passed against a zero-byte library on `&& fail` alone.
assert_eq "$drop_rc" "1" "return code"
assert_contains "$(cat "$d")" "HF_HOME"   # anchor: the file was left intact
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

# --- which claim the patch names --------------------------------------------
#
# A namespace that already has a shared cache under another name would otherwise
# be told to use `model-pvc`, and the obvious response is to provision a SECOND
# terabyte for weights that are already on the cluster.

pvc_list() {
    # $1..$n: "<name>:<accessMode>[:<phase>]"
    local items="" spec name mode phase
    for spec in "$@"; do
        name="${spec%%:*}"; mode="${spec#*:}"; phase="Bound"
        case "$mode" in *:*) phase="${mode#*:}"; mode="${mode%%:*}" ;; esac
        items="${items:+$items,}{\"metadata\":{\"name\":\"$name\"},\"spec\":{\"accessModes\":[\"$mode\"]},\"status\":{\"phase\":\"$phase\"}}"
    done
    printf '{"items":[%s]}' "$items"
}

CASE="an existing shared claim is reused instead of proposing a second one"
reset; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list llm-d-model-cache:ReadWriteMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: llm-d-model-cache"
assert_contains "$doc" "already exists in ns"
assert_not_contains "$doc" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="the only RWX claim is not assumed to be a model cache"
reset; f_llmd_real_downloads
before=$FAILED
# A benchmark namespace has exactly one RWX claim and it is the RESULTS volume.
# Mounting that at /model-cache with HF_HOME inside it is wrong, and with the
# weights opt-in it is wrong on a live workload. One candidate is not the same
# as one candidate that is a model cache.
PVC_FIXTURE="$(pvc_list llm-d-benchmark-results:ReadWriteMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_not_contains "$doc" "llm-d-benchmark-results"
assert_contains "$doc" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="a ReadOnlyMany claim is not proposed for a path we write to"
reset; f_llmd_real_downloads
before=$FAILED
# The emitted patch points HF_HOME INTO the mount. A ROX volume mounts
# read-only, so reusing one turns a re-download into a crash on first write.
PVC_FIXTURE="$(pvc_list shared-model-datasets:ReadOnlyMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_not_contains "$doc" "shared-model-datasets"
assert_contains "$doc" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="a discovered claim is never followed by instructions to create one"
reset; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list llm-d-model-cache:ReadWriteMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: llm-d-model-cache"
# The emitted file used to mount the discovered claim and then tell the reader
# to create `model-pvc` -- the second claim this feature exists to prevent, and
# not even the one the patch mounts.
assert_not_contains "$doc" "kind: PersistentVolumeClaim"
assert_not_contains "$doc" "make model-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="a ReadWriteOnce claim is not proposed as a shared cache"
reset; f_llmd_real_downloads
before=$FAILED
# RWO binds to one node, so reusing it would produce replicas that cannot
# schedule -- the failure this whole area exists to avoid.
PVC_FIXTURE="$(pvc_list scratch:ReadWriteOnce)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: model-pvc"
assert_contains "$doc" "does NOT exist yet"
[ "$FAILED" -eq "$before" ] && ok

CASE="two candidate claims are ambiguous, so neither is guessed"
reset; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list alpha:ReadWriteMany beta:ReadWriteMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="among several, exactly one named for the job is taken"
reset; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list results:ReadWriteMany model-cache:ReadWriteMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: model-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="an unbound claim is not reused"
reset; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list somewhere:ReadWriteMany:Pending)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="an explicit LLMSM_MODEL_PVC_NAME beats anything discovered"
reset; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list llm-d-model-cache:ReadWriteMany)"
doc="$(LLMSM_MODEL_PVC_NAME=mine so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: mine"
[ "$FAILED" -eq "$before" ] && ok

CASE="a forbidden PVC listing falls back rather than claiming none exist"
reset; f_llmd_real_downloads
before=$FAILED
PVC_RC=1
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: model-pvc"
assert_contains "$doc" "preStop"    # anchor: a document was still produced
[ "$FAILED" -eq "$before" ] && ok

# --- plan notes -------------------------------------------------------------
#
# A workload usually has more than one thing wrong with it, and the notes used to
# be joined into a single run-on sentence -- so the second problem, often the
# expensive one, sat mid-paragraph where nobody reads.

CASE="one plan note renders as one line"
before=$FAILED
# The case that caught the real bug: `printf '%s' | while read` never runs its
# body for a final line with no terminator, so a SINGLE note produced no output
# at all. Both counts have to be asserted; only the pair is meaningful.
n="$(so_plan_entry yes ns Deployment w model 1 10 10 "" "" "" "only note" | grep -c '# note:')"
assert_eq "$n" "1" "note lines"
[ "$FAILED" -eq "$before" ] && ok

CASE="two plan notes render as two lines, not one paragraph"
before=$FAILED
two="first note"$'\036'"second note"
out="$(so_plan_entry yes ns Deployment w model 1 10 10 "" "" "" "$two")"
assert_eq "$(printf '%s' "$out" | grep -c '# note:')" "2" "note lines"
assert_contains "$out" "# note: first note"
assert_contains "$out" "# note: second note"
[ "$FAILED" -eq "$before" ] && ok

CASE="an entry with no note renders none"
before=$FAILED
assert_eq "$(so_plan_entry yes ns Deployment w model 1 10 10 "" "" "" "" | grep -c '# note:')" "0" "note lines"
# Anchor: the entry itself must still be produced.
assert_contains "$(so_plan_entry yes ns Deployment w model 1 10 10 "" "" "" "")" "namespace: ns"
[ "$FAILED" -eq "$before" ] && ok

# --- the verify-* health checks ----------------------------------------------
#
# These report on a live cluster and are the kind of code that passes review by
# inspection and fails in the field: every branch is a printf and a counter, and
# the interesting ones are the branches nobody has on their cluster.

# The stub is SAVED and restored below rather than retyped: a hand-written copy
# silently dropped the per-name FIXTURE_MAP branch and broke a later case.
orig_kubectl="$(declare -f kubectl)"

CASE="verify-fma: a namespace that cannot be listed is not an empty one"
reset
before=$FAILED
# Piping a failed `kubectl get` into `wc -l` yields 0, which read as "no launcher
# pods here" -- so a Forbidden listing was reported as nothing to check and the
# run still exited 0.
kubectl() {
    case "${1:-}${2:-}" in
        getpods) return 1 ;;          # the listing is refused
        *) return 0 ;;
    esac
}
out=""; rc=0
out="$(LLMSM_DEFAULT_SO_NS=ns1 so_verify_fma 2>&1)" || rc=$?
assert_contains "$out" "UNREADABLE"
assert_contains "$out" "NOT a report that they are scraped"
assert_eq "$rc" "1" "return code"
assert_not_contains "$out" "0          -"
[ "$FAILED" -eq "$before" ] && ok

CASE="verify-fma: a genuinely empty namespace is reported as such, and passes"
reset
before=$FAILED
kubectl() {
    case "${1:-}${2:-}" in
        getpods) printf '' ;;         # listing succeeds, no launchers
        *) return 0 ;;
    esac
}
out=""; rc=0
out="$(LLMSM_DEFAULT_SO_NS=ns1 so_verify_fma 2>&1)" || rc=$?
assert_eq "$rc" "0" "return code"
assert_contains "$out" "with no launcher pods"
assert_not_contains "$out" "UNREADABLE"
[ "$FAILED" -eq "$before" ] && ok

# Restore the fixture stub for anything after this.
unset -f kubectl
eval "$orig_kubectl"

# --- the driver -------------------------------------------------------------
#
# llmsm_workload_patch's return code is load-bearing: benchmark-standup runs it as
# `... || echo WARNING`, and an earlier version returned 0 on every path, so the
# warning could not fire for the failure it named.

DRIVER_RC=0; DRIVER_OUT=""; DRIVER_LOG=""

serving_list() {
    printf '%s' '{"items":[{"metadata":{"name":"w","labels":{"llm-d.ai/role":"decode"}}}]}'
}

run_driver() {
    local dir; dir="$(mktemp -d)"
    DRIVER_OUT="$dir/patch.yaml"; DRIVER_LOG="$dir/log"
    DRIVER_RC=0
    LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_FILE="$DRIVER_OUT" \
        llmsm_workload_patch >"$DRIVER_LOG" 2>&1 || DRIVER_RC=$?
}

CASE="a namespace that cannot be listed returns 1, not 'all clean'"
reset; LIST_RC=1
before=$FAILED
run_driver
assert_eq "$DRIVER_RC" "1" "return code"
[ -f "$DRIVER_OUT" ] && fail "wrote a patch file for a namespace it could not read"
assert_contains "$(cat "$DRIVER_LOG")" "not being reported as clean"
[ "$FAILED" -eq "$before" ] && ok

CASE="an object that cannot be READ is not reported as healthy"
reset; LIST_FIXTURE="$(serving_list)"; FIXTURE_RC=1
before=$FAILED
run_driver
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$DRIVER_LOG")" "NOT a report that they are healthy"
assert_not_contains "$(cat "$DRIVER_LOG")" "already drain on scale-down"
[ "$FAILED" -eq "$before" ] && ok

CASE="an empty namespace says nothing was checked, not that all is well"
reset; LIST_FIXTURE='{"items":[]}'
before=$FAILED
run_driver
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$DRIVER_LOG")" "No model servers found in scope"
[ "$FAILED" -eq "$before" ] && ok

CASE="a converged workload reports the all-clear and clears its own stale file"
reset; LIST_FIXTURE="$(serving_list)"; f_cached_properly
before=$FAILED
stale="$(mktemp)"; so_workload_patch_header > "$stale"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_FILE="$stale" llmsm_workload_patch >/dev/null 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
[ -f "$stale" ] && { fail "a file this tool wrote should have been removed"; rm -f "$stale"; }
[ "$FAILED" -eq "$before" ] && ok

CASE="a file this tool did NOT write is never deleted"
reset; LIST_FIXTURE="$(serving_list)"; f_cached_properly
before=$FAILED
mine="$(mktemp)"; printf 'hand written, do not delete\n' > "$mine"
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_FILE="$mine" \
    llmsm_workload_patch >"$WORK/nd.log" 2>&1 || true
# Anchor: the tool must have REACHED the decision and said so. Without it a
# library that does not run at all also leaves the file untouched -- this case
# printed `ok` against a zero-byte library.
assert_contains "$(cat "$WORK/nd.log")" "was not written by this tool"
assert_contains "$(cat "$mine" 2>/dev/null || echo GONE)" "hand written"
rm -f "$mine"
[ "$FAILED" -eq "$before" ] && ok

CASE="one workload with gaps is written, with the do-not-apply header"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
run_driver
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$DRIVER_OUT" 2>/dev/null)" "name: w"
assert_contains "$(cat "$DRIVER_OUT" 2>/dev/null)" "scaling-manager-workload-patch: generated"
assert_contains "$(cat "$DRIVER_OUT" 2>/dev/null)" "Do NOT \`kubectl apply -f\`"
[ "$FAILED" -eq "$before" ] && ok

# --- apply mode -------------------------------------------------------------

CASE="apply patches the drain half and never sends a volume"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "preStop"
# A strategic merge cannot replace a volume of the same name; sending one gets
# the WHOLE object rejected, drain half included.
assert_not_contains "$PATCH_BODY" "persistentVolumeClaim"
assert_not_contains "$PATCH_BODY" "volumeMounts"
# ...while the emitted file still carries it, for whoever owns the storage.
assert_contains "$(cat "$dir/p.yaml")" "claimName: model-pvc"
assert_contains "$(cat "$dir/log")" "REPLACES PODS"
[ "$FAILED" -eq "$before" ] && ok

CASE="a rejected patch is counted as a failure and makes the run non-zero"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain; PATCH_RC=1
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$dir/log")" "were NOT patched"
assert_not_contains "$(cat "$dir/log")" "Patched 1 of 1"
[ "$FAILED" -eq "$before" ] && ok

CASE="a workload needing only the weights volume is deferred, not failed"
reset; LIST_FIXTURE="$(serving_list)"; f_cache_mounted_elsewhere
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
# It already has a preStop hook? No -- it has none, so it gets the drain half.
# What matters is that the emitted document keeps the weights half and the patch
# sent to the API server does not.
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$dir/p.yaml")" "claimName: model-pvc"
assert_not_contains "$PATCH_BODY" "claimName"
[ "$FAILED" -eq "$before" ] && ok

CASE="a missing tool is reported, not silently treated as nothing-to-do"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
# `command` is a builtin, but a function of the same name shadows it for the
# unqualified call the library makes -- which is the only way to simulate an
# absent binary from inside a harness that needs those binaries itself.
command() {
    if [ "${1:-}" = "-v" ] && [ "${2:-}" = "yq" ]; then return 1; fi
    builtin command "$@"
}
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
unset -f command
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$dir/log")" "yq is not installed"
# The silence this replaces: yq missing meant an empty index, zero patch calls,
# and a [SUCCESS] line.
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
[ "$FAILED" -eq "$before" ] && ok

# --- applying the weights half ----------------------------------------------
#
# Its own opt-in, separate from APPLY: adding a hook cannot conflict with
# anything, while mounting storage can be refused by the API server outright and
# changes where an engine reads its weights from.

apply_weights_run() {
    local dir; dir="$(mktemp -d)"
    DRIVER_RC=0; DRIVER_LOG="$dir/log"; DRIVER_OUT="$dir/p.yaml"
    LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true \
        LLMSM_WORKLOAD_PATCH_APPLY_WEIGHTS=true LLMSM_WORKLOAD_PATCH_FILE="$DRIVER_OUT" \
        llmsm_workload_patch >"$DRIVER_LOG" 2>&1 || DRIVER_RC=$?
}

CASE="with the opt-in and no collision, the weights volume IS applied"
reset; LIST_FIXTURE="$(serving_list)"; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list model-pvc:ReadWriteMany)"
apply_weights_run
assert_contains "$PATCH_BODY" "claimName"
assert_contains "$PATCH_BODY" "preStop"
[ "$FAILED" -eq "$before" ] && ok

CASE="a volume of the same name blocks the weights half, NOT the drain half"
reset; LIST_FIXTURE="$(serving_list)"
before=$FAILED
# The 422 that made this unconditional once: `volumes` merges with retainKeys,
# which `kubectl patch --type=strategic` does not generate, so a same-named
# volume gets the WHOLE object rejected -- drain hook included.
FIXTURE='{"spec":{"template":{"spec":{
    "terminationGracePeriodSeconds":30,
    "volumes":[{"name":"model-storage","emptyDir":{}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                   "env":[{"name":"HF_HOME","value":"/root/.cache/huggingface"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
PVC_FIXTURE="$(pvc_list model-pvc:ReadWriteMany)"
apply_weights_run
assert_contains "$PATCH_BODY" "preStop"
assert_not_contains "$PATCH_BODY" "claimName"
assert_contains "$(cat "$DRIVER_LOG")" "already exists"
# ...and the emitted file still carries it, for whoever owns the chart.
assert_contains "$(cat "$DRIVER_OUT")" "claimName"
[ "$FAILED" -eq "$before" ] && ok

CASE="a missing claim blocks the weights half rather than stranding pods Pending"
reset; LIST_FIXTURE="$(serving_list)"; f_llmd_real_downloads
before=$FAILED
PVC_RC=1     # the claim cannot be read
apply_weights_run
assert_contains "$PATCH_BODY" "preStop"
assert_not_contains "$PATCH_BODY" "claimName"
assert_contains "$(cat "$DRIVER_LOG")" "does not exist"
[ "$FAILED" -eq "$before" ] && ok

CASE="without the opt-in the weights half is never applied, claim or no claim"
reset; LIST_FIXTURE="$(serving_list)"; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list model-pvc:ReadWriteMany)"
dir="$(mktemp -d)"
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || true
assert_contains "$PATCH_BODY" "preStop"
assert_not_contains "$PATCH_BODY" "claimName"
[ "$FAILED" -eq "$before" ] && ok

CASE="candidate StorageClasses are ranked by observed RWX use"
before=$FAILED
PVC_FIXTURE='{"items":[
  {"spec":{"accessModes":["ReadWriteMany"],"storageClassName":"shared-fs"},"status":{"phase":"Bound"}},
  {"spec":{"accessModes":["ReadWriteMany"],"storageClassName":"shared-fs"},"status":{"phase":"Bound"}},
  {"spec":{"accessModes":["ReadWriteMany"],"storageClassName":"other-fs"},"status":{"phase":"Bound"}},
  {"spec":{"accessModes":["ReadWriteOnce"],"storageClassName":"block"},"status":{"phase":"Bound"}},
  {"spec":{"accessModes":["ReadWriteMany"],"storageClassName":"unbound-fs"},"status":{"phase":"Pending"}}]}'
out="$(so_model_cache_classes)"
assert_contains "$out" "shared-fs (2 bound RWX claim(s))"
assert_contains "$out" "other-fs (1 bound RWX claim(s))"
# A class only ever seen backing RWO is not evidence that it can do RWX.
assert_not_contains "$out" "block"
assert_not_contains "$out" "unbound-fs"
assert_eq "$(printf '%s' "$out" | head -1)" "shared-fs (2 bound RWX claim(s))" "most-used first"
[ "$FAILED" -eq "$before" ] && ok

CASE="an unwritable destination keeps the patch instead of deleting it"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_FILE=/nonexistent-dir/p.yaml \
    llmsm_workload_patch >"$WORK/uw.log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$WORK/uw.log")" "kept at"
kept="$(sed -n 's/.*kept at \([^ ]*\).*/\1/p' "$WORK/uw.log" | tail -1)"
[ -n "$kept" ] && [ -s "$kept" ] || fail "the patch it says it kept is not there"
[ -n "$kept" ] && rm -f "$kept"
[ "$FAILED" -eq "$before" ] && ok


# --- ADDED BY MUTATION TESTING ------------------------------------------------

# The engine identified by IMAGE alone -- the command is a wrapper script.
f_engine_by_image_only() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "containers":[
          {"name":"routing-proxy","image":"quay.io/x/proxy:1","args":["--listen",":80"]},
          {"name":"server","image":"quay.io/x/vllm-openai:v0.11",
           "command":["/opt/entrypoint.sh"]}]}}}}'
}

# The engine identified by COMMAND alone -- the image name says nothing.
f_engine_by_command_only() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "containers":[
          {"name":"routing-proxy","image":"quay.io/x/proxy:1","args":["--listen",":80"]},
          {"name":"engine","image":"registry.example.com/ai/runtime:1.2",
           "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

# The SIDECAR mounts the cache; the engine does not.
f_sidecar_mounts_cache() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[
          {"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
           "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"}],
           "args":["vllm","serve","Qwen/Qwen3-0.6B"]},
          {"name":"sidecar","image":"quay.io/x/proxy:1",
           "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}]}]}}}}'
}

# An engine that already drains but re-downloads -- the DEFERRAL shape.
f_hooked_but_downloads() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

CASE="the engine is found by IMAGE when the command is a wrapper script"
reset; f_engine_by_image_only
before=$FAILED
assert_field 1 "server"
[ "$FAILED" -eq "$before" ] && ok

CASE="the engine is found by COMMAND when the image name says nothing"
reset; f_engine_by_command_only
before=$FAILED
assert_field 1 "engine"
[ "$FAILED" -eq "$before" ] && ok

CASE="a cache the SIDECAR mounts is not a cache the engine can use"
reset; f_sidecar_mounts_cache
before=$FAILED
assert_field 1 "vllm"
assert_field 5 "0"
[ "$FAILED" -eq "$before" ] && ok

CASE="the weights note is silent when the download dir IS on a mount"
reset; f_cached_properly
before=$FAILED
note="$(so_weights_note ns deployments w "$POD_DEPLOY")"
[ -z "$note" ] || fail "expected silence, got: $note"
reset; f_cache_mounted_elsewhere
assert_contains "$(so_weights_note ns deployments w "$POD_DEPLOY")" "Weights do not persist"
[ "$FAILED" -eq "$before" ] && ok

CASE="hasPVC counts PVC-backed volumes, not every volume"
reset; f_llmd_real
before=$FAILED
assert_field 1 "vllm"
assert_field 4 "1"
[ "$FAILED" -eq "$before" ] && ok

CASE="a bare glob as the model argument does not become the answer"
before=$FAILED
tmpd="$(mktemp -d)"; : > "$tmpd/decoy.log"
assert_eq "$(cd "$tmpd" && so_model_source 'vllm serve * --port 8000')" "*" "glob after serve"
assert_eq "$(cd "$tmpd" && so_model_source 'vllm --model *')" "*" "glob as --model"
rm -rf "$tmpd"
[ "$FAILED" -eq "$before" ] && ok

CASE="the emitted preStop is a shell sleep of the configured length"
reset; f_vllm_plain
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" 'command: ["/bin/sh", "-c", "sleep 45"]'
assert_contains "$(LLMSM_DRAIN_SLEEP_SECONDS=90 so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" \
    '"sleep 90"'
[ "$FAILED" -eq "$before" ] && ok

CASE="a workload that already drains but re-downloads is deferred, not patched"
reset; LIST_FIXTURE="$(serving_list)"; f_hooked_but_downloads
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$dir/log")" "need only a volume"
assert_not_contains "$(cat "$dir/log")" "were NOT patched"
assert_contains "$(cat "$dir/p.yaml")" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="each workload is patched with ITS OWN document, not the whole file"
reset
LIST_FIXTURE='{"items":[{"metadata":{"name":"w1","labels":{"llm-d.ai/role":"decode"}}},
                        {"metadata":{"name":"w2","labels":{"llm-d.ai/role":"decode"}}}]}'
before=$FAILED
f_vllm_plain
FIXTURE_MAP=([w1]="$FIXTURE" [w2]="$FIXTURE")
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_eq "$PATCH_CALLS" "2" "kubectl patch calls"
assert_contains "$PATCH_BODY" "name: w2"
assert_not_contains "$PATCH_BODY" "name: w1"
[ "$FAILED" -eq "$before" ] && ok

CASE="a file IS written for the readable half, and the run is still non-zero"
reset
LIST_FIXTURE='{"items":[{"metadata":{"name":"good","labels":{"llm-d.ai/role":"decode"}}},
                        {"metadata":{"name":"bad","labels":{"llm-d.ai/role":"decode"}}}]}'
before=$FAILED
f_vllm_plain
FIXTURE_MAP=([good]="$FIXTURE" [bad]="__RC1__")
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_contains "$(cat "$dir/p.yaml" 2>/dev/null)" "name: good"
assert_eq "$DRIVER_RC" "1" "return code"
[ "$FAILED" -eq "$before" ] && ok

CASE="a listing that cannot be PARSED is not an empty namespace"
reset; LIST_FIXTURE='{"items":['
before=$FAILED
run_driver
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$DRIVER_LOG")" "not being reported as clean"
assert_not_contains "$(cat "$DRIVER_LOG")" "No model servers found in scope"
[ "$FAILED" -eq "$before" ] && ok

CASE="an index that reads back empty is a failure, not nothing-to-do"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
so_index_orig="$(declare -f so_workload_patch_index)"
so_workload_patch_index() { :; }
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
unset -f so_workload_patch_index
[ -n "$so_index_orig" ] && eval "$so_index_orig"
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$dir/log")" "NOTHING was applied"
assert_contains "$(cat "$dir/log")" "were NOT patched"
[ "$FAILED" -eq "$before" ] && ok

CASE="LeaderWorkerSets are looked at only where the CRD exists"
reset; LIST_FIXTURE="$(serving_list)"; f_lws; CRD_RC=1
before=$FAILED
run_driver
assert_not_contains "$(cat "$DRIVER_LOG")" "LeaderWorkerSet ns1/w"
reset; LIST_FIXTURE="$(serving_list)"; f_lws; CRD_RC=0
run_driver
assert_contains "$(cat "$DRIVER_LOG")" "LeaderWorkerSet ns1/w: no drain half"
[ "$FAILED" -eq "$before" ] && ok

# --- the engine-cache half --------------------------------------------------
#
# The same question as the weights, one layer down: vLLM keeps its compile
# caches under VLLM_CACHE_ROOT, and a replica that does not find them warm
# compiles from nothing -- measured, 14 s of torch.compile against 3 s. The
# detector answers where the caches LAND, as the weights one does: unset is
# /tmp, an emptyDir dies with the pod, and only a claim or a hostPath counts.

f_engine_cache_on_claim() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "volumes":[{"name":"engine-cache","persistentVolumeClaim":{"claimName":"engine-cache"}},
                   {"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"},
                              {"name":"VLLM_CACHE_ROOT","value":"/engine-cache/vllm"},
                              {"name":"PYTHONPYCACHEPREFIX","value":"/engine-cache/pycache/5f8fcd7456cd"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"},
                                       {"name":"engine-cache","mountPath":"/engine-cache"}],
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

# The cache variable set, and pointing at an emptyDir: every llm-d pod mounts
# one at /dev/shm, and a cache there is a cache for one replica, which is none.
f_engine_cache_on_emptydir() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "volumes":[{"name":"dshm","emptyDir":{"medium":"Memory"}},
                   {"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"},
                              {"name":"VLLM_CACHE_ROOT","value":"/dev/shm/vllm-cache"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"},
                                       {"name":"dshm","mountPath":"/dev/shm"}],
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

# A hostPath counts: it is the node's disk, and it outlives the pod.
f_engine_cache_on_hostpath() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "volumes":[{"name":"local","hostPath":{"path":"/mnt/local/caches"}},
                   {"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"},
                              {"name":"VLLM_CACHE_ROOT","value":"/mnt/caches/vllm"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"},
                                       {"name":"local","mountPath":"/mnt/caches"}],
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

# Drains, weights on a volume, no cache variable at all: the engine-cache half
# and nothing else.
f_engine_cache_only_gap() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

CASE="engine cache: unset VLLM_CACHE_ROOT is reported as unset"
reset; f_vllm_plain
before=$FAILED
assert_field 7 "unset"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: on a claim the engine mounts counts as on a volume"
reset; f_engine_cache_on_claim
before=$FAILED
assert_field 7 "volume:/engine-cache/vllm"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: on a hostPath counts as on a volume"
reset; f_engine_cache_on_hostpath
before=$FAILED
assert_field 7 "volume:/mnt/caches/vllm"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: on an emptyDir is ephemeral, not a volume"
reset; f_engine_cache_on_emptydir
before=$FAILED
assert_field 7 "ephemeral:/dev/shm/vllm-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a name listed twice in env resolves to its LAST value, as the kubelet does"
reset
before=$FAILED
# Seen live on the benchmark stand: the harness lists VLLM_CACHE_ROOT twice,
# its default /tmp/vllm and then the mount. The kubelet takes the last; a
# detector reading the first reported every harness engine as cold.
FIXTURE='{"spec":{"template":{"spec":{
    "volumes":[{"name":"engine-cache","persistentVolumeClaim":{"claimName":"engine-cache"}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"VLLM_CACHE_ROOT","value":"/tmp/vllm"},
                          {"name":"VLLM_CACHE_ROOT","value":"/engine-cache/vllm"}],
                   "volumeMounts":[{"name":"engine-cache","mountPath":"/engine-cache"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 7 "volume:/engine-cache/vllm"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a claim the SIDECAR mounts is not one the engine can use"
reset
before=$FAILED
FIXTURE='{"spec":{"template":{"spec":{
    "volumes":[{"name":"engine-cache","persistentVolumeClaim":{"claimName":"engine-cache"}}],
    "containers":[
      {"name":"routing-proxy","image":"quay.io/x/proxy:1",
       "volumeMounts":[{"name":"engine-cache","mountPath":"/engine-cache"}]},
      {"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
       "env":[{"name":"VLLM_CACHE_ROOT","value":"/engine-cache/vllm"}],
       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 7 "ephemeral:/engine-cache/vllm"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: SGLang has no such variable, so the field is empty (no opinion)"
reset; f_sglang_proxy_has_hook
before=$FAILED
assert_field 1 "server"      # anchor: the engine was found
assert_field 7 ""
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "preStop"                  # anchor: a document was produced
# No vLLM variable and no engine-cache header: only the bytecode, which is
# any Python engine's (its own cases are below).
assert_not_contains "$doc" "- name: VLLM_CACHE_ROOT"
assert_not_contains "$doc" "#   engine-cache:"
assert_contains "$doc" "PYTHONPYCACHEPREFIX"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: ten fields, argv and cachevol where they were, image, bytecode and FlashInfer after"
reset; f_vllm_plain
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env = [{"name":"FLASHINFER_WORKSPACE_DIR","value":"/x"}]')"
gaps="$(so_workload_gaps ns deployments w "$POD_DEPLOY")"
IFS=$'\037' read -r e1 e2 e3 e4 e5 e6 e7 e8 e9 e10 e11 <<< "$gaps"
assert_eq "$e6" "vllm serve Qwen/Qwen3-0.6B" "argv (field 6)"
assert_eq "$e7" "unset" "cachevol (field 7)"
assert_eq "$e8" "quay.io/x/vllm-openai:v0.11" "image (field 8)"
assert_eq "$e9" "unset" "bytecode (field 9)"
assert_eq "$e10" "dir:/x" "FlashInfer (field 10)"
assert_eq "$e11" "" "an eleventh field"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the patch carries the three variables, the mount and the claim"
reset; f_engine_cache_only_gap
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "#   engine-cache:"
assert_contains "$doc" "- name: VLLM_CACHE_ROOT"
assert_contains "$doc" "value: /engine-cache/vllm"
assert_contains "$doc" "- name: FLASHINFER_WORKSPACE_BASE"
assert_contains "$doc" "- name: TRITON_CACHE_DIR"
assert_contains "$doc" "mountPath: /engine-cache"
assert_contains "$doc" "claimName: engine-cache"
# ...and only that half: no drain, no weights.
assert_not_contains "$doc" "preStop"
assert_not_contains "$doc" "HF_HOME"
assert_not_contains "$doc" "#   cache:"
# The guard travels with it: the command belongs to the chart, so the patch
# cannot add it, and without it a read-only cache directory is a lost replica.
assert_contains "$doc" 'unset $v'
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: with the weights half too, env/volumeMounts/volumes are each ONE list"
reset; f_vllm_plain
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "HF_HOME"
assert_contains "$doc" "VLLM_CACHE_ROOT"
assert_contains "$doc" "preStop"
assert_eq "$(printf '%s\n' "$doc" | grep -c '^        env:$')" "1" "env lists"
assert_eq "$(printf '%s\n' "$doc" | grep -c '^        volumeMounts:$')" "1" "volumeMounts lists"
assert_eq "$(printf '%s\n' "$doc" | grep -c '^      volumes:$')" "1" "volumes lists"
# ...and the document is YAML yq reads back with BOTH volumes in it, which a
# second `volumes:` key would silently have replaced.
d="$(mktemp)"; printf '%s\n' "$doc" > "$d"
assert_eq "$(yq eval '.spec.template.spec.volumes | length' "$d" 2>/dev/null)" "2" "volumes read back"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env | length' "$d" 2>/dev/null)" "5" "env read back (HF_HOME, the three vLLM caches, PYTHONPYCACHEPREFIX)"
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: an ephemeral cache is warned about ON THE CONSOLE, with its path"
reset; f_engine_cache_on_emptydir
before=$FAILED
warn="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1 >/dev/null)"
assert_contains "$warn" "COMPILES FROM NOTHING"
assert_contains "$warn" "/dev/shm/vllm-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a cache on a claim is not warned about, and gets no half"
reset; f_engine_cache_on_claim
before=$FAILED
out="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1)"
[ -z "$out" ] || fail "expected nothing, got: $out"
reset; f_engine_cache_only_gap    # anchor: the same call does produce one here
assert_contains "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" "engine-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a missing claim is said to be missing, with what a node directory must be"
reset; f_engine_cache_only_gap
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "engine-cache does NOT exist yet in ns"
assert_contains "$doc" "make engine-cache NAMESPACE=ns ENGINE_CACHE_PATH=<node dir>"
assert_contains "$doc" "ENGINE_CACHE_SEED_CLAIM"      # the seed is part of the same instruction
assert_contains "$doc" "at least two components"
assert_contains "$doc" "not the node's own"
assert_contains "$doc" "one directory per trust domain"
assert_contains "$doc" "PersistentVolumes"
assert_contains "$doc" "WEIGHTS_NODE_SELECTOR"
# ...and the way to do without one.
assert_contains "$doc" "Without a node directory"
assert_contains "$doc" "LLMSM_ENGINE_CACHE_CLAIM"
assert_not_contains "$doc" "already exists"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: an existing claim is reused, not followed by instructions to create one"
reset; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "engine-cache already exists in ns"
assert_contains "$doc" "engine-cache-status"
assert_not_contains "$doc" "does NOT exist"
assert_not_contains "$doc" "ENGINE_CACHE_PATH"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: LLMSM_ENGINE_CACHE_CLAIM names a shared claim, with its subPath"
reset; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list shared-cache:ReadWriteMany)"
doc="$(LLMSM_ENGINE_CACHE_CLAIM=shared-cache:caches so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: shared-cache"
assert_contains "$doc" "subPath: caches"
assert_contains "$doc" "shared-cache already exists in ns"
assert_not_contains "$doc" "claimName: engine-cache"
# ...and the document parses with the subPath on the mount, not the volume.
d="$(mktemp)"; printf '%s\n' "$doc" > "$d"
assert_eq "$(yq eval '.spec.template.spec.containers[0].volumeMounts[0].subPath' "$d" 2>/dev/null)" "caches" "subPath read back"
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the plan note fires for an unset cache and names the fix"
reset; f_vllm_plain
before=$FAILED
note="$(so_enginecache_note ns deployments w "$POD_DEPLOY")"
assert_contains "$note" "compiles from nothing"
assert_contains "$note" "VLLM_CACHE_ROOT is unset"
assert_contains "$note" "make workload-patch"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the plan note is silent on a claim, speaks only of bytecode on SGLang, and is silent on an unreadable spec"
reset; f_engine_cache_on_claim
before=$FAILED
assert_eq "$(so_enginecache_note ns deployments w "$POD_DEPLOY")" "" "note for a cache on a claim"
reset; f_sglang_proxy_has_hook
note="$(so_enginecache_note ns deployments w "$POD_DEPLOY")"
assert_contains "$note" "recompiles its Python bytecode"
assert_not_contains "$note" "VLLM_CACHE_ROOT"
reset; f_unreadable
assert_eq "$(so_enginecache_note ns deployments w "$POD_DEPLOY")" "" "note for an unreadable spec (the drain note says so)"
assert_contains "$(so_drain_note ns deployments w "$POD_DEPLOY")" "unknown"   # anchor
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the summary counts the workloads that compile from nothing"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$dir/log")" "1 model server(s) COMPILE FROM NOTHING"
assert_not_contains "$(cat "$dir/log")" "RE-DOWNLOAD"
assert_contains "$(cat "$dir/p.yaml")" "claimName: engine-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: without its opt-in the half is emitted, never applied"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany model-pvc:ReadWriteMany)"
dir="$(mktemp -d)"
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || true
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "preStop"
assert_not_contains "$PATCH_BODY" "VLLM_CACHE_ROOT"
assert_not_contains "$PATCH_BODY" "engine-cache"
assert_not_contains "$PATCH_BODY" "env:"
assert_contains "$(cat "$dir/log")" "engine-cache half is emitted only"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: with its opt-in and the claim, the half IS applied -- and the weights half is not"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany model-pvc:ReadWriteMany)"
dir="$(mktemp -d)"
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" llmsm_workload_patch >"$dir/log" 2>&1 || true
assert_contains "$PATCH_BODY" "preStop"
assert_contains "$PATCH_BODY" "VLLM_CACHE_ROOT"
assert_contains "$PATCH_BODY" "claimName: engine-cache"
assert_not_contains "$PATCH_BODY" "HF_HOME"
assert_not_contains "$PATCH_BODY" "claimName: model-pvc"
assert_not_contains "$PATCH_BODY" "model-storage"
# The body is a document the API server would take: one env list of four.
d="$(mktemp)"; printf '%s\n' "$PATCH_BODY" > "$d"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env | length' "$d" 2>/dev/null)" "4" "env read back (the three vLLM caches and PYTHONPYCACHEPREFIX)"
assert_eq "$(yq eval '.spec.template.spec.volumes | length' "$d" 2>/dev/null)" "1" "volumes read back"
rm -f "$d"
# ...and the guard's absence is said at the time.
assert_contains "$(cat "$dir/log")" "guard is NOT applied"
assert_contains "$(cat "$dir/log")" "WITHOUT the guard"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: both opt-ins keep both halves"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany model-pvc:ReadWriteMany)"
dir="$(mktemp -d)"
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    LLMSM_WORKLOAD_PATCH_APPLY_WEIGHTS=true LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || true
assert_contains "$PATCH_BODY" "claimName: engine-cache"
assert_contains "$PATCH_BODY" "claimName: model-pvc"
assert_contains "$PATCH_BODY" "HF_HOME"
assert_contains "$PATCH_BODY" "TRITON_CACHE_DIR"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a missing claim blocks the half rather than stranding pods Pending"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"        # nothing left to send
assert_contains "$(cat "$dir/log")" "does not exist"
assert_contains "$(cat "$dir/log")" "need only a volume"
assert_contains "$(cat "$dir/p.yaml")" "claimName: engine-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a mount already at /engine-cache blocks the half, NOT the drain half"
reset; LIST_FIXTURE="$(serving_list)"
before=$FAILED
FIXTURE='{"spec":{"template":{"spec":{
    "terminationGracePeriodSeconds":30,
    "volumes":[{"name":"scratch","emptyDir":{}},
               {"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"}],
                   "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"},
                                   {"name":"scratch","mountPath":"/engine-cache"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
dir="$(mktemp -d)"
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" llmsm_workload_patch >"$dir/log" 2>&1 || true
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "preStop"
assert_not_contains "$PATCH_BODY" "engine-cache"
assert_contains "$(cat "$dir/log")" "already mounted at /engine-cache"
assert_contains "$(cat "$dir/p.yaml")" "claimName: engine-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a workload with everything on a volume reports the all-clear"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_on_claim
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$dir/log")" "no vLLM among them compiles from nothing"
[ -f "$dir/p.yaml" ] && fail "no file should be written for a converged workload"
[ "$FAILED" -eq "$before" ] && ok

# --- FlashInfer: FLASHINFER_WORKSPACE_BASE, never FLASHINFER_WORKSPACE_DIR ----
#
# FlashInfer reads FLASHINFER_WORKSPACE_BASE and appends .cache/flashinfer to
# it; FLASHINFER_WORKSPACE_DIR is a name inside FlashInfer, read from no
# environment. workload-patch wrote the second before v0.12.1, so a workload
# patched then has its caches on a claim and still builds every FlashInfer
# kernel again on every start.

# A workload patched before v0.12.1: everything on the claim, FlashInfer at the
# variable FlashInfer does not read.
f_engine_cache_old_flashinfer() {
    f_engine_cache_on_claim
    FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [
        {"name":"FLASHINFER_WORKSPACE_DIR","value":"/engine-cache/flashinfer"},
        {"name":"TRITON_CACHE_DIR","value":"/engine-cache/triton"}]')"
}

# The FlashInfer base is keyed by image, as the bytecode is: FlashInfer 0.2
# and 0.3 key their tree by GPU architecture only.
KFI="$(so_pycache_key "quay.io/x/vllm-openai:v0.11")"

CASE="flashinfer: the base is keyed by image, so two images never share a FlashInfer tree"
reset; f_engine_cache_only_gap
before=$FAILED
[ "${#KFI}" -eq 12 ] || fail "no image key: '$KFI'"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
d="$(mktemp)"; printf '%s\n' "$doc" > "$d"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env[] | select(.name == "FLASHINFER_WORKSPACE_BASE") | .value' "$d" 2>/dev/null)" \
    "/engine-cache/flashinfer/$KFI" "FLASHINFER_WORKSPACE_BASE"
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].image = "quay.io/x/vllm-openai:v0.12"')"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"; printf '%s\n' "$doc" > "$d"
other="$(yq eval '.spec.template.spec.containers[0].env[] | select(.name == "FLASHINFER_WORKSPACE_BASE") | .value' "$d" 2>/dev/null)"
assert_eq "$other" "/engine-cache/flashinfer/$(so_pycache_key "quay.io/x/vllm-openai:v0.12")" "FLASHINFER_WORKSPACE_BASE for another image"
[ "$other" != "/engine-cache/flashinfer/$KFI" ] || fail "two images share one FlashInfer base"
# ...and the same key as the bytecode, so one image is one key everywhere.
assert_contains "$doc" "value: /engine-cache/pycache/$(so_pycache_key "quay.io/x/vllm-openai:v0.12")"
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

CASE="flashinfer: FLASHINFER_WORKSPACE_DIR without _BASE is field 10, dir: and its value"
reset; f_engine_cache_old_flashinfer
before=$FAILED
assert_field 7 "volume:/engine-cache/vllm"      # anchor: the caches read as on the claim
assert_field 10 "dir:/engine-cache/flashinfer"
# _BASE beside it, before or after, is the fix: no opinion.
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"FLASHINFER_WORKSPACE_BASE","value":"/engine-cache/flashinfer"}]')"
assert_field 10 ""
assert_field 7 "volume:/engine-cache/vllm"
# From a ConfigMap: the stale name is still the stale name, with no value.
reset; f_engine_cache_on_claim
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"FLASHINFER_WORKSPACE_DIR","valueFrom":{"configMapKeyRef":{"name":"c","key":"k"}}}]')"
assert_field 10 "dir:"
# Neither variable: no opinion here (the other fields judge the caches).
reset; f_engine_cache_on_claim
assert_field 10 ""
assert_field 1 "vllm"
[ "$FAILED" -eq "$before" ] && ok

CASE="flashinfer: a workload patched before v0.12.1 gets FLASHINFER_WORKSPACE_BASE at the old path, env only"
reset; f_engine_cache_old_flashinfer
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "#   flashinfer:"
assert_contains "$doc" "- name: FLASHINFER_WORKSPACE_BASE"
assert_contains "$doc" "value: /engine-cache/flashinfer/$KFI"
assert_not_contains "$doc" "#   engine-cache:"
assert_not_contains "$doc" "volumes:"
assert_not_contains "$doc" "- name: FLASHINFER_WORKSPACE_DIR"
d="$(mktemp)"; printf '%s\n' "$doc" > "$d"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env | map(.name + "=" + .value) | join(",")' "$d" 2>/dev/null)" \
    "FLASHINFER_WORKSPACE_BASE=/engine-cache/flashinfer/$KFI" "env read back"
rm -f "$d"
warn="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1 >/dev/null)"
assert_contains "$warn" "BUILDS ITS FLASHINFER KERNELS AGAIN"
assert_contains "$warn" "Set FLASHINFER_WORKSPACE_BASE=/engine-cache/flashinfer/$KFI instead"
note="$(so_enginecache_note ns deployments w "$POD_DEPLOY")"
assert_contains "$note" "builds its FlashInfer kernels again"
assert_contains "$note" "make workload-patch"
[ "$FAILED" -eq "$before" ] && ok

CASE="flashinfer: the summary counts it, the half is emitted, never applied, and there is no all-clear"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_old_flashinfer
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany model-pvc:ReadWriteMany)"
DS_FIXTURE='{"status":{"desiredNumberScheduled":2,"numberReady":2}}'
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$dir/log")" "1 model server(s) BUILD THEIR FLASHINFER KERNELS AGAIN"
assert_contains "$(cat "$dir/log")" "It is emitted, not applied: put it in your chart values and remove FLASHINFER_WORKSPACE_DIR."
assert_not_contains "$(cat "$dir/log")" "no vLLM among them compiles from nothing"
assert_not_contains "$(cat "$dir/log")" "COMPILE FROM NOTHING"
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$dir/log")" "need only a volume or a cache variable"
assert_contains "$(cat "$dir/p.yaml")" "value: /engine-cache/flashinfer/$KFI"
[ "$FAILED" -eq "$before" ] && ok

CASE="flashinfer: once FLASHINFER_WORKSPACE_BASE is set the re-run is the all-clear, old variable or not"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_old_flashinfer
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"FLASHINFER_WORKSPACE_BASE","value":"/engine-cache/flashinfer"}]')"
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$dir/log")" "no vLLM among them compiles from nothing"
assert_not_contains "$(cat "$dir/log")" "FLASHINFER"
[ -f "$dir/p.yaml" ] && fail "no file should be written for a converged workload"
assert_eq "$(so_enginecache_note ns deployments w "$POD_DEPLOY")" "" "plan note"
[ "$FAILED" -eq "$before" ] && ok

CASE="flashinfer: a guard loop that still names FLASHINFER_WORKSPACE_DIR is told to change, once _BASE is set too"
reset; f_engine_cache_old_flashinfer
before=$FAILED
# What a workload patched for v0.11.1 carries after the owner sets _BASE:
# the env is right, the command still guards the old name.
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"FLASHINFER_WORKSPACE_BASE","value":"/engine-cache/flashinfer"}]
    | .spec.template.spec.containers[0].command = ["/bin/sh","-c"]
    | .spec.template.spec.containers[0].args = ["for v in VLLM_CACHE_ROOT FLASHINFER_WORKSPACE_DIR TRITON_CACHE_DIR PYTHONPYCACHEPREFIX; do :; done\nexec vllm serve Qwen/Qwen3-0.6B"]')"
assert_field 1 "vllm"
assert_field 10 "loop"
doc_rc=0
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>"$WORK/fi.err")" || doc_rc=$?
assert_eq "$doc_rc" "0" "return code"
assert_eq "$doc" "" "document (the command is the chart's; nothing to write)"
assert_contains "$(cat "$WORK/fi.err")" "its command guards FLASHINFER_WORKSPACE_DIR"
# Re-run after the loop is renamed: quiet.
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].args |= map(gsub("FLASHINFER_WORKSPACE_DIR"; "FLASHINFER_WORKSPACE_BASE"))')"
assert_field 10 ""
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >/dev/null 2>"$WORK/fi.err"
assert_not_contains "$(cat "$WORK/fi.err")" "FLASHINFER_WORKSPACE_DIR"
# A stale variable outranks a stale loop: the variable is the bigger miss.
reset; f_engine_cache_old_flashinfer
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].command = ["/bin/sh","-c"]
    | .spec.template.spec.containers[0].args = ["for v in FLASHINFER_WORKSPACE_DIR; do :; done; exec vllm serve Qwen/Qwen3-0.6B"]')"
assert_field 10 "dir:/engine-cache/flashinfer"
[ "$FAILED" -eq "$before" ] && ok

CASE="flashinfer: a value from a ConfigMap, or not a plain path, is said, not written"
reset; f_engine_cache_on_claim
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"FLASHINFER_WORKSPACE_DIR","valueFrom":{"configMapKeyRef":{"name":"c","key":"k"}}}]')"
doc_rc=0
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>"$WORK/fi.err")" || doc_rc=$?
assert_eq "$doc_rc" "0" "return code"
assert_eq "$doc" "" "document"
assert_contains "$(cat "$WORK/fi.err")" "Set FLASHINFER_WORKSPACE_BASE to the directory it names, from the same source"
reset; f_engine_cache_on_claim
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"FLASHINFER_WORKSPACE_DIR","value":"/engine-cache/fi: x"}]')"
assert_eq "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>"$WORK/fi.err")" "" "document for a value that is not a plain path"
assert_contains "$(cat "$WORK/fi.err")" "from the same source"
[ "$FAILED" -eq "$before" ] && ok

CASE="flashinfer: with the engine-cache half the old variable is told to go, and the half carries _BASE"
reset; f_vllm_plain
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env = [{"name":"FLASHINFER_WORKSPACE_DIR","value":"/tmp/fi"}]')"
assert_field 10 "dir:/tmp/fi"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>"$WORK/fi.err")"
assert_contains "$doc" "#   engine-cache:"
assert_contains "$doc" "- name: FLASHINFER_WORKSPACE_BASE"
assert_contains "$doc" "value: /engine-cache/flashinfer/$KFI"
assert_not_contains "$doc" "#   flashinfer:"
assert_not_contains "$doc" "value: /tmp/fi"
assert_contains "$(cat "$WORK/fi.err")" "drop FLASHINFER_WORKSPACE_DIR"
[ "$FAILED" -eq "$before" ] && ok

CASE="trim: the weights half can be kept while the engine-cache half goes, and back"
before=$FAILED
mk_both() {
cat > "$1" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: w, namespace: ns1}
spec:
  template:
    spec:
      terminationGracePeriodSeconds: 120
      containers:
      - name: vllm
        lifecycle:
          preStop:
            exec:
              command: ["/bin/sh", "-c", "sleep 45"]
        env:
        - name: HF_HOME
          value: /model-cache/huggingface
        - name: VLLM_CACHE_ROOT
          value: /engine-cache/vllm
        - name: FLASHINFER_WORKSPACE_BASE
          value: /engine-cache/flashinfer
        - name: TRITON_CACHE_DIR
          value: /engine-cache/triton
        volumeMounts:
        - name: model-storage
          mountPath: /model-cache
        - name: engine-cache
          mountPath: /engine-cache
      volumes:
      - name: model-storage
        persistentVolumeClaim:
          claimName: model-pvc
      - name: engine-cache
        persistentVolumeClaim:
          claimName: engine-cache
YAML
}
d="$(mktemp)"; mk_both "$d"
so_workload_patch_trim "$d" 1 0 || fail "expected the weights half to survive"
left="$(cat "$d")"
assert_contains "$left" "HF_HOME"
assert_contains "$left" "claimName: model-pvc"
assert_contains "$left" "preStop"
assert_not_contains "$left" "VLLM_CACHE_ROOT"
assert_not_contains "$left" "engine-cache"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env | length' "$d")" "1" "env left"
assert_eq "$(yq eval '.spec.template.spec.volumes | length' "$d")" "1" "volumes left"
mk_both "$d"
so_workload_patch_trim "$d" 0 1 || fail "expected the engine-cache half to survive"
left="$(cat "$d")"
assert_contains "$left" "claimName: engine-cache"
assert_contains "$left" "TRITON_CACHE_DIR"
assert_not_contains "$left" "HF_HOME"
assert_not_contains "$left" "model-storage"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env | length' "$d")" "3" "env left"
mk_both "$d"
so_workload_patch_trim "$d" 0 0 || fail "expected the drain half to survive"
left="$(cat "$d")"
assert_not_contains "$left" "env:"
assert_not_contains "$left" "volumes:"
assert_contains "$left" "preStop"
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

CASE="trim: a document with only the two storage halves and neither kept is skipped (rc 1)"
before=$FAILED
d="$(mktemp)"
cat > "$d" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: w, namespace: ns1}
spec:
  template:
    spec:
      containers:
      - name: vllm
        env:
        - name: HF_HOME
          value: /model-cache/huggingface
        - name: VLLM_CACHE_ROOT
          value: /engine-cache/vllm
        volumeMounts:
        - name: model-storage
          mountPath: /model-cache
        - name: engine-cache
          mountPath: /engine-cache
      volumes:
      - name: model-storage
        persistentVolumeClaim:
          claimName: model-pvc
      - name: engine-cache
        persistentVolumeClaim:
          claimName: engine-cache
YAML
trim_rc=0
so_workload_patch_trim "$d" 0 0 || trim_rc=$?
assert_eq "$trim_rc" "1" "return code"
assert_contains "$(cat "$d")" "VLLM_CACHE_ROOT"   # anchor: the file was left intact
# ...while keeping one half makes it worth sending.
so_workload_patch_trim "$d" 0 1 || fail "expected the engine-cache half to be worth sending"
assert_contains "$(cat "$d")" "claimName: engine-cache"
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

# --- review round: the detector's edges, the claim's kind, the preparer ------

CASE="engine cache: a value from a ConfigMap or Secret is no opinion, never unset"
reset
before=$FAILED
# A strategic merge of `value` into an env entry that has `valueFrom` is
# rejected by the API server -- the whole document, drain hook included -- so
# a detector that called this "unset" and emitted the half took the drain
# half down with it on apply.
FIXTURE='{"spec":{"template":{"spec":{
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"VLLM_CACHE_ROOT","valueFrom":{"configMapKeyRef":{"name":"c","key":"k"}}}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 1 "vllm"      # anchor: the engine was found
assert_field 7 ""
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "preStop"
assert_not_contains "$doc" "- name: VLLM_CACHE_ROOT"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: an empty value is unset, not a cache at ''"
reset
before=$FAILED
FIXTURE='{"spec":{"template":{"spec":{
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"VLLM_CACHE_ROOT","value":""}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 7 "unset"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a mount is matched by path component, not string prefix"
reset
before=$FAILED
# /engine-cache2/vllm is not under a claim mounted at /engine-cache; a prefix
# test said it was, and gave an emptyDir cache a clean bill.
FIXTURE='{"spec":{"template":{"spec":{
    "volumes":[{"name":"engine-cache","persistentVolumeClaim":{"claimName":"engine-cache"}},
               {"name":"scratch","emptyDir":{}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"VLLM_CACHE_ROOT","value":"/engine-cache2/vllm"}],
                   "volumeMounts":[{"name":"engine-cache","mountPath":"/engine-cache"},
                                   {"name":"scratch","mountPath":"/engine-cache2"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 7 "ephemeral:/engine-cache2/vllm"
# ...and the same rule for the weights: HF_HOME=/model-cache2/hf is not on
# the claim at /model-cache.
FIXTURE='{"spec":{"template":{"spec":{
    "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"HF_HOME","value":"/model-cache2/hf"}],
                   "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 5 "0"
# ...while the mount path itself, and a path under it, still count.
FIXTURE='{"spec":{"template":{"spec":{
    "volumes":[{"name":"engine-cache","persistentVolumeClaim":{"claimName":"engine-cache"}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"VLLM_CACHE_ROOT","value":"/engine-cache"}],
                   "volumeMounts":[{"name":"engine-cache","mountPath":"/engine-cache"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 7 "volume:/engine-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: an inline nfs volume outlives the pod and counts"
reset
before=$FAILED
FIXTURE='{"spec":{"template":{"spec":{
    "volumes":[{"name":"nas","nfs":{"server":"filer","path":"/export/caches"}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"VLLM_CACHE_ROOT","value":"/cache/vllm"}],
                   "volumeMounts":[{"name":"nas","mountPath":"/cache"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 7 "volume:/cache/vllm"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a NAMED claim that is missing is not answered with make engine-cache"
reset; f_engine_cache_only_gap
before=$FAILED
# `make engine-cache` creates a claim called engine-cache, never the one the
# operator named, so that remedy would leave every pod Pending on the name the
# patch above actually mounts -- the same defect the weights half once had.
doc="$(LLMSM_ENGINE_CACHE_CLAIM=my-rwx so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: my-rwx"
assert_contains "$doc" "my-rwx (LLMSM_ENGINE_CACHE_CLAIM) does NOT exist in ns"
assert_contains "$doc" "drop LLMSM_ENGINE_CACHE_CLAIM"
assert_not_contains "$doc" "ENGINE_CACHE_PATH="
assert_not_contains "$doc" "make engine-cache NAMESPACE"
# ...and what it asks of that claim is said: the trust it takes.
assert_contains "$doc" "runs code in every"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the shared-claim alternative states what it must be trusted as"
reset; f_engine_cache_only_gap
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "Without a node directory"
assert_contains "$doc" "runs code in every engine, on every node at"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the all-clear does not vouch for an engine the detector has no opinion on"
reset; LIST_FIXTURE="$(serving_list)"
before=$FAILED
# An SGLang server that drains and keeps its weights: the all-clear must not
# say its compile caches are on a volume, because nothing looked.
FIXTURE='{"spec":{"template":{"spec":{
    "terminationGracePeriodSeconds":120,
    "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
    "containers":[{"name":"server","image":"lmsysorg/sglang:latest",
                   "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"},
                          {"name":"PYTHONPYCACHEPREFIX","value":"/model-cache/pycache/596e0fdddd36"}],
                   "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                   "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                   "args":["python3","-m","sglang.launch_server","--model-path","meta-llama/Llama-3.1-8B"]}]}}}}'
dir="$(mktemp -d)"
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" llmsm_workload_patch >"$dir/log" 2>&1 || true
assert_contains "$(cat "$dir/log")" "already drain on scale-down"
assert_contains "$(cat "$dir/log")" "no vLLM among them compiles from nothing"
assert_not_contains "$(cat "$dir/log")" "compile caches on a volume"
[ "$FAILED" -eq "$before" ] && ok

CASE="a claim that is not Bound blocks the half, with its phase"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany:Pending)"
dir="$(mktemp -d)"
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" llmsm_workload_patch >"$dir/log" 2>&1 || true
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$dir/log")" "is not Bound (Pending)"
assert_contains "$(cat "$dir/p.yaml")" "claimName: engine-cache"   # anchor: emitted still
[ "$FAILED" -eq "$before" ] && ok

CASE="a ReadWriteOnce claim blocks the half: it binds to one node"
reset; LIST_FIXTURE="$(serving_list)"; f_llmd_real_downloads
before=$FAILED
# The weights half shares the check, so the second replica of a workload
# whose cache is RWO is refused there too.
PVC_FIXTURE="$(pvc_list model-pvc:ReadWriteOnce)"
apply_weights_run
assert_contains "$PATCH_BODY" "preStop"
assert_not_contains "$PATCH_BODY" "claimName"
assert_contains "$(cat "$DRIVER_LOG")" "not ReadWriteMany"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: an unprepared node blocks the half, and is counted"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_FIXTURE='{"status":{"desiredNumberScheduled":16,"numberReady":14}}'
dir="$(mktemp -d)"
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" llmsm_workload_patch >"$dir/log" 2>&1 || true
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$dir/log")" "2 of 16 accelerator node(s) are not prepared"
assert_contains "$(cat "$dir/log")" "engine-cache-status"
# ...and with every node prepared the same run applies it.
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_FIXTURE='{"status":{"desiredNumberScheduled":16,"numberReady":16}}'
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" llmsm_workload_patch >"$dir/log" 2>&1 || true
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "claimName: engine-cache"
assert_contains "$(cat "$dir/log")" "WITHOUT the guard"
[ "$FAILED" -eq "$before" ] && ok

CASE="a volume or claim name that is not a DNS-1123 label is refused at entry"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_ENGINE_CACHE_VOLUME_NAME='engine"cache' LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$dir/log")" "LLMSM_ENGINE_CACHE_VOLUME_NAME must be a DNS-1123 label"
[ -f "$dir/p.yaml" ] && fail "nothing should be written"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_ENGINE_CACHE_CLAIM='shared:../etc' LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$dir/log")" "is not a subPath"
# ...and a well-formed one passes the gate (anchor).
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_ENGINE_CACHE_CLAIM='shared-cache:engine-cache' LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$dir/p.yaml")" "claimName: shared-cache"
[ "$FAILED" -eq "$before" ] && ok

# --- review round 4: the preparer gate, and the names -----------------------

engine_apply_run() {  # the engine-cache opt-in against $FIXTURE, results in DRIVER_*
    DRIVER_OUT="$(mktemp -d)/p.yaml"; DRIVER_LOG="$(mktemp)"; DRIVER_RC=0
    LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
        LLMSM_WORKLOAD_PATCH_FILE="$DRIVER_OUT" llmsm_workload_patch >"$DRIVER_LOG" 2>&1 || DRIVER_RC=$?
}

CASE="engine cache: a preparer that cannot be READ refuses the half (Forbidden is not absent)"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_RC=1                      # a token that may not list DaemonSets, or a timeout
engine_apply_run
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$DRIVER_LOG")" "preparer DaemonSet could not be read"
assert_contains "$(cat "$DRIVER_OUT")" "claimName: engine-cache"   # emitted still
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: no preparer at all (NotFound) is not a refusal -- the claim was checked"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_FIXTURE=""; DS_RC=0       # --ignore-not-found: empty, rc 0
engine_apply_run
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "claimName: engine-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a preparer with no status yet (0 of 0) is not 'every node prepared'"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_FIXTURE='{"status":{}}'
engine_apply_run
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$DRIVER_LOG")" "0 of 0 accelerator node(s) are not prepared"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the gate is for the claim make engine-cache makes, not a namesake DaemonSet"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list my-rwx:ReadWriteMany)"
DS_FIXTURE='{"status":{"desiredNumberScheduled":3,"numberReady":2}}'   # an unrelated DaemonSet called my-rwx
LLMSM_ENGINE_CACHE_CLAIM=my-rwx engine_apply_run
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "claimName: my-rwx"
assert_not_contains "$(cat "$DRIVER_LOG")" "not prepared"
[ "$FAILED" -eq "$before" ] && ok

CASE="the two volume names may not be the same"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
dir="$(mktemp -d)"; DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_ENGINE_CACHE_VOLUME_NAME=model-storage LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$dir/log")" "the two volumes need two names"
[ -f "$dir/p.yaml" ] && fail "nothing should be written"
[ "$FAILED" -eq "$before" ] && ok

CASE="the claim reference is checked as claim and subPath, each by its own rule"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
dir="$(mktemp -d)"
run_ref() { DRIVER_RC=0; LLMSM_DEFAULT_SO_NS=ns1 LLMSM_ENGINE_CACHE_CLAIM="$1" LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?; rm -f "$dir/p.yaml"; }
# refused: a slash, an underscore, upper case or a leading dot in the CLAIM part
for bad in a/b a_b Shared .a a..b; do
    run_ref "$bad"; assert_eq "$DRIVER_RC" "1" "rc for claim '$bad'"; assert_contains "$(cat "$dir/log")" "is not a claim name"
done
# refused: an absolute, empty or escaping SUBPATH
for bad in shared:/abs shared: shared:.. shared:../x shared:a/../b; do
    run_ref "$bad"; assert_eq "$DRIVER_RC" "1" "rc for '$bad'"; assert_contains "$(cat "$dir/log")" "is not a subPath"
done
# accepted: a dotted claim, a mixed-case and nested subPath (what --seed-claim takes too)
for good in shared-cache shared.cache:engine-cache shared-cache:Models/qwen_3; do
    run_ref "$good"; assert_eq "$DRIVER_RC" "0" "rc for '$good'"
done
[ "$FAILED" -eq "$before" ] && ok

# --- Python bytecode ----------------------------------------------------------
#
# Engine images ship almost no .pyc, so every container compiles its imports
# again: measured on vLLM v0.26.0, ~11 s of every start. PYTHONPYCACHEPREFIX
# under the engine cache keeps them, one directory per image, because a .pyc
# is checked against its source's mtime and size only.

KEY_VLLM="$(so_pycache_key "quay.io/x/vllm-openai:v0.11")"
KEY_SGLANG="$(so_pycache_key "lmsysorg/sglang:latest")"

# The vLLM caches already on a claim; only the bytecode is missing.
f_engine_cache_on_claim_no_pyc() {
    f_engine_cache_on_claim
    FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env |= map(select(.name != "PYTHONPYCACHEPREFIX"))')"
}

# SGLang that drains and keeps its weights, with nothing for the bytecode.
f_sglang_no_pyc() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"server","image":"lmsysorg/sglang:latest",
                       "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["python3","-m","sglang.launch_server","--model-path","meta-llama/Llama-3.1-8B"]}]}}}}'
}

CASE="bytecode: the vLLM engine-cache half carries PYTHONPYCACHEPREFIX, keyed by the image"
reset; f_engine_cache_only_gap
before=$FAILED
assert_field 9 "unset"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "#   bytecode:"
assert_contains "$doc" "- name: PYTHONPYCACHEPREFIX"
assert_contains "$doc" "value: /engine-cache/pycache/${KEY_VLLM}"
assert_contains "$doc" "image reference quay.io/x/vllm-openai:v0.11"
assert_eq "${#KEY_VLLM}" "12" "key length"
d="$(mktemp)"; printf '%s\n' "$doc" > "$d"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env[] | select(.name == "PYTHONPYCACHEPREFIX") | .value' "$d" 2>/dev/null)" \
    "/engine-cache/pycache/${KEY_VLLM}" "PYTHONPYCACHEPREFIX read back"
assert_eq "$(yq eval '.spec.template.spec.volumes | length' "$d" 2>/dev/null)" "1" "one volume: the bytecode shares the vLLM half's"
rm -f "$d"
# ...and the console says so with the vLLM warning, not a second one.
warn="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1 >/dev/null)"
assert_contains "$warn" "COMPILES FROM NOTHING"
assert_contains "$warn" "Python bytecode in the container"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: two images get two keys, and a digest is part of the key"
reset
before=$FAILED
k1="$(so_pycache_key "quay.io/x/vllm-openai:v0.11")"
k2="$(so_pycache_key "quay.io/x/vllm-openai:v0.12")"
k3="$(so_pycache_key "quay.io/x/vllm-openai:v0.11@sha256:0000000000000000000000000000000000000000000000000000000000000000")"
# Pinned: the first 12 hex digits of the SHA-256 of the exact string, as
# the fixtures above write it; and the shasum branch (macOS) gives the same.
assert_eq "$k1" "5f8fcd7456cd" "key of the exact string"
assert_eq "$KEY_SGLANG" "596e0fdddd36" "SGLang key"
SUMS="$(mktemp -d)"
if command -v shasum >/dev/null 2>&1; then
    ln -s "$(command -v shasum)" "$SUMS/shasum"
    assert_eq "$(PATH="$SUMS" so_pycache_key "quay.io/x/vllm-openai:v0.11")" "5f8fcd7456cd" "key through shasum"
else
    fail "shasum is not installed, so the macOS branch of so_pycache_key was not run"
fi
assert_eq "$(PATH="$SUMS/none" so_pycache_key "quay.io/x/vllm-openai:v0.11"; echo "rc=$?")" "rc=1" "no hash tool: no key"
rm -rf "$SUMS"
[ -n "$k2" ] && [ "$k1" != "$k2" ] || fail "two tags gave one key: '$k1' '$k2'"
[ -n "$k3" ] && [ "$k1" != "$k3" ] || fail "a digest did not change the key: '$k1' '$k3'"
so_pycache_key "" >/dev/null && fail "an empty image must give no key"
# ...and through the patch: the same pod spec on another image, another path.
f_engine_cache_only_gap
doc1="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
FIXTURE="${FIXTURE//vllm-openai:v0.11/vllm-openai:v0.12}"
doc2="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc1" "/engine-cache/pycache/${k1}"
assert_contains "$doc2" "/engine-cache/pycache/${k2}"
assert_not_contains "$doc2" "/engine-cache/pycache/${k1}"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: a PYTHONPYCACHEPREFIX the workload sets is respected, value or valueFrom"
reset
before=$FAILED
FIXTURE='{"spec":{"template":{"spec":{
    "terminationGracePeriodSeconds":120,
    "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"},
                          {"name":"PYTHONPYCACHEPREFIX","value":"/opt/my-pyc"}],
                   "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                   "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 9 "set:/opt/my-pyc"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1)"
assert_contains "$doc" "- name: VLLM_CACHE_ROOT"          # anchor: the vLLM half is still written
assert_not_contains "$doc" "- name: PYTHONPYCACHEPREFIX"
assert_not_contains "$doc" "#   bytecode:"
assert_not_contains "$doc" "Python bytecode"
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env[1] = {"name":"PYTHONPYCACHEPREFIX","valueFrom":{"configMapKeyRef":{"name":"c","key":"k"}}}')"
assert_field 9 "set:"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "- name: VLLM_CACHE_ROOT"
assert_not_contains "$doc" "- name: PYTHONPYCACHEPREFIX"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: a prefix keyed for another image is warned about, and left as set"
reset; f_engine_cache_on_claim
before=$FAILED
FIXTURE="${FIXTURE//pycache\/${KEY_VLLM}/pycache\/aaaaaaaaaaaa}"
assert_field 9 "set:/engine-cache/pycache/aaaaaaaaaaaa"
warn="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1 >/dev/null)"
assert_contains "$warn" "keyed for another image"
assert_contains "$warn" ".../pycache/${KEY_VLLM}"
assert_eq "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" "" "document"
# ...while the right key is silent (anchor: the warning is about the key).
reset; f_engine_cache_on_claim
assert_eq "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1)" "" "output for the right key"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: a vLLM cache already on a claim gets the prefix under that mount, env only"
reset; f_engine_cache_on_claim_no_pyc
before=$FAILED
assert_field 7 "volume:/engine-cache/vllm"
assert_field 9 "mount:/engine-cache"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "#   bytecode:"
assert_contains "$doc" "value: /engine-cache/pycache/${KEY_VLLM}"
assert_not_contains "$doc" "#   engine-cache:"
assert_not_contains "$doc" "volumes:"
assert_not_contains "$doc" "volumeMounts:"
assert_not_contains "$doc" "- name: VLLM_CACHE_ROOT"
d="$(mktemp)"; printf '%s\n' "$doc" > "$d"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env | length' "$d" 2>/dev/null)" "1" "env read back"
rm -f "$d"
warn="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1 >/dev/null)"
assert_contains "$warn" "RECOMPILES ITS PYTHON BYTECODE"
# A hostPath counts the same way, under its own mount.
reset; f_engine_cache_on_hostpath
assert_field 9 "mount:/mnt/caches"
assert_contains "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" "value: /mnt/caches/pycache/${KEY_VLLM}"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: SGLang gets the engine-cache volume for the bytecode alone, with the guard"
reset; f_sglang_no_pyc
before=$FAILED
assert_field 1 "server"
assert_field 7 ""
assert_field 9 "unset"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "#   bytecode:"
assert_contains "$doc" "value: /engine-cache/pycache/${KEY_SGLANG}"
assert_contains "$doc" "mountPath: /engine-cache"
assert_contains "$doc" "claimName: engine-cache"
assert_contains "$doc" "engine-cache does NOT exist yet in ns"   # the claim note travels with the volume
# The guard travels with it: an unwritable prefix is slower than none.
assert_contains "$doc" "for v in VLLM_CACHE_ROOT FLASHINFER_WORKSPACE_BASE TRITON_CACHE_DIR PYTHONPYCACHEPREFIX; do"
assert_not_contains "$doc" "- name: VLLM_CACHE_ROOT"
assert_not_contains "$doc" "preStop"                             # it drains already
# ...and a mount already at the engine-cache path is used, with no new volume.
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.volumes += [{"name":"ec","persistentVolumeClaim":{"claimName":"engine-cache"}}]
    | .spec.template.spec.containers[0].volumeMounts += [{"name":"ec","mountPath":"/engine-cache"}]')"
assert_field 9 "mount:/engine-cache"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "value: /engine-cache/pycache/${KEY_SGLANG}"
assert_not_contains "$doc" "volumes:"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: the simulator is not a Python engine"
reset
before=$FAILED
FIXTURE='{"spec":{"template":{"spec":{
    "containers":[{"name":"sim","image":"ghcr.io/llm-d/llm-d-inference-sim:v0.9.0",
                   "args":["--model","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 1 "sim"           # anchor: found, as an engine for the drain half
assert_field 9 ""
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "preStop"
assert_not_contains "$doc" "PYTHONPYCACHEPREFIX"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: the summary counts it, and an env-only half is emitted, never applied"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_on_claim_no_pyc
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
dir="$(mktemp -d)"
DRIVER_RC=0
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" llmsm_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$dir/log")" "1 model server(s) RECOMPILE THEIR PYTHON BYTECODE"
assert_not_contains "$(cat "$dir/log")" "COMPILE FROM NOTHING"
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$dir/log")" "need only a volume or a cache variable"
assert_contains "$(cat "$dir/p.yaml")" "value: /engine-cache/pycache/${KEY_VLLM}"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: without the engine-cache opt-in the prefix is trimmed with the half; with it, kept"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany model-pvc:ReadWriteMany)"
dir="$(mktemp -d)"
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    llmsm_workload_patch >"$dir/log" 2>&1 || true
assert_contains "$PATCH_BODY" "preStop"
assert_not_contains "$PATCH_BODY" "PYTHONPYCACHEPREFIX"
assert_contains "$(cat "$dir/p.yaml")" "PYTHONPYCACHEPREFIX"     # emitted still
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany model-pvc:ReadWriteMany)"
LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_APPLY=true LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    LLMSM_WORKLOAD_PATCH_FILE="$dir/p.yaml" llmsm_workload_patch >"$dir/log" 2>&1 || true
assert_contains "$PATCH_BODY" "value: /engine-cache/pycache/${KEY_VLLM}"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: a bytecode-only half is applied with its opt-in, and its missing guard is named for what it costs"
reset; LIST_FIXTURE="$(serving_list)"; f_sglang_no_pyc
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_FIXTURE='{"status":{"desiredNumberScheduled":2,"numberReady":2}}'
engine_apply_run
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "value: /engine-cache/pycache/${KEY_SGLANG}"
assert_contains "$PATCH_BODY" "claimName: engine-cache"
assert_contains "$(cat "$DRIVER_LOG")" "starts slower than with no PYTHONPYCACHEPREFIX"
assert_not_contains "$(cat "$DRIVER_LOG")" "loses the replica"
# ...while the vLLM half warns of the replica (anchor).
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_FIXTURE='{"status":{"desiredNumberScheduled":2,"numberReady":2}}'
engine_apply_run
assert_contains "$(cat "$DRIVER_LOG")" "loses the replica"
[ "$FAILED" -eq "$before" ] && ok

CASE="flashinfer: a bytecode volume applied with its opt-in does not carry the FlashInfer half live"
reset; LIST_FIXTURE="$(serving_list)"; f_sglang_no_pyc
before=$FAILED
# SGLang with the old variable: the document has the bytecode half (with the
# engine-cache volume) and the FlashInfer half (FLASHINFER_WORKSPACE_BASE,
# env only). The bytecode half may go live; the FlashInfer half never does.
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"FLASHINFER_WORKSPACE_DIR","value":"/engine-cache/flashinfer"}]')"
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_FIXTURE='{"status":{"desiredNumberScheduled":2,"numberReady":2}}'
engine_apply_run
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "value: /engine-cache/pycache/${KEY_SGLANG}"
assert_contains "$PATCH_BODY" "claimName: engine-cache"
assert_not_contains "$PATCH_BODY" "FLASHINFER_WORKSPACE_BASE"
assert_contains "$(cat "$DRIVER_OUT")" "- name: FLASHINFER_WORKSPACE_BASE"     # ...but the file has it
# ...while the vLLM half keeps the FlashInfer variable it sets itself (anchor).
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_FIXTURE='{"status":{"desiredNumberScheduled":2,"numberReady":2}}'
engine_apply_run
assert_contains "$PATCH_BODY" "VLLM_CACHE_ROOT"
assert_contains "$PATCH_BODY" "FLASHINFER_WORKSPACE_BASE"
[ "$FAILED" -eq "$before" ] && ok

# The guard is emitted as text for the chart's command. Run that text, as
# written in the patch, against real directories: a writable prefix is kept,
# an unwritable one is unset with one log line.
guard_from() {   # $1 the emitted document; prints the guard as a script
    printf '%s\n' "$1" | sed -n '/^#     for v in /,/^#     done$/p' | sed 's/^#     //'
}

CASE="bytecode: the emitted guard keeps a writable prefix and unsets an unwritable one"
reset; f_sglang_no_pyc
before=$FAILED
G="$(mktemp -d)"
guard_from "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" > "$G/guard.sh"
grep -q 'PYTHONPYCACHEPREFIX; do$' "$G/guard.sh" || fail "no guard extracted: $(cat "$G/guard.sh")"
printf '%s\n' 'echo "PYC=${PYTHONPYCACHEPREFIX-unset}"' >> "$G/guard.sh"
mkdir -p "$G/ok" "$G/ro"
out="$(PYTHONPYCACHEPREFIX="$G/ok/pycache/k" sh "$G/guard.sh" 2>&1)"
assert_contains "$out" "PYC=$G/ok/pycache/k"
assert_not_contains "$out" "not writable"
if [ "$(id -u)" = 0 ]; then
    ok_note="(the unwritable half needs a non-root check runner)"
    assert_contains "$ok_note" "non-root"
else
    chmod 555 "$G/ro"
    out="$(PYTHONPYCACHEPREFIX="$G/ro/pycache/k" sh "$G/guard.sh" 2>&1)"
    chmod 755 "$G/ro"
    assert_contains "$out" "PYC=unset"
    assert_contains "$out" "engine cache PYTHONPYCACHEPREFIX=$G/ro/pycache/k is not writable"
    assert_eq "$(printf '%s\n' "$out" | grep -c 'not writable')" "1" "log lines"
fi
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

CASE="guard: an entry anywhere under FLASHINFER_WORKSPACE_BASE that this user cannot write sends it to its own copy, else unsets it"
reset; f_vllm_plain
before=$FAILED
G="$(mktemp -d)"
guard_from "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" > "$G/guard.sh"
grep -q 'FLASHINFER_WORKSPACE_BASE TRITON_CACHE_DIR PYTHONPYCACHEPREFIX; do$' "$G/guard.sh" || fail "no guard extracted: $(cat "$G/guard.sh")"
printf '%s\n' 'echo "FI=${FLASHINFER_WORKSPACE_BASE-unset}"' >> "$G/guard.sh"
mkdir -p "$G/fi/.cache/flashinfer/90/cached_ops/x"; echo x > "$G/fi/.cache/flashinfer/90/cached_ops/x/x.so"
out="$(FLASHINFER_WORKSPACE_BASE="$G/fi" sh "$G/guard.sh" 2>&1)"
assert_contains "$out" "FI=$G/fi"
assert_not_contains "$out" "cannot write"
out="$(FLASHINFER_WORKSPACE_BASE="$G/new/fi" sh "$G/guard.sh" 2>&1)"
assert_contains "$out" "FI=$G/new/fi"            # a base that does not exist yet is made, and kept
if [ "$(id -u)" = 0 ]; then
    ok_note="(the unwritable half needs a non-root check runner)"
    assert_contains "$ok_note" "non-root"
else
    me="$(id -u)"
    # A directory deep in the tree, as another UID would have made it: the top
    # passes the .writable probe, the tree does not. The tree is this user's
    # (its marker), so this is not "another user's": it gets its own line.
    chmod 555 "$G/fi/.cache/flashinfer/90/cached_ops/x"
    out="$(FLASHINFER_WORKSPACE_BASE="$G/fi" sh "$G/guard.sh" 2>&1)"
    assert_contains "$out" "FI=$G/fi.uid-$me"
    assert_contains "$out" "engine cache FLASHINFER_WORKSPACE_BASE=$G/fi holds an entry this user cannot write; using $G/fi.uid-$me, user $me's own"
    assert_not_contains "$out" "another user's"
    assert_eq "$(printf '%s\n' "$out" | grep -c 'engine cache')" "1" "log lines"
    # Its own copy taken too, and nowhere to make one: the engine default.
    rm -rf "$G/fi.uid-$me"; : > "$G/fi.uid-$me"; : > "$G/fi/.uid-$me"
    out="$(FLASHINFER_WORKSPACE_BASE="$G/fi" sh "$G/guard.sh" 2>&1)"
    assert_contains "$out" "FI=unset"
    assert_contains "$out" "engine cache FLASHINFER_WORKSPACE_BASE=$G/fi holds an entry this user cannot write; using the engine default instead"
    assert_eq "$(printf '%s\n' "$out" | grep -c 'engine cache')" "1" "log lines, no copy"
    chmod 755 "$G/fi/.cache/flashinfer/90/cached_ops/x"
    # ...and a file: the ninja log FlashInfer appends to when it builds.
    chmod 444 "$G/fi/.cache/flashinfer/90/cached_ops/x/x.so"
    out="$(FLASHINFER_WORKSPACE_BASE="$G/fi" sh "$G/guard.sh" 2>&1)"
    assert_contains "$out" "FI=unset"
    chmod 644 "$G/fi/.cache/flashinfer/90/cached_ops/x/x.so"
    rm -f "$G/fi.uid-$me" "$G/fi/.uid-$me"
    # The check is for FlashInfer only: an unwritable file under the vLLM
    # cache root is not this check's to judge.
    mkdir -p "$G/v/sub"; chmod 555 "$G/v/sub"
    out="$(VLLM_CACHE_ROOT="$G/v" sh -c ". $G/guard.sh; echo \"V=\${VLLM_CACHE_ROOT-unset}\"" 2>&1)"
    chmod 755 "$G/v/sub"
    assert_contains "$out" "V=$G/v"
fi
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

# --- the guard reads the installed engine -------------------------------------
#
# The rest of the guard decides from the installed package, never the image
# tag: the vLLM version in its dist-info METADATA (VLLM_USE_MEGA_AOT_ARTIFACT
# on 0.26 only) and whether the engine package ships its bytecode (the prefix
# then only costs, so it is unset). Run the emitted text, as written, under
# every POSIX shell here, against fake site-packages trees found through the
# real python3 of the runner.
guard_all_from() {   # $1 the emitted document; prints the whole guard as a script
    printf '%s\n' "$1" | awk '/^#     for v in /{on=1} on && !/^#     /{exit} on{sub(/^#     /, ""); print}'
}
command -v python3 >/dev/null 2>&1 || { printf 'FATAL: python3 is required to run the guard cases.\n' >&2; exit 1; }
PYTAG="$(python3 -S -c 'import sys; print(sys.implementation.cache_tag)')"
GUARD_SHELLS="sh"
command -v dash >/dev/null 2>&1 && GUARD_SHELLS="$GUARD_SHELLS dash"
GUARD_SHELLS="$GUARD_SHELLS bash"
# fake_engine <site-packages> <pkg> <version> <pyc: none|all|init>: the engine
# package, its dist-info, and (once per tree) a torch package with the same
# bytecode, since the guard asks both.
fake_engine() {
    local sp="$1" pkg="$2" ver="$3" pyc="$4" f
    fake_dist "$sp" "$pkg" "$ver"
    fake_pkg "$sp" "$pkg" "$pyc"
    [ -d "$sp/torch" ] || fake_pkg "$sp" torch "$pyc"
}
fake_dist() {   # <site-packages> <name> <version>: a dist-info alone
    mkdir -p "$1/$2-$3.dist-info"
    printf 'Metadata-Version: 2.1\nName: %s\nVersion: %s\n\nVersion: 9.9.9 in the body is not the version\n' "$2" "$3" \
        > "$1/$2-$3.dist-info/METADATA"
}
fake_pkg() {    # <dir> <pkg> <pyc: none|all|init>: a package alone
    local f
    mkdir -p "$1/$2/__pycache__"
    for f in __init__ envs logger scripts; do : > "$1/$2/$f.py"; done
    case "$3" in
        all)  for f in __init__ envs logger scripts; do : > "$1/$2/__pycache__/$f.$PYTAG.pyc"; done ;;
        init) : > "$1/$2/__pycache__/__init__.$PYTAG.pyc" ;;
    esac
}
# run_guard <shell> <guard> <site-packages> [VAR=value ...]: the guard, then what it left
run_guard() {
    local shell="$1" g="$2" sp="$3"
    shift 3
    env -u VLLM_USE_MEGA_AOT_ARTIFACT -u PYTHONPYCACHEPREFIX -u VLLM_CACHE_ROOT \
        PYTHONPATH="$sp" "$@" "$shell" -c ". \"$g\"; echo \"AOT=\${VLLM_USE_MEGA_AOT_ARTIFACT-unset} PYC=\${PYTHONPYCACHEPREFIX-unset}\"" 2>&1
}
# modeof PATH: its mode and numeric owner, "drwxr-xr-x 1000"
modeof() { ls -ldn "$1" | awk '{ print substr($1, 1, 10), $3 }'; }
# run_loop <shell> <guard loop> [VAR=value ...]: the loop alone, then where two caches landed.
# LOOP_UMASK sets the umask it runs under; LOOP_PATH is put first on its PATH.
run_loop() {
    local shell="$1" g="$2"
    shift 2
    env -u VLLM_CACHE_ROOT -u FLASHINFER_WORKSPACE_BASE -u TRITON_CACHE_DIR -u PYTHONPYCACHEPREFIX \
        PATH="${LOOP_PATH:+$LOOP_PATH:}$PATH" "$@" \
        "$shell" -c "umask ${LOOP_UMASK:-022}; . \"$g\"; echo \"VLLM=\${VLLM_CACHE_ROOT-unset} PYC=\${PYTHONPYCACHEPREFIX-unset}\"" 2>&1
}

# A node cache is shared by every engine on the node, and the first one to
# start under a directory makes what is under it as its own user (its marker,
# a bytecode key directory 755). An engine of a second user used to fail the
# check and lose every cache, on every start. It now keeps a directory of its
# own beside that one (else inside it), and never writes the first user's.
# This case fakes the other user (a runner cannot own a file as one); the
# next one runs real users when it can.
CASE="guard: a cache another user started first is left to it; a second user gets its own, warm from its second start"
reset; f_engine_cache_only_gap
before=$FAILED
G="$(mktemp -d)"
guard_from "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" > "$G/guard.sh"
grep -q 'PYTHONPYCACHEPREFIX; do$' "$G/guard.sh" || fail "no guard extracted: $(cat "$G/guard.sh")"
me="$(id -u)"
# mktemp for a filesystem that does not report owners: the file it makes
# reads as another user's (a link to /dev/null, which root owns)
mkdir -p "$G/noown"
printf '%s\n' '#!/bin/sh' 'f="${1%XXXXXX}fake"; ln -s /dev/null "$f" && echo "$f"' > "$G/noown/mktemp"
chmod 755 "$G/noown/mktemp"
if [ "$me" = 0 ]; then
    skip "as root every file reads as owned by this user; the real-user case below covers it"
fi
for sh_ in $GUARD_SHELLS; do
    [ "$me" != 0 ] || break
    C="$G/$sh_"
    mkdir -p "$C/ec/vllm" "$C/ec/pycache"
    # The first user: kept where it is, silently, and nothing left beside it
    # (the owner probe included).
    out="$(run_loop "$sh_" "$G/guard.sh" VLLM_CACHE_ROOT="$C/ec/vllm" PYTHONPYCACHEPREFIX="$C/ec/pycache/k")"
    assert_contains "$out" "VLLM=$C/ec/vllm PYC=$C/ec/pycache/k"
    assert_not_contains "$out" "engine cache"
    assert_eq "$(ls -A "$C/ec" | tr '\n' ' ')" "pycache vllm " "$sh_: entries beside the first user's caches"
    assert_eq "$(ls -A "$C/ec/vllm" | tr '\n' ' ')" ".writable " "$sh_: entries in the first user's vLLM root"
    # Now another user's: a marker that is not this user's plain file (a
    # link), and a bytecode key directory this user cannot write (555).
    rm -f "$C/ec/vllm/.writable" "$C/ec/pycache/k/.writable"
    ln -s /dev/null "$C/ec/vllm/.writable"
    ln -s /dev/null "$C/ec/pycache/k/.writable"
    chmod 555 "$C/ec/pycache/k"
    # ...under umask 000, which the user's own directory must not inherit
    out="$(LOOP_UMASK=000 run_loop "$sh_" "$G/guard.sh" VLLM_CACHE_ROOT="$C/ec/vllm" PYTHONPYCACHEPREFIX="$C/ec/pycache/k")"
    assert_contains "$out" "VLLM=$C/ec/vllm.uid-$me PYC=$C/ec/pycache/k.uid-$me"
    assert_contains "$out" "engine cache VLLM_CACHE_ROOT=$C/ec/vllm is another user's; using $C/ec/vllm.uid-$me, user $me's own"
    assert_contains "$out" "engine cache PYTHONPYCACHEPREFIX=$C/ec/pycache/k is another user's; using $C/ec/pycache/k.uid-$me, user $me's own"
    assert_not_contains "$out" "not writable"
    assert_eq "$(modeof "$C/ec/vllm.uid-$me") $(modeof "$C/ec/pycache/k.uid-$me")" "drwxr-xr-x $me drwxr-xr-x $me" "$sh_: the user's own directories under umask 000"
    assert_eq "$(ls -A "$C/ec/vllm" | tr '\n' ' ')" ".writable " "$sh_: the first user's vLLM root, after the second user"
    assert_eq "$(ls -A "$C/ec/pycache/k" | tr '\n' ' ')" ".writable " "$sh_: the first user's bytecode, after the second user"
    # Its second start finds what its first one left; a trailing / is the same directory.
    : > "$C/ec/pycache/k.uid-$me/compiled"
    out="$(run_loop "$sh_" "$G/guard.sh" VLLM_CACHE_ROOT="$C/ec/vllm/" PYTHONPYCACHEPREFIX="$C/ec/pycache/k")"
    assert_contains "$out" "VLLM=$C/ec/vllm.uid-$me PYC=$C/ec/pycache/k.uid-$me"
    [ -f "$C/ec/pycache/k.uid-$me/compiled" ] || fail "$sh_: the second user's own bytecode is gone on its second start"
    # No room beside it (the parent is the node directory, root's 755): inside it.
    mkdir -p "$C/ec/vllm2"
    ln -s /dev/null "$C/ec/vllm2/.writable"
    chmod 555 "$C/ec"
    out="$(run_loop "$sh_" "$G/guard.sh" VLLM_CACHE_ROOT="$C/ec/vllm2")"
    chmod 755 "$C/ec"
    assert_contains "$out" "VLLM=$C/ec/vllm2/.uid-$me PYC=unset"
    assert_contains "$out" "using $C/ec/vllm2/.uid-$me, user $me's own"
    # Never a link where the user's own directory goes, even to a directory
    # this user owns: whoever planted it can point it elsewhere later. Not a
    # directory another user made either (a link to /, root's). With no other
    # place, the engine default.
    mkdir -p "$C/mine" "$C/ec/pycache/j" "$C/ec/pycache/i"
    ln -s "$C/mine" "$C/ec/pycache/j.uid-$me"
    ln -s / "$C/ec/pycache/i.uid-$me"
    chmod 555 "$C/ec/pycache/j" "$C/ec/pycache/i"
    for k in j i; do
        out="$(run_loop "$sh_" "$G/guard.sh" PYTHONPYCACHEPREFIX="$C/ec/pycache/$k")"
        assert_contains "$out" "VLLM=unset PYC=unset"
        assert_contains "$out" "engine cache PYTHONPYCACHEPREFIX=$C/ec/pycache/$k is not writable; using the engine default instead"
        assert_eq "$(printf '%s\n' "$out" | grep -c 'engine cache')" "1" "$sh_: log lines for $k"
    done
    assert_eq "$(ls -A "$C/mine" | tr '\n' ' ')" "" "$sh_: the directory the planted link points at"
    # A filesystem that does not report owners: the directory is used when it
    # is writable, whoever the marker says made it, as before this guard knew
    # of owners -- and never a per-user directory, which would be decided by
    # owners it cannot read.
    out="$(LOOP_PATH="$G/noown" run_loop "$sh_" "$G/guard.sh" VLLM_CACHE_ROOT="$C/ec/vllm")"
    assert_contains "$out" "VLLM=$C/ec/vllm PYC=unset"
    assert_contains "$out" "engine cache VLLM_CACHE_ROOT=$C/ec/vllm: this filesystem does not report file owners; using it because it is writable"
    assert_eq "$(printf '%s\n' "$out" | grep -c 'engine cache')" "1" "$sh_: log lines without owners"
    [ ! -e "$C/ec/vllm/.own.fake" ] && [ ! -L "$C/ec/vllm/.own.fake" ] || fail "$sh_: the owner probe was left behind"
    chmod 755 "$C/ec/pycache/k" "$C/ec/pycache/j" "$C/ec/pycache/i"
done
rm -rf "$G"
[ "$me" != 0 ] && [ "$FAILED" -eq "$before" ] && ok

# A directory of its own that the guard made and then refused is removed
# again: nothing empty is left beside the other user's cache. On a
# filesystem that does not report owners -O refuses it; here a chmod that
# always fails does, which works for any user, root included.
CASE="guard: a directory of its own that it made and then refused is removed, not left empty"
reset; f_engine_cache_only_gap
before=$FAILED
G="$(mktemp -d)"
guard_from "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" > "$G/guard.sh"
grep -q 'PYTHONPYCACHEPREFIX; do$' "$G/guard.sh" || fail "no guard extracted: $(cat "$G/guard.sh")"
me="$(id -u)"
mkdir -p "$G/nochmod"
printf '%s\n' '#!/bin/sh' 'exit 1' > "$G/nochmod/chmod"
chmod 755 "$G/nochmod/chmod"
for sh_ in $GUARD_SHELLS; do
    C="$G/$sh_"
    # Another user's: its marker is a link, and (for a non-root user) the
    # directory is not writable.
    mkdir -p "$C/ec/pycache/h"
    ln -s /dev/null "$C/ec/pycache/h/.writable"
    chmod 555 "$C/ec/pycache/h"
    out="$(LOOP_PATH="$G/nochmod" run_loop "$sh_" "$G/guard.sh" PYTHONPYCACHEPREFIX="$C/ec/pycache/h")"
    assert_contains "$out" "VLLM=unset PYC=unset"
    assert_contains "$out" "engine cache PYTHONPYCACHEPREFIX=$C/ec/pycache/h is not writable; using the engine default instead"
    [ ! -e "$C/ec/pycache/h.uid-$me" ] || fail "$sh_: a refused directory of its own was left behind: $C/ec/pycache/h.uid-$me"
    [ ! -e "$C/ec/pycache/h/.uid-$me" ] || fail "$sh_: a refused directory of its own was left behind: $C/ec/pycache/h/.uid-$me"
    # One that was there before it ran is never removed.
    mkdir -p "$C/ec/pycache/h.uid-$me"
    out="$(LOOP_PATH="$G/nochmod" run_loop "$sh_" "$G/guard.sh" PYTHONPYCACHEPREFIX="$C/ec/pycache/h")"
    [ -d "$C/ec/pycache/h.uid-$me" ] || fail "$sh_: a directory that was there before the guard ran was removed"
    chmod 755 "$C/ec/pycache/h"
done
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

# The same, as real users: engines run under setpriv as other UIDs, on the
# layout the preparer makes (a root 755 node directory, 1777 cache
# directories). It needs root or passwordless sudo, and setpriv -- CI has
# both. Without them it FAILS, unless LLMSM_CHECK_SKIP_MULTIUSER=1 says to
# skip it, which it then says out loud.
CASE="guard, as real users: the first keeps its cache, a second (root included) gets its own, a planted link is refused"
reset; f_engine_cache_only_gap
before=$FAILED
AS_ROOT=none
if [ "$(id -u)" = 0 ]; then AS_ROOT=""; elif sudo -n true 2>/dev/null; then AS_ROOT="sudo -n"; fi
command -v setpriv >/dev/null 2>&1 || AS_ROOT=none
if [ "$AS_ROOT" = none ]; then
    if [ "${LLMSM_CHECK_SKIP_MULTIUSER:-}" = 1 ]; then
        skip "no root, passwordless sudo or setpriv here (LLMSM_CHECK_SKIP_MULTIUSER=1)"
    else
        fail "needs root or passwordless sudo, and setpriv, to run engines as other users; LLMSM_CHECK_SKIP_MULTIUSER=1 skips it, out loud"
    fi
else
    R="$(mktemp -d)"
    case "$R" in /tmp/?*|/var/tmp/?*) ;; *) fail "mktemp -d gave '$R'"; R="" ;; esac
fi
if [ "$AS_ROOT" != none ] && [ -n "$R" ]; then
    chmod 755 "$R"
    guard_from "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" > "$R/guard.sh"
    chmod 644 "$R/guard.sh"
    $AS_ROOT sh -c 'mkdir -p "$1/n/vllm" "$1/n/pycache" "$1/n/flashinfer" && chown 0:0 "$1/n" && chmod 755 "$1/n" && chmod 1777 "$1/n/vllm" "$1/n/pycache" "$1/n/flashinfer"' sh "$R" \
        || fail "could not lay out the node directory as root"
    # eng UID [UMASK]: the guard as that user, then where each cache landed,
    # then what an engine writes under each (FlashInfer: a kernel directory
    # in its tree, as it makes one under its umask)
    eng() {
        $AS_ROOT setpriv --reuid="$1" --regid="$1" --clear-groups env -i PATH=/usr/bin:/bin \
            VLLM_CACHE_ROOT="$R/n/vllm" PYTHONPYCACHEPREFIX="$R/n/pycache/k" FLASHINFER_WORKSPACE_BASE="$R/n/flashinfer/k" \
            sh -c "umask ${2:-022}; . \"$R/guard.sh\"; echo \"VLLM=\${VLLM_CACHE_ROOT-unset} PYC=\${PYTHONPYCACHEPREFIX-unset} FI=\${FLASHINFER_WORKSPACE_BASE-unset}\"
                for d in \"\${VLLM_CACHE_ROOT:-}\" \"\${PYTHONPYCACHEPREFIX:-}\"; do
                    [ -z \"\$d\" ] || { mkdir -p \"\$d/key\" && : > \"\$d/key/f\"; } || echo \"WRITE FAILED \$d\"
                done
                d=\${FLASHINFER_WORKSPACE_BASE:-}
                [ -z \"\$d\" ] || { mkdir -p \"\$d/.cache/flashinfer/90/cached_ops/op\" && : > \"\$d/.cache/flashinfer/90/cached_ops/op/op.so\"; } || echo \"WRITE FAILED \$d\"" 2>&1
    }
    out="$(eng 4242)"
    assert_contains "$out" "VLLM=$R/n/vllm PYC=$R/n/pycache/k FI=$R/n/flashinfer/k"
    assert_not_contains "$out" "engine cache"
    assert_not_contains "$out" "WRITE FAILED"
    [ ! -e "$R/n/flashinfer/k/.engine-uid" ] && [ ! -L "$R/n/flashinfer/k/.engine-uid" ] || fail "the guard left a .engine-uid claim"
    # root second: its own, beside (root can write the node directory)
    out="$(eng 0)"
    assert_contains "$out" "VLLM=$R/n/vllm.uid-0 PYC=$R/n/pycache/k.uid-0 FI=$R/n/flashinfer/k.uid-0"
    assert_contains "$out" "engine cache VLLM_CACHE_ROOT=$R/n/vllm is another user's; using $R/n/vllm.uid-0, user 0's own"
    assert_contains "$out" "engine cache FLASHINFER_WORKSPACE_BASE=$R/n/flashinfer/k is another user's; using $R/n/flashinfer/k.uid-0, user 0's own"
    assert_not_contains "$out" "WRITE FAILED"
    # a third user, under umask 000: inside the vLLM root, beside the keys
    out="$(eng 4243 000)"
    assert_contains "$out" "VLLM=$R/n/vllm/.uid-4243 PYC=$R/n/pycache/k.uid-4243 FI=$R/n/flashinfer/k.uid-4243"
    assert_not_contains "$out" "WRITE FAILED"
    assert_eq "$(modeof "$R/n/vllm/.uid-4243") $(modeof "$R/n/pycache/k.uid-4243") $(modeof "$R/n/flashinfer/k.uid-4243")" "drwxr-xr-x 4243 drwxr-xr-x 4243 drwxr-xr-x 4243" "the third user's own directories"
    # Another user's own directory inside the first user's FlashInfer base,
    # as only root could make it there: 755, so the first user cannot write
    # it. The tree check passes over it.
    $AS_ROOT sh -c 'mkdir -p "$1/n/flashinfer/k/.uid-4246/.cache" && : > "$1/n/flashinfer/k/.uid-4246/.cache/f" && chown -R 4246:4246 "$1/n/flashinfer/k/.uid-4246" && chmod 755 "$1/n/flashinfer/k/.uid-4246"' sh "$R"
    # the first user again: unchanged, and none of the others wrote its tree
    out="$(eng 4242)"
    assert_contains "$out" "VLLM=$R/n/vllm PYC=$R/n/pycache/k FI=$R/n/flashinfer/k"
    assert_not_contains "$out" "engine cache"
    assert_not_contains "$out" "WRITE FAILED"
    assert_eq "$(find "$R/n/vllm" "$R/n/pycache/k" "$R/n/flashinfer/k" -path "$R/n/vllm/.uid-*" -prune -o -path "$R/n/flashinfer/k/.uid-*" -prune -o ! -user 4242 ! -path "$R/n/vllm" -print | wc -l | tr -d ' ')" "0" "entries in the first user's tree owned by another user"
    # A link planted where a user's own directory goes, to a directory that
    # user owns: refused, and nothing written through it. In a sticky
    # directory Linux already refuses to follow another user's link
    # (fs.protected_symlinks), so the link goes where that does not hold: a
    # node directory every engine can write and that is not sticky, as
    # OpenShift's 2775 group-0 one is (777 here, for a shared group).
    $AS_ROOT sh -c 'mkdir -p "$1/o/vllm" && chmod 777 "$1/o" && chmod 1777 "$1/o/vllm"' sh "$R"
    $AS_ROOT setpriv --reuid=4242 --regid=4242 --clear-groups touch "$R/o/vllm/.writable"
    $AS_ROOT setpriv --reuid=4245 --regid=4245 --clear-groups mkdir "$R/n/pycache/mine"
    $AS_ROOT setpriv --reuid=4244 --regid=4244 --clear-groups ln -s "$R/n/pycache/mine" "$R/o/vllm.uid-4245"
    out="$($AS_ROOT setpriv --reuid=4245 --regid=4245 --clear-groups env -i PATH=/usr/bin:/bin VLLM_CACHE_ROOT="$R/o/vllm" \
        sh -c ". \"$R/guard.sh\"; echo \"VLLM=\${VLLM_CACHE_ROOT-unset}\"" 2>&1)"
    assert_contains "$out" "VLLM=$R/o/vllm/.uid-4245"
    assert_contains "$out" "engine cache VLLM_CACHE_ROOT=$R/o/vllm is another user's; using $R/o/vllm/.uid-4245, user 4245's own"
    assert_eq "$(ls -A "$R/n/pycache/mine" | tr '\n' ' ')" "" "the directory the planted link points at"
    $AS_ROOT rm -rf "$R"
fi
[ "$AS_ROOT" != none ] && [ "$FAILED" -eq "$before" ] && ok

CASE="guard: VLLM_USE_MEGA_AOT_ARTIFACT=1 on an installed vLLM 0.26 only, never over a user value"
reset; f_engine_cache_only_gap
before=$FAILED
G="$(mktemp -d)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
guard_all_from "$doc" > "$G/guard.sh"
grep -q 'PYTHONPYCACHEPREFIX; do$' "$G/guard.sh" || fail "no guard extracted: $(cat "$G/guard.sh")"
assert_contains "$(cat "$G/guard.sh")" "export VLLM_USE_MEGA_AOT_ARTIFACT=1"
fake_engine "$G/v26" vllm 0.26.0 none
fake_engine "$G/v27" vllm 0.27.0 none
fake_engine "$G/v25" vllm 0.25.1 none
mkdir -p "$G/c"
for sh_ in $GUARD_SHELLS; do
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/v26" VLLM_CACHE_ROOT="$G/c/vllm" PYTHONPYCACHEPREFIX="$G/c/pyc")"
    assert_contains "$out" "AOT=1 PYC=$G/c/pyc"
    assert_contains "$out" "engine cache: vLLM 0.26.0, VLLM_USE_MEGA_AOT_ARTIFACT=1"
    # 0.27 and later: the default there, so not set.
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/v27" VLLM_CACHE_ROOT="$G/c/vllm")"
    assert_contains "$out" "AOT=unset"
    # Below 0.26 it was never measured.
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/v25" VLLM_CACHE_ROOT="$G/c/vllm")"
    assert_contains "$out" "AOT=unset"
    # A value the user set, even empty, is theirs.
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/v26" VLLM_CACHE_ROOT="$G/c/vllm" VLLM_USE_MEGA_AOT_ARTIFACT=0)"
    assert_contains "$out" "AOT=0 "
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/v26" VLLM_CACHE_ROOT="$G/c/vllm" VLLM_USE_MEGA_AOT_ARTIFACT=)"
    assert_contains "$out" "AOT= "
    # No cache root left (the loop unset it, or none was set): nothing to load from.
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/v26")"
    assert_contains "$out" "AOT=unset"
done
# LLMSM_ENGINE_CACHE_AOT=false leaves the block out of the text.
doc="$(LLMSM_ENGINE_CACHE_AOT=false so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "PYTHONPYCACHEPREFIX; do"                     # anchor: the guard is there
assert_not_contains "$doc" "VLLM_USE_MEGA_AOT_ARTIFACT"
guard_all_from "$doc" > "$G/noaot.sh"
out="$(run_guard sh "$G/noaot.sh" "$G/v26" VLLM_CACHE_ROOT="$G/c/vllm")"
assert_contains "$out" "AOT=unset"
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

CASE="guard: a prefix is unset when the installed package ships its bytecode, kept when it does not"
reset; f_engine_cache_only_gap
before=$FAILED
G="$(mktemp -d)"
guard_all_from "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" > "$G/guard.sh"
fake_engine "$G/v32" vllm 0.32.0 all
fake_engine "$G/v31" vllm 0.31.0 none
fake_engine "$G/part" vllm 0.31.0 init     # a build-time import compiled __init__ only
mkdir -p "$G/c"
for sh_ in $GUARD_SHELLS; do
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/v32" VLLM_CACHE_ROOT="$G/c/vllm" PYTHONPYCACHEPREFIX="$G/c/pyc")"
    assert_contains "$out" "AOT=unset PYC=unset"
    assert_contains "$out" "engine cache PYTHONPYCACHEPREFIX: $G/v32/vllm ships its bytecode; using that instead"
    assert_eq "$(printf '%s\n' "$out" | grep -c 'ships its bytecode')" "1" "log lines ($sh_)"
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/v31" VLLM_CACHE_ROOT="$G/c/vllm" PYTHONPYCACHEPREFIX="$G/c/pyc")"
    assert_contains "$out" "PYC=$G/c/pyc"
    assert_not_contains "$out" "ships its bytecode"
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/part" VLLM_CACHE_ROOT="$G/c/vllm" PYTHONPYCACHEPREFIX="$G/c/pyc")"
    assert_contains "$out" "PYC=$G/c/pyc"
done
# Bytecode for ANOTHER interpreter is not bytecode this one can use.
mv "$G/v32/vllm/__pycache__/envs.$PYTAG.pyc" "$G/v32/vllm/__pycache__/envs.cpython-20.pyc"
out="$(run_guard sh "$G/guard.sh" "$G/v32" PYTHONPYCACHEPREFIX="$G/c/pyc")"
assert_contains "$out" "PYC=$G/c/pyc"
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

CASE="guard: found through the site-packages of a venv interpreter, with no PYTHONPATH"
reset; f_engine_cache_only_gap
before=$FAILED
G="$(mktemp -d)"
guard_all_from "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" > "$G/guard.sh"
# A venv by hand, as `python -m venv` lays one out: pyvenv.cfg beside bin/.
PYREAL="$(python3 -S -c 'import sys, os; print(os.path.realpath(sys.executable))')"
PYVER="$(python3 -S -c 'import sys; print("python%d.%d" % sys.version_info[:2])')"
mkdir -p "$G/venv/bin" "$G/c"
printf 'home = %s\ninclude-system-site-packages = false\n' "$(dirname "$PYREAL")" > "$G/venv/pyvenv.cfg"
ln -s "$PYREAL" "$G/venv/bin/python3"
fake_engine "$G/venv/lib/$PYVER/site-packages" vllm 0.26.0 all
for sh_ in $GUARD_SHELLS; do
    out="$(env -u VLLM_USE_MEGA_AOT_ARTIFACT -u PYTHONPATH PATH="$G/venv/bin:$PATH" \
        VLLM_CACHE_ROOT="$G/c/vllm" PYTHONPYCACHEPREFIX="$G/c/pyc" \
        "$sh_" -c ". \"$G/guard.sh\"; echo \"AOT=\${VLLM_USE_MEGA_AOT_ARTIFACT-unset} PYC=\${PYTHONPYCACHEPREFIX-unset}\"" 2>&1)"
    assert_contains "$out" "AOT=1 PYC=unset"
done
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

CASE="guard: SGLang gets the bytecode check on its own package, and no AOT block"
reset; f_sglang_no_pyc
before=$FAILED
G="$(mktemp -d)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "ships its bytecode"
assert_not_contains "$doc" "VLLM_USE_MEGA_AOT_ARTIFACT"
guard_all_from "$doc" > "$G/guard.sh"
fake_engine "$G/sg" sglang 0.5.9 all
fake_engine "$G/sgno" sglang 0.5.9 none
mkdir -p "$G/c"
for sh_ in $GUARD_SHELLS; do
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/sg" PYTHONPYCACHEPREFIX="$G/c/pyc")"
    assert_contains "$out" "PYC=unset"
    assert_contains "$out" "$G/sg/sglang ships its bytecode"
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/sgno" PYTHONPYCACHEPREFIX="$G/c/pyc")"
    assert_contains "$out" "PYC=$G/c/pyc"
done
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

CASE="guard: the version is the one installed beside the package, not a stale dist-info earlier on the path"
reset; f_engine_cache_only_gap
before=$FAILED
G="$(mktemp -d)"
guard_all_from "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" > "$G/guard.sh"
fake_dist "$G/stale" vllm 0.27.0                  # a dist-info left behind, no package
fake_engine "$G/real" vllm 0.26.0 none
# ...and one installed as editable: the package in a source tree, its dist-info elsewhere.
fake_pkg "$G/src" vllm none
fake_dist "$G/sp" vllm 0.26.0
mkdir -p "$G/c"
for sh_ in $GUARD_SHELLS; do
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/stale:$G/real" VLLM_CACHE_ROOT="$G/c/vllm")"
    assert_contains "$out" "engine cache: vLLM 0.26.0, VLLM_USE_MEGA_AOT_ARTIFACT=1"
    assert_contains "$out" "AOT=1"
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/src:$G/sp" VLLM_CACHE_ROOT="$G/c/vllm")"
    assert_contains "$out" "AOT=1"
done
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

CASE="guard: AOT for a released 0.26.x only -- a +local build yes, dev, rc, unparsable or empty no"
reset; f_engine_cache_only_gap
before=$FAILED
G="$(mktemp -d)"
guard_all_from "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" > "$G/guard.sh"
mkdir -p "$G/c"
i=0
for v in "0.26.0+cu129:1" "0.26.12:1" "0.26.1.dev1:unset" "0.26.0rc1:unset" "0.26.0a1:unset" "banana:unset" ":unset"; do
    i=$((i + 1))
    fake_engine "$G/t$i" vllm "${v%:*}" none
    for sh_ in $GUARD_SHELLS; do
        out="$(run_guard "$sh_" "$G/guard.sh" "$G/t$i" VLLM_CACHE_ROOT="$G/c/vllm")"
        assert_contains "$out" "AOT=${v##*:} "
        assert_not_contains "$out" "rror"
    done
done
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

CASE="guard: torch must ship its bytecode too, and nullglob does not read an empty glob as shipped"
reset; f_engine_cache_only_gap
before=$FAILED
G="$(mktemp -d)"
guard_all_from "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" > "$G/guard.sh"
fake_engine "$G/notorch" vllm 0.32.0 all
rm -f "$G/notorch/torch/__pycache__/"*.pyc     # vllm shipped, torch not
fake_engine "$G/ok" vllm 0.32.0 all
# A package with no .py at the top (a compiled __init__): nothing to judge.
EXT="$(python3 -S -c 'import importlib.machinery as m; print(m.EXTENSION_SUFFIXES[0])')"
mkdir -p "$G/so/vllm/__pycache__"
: > "$G/so/vllm/__init__$EXT"
fake_dist "$G/so" vllm 0.32.0
fake_pkg "$G/so" torch all
mkdir -p "$G/c"
for sh_ in $GUARD_SHELLS; do
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/notorch" PYTHONPYCACHEPREFIX="$G/c/pyc")"
    assert_contains "$out" "PYC=$G/c/pyc"
    out="$(run_guard "$sh_" "$G/guard.sh" "$G/ok" PYTHONPYCACHEPREFIX="$G/c/pyc")"
    assert_contains "$out" "PYC=unset"                        # anchor
done
out="$(env -u PYTHONPYCACHEPREFIX PYTHONPATH="$G/so" PYTHONPYCACHEPREFIX="$G/c/pyc" \
    bash -O nullglob -c ". \"$G/guard.sh\"; echo \"PYC=\${PYTHONPYCACHEPREFIX-unset}\"" 2>&1)"
assert_contains "$out" "PYC=$G/c/pyc"
assert_not_contains "$out" "ships its bytecode"
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

CASE="guard: runs under set -eu, and with no python3 on PATH changes nothing"
reset; f_engine_cache_only_gap
before=$FAILED
G="$(mktemp -d)"
guard_all_from "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" > "$G/guard.sh"
fake_engine "$G/v26" vllm 0.26.0 none
fake_engine "$G/v32" vllm 0.32.0 all
mkdir -p "$G/c" "$G/bin"
for t in mkdir touch; do ln -s "$(command -v "$t")" "$G/bin/$t"; done
for sh_ in $GUARD_SHELLS; do
    shp="$(command -v "$sh_")"
    # Only the guard variables set, everything else unset: -u must not trip.
    out="$(env -u VLLM_USE_MEGA_AOT_ARTIFACT -u PYTHONPYCACHEPREFIX -u VLLM_CACHE_ROOT \
        -u FLASHINFER_WORKSPACE_BASE -u TRITON_CACHE_DIR PYTHONPATH="$G/v26" VLLM_CACHE_ROOT="$G/c/vllm" \
        "$sh_" -c "set -eu; . \"$G/guard.sh\"; echo \"AOT=\${VLLM_USE_MEGA_AOT_ARTIFACT-unset} done\"" 2>&1)"
    assert_contains "$out" "AOT=1 done"
    out="$(env -u VLLM_USE_MEGA_AOT_ARTIFACT -u FLASHINFER_WORKSPACE_BASE -u TRITON_CACHE_DIR \
        PYTHONPATH="$G/v32" PATH="$G/bin" VLLM_CACHE_ROOT="$G/c/vllm" PYTHONPYCACHEPREFIX="$G/c/pyc" \
        "$shp" -c "set -eu; . \"$G/guard.sh\"; echo \"AOT=\${VLLM_USE_MEGA_AOT_ARTIFACT-unset} PYC=\${PYTHONPYCACHEPREFIX-unset} done\"" 2>&1)"
    assert_contains "$out" "AOT=unset PYC=$G/c/pyc done"
done
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok


# The FlashInfer check must fail closed: under `set -eo pipefail` (a chart
# command often runs so), through a symbolic link at the top, without find,
# and when a find cannot read part of the tree. Run as the chart would: the
# guard sourced by sh, dash and bash.
CASE="guard: the FlashInfer check fails closed -- pipefail, a linked base, an unreadable directory, no find"
reset; f_vllm_plain
before=$FAILED
G="$(mktemp -d)"
guard_from "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" > "$G/guard.sh"
grep -q 'FLASHINFER_WORKSPACE_BASE TRITON_CACHE_DIR PYTHONPYCACHEPREFIX; do$' "$G/guard.sh" || fail "no guard extracted: $(cat "$G/guard.sh")"
mkdir -p "$G/real/.cache/flashinfer/90/cached_ops/x"
ln -s "$G/real" "$G/link"
fi_run() {   # <shell> <base> [set options]: the guard, then what it left
    FLASHINFER_WORKSPACE_BASE="$2" "$1" -c "${3:+set $3; }. \"$G/guard.sh\"; echo \"FI=\${FLASHINFER_WORKSPACE_BASE-unset}\"" 2>&1
}
me="$(id -u)"
for sh_ in $GUARD_SHELLS; do
    out="$(fi_run "$sh_" "$G/link")"
    assert_contains "$out" "FI=$G/link"                        # anchor: a clean linked tree is kept
done
if [ "$me" = 0 ]; then
    ok_note="(the unwritable half needs a non-root check runner)"
    assert_contains "$ok_note" "non-root"
else
    # Unwritable deep in the tree, reached through the link: checked, and
    # the tree not used. This user's own copy instead.
    chmod 555 "$G/real/.cache/flashinfer/90/cached_ops/x"
    for sh_ in $GUARD_SHELLS; do
        out="$(fi_run "$sh_" "$G/link")"
        assert_contains "$out" "FI=$G/link.uid-$me"
        assert_contains "$out" "engine cache FLASHINFER_WORKSPACE_BASE=$G/link holds an entry this user cannot write; using $G/link.uid-$me, user $me's own"
        rm -rf "$G/link.uid-$me"
    done
    chmod 755 "$G/real/.cache/flashinfer/90/cached_ops/x"
    # A directory find cannot read: under pipefail the old pipeline read the
    # failed find as "every entry writable" and kept the variable. With no
    # copy of its own to take, the engine default.
    : > "$G/real.uid-$me"; : > "$G/real/.uid-$me"
    chmod 000 "$G/real/.cache/flashinfer/90/cached_ops/x"
    out="$(fi_run bash "$G/real" "-eo pipefail")"
    assert_contains "$out" "FI=unset"
    assert_contains "$out" "engine cache FLASHINFER_WORKSPACE_BASE=$G/real holds an entry this user cannot write; using the engine default instead"
    out="$(fi_run bash "$G/real")"
    assert_contains "$out" "FI=unset"
    chmod 755 "$G/real/.cache/flashinfer/90/cached_ops/x"
    rm -f "$G/real.uid-$me" "$G/real/.uid-$me"
fi
# No find in the image: unset, and said as that -- not as an unwritable entry.
mkdir -p "$G/bin"
for t in mkdir touch id ln readlink; do
    p="$(command -v "$t")" && ln -s "$p" "$G/bin/$t"
done
[ ! -e "$G/bin/find" ] || fail "the find-less PATH has find"
out="$(FLASHINFER_WORKSPACE_BASE="$G/real" PATH="$G/bin" /bin/sh -c ". \"$G/guard.sh\"; echo \"FI=\${FLASHINFER_WORKSPACE_BASE-unset}\"" 2>&1)"
assert_contains "$out" "FI=unset"
assert_contains "$out" "engine cache FLASHINFER_WORKSPACE_BASE=$G/real cannot be checked: this image has no find"
assert_not_contains "$out" "cannot write"
chmod -R u+rwx "$G"; rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

# Two users on one node: a FlashInfer tree is the first user's, as the other
# caches are (its .writable marker, -O), with no claim of its own. Engines of
# that user, any number at once, keep it. Another user gets its own copy
# beside it. Another user's own directory inside the base (.uid-<uid>, which
# only root can make there) does not fail the first user's tree check. This
# case fakes the other user (a linked marker, a 555 directory); the
# real-user case above runs them.
CASE="guard: a FlashInfer tree is the first user's; another user gets its own copy, and its directory in the base is passed over"
reset; f_vllm_plain
before=$FAILED
G="$(mktemp -d)"
guard_from "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" > "$G/guard.sh"
grep -q 'FLASHINFER_WORKSPACE_BASE TRITON_CACHE_DIR PYTHONPYCACHEPREFIX; do$' "$G/guard.sh" || fail "no guard extracted: $(cat "$G/guard.sh")"
me="$(id -u)"
fi_one() { FLASHINFER_WORKSPACE_BASE="$G/fi" "$1" -c ". \"$G/guard.sh\"; echo \"FI=\${FLASHINFER_WORKSPACE_BASE-unset}\"" 2>&1; }
for sh_ in $GUARD_SHELLS; do
    rm -rf "$G/fi" "$G/fi.uid-$me"
    out="$(fi_one "$sh_")"
    assert_contains "$out" "FI=$G/fi"
    assert_not_contains "$out" "engine cache"
    assert_eq "$(ls -A "$G/fi" | tr '\n' ' ')" ".writable " "$sh_: what the guard leaves in a new base"
    # The same user again, and four at once: all keep it.
    out="$(fi_one "$sh_")"
    assert_contains "$out" "FI=$G/fi"
    rm -rf "$G/fi" "$G/par"; mkdir -p "$G/par"
    for i in 1 2 3 4; do
        fi_one "$sh_" > "$G/par/$i" &
    done
    wait
    assert_eq "$(cat "$G/par/"* | grep -c "^FI=$G/fi\$")" "4" "$sh_: concurrent starts of one user that kept the base"
    [ "$me" != 0 ] || continue
    # Another user's own directory in the base, 555 to this user: passed over.
    mkdir -p "$G/fi/.uid-4242/.cache"; chmod 555 "$G/fi/.uid-4242"
    out="$(fi_one "$sh_")"
    assert_contains "$out" "FI=$G/fi"
    assert_not_contains "$out" "engine cache"
    chmod 755 "$G/fi/.uid-4242"
    # Only at the top of the base: the same name deeper is FlashInfer's tree.
    mkdir -p "$G/fi/.cache/.uid-4242"; chmod 555 "$G/fi/.cache/.uid-4242"
    out="$(fi_one "$sh_")"
    assert_contains "$out" "FI=$G/fi.uid-$me"
    chmod 755 "$G/fi/.cache/.uid-4242"; rm -rf "$G/fi/.cache/.uid-4242" "$G/fi.uid-$me"
    # Another user's tree (a marker that is not this user's plain file): its
    # own copy beside it, said as that, and the first user's tree untouched.
    rm -f "$G/fi/.writable"; ln -s /dev/null "$G/fi/.writable"
    out="$(fi_one "$sh_")"
    assert_contains "$out" "FI=$G/fi.uid-$me"
    assert_contains "$out" "engine cache FLASHINFER_WORKSPACE_BASE=$G/fi is another user's; using $G/fi.uid-$me, user $me's own"
    assert_eq "$(printf '%s\n' "$out" | grep -c 'engine cache')" "1" "$sh_: log lines for another user's tree"
    assert_eq "$(ls -A "$G/fi" | tr '\n' ' ')" ".cache .uid-4242 .writable " "$sh_: the first user's base, after the second user"
    # Its second start finds the copy it made.
    : > "$G/fi.uid-$me/kept"
    out="$(fi_one "$sh_")"
    assert_contains "$out" "FI=$G/fi.uid-$me"
    [ -f "$G/fi.uid-$me/kept" ] || fail "$sh_: the second user's own FlashInfer tree is gone on its second start"
done
[ "$me" != 0 ] || printf 'SKIP  %s: the other-user half needs a non-root check runner; the real-user case covers it\n' "$CASE" >&2
chmod -R u+rwx "$G"; rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

# A workload that needs only the FlashInfer half still gets the guard: the
# old guard in its command does not check FLASHINFER_WORKSPACE_BASE, so the
# file must carry the one that replaces it. No AOT block: no vLLM half.
CASE="flashinfer: the FlashInfer half alone carries the guard, and the guard runs"
reset; f_engine_cache_old_flashinfer
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "#   flashinfer:"                       # anchor: the half fired alone
assert_not_contains "$doc" "#   engine-cache:"
assert_contains "$doc" "FLASHINFER_WORKSPACE_BASE TRITON_CACHE_DIR PYTHONPYCACHEPREFIX; do"
assert_contains "$doc" "replace all of it; renaming its variables is not enough"
assert_not_contains "$doc" "VLLM_USE_MEGA_AOT_ARTIFACT"
G="$(mktemp -d)"
guard_from "$doc" > "$G/guard.sh"
out="$(FLASHINFER_WORKSPACE_BASE="$G/fi" sh -c ". \"$G/guard.sh\"; echo \"FI=\${FLASHINFER_WORKSPACE_BASE-unset}\"" 2>&1)"
assert_contains "$out" "FI=$G/fi"
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

# The old name anywhere in the command is not the old guard: only a for
# loop's own word list is. An `unset FLASHINFER_WORKSPACE_DIR` -- what the
# migration asks for -- or a comment naming it must not be told to change.
CASE="flashinfer: only a guard loop over the old name reads as the old guard"
reset; f_engine_cache_old_flashinfer
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"FLASHINFER_WORKSPACE_BASE","value":"/engine-cache/flashinfer"}]
    | .spec.template.spec.containers[0].command = ["/bin/sh","-c"]')"
fi_args() { FIXTURE="$(printf '%s' "$FIXTURE" | jq -c --arg a "$1" '.spec.template.spec.containers[0].args = [$a]')"; }
fi_args "for v in VLLM_CACHE_ROOT FLASHINFER_WORKSPACE_DIR TRITON_CACHE_DIR; do :; done; exec vllm serve Qwen/Qwen3-0.6B"
assert_field 10 "loop"                                         # anchor: the old guard is seen
fi_args "unset FLASHINFER_WORKSPACE_DIR; exec vllm serve Qwen/Qwen3-0.6B"
assert_field 10 ""
fi_args "# FLASHINFER_WORKSPACE_DIR is not read by FlashInfer
exec vllm serve Qwen/Qwen3-0.6B"
assert_field 10 ""
fi_args "for m in a b; do echo \$m; done; echo FLASHINFER_WORKSPACE_DIR; exec vllm serve Qwen/Qwen3-0.6B"
assert_field 10 ""
fi_args "for x in FLASHINFER_WORKSPACE_DIRS; do :; done; exec vllm serve Qwen/Qwen3-0.6B"
assert_field 10 ""
# The old guard on its own line, the way a chart writes it: still seen.
fi_args "set -e
for v in VLLM_CACHE_ROOT FLASHINFER_WORKSPACE_DIR TRITON_CACHE_DIR PYTHONPYCACHEPREFIX; do
  :
done
exec vllm serve Qwen/Qwen3-0.6B"
assert_field 10 "loop"
[ "$FAILED" -eq "$before" ] && ok

# No image key (the image reference is empty): the base is the directory
# alone, for the engine-cache half and the FlashInfer half both.
CASE="flashinfer: with no image key the base is the directory itself"
reset; f_engine_cache_old_flashinfer
before=$FAILED
assert_eq "$(so_flashinfer_base /engine-cache/flashinfer "")" "/engine-cache/flashinfer" "no image"
assert_eq "$(so_flashinfer_base /engine-cache/flashinfer "quay.io/x/vllm-openai:v0.11")" "/engine-cache/flashinfer/$KFI" "an image"
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c 'del(.spec.template.spec.containers[0].image)')"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>"$WORK/fi.err")"
assert_contains "$doc" "#   flashinfer:"
d="$(mktemp)"; printf '%s\n' "$doc" > "$d"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env[] | select(.name == "FLASHINFER_WORKSPACE_BASE") | .value' "$d" 2>/dev/null)" \
    "/engine-cache/flashinfer" "FlashInfer half, no image"
reset; f_engine_cache_only_gap
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c 'del(.spec.template.spec.containers[0].image)')"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"; printf '%s\n' "$doc" > "$d"
assert_contains "$doc" "#   engine-cache:"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env[] | select(.name == "FLASHINFER_WORKSPACE_BASE") | .value' "$d" 2>/dev/null)" \
    "/engine-cache/flashinfer" "engine-cache half, no image"
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

# --- the KV-bytes proposal ----------------------------------------------------
#
# Model Runner V2 profiles memory with a full CUDA-graph capture; a fixed
# --kv-cache-memory-bytes skips it. The value comes from the log of a Ready
# replica and is PROPOSED -- in a comment and on the console -- never applied.
f_kv_deploy() {   # the engine-cache gap, with a selector and a TP=2 vLLM 0.32
    f_engine_cache_only_gap
    FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.selector = {"matchLabels":{"app":"w"}}
        | .spec.template.spec.containers[0].image = "quay.io/x/vllm-openai:v0.32.0"')"
    PODS_FIXTURE='{"items":[
      {"metadata":{"name":"w-0"},"status":{"phase":"Pending","conditions":[{"type":"Ready","status":"False"}]}},
      {"metadata":{"name":"w-2"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},
      {"metadata":{"name":"w-1","deletionTimestamp":"2026-10-27T00:00:00Z"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}'
}
# The lines vLLM 0.32 prints at start (vllm/v1/worker/gpu_worker.py), one per rank.
KV_LOG_V2='(APIServer pid=1) INFO 10-18 13:30:55 [utils.py:253] non-default args: {"model": "Qwen/Qwen3-32B", "tensor_parallel_size": 2}
(Worker_TP0 pid=210) INFO 10-18 13:31:02 [gpu_worker.py:518] Using V2 Model Runner
(Worker_TP1 pid=211) INFO 10-18 13:31:02 [gpu_worker.py:518] Using V2 Model Runner
(Worker_TP0 pid=210) INFO 10-18 13:31:20 [gpu_worker.py:760] Available KV cache memory: 60.24 GiB
(Worker_TP1 pid=211) INFO 10-18 13:31:20 [gpu_worker.py:760] Available KV cache memory: 60.3 GiB
(EngineCore pid=150) INFO 10-18 13:31:21 [kv_cache_utils.py:1311] GPU KV cache size: 493,472 tokens'
KV_LOG_V1='(EngineCore_DP0 pid=150) INFO 10-18 11:52:30 [gpu_worker.py:561] Available KV cache memory: 63.08 GiB
(EngineCore_DP0 pid=150) INFO 10-18 11:52:30 [kv_cache_utils.py:1287] GPU KV cache size: 258,352 tokens'

CASE="kv bytes: a Ready V2 replica gets the flag PROPOSED in a comment and on the console, never as an arg"
reset; f_kv_deploy
before=$FAILED
LOGS_FIXTURE="$KV_LOG_V2"
G="$(mktemp -d)"
# 60.24 GiB, less the 0.005 rounding, floored to the MiB: 61680 MiB (TP1 logged
# 60.3; the smaller rank decides).
KUBECTL_TRACE="$G/trace"
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >"$G/doc" 2>"$G/log"
assert_contains "$(cat "$G/doc")" "#   kv-cache-memory: PROPOSED, NOT APPLIED. w-2 runs Model Runner V2"
assert_contains "$(cat "$G/doc")" "--kv-cache-memory-bytes=64676167680"
assert_contains "$(cat "$G/doc")" "Available KV cache memory:"
assert_contains "$(cat "$G/doc")" "Derive it again after ANY change of vLLM version, model, GPU"
assert_contains "$(cat "$G/log")" "PROPOSED, not applied: --kv-cache-memory-bytes=64676167680"
assert_contains "$(cat "$G/log")" "~8 s of every start"
assert_contains "$(cat "$G/trace")" "get pods -n ns -l app=w -o json"
assert_contains "$(cat "$G/trace")" "logs w-2 -n ns -c vllm"
# Only ever a comment: no line of the document outside a comment names it.
assert_eq "$(grep -v '^[[:space:]]*#' "$G/doc" | grep -c 'kv-cache-memory')" "0" "uncommented mentions"
assert_contains "$(grep -v '^[[:space:]]*#' "$G/doc")" "- name: VLLM_CACHE_ROOT"   # anchor: the half itself
assert_eq "$(yq eval '.spec.template.spec.containers[0] | has("args")' "$G/doc" 2>/dev/null)" "false" "args in the document"
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

CASE="kv bytes: V1, no Ready pod, no log line, or the flag already set -- said, and nothing proposed"
reset; f_kv_deploy
before=$FAILED
G="$(mktemp -d)"
LOGS_FIXTURE="$KV_LOG_V1"
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >"$G/doc" 2>"$G/log"
assert_contains "$(cat "$G/log")" "ns/w: w-2 runs the V1 model runner"
assert_contains "$(cat "$G/doc")" "- name: VLLM_CACHE_ROOT"           # anchor: a document was written
assert_not_contains "$(cat "$G/doc")" "kv-cache-memory"
LOGS_FIXTURE='(APIServer pid=1) INFO 10-18 13:30:55 [api_server.py:1] Starting'
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >"$G/doc" 2>"$G/log"
assert_contains "$(cat "$G/log")" "ns/w: w-2 logged no \"Available KV cache memory\" line"
assert_not_contains "$(cat "$G/doc")" "kv-cache-memory"
LOGS_FIXTURE="$KV_LOG_V2"
PODS_FIXTURE="$(printf '%s' "$PODS_FIXTURE" | jq -c '.items |= map(select(.metadata.name != "w-2"))')"
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >"$G/doc" 2>"$G/log"
assert_contains "$(cat "$G/log")" "ns/w: no Ready pod to read its KV cache size from"
assert_not_contains "$(cat "$G/doc")" "kv-cache-memory"
# ...and a workload with nothing to fix is not told about it at all.
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"VLLM_CACHE_ROOT","value":"/model-cache/vllm"},{"name":"PYTHONPYCACHEPREFIX","value":"/model-cache/pyc"}]')"
assert_field 7 "volume:/model-cache/vllm"                           # anchor: no gap left
assert_eq "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1)" "" "output for a workload with nothing to fix"
reset; f_kv_deploy
LOGS_FIXTURE="$KV_LOG_V2"
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].args += ["--kv-cache-memory-bytes","1000"]')"
KUBECTL_TRACE="$G/trace2"
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >"$G/doc" 2>"$G/log"
assert_contains "$(cat "$G/doc")" "- name: VLLM_CACHE_ROOT"
assert_not_contains "$(cat "$G/doc")" "PROPOSED"
assert_not_contains "$(cat "$G/log")" "PROPOSED"
assert_eq "$(cat "$G/trace2" 2>/dev/null)" "" "kubectl reads for a workload that sets the flag"
# SGLang is not asked at all.
reset; f_sglang_no_pyc
LOGS_FIXTURE="$KV_LOG_V2"
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >"$G/doc" 2>"$G/log"
assert_contains "$(cat "$G/doc")" "#   bytecode:"
assert_not_contains "$(cat "$G/log")" "KV cache"
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

CASE="kv bytes: a failed read is said as a failure, with its reason, not as no pod or a rotated log"
reset; f_kv_deploy
before=$FAILED
G="$(mktemp -d)"
LOGS_FIXTURE="$KV_LOG_V2"
LOGS_RC=1 LOGS_ERR='Error from server (Forbidden): pods "w-2" is forbidden: User "u" cannot get resource "pods/log"'
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >"$G/doc" 2>"$G/log"
assert_contains "$(cat "$G/log")" "ns/w: could not read the logs of w-2 (Error from server (Forbidden): pods"
assert_not_contains "$(cat "$G/log")" "rotated"
assert_contains "$(cat "$G/doc")" "- name: VLLM_CACHE_ROOT"           # anchor: the document is still written
LOGS_RC=0 PODS_RC=1 PODS_ERR='Error from server (Forbidden): pods is forbidden'
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >"$G/doc" 2>"$G/log"
assert_contains "$(cat "$G/log")" "ns/w: could not list its pods (Error from server (Forbidden): pods is forbidden)"
assert_not_contains "$(cat "$G/log")" "no Ready pod"
PODS_RC=0
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.selector = {"matchExpressions":[{"key":"app","operator":"In","values":["w"]}]}')"
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >"$G/doc" 2>"$G/log"
assert_contains "$(cat "$G/log")" "ns/w: its selector has no matchLabels"
assert_not_contains "$(cat "$G/log")" "no Ready pod"
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

CASE="kv bytes: the newest Ready pod of the current ReplicaSet is read, not the first by name"
reset; f_kv_deploy
before=$FAILED
G="$(mktemp -d)"
LOGS_FIXTURE="$KV_LOG_V2"
RS_FIXTURE='{"items":[
  {"metadata":{"name":"w-old","labels":{"pod-template-hash":"old"},"annotations":{"deployment.kubernetes.io/revision":"9"},
   "ownerReferences":[{"kind":"Deployment","name":"w"}]}},
  {"metadata":{"name":"w-new","labels":{"pod-template-hash":"new"},"annotations":{"deployment.kubernetes.io/revision":"10"},
   "ownerReferences":[{"kind":"Deployment","name":"w"}]}},
  {"metadata":{"name":"x-rs","labels":{"pod-template-hash":"x"},"annotations":{"deployment.kubernetes.io/revision":"99"},
   "ownerReferences":[{"kind":"Deployment","name":"x"}]}}]}'
READY='"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}'
PODS_FIXTURE="{\"items\":[
  {\"metadata\":{\"name\":\"w-a\",\"creationTimestamp\":\"2026-10-27T09:00:00Z\",\"labels\":{\"pod-template-hash\":\"old\"}},$READY},
  {\"metadata\":{\"name\":\"w-c\",\"creationTimestamp\":\"2026-10-27T08:00:00Z\",\"labels\":{\"pod-template-hash\":\"new\"}},$READY},
  {\"metadata\":{\"name\":\"w-d\",\"creationTimestamp\":\"2026-10-27T08:30:00Z\",\"labels\":{\"pod-template-hash\":\"new\"}},$READY},
  {\"metadata\":{\"name\":\"w-x\",\"creationTimestamp\":\"2026-10-27T10:00:00Z\",\"labels\":{\"pod-template-hash\":\"x\"}},$READY}]}"
KUBECTL_TRACE="$G/trace"
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >"$G/doc" 2>"$G/log"
assert_contains "$(cat "$G/trace")" "logs w-d -n ns -c vllm"
assert_contains "$(cat "$G/doc")" "PROPOSED, NOT APPLIED. w-d runs Model Runner V2"
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

CASE="kv bytes: an engine restarted inside the log is read at its last start, and .5 parses"
reset; f_kv_deploy
before=$FAILED
G="$(mktemp -d)"
LOGS_FIXTURE='(EngineCore pid=150) INFO 10-18 13:31:02 [gpu_worker.py:518] Using V2 Model Runner
(EngineCore pid=150) INFO 10-18 13:31:20 [gpu_worker.py:760] Available KV cache memory: 50.0 GiB
(EngineCore pid=480) INFO 10-18 14:02:11 [gpu_worker.py:518] Using V2 Model Runner
(EngineCore pid=480) INFO 10-18 14:02:30 [gpu_worker.py:760] Available KV cache memory: 60.24 GiB'
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >"$G/doc" 2>"$G/log"
assert_contains "$(cat "$G/doc")" "--kv-cache-memory-bytes=64676167680"
assert_not_contains "$(cat "$G/doc")" "50.0 GiB"
# (0.5 - 0.005) GiB floored to the MiB: 506 MiB.
LOGS_FIXTURE='(EngineCore pid=150) INFO 10-18 13:31:02 [gpu_worker.py:518] Using V2 Model Runner
(EngineCore pid=150) INFO 10-18 13:31:20 [gpu_worker.py:760] Available KV cache memory: .5 GiB'
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >"$G/doc" 2>"$G/log"
assert_contains "$(cat "$G/doc")" "--kv-cache-memory-bytes=530579456"
rm -rf "$G"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: an env-only half carries the guard too, and the re-prepare note"
reset; f_engine_cache_on_claim_no_pyc
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "PYTHONPYCACHEPREFIX; do"
assert_contains "$doc" "run \`make engine-cache\` again first"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: PYTHONDONTWRITEBYTECODE in the pod spec means no prefix is written"
reset; f_engine_cache_only_gap
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"PYTHONDONTWRITEBYTECODE","value":"1"}]')"
assert_field 9 "dontwrite"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "- name: VLLM_CACHE_ROOT"           # anchor: the vLLM half is still written
assert_not_contains "$doc" "- name: PYTHONPYCACHEPREFIX"
assert_not_contains "$doc" "#   bytecode:"
# ...and SGLang with it gets no half at all.
reset; f_sglang_no_pyc
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"PYTHONDONTWRITEBYTECODE","valueFrom":{"configMapKeyRef":{"name":"c","key":"k"}}}]')"
assert_field 9 "dontwrite"
assert_eq "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1)" "" "SGLang output"
# An empty value is unset, as Python reads it (anchor).
reset; f_sglang_no_pyc
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"PYTHONDONTWRITEBYTECODE","value":""}]')"
assert_field 9 "unset"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: VLLM_CACHE_ROOT from a ConfigMap is reported, with no volume proposed"
reset; f_engine_cache_only_gap
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"VLLM_CACHE_ROOT","valueFrom":{"configMapKeyRef":{"name":"c","key":"k"}}}]')"
assert_field 7 ""
assert_field 9 "rootfrom"
out="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1)"
assert_contains "$out" "RECOMPILES ITS PYTHON BYTECODE"
assert_contains "$out" "PYTHONPYCACHEPREFIX=<your cache volume>/pycache/${KEY_VLLM}"
assert_not_contains "$out" "claimName: engine-cache"
assert_not_contains "$out" "- name: PYTHONPYCACHEPREFIX"
assert_not_contains "$out" "apiVersion:"            # nothing else is wanted: no document at all
assert_contains "$(so_enginecache_note ns deployments w "$POD_DEPLOY")" "no volume is proposed"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: a tag and a digest of one image get two keys through the patch"
reset; f_engine_cache_only_gap
before=$FAILED
doc1="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
DIGEST="quay.io/x/vllm-openai:v0.11@sha256:1111111111111111111111111111111111111111111111111111111111111111"
FIXTURE="${FIXTURE//quay.io\/x\/vllm-openai:v0.11/$DIGEST}"
assert_field 8 "$DIGEST"
doc2="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc1" "/engine-cache/pycache/${KEY_VLLM}"
assert_contains "$doc2" "/engine-cache/pycache/$(so_pycache_key "$DIGEST")"
assert_not_contains "$doc2" "/engine-cache/pycache/${KEY_VLLM}"
assert_contains "$doc2" "image reference ${DIGEST}"
[ "$FAILED" -eq "$before" ] && ok

CASE="bytecode: SGLang with a prefix keyed for another image is warned about too"
reset; f_sglang_no_pyc
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].env += [{"name":"PYTHONPYCACHEPREFIX","value":"/engine-cache/pycache/bbbbbbbbbbbb"}]')"
warn="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1 >/dev/null)"
assert_contains "$warn" "keyed for another image"
assert_contains "$warn" ".../pycache/${KEY_SGLANG}"
assert_eq "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" "" "document"
[ "$FAILED" -eq "$before" ] && ok

# --- LeaderWorkerSets: the cache halves, per template ----------------------
#
# A LeaderWorkerSet keeps two pod templates, and each is judged as a
# Deployment's would be: its own engine container, its own image and so its own
# bytecode key. There is no drain half -- a worker that stops first aborts the
# leader's generations whatever hook either pod has -- and that is said, not
# papered over with a preStop on each pod. Applied, the document goes as a JSON
# patch: the API server refuses a strategic merge on a custom resource.

LWS_LEADER_IMAGE="quay.io/x/vllm-openai:v0.11"
LWS_WORKER_IMAGE="quay.io/x/vllm-openai:v0.11-ray"
KEY_LEADER="$(so_pycache_key "$LWS_LEADER_IMAGE")"
KEY_WORKER="$(so_pycache_key "$LWS_WORKER_IMAGE")"

# Leader serves the API; the worker joins it over Ray and names no model. No
# env, volumeMounts or volumes anywhere: every list is absent.
f_lws_two() {
    FIXTURE='{"metadata":{"name":"w","resourceVersion":"7"},"spec":{"leaderWorkerTemplate":{
        "leaderTemplate":{"spec":{"containers":[{"name":"vllm-leader","image":"'"$LWS_LEADER_IMAGE"'",
            "command":["/bin/sh","-c"],"args":["vllm serve Qwen/Qwen3-0.6B --tensor-parallel-size 2"]}]}},
        "workerTemplate":{"spec":{"containers":[{"name":"vllm-worker","image":"'"$LWS_WORKER_IMAGE"'",
            "command":["/bin/sh","-c"],"args":["ray start --block --address=$(LWS_LEADER_ADDRESS):6379"]}]}}}}}'
}

# No leaderTemplate: the LWS controller builds the leader from workerTemplate.
f_lws_worker_only() {
    FIXTURE='{"metadata":{"name":"w","resourceVersion":"7"},"spec":{"leaderWorkerTemplate":{
        "workerTemplate":{"spec":{"containers":[{"name":"vllm","image":"'"$LWS_LEADER_IMAGE"'",
            "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}}'
}

# Every list present: a sidecar ahead of the engine, env, mounts and a volume
# in each template, and the leader keeps VLLM_CACHE_ROOT under /tmp.
f_lws_lists_present() {
    FIXTURE='{"metadata":{"name":"w","resourceVersion":"7"},"spec":{"leaderWorkerTemplate":{
        "leaderTemplate":{"spec":{
            "volumes":[{"name":"dshm","emptyDir":{"medium":"Memory"}}],
            "containers":[{"name":"proxy","image":"quay.io/x/proxy:1"},
                          {"name":"vllm-leader","image":"'"$LWS_LEADER_IMAGE"'",
                           "env":[{"name":"HF_HOME","value":"/tmp/hf"},{"name":"VLLM_CACHE_ROOT","value":"/tmp/vllm"}],
                           "volumeMounts":[{"name":"dshm","mountPath":"/dev/shm"}],
                           "args":["vllm","serve","/models/qwen"]}]}},
        "workerTemplate":{"spec":{
            "volumes":[{"name":"dshm","emptyDir":{"medium":"Memory"}}],
            "containers":[{"name":"vllm-worker","image":"'"$LWS_WORKER_IMAGE"'",
                           "env":[{"name":"NCCL_DEBUG","value":"WARN"}],
                           "volumeMounts":[{"name":"dshm","mountPath":"/dev/shm"}],
                           "command":["/bin/sh","-c"],"args":["ray start --block"]}]}}}}}'
}

# Nothing missing in either template.
f_lws_converged() {
    FIXTURE='{"metadata":{"name":"w","resourceVersion":"7"},"spec":{"leaderWorkerTemplate":{
        "leaderTemplate":{"spec":{
            "volumes":[{"name":"engine-cache","persistentVolumeClaim":{"claimName":"engine-cache"}}],
            "containers":[{"name":"vllm","image":"'"$LWS_LEADER_IMAGE"'",
                           "env":[{"name":"VLLM_CACHE_ROOT","value":"/engine-cache/vllm"},
                                  {"name":"PYTHONPYCACHEPREFIX","value":"/engine-cache/pycache/'"$KEY_LEADER"'"}],
                           "volumeMounts":[{"name":"engine-cache","mountPath":"/engine-cache"}],
                           "args":["vllm","serve","/models/qwen"]}]}},
        "workerTemplate":{"spec":{
            "volumes":[{"name":"engine-cache","persistentVolumeClaim":{"claimName":"engine-cache"}}],
            "containers":[{"name":"vllm","image":"'"$LWS_LEADER_IMAGE"'",
                           "env":[{"name":"VLLM_CACHE_ROOT","value":"/engine-cache/vllm"},
                                  {"name":"PYTHONPYCACHEPREFIX","value":"/engine-cache/pycache/'"$KEY_LEADER"'"}],
                           "volumeMounts":[{"name":"engine-cache","mountPath":"/engine-cache"}],
                           "args":["vllm","serve","/models/qwen","--headless"]}]}}}}}'
}

# lws_get <doc> <yq expression>: one value from an emitted document.
lws_get() { printf '%s\n' "$1" | yq eval "$2" - 2>/dev/null; }

# json_patch_apply <live json> <ops json>: applies add, replace and test the way
# RFC 6902 does, so a case can check the object the API server would hold
# rather than the text of the operations. Exits non-zero on a failed test op or
# an add into a parent that does not exist.
json_patch_apply() {
    jq -c --argjson ops "$2" '
        def toks: ltrimstr("/") | split("/") | map(gsub("~1"; "/") | gsub("~0"; "~"));
        def resolve($doc):
            reduce .[] as $k ([];
                . as $acc
                | ($doc | getpath($acc)) as $here
                | . + [ if ($here | type) == "array" and $k != "-" then ($k | tonumber) else $k end ]);
        reduce $ops[] as $o (.;
            . as $doc
            | ($o.path | toks | resolve($doc)) as $p
            | ($p[:-1]) as $parent
            | if ($doc | getpath($parent)) == null then error("no parent for \($o.path)")
              elif $o.op == "test" then (if getpath($p) == $o.value then . else error("test failed at \($o.path)") end)
              elif $o.op == "replace" then (if getpath($p) == null then error("nothing to replace at \($o.path)") else setpath($p; $o.value) end)
              elif $o.op == "add" and $p[-1] == "-" then setpath($parent; getpath($parent) + [$o.value])
              elif $o.op == "add" then setpath($p; $o.value)
              else error("unexpected op \($o.op)") end)' <<< "$1"
}

lws_driver() {   # $1 APPLY, $2 APPLY_ENGINE_CACHE, $3 APPLY_WEIGHTS (each true or false); results in DRIVER_*
    local dir; dir="$(mktemp -d)"
    DRIVER_OUT="$dir/p.yaml"; DRIVER_LOG="$dir/log"; DRIVER_RC=0
    LLMSM_DEFAULT_SO_NS=ns1 LLMSM_WORKLOAD_PATCH_FILE="$DRIVER_OUT"         LLMSM_WORKLOAD_PATCH_APPLY="${1:-false}" LLMSM_WORKLOAD_PATCH_APPLY_ENGINE_CACHE="${2:-false}"         LLMSM_WORKLOAD_PATCH_APPLY_WEIGHTS="${3:-false}"         llmsm_workload_patch >"$DRIVER_LOG" 2>&1 || DRIVER_RC=$?
}

lws_scene() {   # an LWS-only namespace, CRD present, the claim and preparer ready
    LIST_FIXTURE='{"items":[]}'; LIST_FIXTURE_LWS="$(serving_list)"; CRD_RC=0
    PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany model-pvc:ReadWriteMany)"
    DS_FIXTURE='{"status":{"desiredNumberScheduled":2,"numberReady":2}}'
}

CASE="LWS: leader and worker each get the engine-cache half at their own template path"
reset; f_lws_two
before=$FAILED
doc_rc=0
doc="$(so_workload_patch_doc ns leaderworkersets w "$POD_LWS" 2>"$WORK/lws.err")" || doc_rc=$?
assert_eq "$doc_rc" "0" "return code"
for t in leaderTemplate workerTemplate; do
    assert_eq "$(lws_get "$doc" ".spec.leaderWorkerTemplate.$t.spec.volumes[] | select(.name == \"engine-cache\") | .persistentVolumeClaim.claimName")" "engine-cache" "$t engine-cache claim"
    assert_eq "$(lws_get "$doc" ".spec.leaderWorkerTemplate.$t.spec.containers[0].volumeMounts[] | select(.name == \"engine-cache\") | .mountPath")" "/engine-cache" "$t mount"
    assert_eq "$(lws_get "$doc" ".spec.leaderWorkerTemplate.$t.spec.containers[0].env[] | select(.name == \"VLLM_CACHE_ROOT\") | .value")" "/engine-cache/vllm" "$t VLLM_CACHE_ROOT"
    assert_eq "$(lws_get "$doc" ".spec.leaderWorkerTemplate.$t.spec.containers[0].env[] | select(.name == \"TRITON_CACHE_DIR\") | .value")" "/engine-cache/triton" "$t TRITON_CACHE_DIR"
done
# Each template names ITS engine container, not the other one.
assert_eq "$(lws_get "$doc" '.spec.leaderWorkerTemplate.leaderTemplate.spec.containers[0].name')" "vllm-leader" "leader container"
assert_eq "$(lws_get "$doc" '.spec.leaderWorkerTemplate.workerTemplate.spec.containers[0].name')" "vllm-worker" "worker container"
assert_eq "$(lws_get "$doc" '.apiVersion + " " + .kind')" "leaderworkerset.x-k8s.io/v1 LeaderWorkerSet" "kind"
assert_eq "$(lws_get "$doc" '.spec.template // "none"')" "none" "a Deployment-shaped field"
assert_contains "$doc" "PYTHONPYCACHEPREFIX; do"                  # the guard
assert_contains "$doc" "leader command AND the worker"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS: a leader and a worker on different images get two bytecode keys"
reset; f_lws_two
before=$FAILED
doc="$(so_workload_patch_doc ns leaderworkersets w "$POD_LWS" 2>/dev/null)"
[ "$KEY_LEADER" != "$KEY_WORKER" ] || fail "the two fixture images hash alike"
assert_eq "$(lws_get "$doc" '.spec.leaderWorkerTemplate.leaderTemplate.spec.containers[0].env[] | select(.name == "PYTHONPYCACHEPREFIX") | .value')" "/engine-cache/pycache/$KEY_LEADER" "leader key"
assert_eq "$(lws_get "$doc" '.spec.leaderWorkerTemplate.workerTemplate.spec.containers[0].env[] | select(.name == "PYTHONPYCACHEPREFIX") | .value')" "/engine-cache/pycache/$KEY_WORKER" "worker key"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS: a Ray worker that names no model takes the leader's for the weights half"
reset; f_lws_two
before=$FAILED
doc="$(so_workload_patch_doc ns leaderworkersets w "$POD_LWS" 2>"$WORK/lws.err")"
assert_eq "$(lws_get "$doc" '.spec.leaderWorkerTemplate.workerTemplate.spec.containers[0].env[] | select(.name == "HF_HOME") | .value')" "/model-cache/huggingface" "worker HF_HOME"
assert_eq "$(lws_get "$doc" '.spec.leaderWorkerTemplate.leaderTemplate.spec.volumes[] | select(.name == "model-storage") | .persistentVolumeClaim.claimName')" "model-pvc" "leader weights claim"
assert_contains "$(cat "$WORK/lws.err")" "ns/w (workerTemplate) RE-DOWNLOADS ITS WEIGHTS"
# One reason line per DOCUMENT: the summary counts them as model servers.
assert_eq "$(printf '%s\n' "$doc" | grep -c '^#   cache:')" "1" "cache reason lines"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS: without a leaderTemplate there is one template, workerTemplate"
reset; f_lws_worker_only
before=$FAILED
doc_rc=0
doc="$(so_workload_patch_doc ns leaderworkersets w "$POD_LWS" 2>/dev/null)" || doc_rc=$?
assert_eq "$doc_rc" "0" "return code"
assert_eq "$(lws_get "$doc" '.spec.leaderWorkerTemplate | keys | join(",")')" "workerTemplate" "templates"
assert_eq "$(lws_get "$doc" '.spec.leaderWorkerTemplate.workerTemplate.spec.containers[0].env[] | select(.name == "PYTHONPYCACHEPREFIX") | .value')" "/engine-cache/pycache/$KEY_LEADER" "key"
assert_contains "$doc" "workerTemplate (leader and workers), engine container vllm: weights, engine-cache, bytecode"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS: without a leaderTemplate the plan notes read workerTemplate"
reset; f_lws_worker_only
before=$FAILED
assert_contains "$(so_workload_gaps ns leaderworkersets w "$POD_LWS")" "vllm"
assert_contains "$(so_weights_note ns leaderworkersets w "$POD_LWS")" "Weights do not persist"
assert_contains "$(so_enginecache_note ns leaderworkersets w "$POD_LWS")" "Every start compiles from nothing"
assert_contains "$(so_drain_note ns leaderworkersets w "$POD_LWS")" "Scale-down will cut in-flight requests"
# A leaderTemplate that IS there is still the one read (anchor: its own container).
reset; f_lws_two
assert_contains "$(so_workload_gaps ns leaderworkersets w "$POD_LWS")" "vllm-leader"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS: a template with no engine container is skipped; the other still gets its halves"
reset; f_lws_two
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.leaderWorkerTemplate.workerTemplate.spec.containers[0] |= (.image = "quay.io/x/proxy:1" | .args = ["--listen", ":80"])')"
doc="$(so_workload_patch_doc ns leaderworkersets w "$POD_LWS" 2>"$WORK/lws.err")"
assert_contains "$(cat "$WORK/lws.err")" "No vLLM or SGLang container found in the workerTemplate"
assert_eq "$(lws_get "$doc" '.spec.leaderWorkerTemplate | keys | join(",")')" "leaderTemplate" "templates"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS: drain is still skipped -- no hook emitted, the reason printed once, and rc 0 with nothing else to do"
reset; f_lws_converged
before=$FAILED
doc_rc=0
doc="$(so_workload_patch_doc ns leaderworkersets w "$POD_LWS" 2>"$WORK/lws.err")" || doc_rc=$?
assert_eq "$doc_rc" "0" "return code"
assert_eq "$doc" "" "document"
assert_eq "$(grep -c 'no drain half' "$WORK/lws.err")" "1" "drain reason lines"
assert_contains "$(cat "$WORK/lws.err")" "a worker that stops first aborts the leader"
reset; f_lws_two
doc="$(so_workload_patch_doc ns leaderworkersets w "$POD_LWS" 2>/dev/null)"
assert_contains "$doc" "kind: LeaderWorkerSet"                    # anchor
assert_eq "$(lws_get "$doc" '[.. | select(has("lifecycle") or has("terminationGracePeriodSeconds"))] | length')" "0" "drain fields"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS apply: flags off write the file and patch nothing; APPLY alone defers it"
reset; lws_scene; f_lws_two
before=$FAILED
lws_driver
assert_eq "$DRIVER_RC" "0" "return code"
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$DRIVER_OUT" 2>/dev/null)" "kind: LeaderWorkerSet"
assert_contains "$(cat "$DRIVER_LOG")" "1 model server(s) COMPILE FROM NOTHING"
lws_driver true
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls with APPLY alone"
assert_contains "$(cat "$DRIVER_LOG")" "ns1/w needs only a volume, a cache variable or the weights gate -- emitted, not applied"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS apply: absent lists are created whole, by a JSON patch, at both templates"
reset; lws_scene; f_lws_two
before=$FAILED
lws_driver true true
assert_eq "$DRIVER_RC" "0" "return code"
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_ARGS" "patch leaderworkersets w -n ns1 --type=json"
assert_eq "$(jq -r '[.[] | select(.path | endswith("/-"))] | length' <<< "$PATCH_BODY")" "0" "appends into absent lists"
assert_eq "$(jq -r '[.[] | select(.op == "add" and (.path | endswith("/spec/volumes")))] | length' <<< "$PATCH_BODY")" "2" "volumes created"
after="$(json_patch_apply "$FIXTURE" "$PATCH_BODY")" || fail "the patch does not apply: $PATCH_BODY"
for t in leaderTemplate workerTemplate; do
    assert_eq "$(jq -r ".spec.leaderWorkerTemplate.$t.spec.containers[0].env | map(.name) | join(\",\")" <<< "$after")" "VLLM_CACHE_ROOT,FLASHINFER_WORKSPACE_BASE,TRITON_CACHE_DIR,PYTHONPYCACHEPREFIX" "$t env"
    assert_eq "$(jq -r ".spec.leaderWorkerTemplate.$t.spec.volumes[0].persistentVolumeClaim.claimName" <<< "$after")" "engine-cache" "$t volume"
done
# The weights half had no opt-in, so neither its volume nor HF_HOME went.
assert_not_contains "$PATCH_BODY" "HF_HOME"
assert_not_contains "$PATCH_BODY" "model-storage"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS apply: present lists are appended to, an env entry of the same name replaced, the sidecar untouched"
reset; lws_scene; f_lws_lists_present
before=$FAILED
lws_driver true true
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" '"path":"/spec/leaderWorkerTemplate/leaderTemplate/spec/containers/1/name"'   # the test op, by name
after="$(json_patch_apply "$FIXTURE" "$PATCH_BODY")" || fail "the patch does not apply: $PATCH_BODY"
assert_eq "$(jq -r '.spec.leaderWorkerTemplate.leaderTemplate.spec.containers[1].env | map(.name + "=" + .value) | join(",")' <<< "$after")" \
    "HF_HOME=/tmp/hf,VLLM_CACHE_ROOT=/engine-cache/vllm,FLASHINFER_WORKSPACE_BASE=/engine-cache/flashinfer/$KEY_LEADER,TRITON_CACHE_DIR=/engine-cache/triton,PYTHONPYCACHEPREFIX=/engine-cache/pycache/$KEY_LEADER" "leader env"
assert_eq "$(jq -r '.spec.leaderWorkerTemplate.leaderTemplate.spec.volumes | map(.name) | join(",")' <<< "$after")" "dshm,engine-cache" "leader volumes"
assert_eq "$(jq -r '.spec.leaderWorkerTemplate.leaderTemplate.spec.containers[1].volumeMounts | map(.name) | join(",")' <<< "$after")" "dshm,engine-cache" "leader mounts"
assert_eq "$(jq -c '.spec.leaderWorkerTemplate.leaderTemplate.spec.containers[0]' <<< "$after")" '{"name":"proxy","image":"quay.io/x/proxy:1"}' "sidecar"
assert_eq "$(jq -r '.spec.leaderWorkerTemplate.workerTemplate.spec.containers[0].env | map(.name) | join(",")' <<< "$after")" \
    "NCCL_DEBUG,VLLM_CACHE_ROOT,FLASHINFER_WORKSPACE_BASE,TRITON_CACHE_DIR,PYTHONPYCACHEPREFIX" "worker env"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS: a template with FLASHINFER_WORKSPACE_DIR gets the FlashInfer half, emitted and never applied"
reset; lws_scene; f_lws_converged
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.leaderWorkerTemplate.leaderTemplate.spec.containers[0].env += [{"name":"FLASHINFER_WORKSPACE_DIR","value":"/engine-cache/flashinfer"}]')"
doc="$(so_workload_patch_doc ns leaderworkersets w "$POD_LWS" 2>/dev/null)"
assert_contains "$doc" "#   flashinfer:"
assert_contains "$doc" "leaderTemplate, engine container vllm: flashinfer"
assert_eq "$(lws_get "$doc" '.spec.leaderWorkerTemplate.leaderTemplate.spec.containers[0].env | map(.name + "=" + .value) | join(",")')" \
    "FLASHINFER_WORKSPACE_BASE=/engine-cache/flashinfer/$KEY_LEADER" "leader env"
assert_eq "$(lws_get "$doc" '.spec.leaderWorkerTemplate.workerTemplate')" "null" "worker template (nothing to change)"
lws_driver true true
assert_eq "$DRIVER_RC" "0" "return code"
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$DRIVER_LOG")" "1 model server(s) BUILD THEIR FLASHINFER KERNELS AGAIN"
assert_contains "$(cat "$DRIVER_LOG")" "ns1/w needs only a volume, a cache variable or the weights gate -- emitted, not applied"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS apply: a volume name taken in either template refuses that half for the whole LWS"
reset; lws_scene; f_lws_two
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.leaderWorkerTemplate.workerTemplate.spec.volumes = [{"name":"engine-cache","emptyDir":{}}]')"
lws_driver true true true
assert_contains "$(cat "$DRIVER_LOG")" "the engine-cache volume is emitted, NOT applied -- a volume named engine-cache already exists"
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls (the weights half still goes)"
assert_contains "$PATCH_BODY" "model-storage"
assert_not_contains "$PATCH_BODY" "VLLM_CACHE_ROOT"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS apply: a rejected JSON patch is a failure, and the run is non-zero"
reset; lws_scene; f_lws_two; PATCH_RC=1
before=$FAILED
lws_driver true true
assert_eq "$DRIVER_RC" "1" "return code"
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$(cat "$DRIVER_LOG")" "1 of 1 workload(s) were NOT patched"
[ "$FAILED" -eq "$before" ] && ok


# Converged but for FlashInfer: both templates patched before v0.12.1, so each
# sets FLASHINFER_WORKSPACE_DIR. The worker runs another image.
f_lws_old_flashinfer() {
    f_lws_converged
    FIXTURE="$(printf '%s' "$FIXTURE" | jq -c --arg wi "$LWS_WORKER_IMAGE" '
        .spec.leaderWorkerTemplate.workerTemplate.spec.containers[0].image = $wi
        | .spec.leaderWorkerTemplate.workerTemplate.spec.containers[0].env[1].value = "/engine-cache/pycache/'"$KEY_WORKER"'"
        | (.spec.leaderWorkerTemplate.leaderTemplate, .spec.leaderWorkerTemplate.workerTemplate).spec.containers[0].env
            += [{"name":"FLASHINFER_WORKSPACE_DIR","value":"/engine-cache/flashinfer"}]')"
}

CASE="LWS: the FlashInfer half reaches the leader and the workers, each keyed by its image, with the guard for both"
reset; f_lws_old_flashinfer
before=$FAILED
doc_rc=0
doc="$(so_workload_patch_doc ns leaderworkersets w "$POD_LWS" 2>"$WORK/lws.err")" || doc_rc=$?
assert_eq "$doc_rc" "0" "return code"
assert_contains "$doc" "#   flashinfer:"                                   # anchor: the half fired
assert_eq "$(lws_get "$doc" '.spec.leaderWorkerTemplate.leaderTemplate.spec.containers[0].env[] | select(.name == "FLASHINFER_WORKSPACE_BASE") | .value')" "/engine-cache/flashinfer/$KEY_LEADER" "leader base"
assert_eq "$(lws_get "$doc" '.spec.leaderWorkerTemplate.workerTemplate.spec.containers[0].env[] | select(.name == "FLASHINFER_WORKSPACE_BASE") | .value')" "/engine-cache/flashinfer/$KEY_WORKER" "worker base"
assert_eq "$(lws_get "$doc" '[.spec.leaderWorkerTemplate[].spec.volumes // [] | .[]] | length')" "0" "volumes (env only)"
assert_contains "$doc" "FLASHINFER_WORKSPACE_BASE TRITON_CACHE_DIR PYTHONPYCACHEPREFIX; do"
assert_contains "$doc" "leader command AND the worker"
assert_contains "$(cat "$WORK/lws.err")" "ns/w (leaderTemplate) BUILDS ITS FLASHINFER KERNELS AGAIN"
assert_contains "$(cat "$WORK/lws.err")" "ns/w (workerTemplate) BUILDS ITS FLASHINFER KERNELS AGAIN"
# Never applied: env only, like the Deployment's.
reset; lws_scene; f_lws_old_flashinfer
lws_driver true true true
assert_eq "$DRIVER_RC" "0" "driver return code"
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$DRIVER_LOG")" "BUILD THEIR FLASHINFER KERNELS AGAIN"
[ "$FAILED" -eq "$before" ] && ok

# --- LeaderWorkerSets: review round -----------------------------------------

# f_lws_hooked: converged caches, and a preStop on leader and worker; $1 the
# leader grace, $2 the worker grace.
f_lws_hooked() {
    f_lws_converged
    FIXTURE="$(printf '%s' "$FIXTURE" | jq -c --argjson lg "$1" --argjson wg "$2" '
        .spec.leaderWorkerTemplate.leaderTemplate.spec.terminationGracePeriodSeconds = $lg
        | .spec.leaderWorkerTemplate.workerTemplate.spec.terminationGracePeriodSeconds = $wg
        | .spec.leaderWorkerTemplate[].spec.containers[0].lifecycle = {"preStop":{"exec":{"command":["sleep","45"]}}}')"
}

CASE="LWS: the drain line is quiet once both templates hook and the worker outlasts the leader"
reset; f_lws_hooked 120 120
before=$FAILED
doc_rc=0
doc="$(so_workload_patch_doc ns leaderworkersets w "$POD_LWS" 2>"$WORK/lws.err")" || doc_rc=$?
assert_eq "$doc_rc" "0" "return code"
assert_eq "$(grep -c 'no drain half' "$WORK/lws.err")" "0" "drain reason lines"
# A worker that leaves before the leader still cuts the group: the line, with the action.
reset; f_lws_hooked 600 120
so_workload_patch_doc ns leaderworkersets w "$POD_LWS" >/dev/null 2>"$WORK/lws.err"
assert_contains "$(cat "$WORK/lws.err")" "Add a preStop to the leader AND the worker, with the worker terminationGracePeriodSeconds at least the leader one"
[ "$FAILED" -eq "$before" ] && ok

# --- drain grace: preStop + --shutdown-timeout ------------------------------
#
# vLLM --shutdown-timeout N waits up to N s for in-flight requests after
# SIGTERM, and SIGTERM comes only once the preStop hook ends, so the grace
# period has to cover sleep + N + 15 s. A grace sized without N SIGKILLs the
# end of the drain.

# f_drain <grace> <args JSON> [<preStop JSON>]: one vLLM container.
f_drain() {
    FIXTURE="$(jq -cn --argjson g "$1" --argjson a "$2" --argjson h "${3:-null}" '
        {spec:{template:{spec:{terminationGracePeriodSeconds:$g,
          containers:[{name:"vllm",image:"quay.io/x/vllm-openai:v0.11",args:$a}
                      + (if $h == null then {} else {lifecycle:{preStop:$h}} end)]}}}}')"
}
drain_times() { so_drain_times ns deployments w "$POD_DEPLOY" vllm | tr '\037' '|'; }
drain_grace() { so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null | yq eval '.spec.template.spec.terminationGracePeriodSeconds' - 2>/dev/null; }
TO90='["vllm","serve","/models/qwen","--shutdown-timeout","90"]'

CASE="drain grace: no --shutdown-timeout keeps the configured grace, floored at preStop + 15 s"
reset; f_drain 30 '["vllm","serve","/models/qwen"]'
before=$FAILED
assert_eq "$(drain_times)" "0|0|0|" "times"
assert_eq "$(drain_grace)" "120" "grace"
assert_not_contains "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" "Its --shutdown-timeout"
# A configured grace below the preStop sleep itself is raised to cover it.
assert_eq "$(LLMSM_DRAIN_GRACE_SECONDS=40 drain_grace)" "60" "grace under a 40 s setting"
[ "$FAILED" -eq "$before" ] && ok

CASE="drain grace: a --shutdown-timeout longer than the grace raises it to preStop + timeout + 15 s"
reset; f_drain 30 "$TO90"
before=$FAILED
assert_eq "$(drain_times)" "0|0|90|" "times"
assert_eq "$(drain_grace)" "150" "grace"
assert_contains "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" "45 s of preStop"
assert_eq "$(LLMSM_DRAIN_SLEEP_SECONDS=60 drain_grace)" "165" "grace with a 60 s preStop"
# The =N form inside a shell command.
f_drain 30 '["/bin/sh","-c","exec vllm serve /models/qwen --shutdown-timeout=200 --port 8000"]'
assert_eq "$(drain_grace)" "260" "grace, =N in sh -c"
# The underscore spelling, and the last one wins, as argparse reads it.
f_drain 30 '["vllm","serve","/models/qwen","--shutdown_timeout","10","--shutdown-timeout","120"]'
assert_eq "$(drain_grace)" "180" "grace, last flag"
# A longer grace than the drain needs is kept.
f_drain 300 "$TO90"
assert_eq "$(drain_grace)" "300" "grace, longer kept"
# The plan note for an unhooked engine says what the grace must be.
f_drain 30 "$TO90"
assert_contains "$(so_drain_note ns deployments w "$POD_DEPLOY")" "the grace period needs at least 150 s"
[ "$FAILED" -eq "$before" ] && ok

CASE="drain grace: a preStop sleep plus the timeout over the grace is reported, never patched"
reset; f_drain 90 "$TO90" '{"exec":{"command":["/bin/sh","-c","sleep 30"]}}'
before=$FAILED
assert_eq "$(drain_times)" "1|30|90|" "times"
note="$(so_drain_note ns deployments w "$POD_DEPLOY")"
assert_contains "$note" "30 s of preStop + 90 s of --shutdown-timeout + 15 s = 135 s, and terminationGracePeriodSeconds is 90"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>"$WORK/drain.err")"
assert_contains "$(cat "$WORK/drain.err")" "ns/w: Scale-down can still cut in-flight requests"
assert_not_contains "$doc" "terminationGracePeriodSeconds:"
assert_not_contains "$doc" "preStop"
# At the boundary it is quiet; one second under, it is not.
f_drain 135 "$TO90" '{"exec":{"command":["/bin/sh","-c","sleep 30"]}}'
assert_eq "$(so_drain_note ns deployments w "$POD_DEPLOY")" "" "note at 135"
f_drain 134 "$TO90" '{"exec":{"command":["/bin/sh","-c","sleep 30"]}}'
assert_contains "$(so_drain_note ns deployments w "$POD_DEPLOY")" "= 135 s"
[ "$FAILED" -eq "$before" ] && ok

CASE="drain grace: every hook shape is read, and what cannot be read counts as 0 s and says so"
reset
before=$FAILED
f_drain 30 '["vllm","serve","/models/qwen"]' '{"exec":{"command":["sleep","45"]}}'
assert_eq "$(drain_times)" "1|45|0|" "argv sleep"
assert_contains "$(so_drain_note ns deployments w "$POD_DEPLOY")" "= 60 s, and terminationGracePeriodSeconds is 30"
f_drain 300 "$TO90" '{"sleep":{"seconds":20}}'
assert_eq "$(drain_times)" "1|20|90|" "sleep action"
f_drain 300 "$TO90" '{"exec":{"command":["/bin/sh","-c","sleep 2m"]}}'
assert_eq "$(drain_times)" "1|120|90|" "minutes"
f_drain 300 "$TO90" '{"exec":{"command":["/bin/sh","-c","sleep 5; curl -s localhost:8000/drain; /bin/sleep 10s"]}}'
assert_eq "$(drain_times)" "1|15|90|" "two sleeps"
f_drain 300 "$TO90" '{"exec":{"command":["/bin/sh","-c","sleep infinity; nosleep 9"]}}'
assert_eq "$(drain_times)" "1|0|90|sleep" "no readable sleep"
f_drain 60 "$TO90" '{"httpGet":{"path":"/drain","port":8000}}'
note="$(so_drain_note ns deployments w "$POD_DEPLOY")"
assert_contains "$note" "0 s of preStop + 90 s of --shutdown-timeout + 15 s = 105 s"
assert_contains "$note" "could not be read and counts as 0 s"
f_drain 30 '["/bin/sh","-c","vllm serve /models/qwen --shutdown-timeout $(T)"]' '{"exec":{"command":["sleep","45"]}}'
assert_eq "$(drain_times)" "1|45|0|timeout" "a variable timeout"
assert_contains "$(so_drain_note ns deployments w "$POD_DEPLOY")" "is not a number this can read"
[ "$FAILED" -eq "$before" ] && ok

CASE="drain grace: a hooked Deployment too short for its drain is not in the all-clear"
reset; LIST_FIXTURE="$(serving_list)"
before=$FAILED
f_cached_properly
run_driver
assert_contains "$(cat "$DRIVER_LOG")" "1 model server(s) already drain on scale-down"   # anchor
f_cached_properly
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.template.spec.containers[0].args += ["--shutdown-timeout","90"]')"
run_driver
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$DRIVER_LOG")" "45 s of preStop + 90 s of --shutdown-timeout + 15 s = 150 s"
assert_contains "$(cat "$DRIVER_LOG")" "1 model server(s) have a preStop hook but a grace period shorter than, or not checkable against, their drain"
assert_not_contains "$(cat "$DRIVER_LOG")" "already drain on scale-down"
[ "$FAILED" -eq "$before" ] && ok

CASE="drain grace: LWS leader and worker templates are each checked, and counted once"
reset; f_lws_hooked 120 120
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.leaderWorkerTemplate.workerTemplate.spec.containers[0].args += ["--shutdown-timeout","120"]')"
so_workload_patch_doc ns leaderworkersets w "$POD_LWS" >/dev/null 2>"$WORK/lws.err"
assert_contains "$(cat "$WORK/lws.err")" "LeaderWorkerSet ns/w (workerTemplate): Scale-down can still cut in-flight requests"
assert_contains "$(cat "$WORK/lws.err")" "45 s of preStop + 120 s of --shutdown-timeout + 15 s = 180 s"
assert_not_contains "$(cat "$WORK/lws.err")" "(leaderTemplate): Scale-down"
assert_eq "$(grep -c 'no drain half' "$WORK/lws.err")" "0" "no-drain-half lines"
# The leader too, and the all-clear leaves the LWS out.
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.leaderWorkerTemplate.leaderTemplate.spec.containers[0].args += ["--shutdown-timeout","120"]')"
so_workload_patch_doc ns leaderworkersets w "$POD_LWS" >/dev/null 2>"$WORK/lws.err"
assert_contains "$(cat "$WORK/lws.err")" "LeaderWorkerSet ns/w (leaderTemplate): Scale-down can still cut"
LIST_FIXTURE='{"items":[{"metadata":{"name":"w","labels":{"llm-d.ai/role":"decode"}}}]}'
LIST_FIXTURE_LWS='{"items":[{"metadata":{"name":"l","labels":{"llm-d.ai/role":"decode"}}}]}'
CRD_RC=0
FIXTURE_MAP[l]="$FIXTURE"
f_cached_properly; FIXTURE_MAP[w]="$FIXTURE"
run_driver
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$DRIVER_LOG")" "1 model server(s) already drain on scale-down"
assert_contains "$(cat "$DRIVER_LOG")" "1 model server(s) have a preStop hook but a grace period shorter than, or not checkable against, their drain"
[ "$FAILED" -eq "$before" ] && ok

CASE="drain grace: DisaggregatedSet roles get the same check, leader falling back to worker"
reset; CRD_RC=0
before=$FAILED
# prefill: worker template only, a 30 s hook, --shutdown-timeout=90, grace 90.
# decode: a leader with no hook, --shutdown-timeout 90, grace 180.
DSET_FIXTURE="$(jq -cn '
    def eng($args; $grace; $hook):
        { metadata: { labels: { "llm-d.ai/role": "decode" } },
          spec: { terminationGracePeriodSeconds: $grace,
                  containers: [ { name: "routing-proxy", image: "quay.io/x/proxy:1" },
                                ( { name: "vllm", image: "quay.io/x/vllm-openai:v0.11",
                                    command: ["/bin/sh", "-c"], args: [$args] }
                                  + (if $hook then { lifecycle: { preStop: { exec: { command: ["sleep", "30"] } } } } else {} end) ) ] } };
    { metadata: { name: "glm" },
      spec: { roles: [
        { name: "prefill", spec: { leaderWorkerTemplate: {
            workerTemplate: eng("exec vllm serve /models/glm --shutdown-timeout=90"; 90; true) } } },
        { name: "decode", spec: { leaderWorkerTemplate: {
            leaderTemplate: eng("exec vllm serve /models/glm --shutdown-timeout 90"; 180; false),
            workerTemplate: eng("exec vllm serve /models/glm --headless"; 180; false) } } } ] } }')"
assert_eq "$(so_drain_times ns disaggregatedsets glm "$(so_dset_role_pod_path 0)" vllm | tr '\037' '|')" "1|30|90|" "prefill times"
assert_contains "$(so_drain_note ns disaggregatedsets glm "$(so_dset_role_pod_path 0)")" "30 s of preStop + 90 s of --shutdown-timeout + 15 s = 135 s"
assert_contains "$(so_drain_note ns disaggregatedsets glm "$(so_dset_role_pod_path 1)")" "the grace period needs at least 150 s"
out="$(so_workload_report_dsets ns 2>"$WORK/dset.err")"
assert_eq "$out" "2 2" "roles checked, roles with a gap"
assert_contains "$(cat "$WORK/dset.err")" "role prefill (spec.roles[0]): not patched"
assert_contains "$(cat "$WORK/dset.err")" "drain: Scale-down can still cut in-flight requests: vllm has a preStop hook"
assert_contains "$(cat "$WORK/dset.err")" "drain: Scale-down will cut in-flight requests: vllm declares no preStop hook"
DSET_FIXTURE=''
[ "$FAILED" -eq "$before" ] && ok

CASE="drain grace: a short hook beside a written document is summarised; bad numbers are refused or capped"
reset; LIST_FIXTURE="$(serving_list)"
before=$FAILED
# A repository id with no volume: a weights document is written, the grace is not.
f_drain 90 '["vllm","serve","Qwen/Qwen3-0.6B","--shutdown-timeout","90"]' '{"exec":{"command":["sleep","30"]}}'
run_driver
assert_contains "$(cat "$DRIVER_OUT" 2>/dev/null)" "#   cache:"   # anchor: a document was written
assert_not_contains "$(cat "$DRIVER_OUT" 2>/dev/null)" "terminationGracePeriodSeconds:"
assert_contains "$(cat "$DRIVER_LOG")" "1 model server(s) have a preStop hook but a grace period shorter than, or not checkable against, their drain"
assert_contains "$(cat "$DRIVER_LOG")" "That is not in"
# A sleep setting the arithmetic cannot use is refused before anything is read.
LLMSM_DRAIN_SLEEP_SECONDS=45s run_driver
assert_eq "$DRIVER_RC" "1" "return code, LLMSM_DRAIN_SLEEP_SECONDS=45s"
assert_contains "$(cat "$DRIVER_LOG")" "LLMSM_DRAIN_SLEEP_SECONDS must be a whole number of seconds"
# The plan note falls back to 45 s instead of ending the run.
f_drain 30 "$TO90"
assert_contains "$(LLMSM_DRAIN_SLEEP_SECONDS=45s so_drain_note ns deployments w "$POD_DEPLOY")" "the grace period needs at least 150 s"
# Ten digits are not read; a tab separates like a space.
f_drain 30 '["vllm","serve","/models/qwen","--shutdown-timeout","9999999999"]'
assert_eq "$(drain_times)" "0|0|0|timeout" "ten-digit timeout"
f_drain 30 '["/bin/sh","-c","vllm serve /models/qwen --shutdown-timeout\t90"]'
assert_eq "$(drain_times)" "0|0|90|" "tab-separated timeout"
[ "$FAILED" -eq "$before" ] && ok

CASE="drain grace: an unreadable timeout without a hook is said; settings are read as decimal"
reset
before=$FAILED
f_drain 30 '["/bin/sh","-c","vllm serve /models/qwen --shutdown-timeout $(T)"]'
so_workload_patch_doc ns deployments w "$POD_DEPLOY" >/dev/null 2>"$WORK/drain.err"
assert_contains "$(cat "$WORK/drain.err")" "ns/w: --shutdown-timeout is not a number this can read"
assert_contains "$(so_drain_note ns deployments w "$POD_DEPLOY")" "Its --shutdown-timeout is not a number this can read"
# A sleep action too large to add up is not read as a number.
f_drain 300 "$TO90" '{"sleep":{"seconds":10000000000}}'
assert_eq "$(drain_times)" "1|0|90|sleep" "ten-digit sleep action"
# 0200 is 200 seconds, not octal 128, and it is written as 200.
LIST_FIXTURE="$(serving_list)"
f_drain 30 '["vllm","serve","/models/qwen"]'
LLMSM_DRAIN_GRACE_SECONDS=0200 run_driver
assert_contains "$(cat "$DRIVER_OUT" 2>/dev/null)" "terminationGracePeriodSeconds: 200"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS: a healthy LWS beside a healthy Deployment is an all-clear, nothing skipped"
reset
before=$FAILED
LIST_FIXTURE='{"items":[{"metadata":{"name":"w","labels":{"llm-d.ai/role":"decode"}}}]}'
LIST_FIXTURE_LWS='{"items":[{"metadata":{"name":"l","labels":{"llm-d.ai/role":"decode"}}}]}'
CRD_RC=0
f_cached_properly; FIXTURE_MAP[w]="$FIXTURE"
f_lws_hooked 120 120; FIXTURE_MAP[l]="$FIXTURE"
run_driver
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$DRIVER_LOG")" "2 model server(s) already drain on scale-down"
assert_not_contains "$(cat "$DRIVER_LOG")" "skipped"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS: a worker-only LWS marked serving on its pod labels is found"
reset
before=$FAILED
CRD_RC=0; LIST_FIXTURE='{"items":[]}'
f_lws_worker_only
# The marker only on the workerTemplate pod labels, none on the object.
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.metadata.name = "l" | .spec.leaderWorkerTemplate.workerTemplate.metadata = {"labels":{"llm-d.ai/role":"decode"}}')"
LIST_FIXTURE_LWS="$(printf '%s' "$FIXTURE" | jq -c '{items: [.]}')"
run_driver
assert_contains "$(cat "$DRIVER_OUT" 2>/dev/null)" "leaderworkersets/l in ns1"
# The cluster-wide namespace walk reads the same labels.
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '{items: [.metadata.namespace = "ns9"]}')"
assert_eq "$(so_namespaces_of leaderworkersets "$POD_LWS")" "ns9" "namespaces"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS: a model passed in a variable is reported, and no weights half is guessed"
reset; f_lws_two
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.leaderWorkerTemplate.leaderTemplate.spec.containers[0].args = ["vllm serve $(MODEL) --tensor-parallel-size 2"]')"
doc="$(so_workload_patch_doc ns leaderworkersets w "$POD_LWS" 2>"$WORK/lws.err")"
assert_contains "$(cat "$WORK/lws.err")" "ns/w (leaderTemplate): the model could not be identified"
assert_contains "$(cat "$WORK/lws.err")" "ns/w (workerTemplate): the model could not be identified"
assert_contains "$doc" "- name: VLLM_CACHE_ROOT"                 # anchor: the engine cache still goes
assert_not_contains "$doc" "HF_HOME"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS apply: a controller-owned LWS is emitted, not applied, and the owner is named"
reset; lws_scene; f_lws_two
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.metadata.ownerReferences = [{"kind":"ModelService","name":"glm","controller":true}]')"
lws_driver true true
# Asked to apply and refused: the run says so in its exit code, as a failure does.
assert_eq "$DRIVER_RC" "1" "return code"
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$DRIVER_LOG")" "NOT applied -- it is controlled by ModelService/glm"
# Refused before any half is judged: no line says a volume is being applied.
assert_not_contains "$(cat "$DRIVER_LOG")" "applying the engine-cache volume too"
assert_contains "$(cat "$DRIVER_LOG")" "1 workload(s) were NOT applied because a controller owns them"
assert_contains "$(cat "$DRIVER_OUT")" "kind: LeaderWorkerSet"
# An owner that is not the controller (a garbage-collection parent) does not refuse.
reset; lws_scene; f_lws_two
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.metadata.ownerReferences = [{"kind":"ConfigMap","name":"x"}]')"
lws_driver true true
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls without a controller"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS apply: the patch tests the resourceVersion it was built from, and a changed object takes none of it"
reset; lws_scene; f_lws_two
before=$FAILED
lws_driver true true
assert_eq "$(jq -c '.[0]' <<< "$PATCH_BODY")" '{"op":"test","path":"/metadata/resourceVersion","value":"7"}' "first op"
moved="$(printf '%s' "$FIXTURE" | jq -c '.metadata.resourceVersion = "8"')"
json_patch_apply "$moved" "$PATCH_BODY" >/dev/null 2>&1 && fail "the patch applied to a changed object"
# What the operator reads when the API server refuses it.
reset; lws_scene; f_lws_two; PATCH_RC=1
lws_driver true true
assert_contains "$(cat "$DRIVER_LOG")" "the API server refused the JSON patch, so NOTHING was applied"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS apply: an env entry is replaced only after a test of its name at that index"
reset; lws_scene; f_lws_lists_present
before=$FAILED
lws_driver true true
assert_eq "$(jq -c '[.[] | select(.path | test("/containers/1/env/1"))]' <<< "$PATCH_BODY")" \
    '[{"op":"test","path":"/spec/leaderWorkerTemplate/leaderTemplate/spec/containers/1/env/1/name","value":"VLLM_CACHE_ROOT"},{"op":"replace","path":"/spec/leaderWorkerTemplate/leaderTemplate/spec/containers/1/env/1","value":{"name":"VLLM_CACHE_ROOT","value":"/engine-cache/vllm"}}]' "ops at env/1"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS apply: the warning names the groups it replaces (replicas 1, and 3 of size 4)"
reset; lws_scene; f_lws_two
before=$FAILED
lws_driver true true
assert_contains "$(cat "$DRIVER_LOG")" "this REPLACES THE GROUP (replicas: 1, size: 1)"
assert_contains "$(cat "$DRIVER_LOG")" "the model serves nothing for one multi-node start"
reset; lws_scene; f_lws_two
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.replicas = 3 | .spec.leaderWorkerTemplate.size = 4')"
lws_driver true true
assert_contains "$(cat "$DRIVER_LOG")" "this REPLACES EVERY GROUP (replicas: 3, size: 4)"
assert_contains "$(cat "$DRIVER_LOG")" "up to 1 at a time (maxUnavailable: 1, maxSurge: 0)"
after="$(json_patch_apply "$FIXTURE" "$PATCH_BODY")" || fail "the patch does not apply at size 4"
assert_eq "$(jq -r '.spec.leaderWorkerTemplate.size' <<< "$after")" "4" "size kept"
assert_eq "$(jq -r '.spec.leaderWorkerTemplate.workerTemplate.spec.volumes[0].name' <<< "$after")" "engine-cache" "worker volume at size 4"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS apply: a worker-only LWS is patched at workerTemplate alone"
reset; lws_scene; f_lws_worker_only
before=$FAILED
lws_driver true true
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_eq "$(jq -r '[.[] | select(.path | contains("leaderTemplate"))] | length' <<< "$PATCH_BODY")" "0" "leaderTemplate ops"
after="$(json_patch_apply "$FIXTURE" "$PATCH_BODY")" || fail "the patch does not apply: $PATCH_BODY"
assert_eq "$(jq -r '.spec.leaderWorkerTemplate.workerTemplate.spec.containers[0].env | map(.name) | join(",")' <<< "$after")" \
    "VLLM_CACHE_ROOT,FLASHINFER_WORKSPACE_BASE,TRITON_CACHE_DIR,PYTHONPYCACHEPREFIX" "worker env"
assert_eq "$(jq -r '.spec.leaderWorkerTemplate | has("leaderTemplate")' <<< "$after")" "false" "no leaderTemplate created"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS apply: two containers in one template -- only the engine is touched"
reset; lws_scene; f_lws_two
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.leaderWorkerTemplate.workerTemplate.spec.containers += [{"name":"metrics","image":"quay.io/x/exporter:1","env":[{"name":"PORT","value":"9400"}]}]')"
lws_driver true true
after="$(json_patch_apply "$FIXTURE" "$PATCH_BODY")" || fail "the patch does not apply: $PATCH_BODY"
assert_eq "$(jq -c '.spec.leaderWorkerTemplate.workerTemplate.spec.containers[1]' <<< "$after")" \
    '{"name":"metrics","image":"quay.io/x/exporter:1","env":[{"name":"PORT","value":"9400"}]}' "sidecar"
assert_eq "$(jq -r '.spec.leaderWorkerTemplate.workerTemplate.spec.containers[0].env | map(.name) | join(",")' <<< "$after")" \
    "VLLM_CACHE_ROOT,FLASHINFER_WORKSPACE_BASE,TRITON_CACHE_DIR,PYTHONPYCACHEPREFIX" "engine env"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS apply: the warning reads the rollout strategy the LWS declares"
reset; lws_scene; f_lws_two
before=$FAILED
FIXTURE="$(printf '%s' "$FIXTURE" | jq -c '.spec.rolloutStrategy = {"type":"RollingUpdate","rollingUpdateConfiguration":{"maxUnavailable":0,"maxSurge":1}}')"
lws_driver true true
assert_contains "$(cat "$DRIVER_LOG")" "with maxSurge: 1 (maxUnavailable: 0) it starts the new group first"
assert_not_contains "$(cat "$DRIVER_LOG")" "serves nothing"
[ "$FAILED" -eq "$before" ] && ok

CASE="LWS apply: an LWS-only run does not describe a Deployment rollout"
reset; lws_scene; f_lws_two
before=$FAILED
lws_driver true true
assert_contains "$(cat "$DRIVER_LOG")" "this REPLACES THE GROUP"          # anchor
assert_not_contains "$(cat "$DRIVER_LOG")" "the new pod must schedule before the old one goes"
# ...while a Deployment run still does.
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
lws_driver true
assert_contains "$(cat "$DRIVER_LOG")" "the new pod must schedule before the old one goes"
[ "$FAILED" -eq "$before" ] && ok

# The two serving-workload counters outside this library read the pod template
# the same way, so a worker-only LWS counts there too. Loaded one function at a
# time: their libraries need more than this harness sources.
load_fn() { eval "$(tr -d '' < "$ROOT/deploy/lib/$1" | sed -n "/^$2() {/,/^}/p")"; }
load_fn infra_monitoring.sh llmsm_serving_workload_count
load_fn prereqs.sh llmsm_model_server_count

CASE="LWS: a worker-only LWS marked on its pod labels is counted by both counters"
reset
before=$FAILED
LIST_FIXTURE='{"items":[]}'
f_lws_worker_only
LIST_FIXTURE_LWS="$(printf '%s' "$FIXTURE" | jq -c '{items: [.spec.leaderWorkerTemplate.workerTemplate.metadata = {"labels":{"llm-d.ai/role":"decode"}}]}')"
assert_eq "$(llmsm_serving_workload_count ns1)" "1" "llmsm_serving_workload_count"
assert_eq "$(llmsm_model_server_count ns1)" "1" "llmsm_model_server_count"
[ "$FAILED" -eq "$before" ] && ok

# ----------------------------------------------------------------------------

if [ "$FAILED" -gt 0 ]; then
    printf '\n%d workload-readiness assertion(s) failed.\n' "$FAILED" >&2
    exit 1
fi
# Every `ok` above increments RAN. A run that ends early -- a library that
# aborts, a case whose command substitution dies under `set -u`, a case deleted
# by accident -- otherwise exits 0 having checked less than it claims.
if [ $((RAN + SKIPPED)) -ne "$EXPECT_CASES" ]; then
    printf '\n%d case(s) reported ok and %d skipped, %d expected -- the run did not finish.\n' \
        "$RAN" "$SKIPPED" "$EXPECT_CASES" >&2
    exit 1
fi
# A run that skipped everything checked nothing. Each skip site can skip one
# case, so more skips than sites, or no ok at all, is a broken run.
if [ "$RAN" -eq 0 ] || [ "$SKIPPED" -gt "$SKIP_SITES" ]; then
    printf '\n%d case(s) reported ok and %d skipped, at most %d may skip -- the run checked too little.\n' \
        "$RAN" "$SKIPPED" "$SKIP_SITES" >&2
    exit 1
fi
if [ "$SKIPPED" -gt 0 ]; then
    printf '\nWorkload readiness checks passed: %d case(s), %d skipped.\n' "$RAN" "$SKIPPED"
else
    printf '\nWorkload readiness checks passed: %d case(s).\n' "$RAN"
fi
