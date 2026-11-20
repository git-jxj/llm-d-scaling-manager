#!/usr/bin/env bash
# `deploy/warmpool.sh create` refuses a pool that fits no node once the host RAM
# serving engines hold is taken off.
#
# A level-1 sleeper lives in host RAM, and so does a CPU KV-offload tier. A pool
# sized from the node's RAM on nodes whose engines pin a tier is Pending there,
# or evicted under memory pressure with every resident model in it. The node is
# stated by hand (--ram-gib, --reserved-ram-gib), so this reads no cluster.
set -u
# No cluster is read: kubectl renders and refuses the rest (hack/offline-kubectl).
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/offline-kubectl.sh"

SCRIPT="$(cd "$(dirname "$0")/.." && pwd)/deploy/warmpool.sh"
FAILED=0

# Every case reports ok or FAIL exactly once; a run that stops early fails.
EXPECT_CASES=9
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
[ "$RC" -ne 0 ] && ok "a pool that fits no node is refused" \
  || fail "a pool that fits no node was created (rc 0)"
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
[ "$RC" -eq 0 ] && ok "a node with nothing held is checked and passes" \
  || fail "a node with nothing held was refused (rc $RC): $OUT"

# --- a dry run with no node stated reads no cluster, so it does not check --------
create --reserved-ram-gib 1950
[ "$RC" -eq 0 ] && ! printf '%s\n' "$OUT" | grep -q "Host RAM:" \
  && ok "a dry run with no --ram-gib renders without checking" \
  || fail "a dry run with no --ram-gib checked or failed (rc $RC): $OUT"

# --- a value that is not a number is refused, not passed on ----------------------
create --ram-gib 2Ti
[ "$RC" -ne 0 ] && printf '%s\n' "$OUT" | grep -q "must be a number of GiB" \
  && ok "--ram-gib 2Ti is refused" || fail "--ram-gib 2Ti was not refused (rc $RC): $OUT"

# --- sizing subtracts the held RAM too --------------------------------------------
RC=0
OUT="$(bash "$SCRIPT" sizing --params 8B --dtype bf16 --gpus-per-node 8 --gpu-mem-gib 141 \
  --ram-gib 2016 --reserved-ram-gib 1500 2>&1)" || RC=$?
[ "$RC" -eq 0 ] && printf '%s\n' "$OUT" | grep -q "level 1 IMPOSSIBLE" \
  && printf '%s\n' "$OUT" | grep -q "1500 GiB of host RAM held" \
  && ok "sizing says level 1 does not fit beside 1500 GiB held" \
  || fail "sizing ignored the held RAM (rc $RC): $OUT"

if [ "$CASES" -ne "$EXPECT_CASES" ]; then
  printf 'FAIL  ran %d cases, expected %d\n' "$CASES" "$EXPECT_CASES"
  exit 1
fi
[ "$FAILED" -eq 0 ] || exit 1
printf 'all %d cases passed\n' "$CASES"
