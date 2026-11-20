#!/usr/bin/env bash
# `deploy/warmpool.sh create` refuses a pool that fits no node once the host RAM
# serving engines hold is taken off.
#
# A level-1 sleeper lives in host RAM, and so does a CPU KV-offload tier. A pool
# sized from the node's RAM on nodes whose engines pin a tier is Pending there,
# or evicted under memory pressure with every resident model in it. The node is
# stated by hand (--ram-gib, --reserved-ram-gib), or a kubectl stub answers for
# a real apply, so this reads no cluster.
set -u
# No cluster is read: kubectl renders and refuses the rest (hack/offline-kubectl).
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/offline-kubectl.sh"

SCRIPT="$(cd "$(dirname "$0")/.." && pwd)/deploy/warmpool.sh"
FAILED=0

# Every case reports ok or FAIL exactly once; a run that stops early fails.
EXPECT_CASES=12
CASES=0
ok()   { CASES=$((CASES + 1)); printf 'ok    %s\n' "$1"; }
fail() { CASES=$((CASES + 1)); printf 'FAIL  %s\n' "$1"; FAILED=1; }

# create prints the manifests and the log, and sets RC. 4 x 8B is 100Gi.
create() {
  RC=0
  OUT="$(bash "$SCRIPT" create -n tenant --name pool \
    --proxy-image example.invalid/proxy:v1 --scaling-manager-namespace scaling-manager-system \
    --models 4 --model-size 8B --accelerator NVIDIA-H200 --dry-run "$@" 2>&1)" || RC=$?
}

# --- refused: 2016 GiB - 1950 GiB held leaves 66 GiB for a 100Gi pool ---------
create --ram-gib 2016 --reserved-ram-gib 1950
[ "$RC" -ne 0 ] && printf '%s\n' "$OUT" | grep -q "Nothing was created: a pool Pod with 100Gi fits no node" \
  && ok "a pool that fits no node is refused, and says so" \
  || fail "a pool that fits no node was not refused for that reason (rc $RC): $OUT"
printf '%s\n' "$OUT" | grep -q "2016 GiB allocatable - 1950 GiB held by serving workloads" \
  && printf '%s\n' "$OUT" | grep -q "66 GiB for the pool, which needs 100 GiB" \
  && ok "the refusal names the node's RAM, the held RAM and the pool's limit" \
  || fail "the refusal does not give the numbers: $OUT"
printf '%s\n' "$OUT" | grep -q '^kind: Deployment' \
  && fail "a refused pool still printed its manifests" \
  || ok "a refused pool prints no manifests"

# --- fits: 1000 GiB held leaves 1016 GiB --------------------------------------
create --ram-gib 2016 --reserved-ram-gib 1000
[ "$RC" -eq 0 ] && printf '%s\n' "$OUT" | grep -q '^kind: Deployment' \
  && ok "a pool that fits after the held RAM is created" \
  || fail "a pool that fits was refused (rc $RC): $OUT"
printf '%s\n' "$OUT" | grep -q "Host RAM: 1 node(s): 2016 GiB allocatable - 1000 GiB held" \
  && ok "the check that passed says what it checked" \
  || fail "the passing check printed no numbers: $OUT"

# --- nothing held ---------------------------------------------------------------
create --ram-gib 2016
[ "$RC" -eq 0 ] && printf '%s\n' "$OUT" | grep -q "Host RAM: 1 node(s): 2016 GiB allocatable - 0 GiB held" \
  && ok "a node with nothing held is checked and passes" \
  || fail "a node with nothing held was refused (rc $RC): $OUT"

# --- a dry run with no node stated reads no cluster, so it does not check --------
create --reserved-ram-gib 1950
[ "$RC" -eq 0 ] && ! printf '%s\n' "$OUT" | grep -q "Host RAM:" \
  && printf '%s\n' "$OUT" | grep -q "reserved-ram-gib is not checked on a dry run without --ram-gib" \
  && ok "a dry run with no --ram-gib renders without checking, and says so" \
  || fail "a dry run with no --ram-gib checked or failed (rc $RC): $OUT"

# --- a value that is not a number is refused, not passed on ----------------------
create --ram-gib 2Ti
[ "$RC" -ne 0 ] && printf '%s\n' "$OUT" | grep -q "must be a number of GiB" \
  && ok "--ram-gib 2Ti is refused" || fail "--ram-gib 2Ti was not refused (rc $RC): $OUT"

# --- a real apply reads the cluster: a kubectl stub answers ----------------------
# One H200 node (2000 GiB allocatable) running a Pod that pins a 1950 GiB CPU tier.
# The stub logs every call and answers NotFound to anything else, so nothing is
# ever applied. STUB_NODES=forbidden makes the node read fail.
STUB="$(mktemp -d)"
trap 'rm -rf "$STUB"' EXIT
cat > "$STUB/kubectl" <<'KUBECTL'
#!/usr/bin/env bash
echo "kubectl $*" >> "$STUB_LOG"
case " $* " in
  *" get nodes "*)
    if [ "${STUB_NODES:-}" = forbidden ]; then echo 'Error from server (Forbidden): nodes is forbidden' >&2; exit 1; fi
    cat <<'JSON'
{"items":[{"metadata":{"name":"n1","labels":{"nvidia.com/gpu.product":"NVIDIA-H200"}},
 "spec":{},"status":{"capacity":{"nvidia.com/gpu":"8","memory":"2016Gi"},"allocatable":{"nvidia.com/gpu":"8","memory":"2000Gi"}}}]}
JSON
    ;;
  *" get pods "*)
    cat <<'JSON'
{"items":[{"metadata":{"name":"prefill-0"},"status":{"phase":"Running"},
 "spec":{"nodeName":"n1","containers":[{"name":"vllm","args":["--kv-offloading-size","1950"],
  "resources":{"limits":{"nvidia.com/gpu":"8"}}}]}}]}
JSON
    ;;
  *" get deployments "*|*" get statefulsets "*|*" get leaderworkersets "*) echo '{"items":[]}' ;;
  *) echo "Error from server (NotFound): not found" >&2; exit 1 ;;
esac
KUBECTL
chmod +x "$STUB/kubectl"
export STUB_LOG="$STUB/calls"
apply_create() {
  RC=0
  OUT="$(PATH="$STUB:$PATH" bash "$SCRIPT" create -n tenant --name pool \
    --proxy-image example.invalid/proxy:v1 --scaling-manager-namespace scaling-manager-system \
    --models 4 --model-size 8B --accelerator NVIDIA-H200 2>&1)" || RC=$?
}
: > "$STUB_LOG"
apply_create
[ "$RC" -ne 0 ] && printf '%s\n' "$OUT" | grep -q "Nothing was created: a pool Pod with 100Gi fits no node" \
  && printf '%s\n' "$OUT" | grep -q "2000 GiB allocatable - 1950 GiB held by serving workloads (pod/prefill-0)" \
  && ok "a real apply reads the Pods on the node and refuses, with the numbers" \
  || fail "a real apply did not refuse on what the node's Pods hold (rc $RC): $OUT"
grep -q -- "get nodes -l nvidia.com/gpu.product=NVIDIA-H200" "$STUB_LOG" \
  && grep -q -- "get pods -n tenant" "$STUB_LOG" && ! grep -q " apply " "$STUB_LOG" \
  && ok "it read the accelerator's nodes and the namespace's Pods, and applied nothing" \
  || fail "unexpected kubectl calls: $(tr '\n' ';' < "$STUB_LOG")"
: > "$STUB_LOG"
STUB_NODES=forbidden apply_create
printf '%s\n' "$OUT" | grep -q "Could not check host RAM on the nodes, so the pool is created unchecked" \
  && printf '%s\n' "$OUT" | grep -q "To check by hand: --ram-gib" \
  && ok "nodes it may not read: a warning, not a refusal" \
  || fail "a Forbidden node read did not warn and go on: $OUT"

# --- sizing subtracts the held RAM too --------------------------------------------
RC=0
OUT="$(bash "$SCRIPT" sizing --params 8B --dtype bf16 --gpus-per-node 8 --gpu-mem-gib 141 \
  --ram-gib 2016 --reserved-ram-gib 1950 2>&1)" || RC=$?
[ "$RC" -eq 0 ] && printf '%s\n' "$OUT" | grep -q "level 1 IMPOSSIBLE" \
  && printf '%s\n' "$OUT" | grep -q "1950 GiB of host RAM held" \
  && ok "sizing says level 1 does not fit beside 1950 GiB held" \
  || fail "sizing ignored the held RAM (rc $RC): $OUT"

if [ "$CASES" -ne "$EXPECT_CASES" ]; then
  printf 'FAIL  ran %d cases, expected %d\n' "$CASES" "$EXPECT_CASES"
  exit 1
fi
[ "$FAILED" -eq 0 ] || exit 1
printf 'all %d cases passed\n' "$CASES"
