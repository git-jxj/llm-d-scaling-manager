#!/usr/bin/env bash
# Runs the capacity planner image (Dockerfile.capacity-planner) the way the
# guide does and asserts that it serves:
#
#   PLANNER_IMG=<image> [PLANNER_VERSION=<tag>] bash hack/check-capacity-planner-image.sh
#
# With its default command, run as the caller (--user) and a committed
# recording mounted at /recordings with mode 0600, as recordings are written,
# published to the host's loopback, POST /v1/inputs resolves the recording and
# POST /v1/plan prices it: both 200, with the inputs id carried through and a
# TTFT curve in the plan. Then what must stay refused: a request naming a host
# other than loopback (403), and serve on 0.0.0.0 without
# --listen-all-in-container (exit 2). Also: the image runs as non-root, and
# `version` prints PLANNER_VERSION when it is set.
#
# Needs docker, curl and jq. ci-pr-checks.yaml (capacity-planner-image) runs it.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

IMG="${PLANNER_IMG:-}"
[ -n "$IMG" ] || { echo "FAIL: set PLANNER_IMG=<image>"; exit 1; }
for tool in docker curl jq; do
    command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: $tool is required (not skipping: a skipped check reads as a pass)"; exit 1; }
done

# The committed recording the serve tests price (cmd/capacity-planner, shortRun).
RECORDING="$ROOT/internal/engines/analyzers/learnreplay/testdata/goldens/ttft-open-loop-short/recording.jsonl.gz"
[ -s "$RECORDING" ] || { echo "FAIL: $RECORDING is missing"; exit 1; }

T="$(mktemp -d)"
NAME="capacity-planner-check-$$"
cleanup() {
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    rm -rf "$T"
}
trap cleanup EXIT
FAILED=0
PASSED=0
# One ok or FAIL per case: non-root, version, started, warning, inputs, plan,
# foreign Host, and 0.0.0.0 without the opt-in. A run that stops early, or a
# case dropped, fails on the count. Change this deliberately with the cases.
EXPECT_CASES=8
ok()   { printf '  ok   %s\n' "$1"; PASSED=$((PASSED + 1)); }
fail() { printf '  FAIL %s\n' "$1"; FAILED=$((FAILED + 1)); }

echo "Checking the capacity planner image $IMG"

# Non-root, as the guide runs it.
user="$(docker image inspect --format '{{.Config.User}}' "$IMG")"
case "$user" in
    ""|0|0:*|root|root:*) fail "the image runs as root (User '$user')" ;;
    *) ok "runs as $user" ;;
esac

ver="$(docker run --rm "$IMG" version)"
if [ -n "${PLANNER_VERSION:-}" ]; then
    case "$ver" in
        "capacity-planner $PLANNER_VERSION (commit "*) ok "version prints $PLANNER_VERSION: $ver" ;;
        *) fail "version printed '$ver', want capacity-planner $PLANNER_VERSION" ;;
    esac
else
    case "$ver" in
        "capacity-planner "*) ok "version: $ver" ;;
        *) fail "version printed '$ver'" ;;
    esac
fi

# Mode 0600, as capacity-planner generate writes a recording: readable by
# the caller only, so the container must run as the caller (--user), as the
# guide runs it. The image's own user (65532) cannot read it.
mkdir -p "$T/recordings"
cp "$RECORDING" "$T/recordings/short.jsonl.gz"
chmod 0755 "$T/recordings"
chmod 0600 "$T/recordings/short.jsonl.gz"

# The image's own command, published to loopback only, as the guide does.
docker run -d --name "$NAME" --user "$(id -u):$(id -g)" -p 127.0.0.1::8080 \
    -v "$T/recordings:/recordings:ro" "$IMG" >/dev/null
# Up to 30 s for serve to say it serves and docker to report the published
# loopback port; a container that exits stops the wait at once.
up=false
port=""
for _ in $(seq 1 60); do
    if [ -z "$port" ]; then
        port="$( { docker port "$NAME" 8080/tcp 2>/dev/null || true; } | sed -n 's/^127\.0\.0\.1:\([0-9][0-9]*\)$/\1/p' | head -1)"
    fi
    if [ -n "$port" ] && docker logs "$NAME" 2>&1 | grep -q 'serving the capacity planner'; then up=true; break; fi
    if [ "$(docker inspect --format '{{.State.Running}}' "$NAME" 2>/dev/null || true)" != true ]; then break; fi
    sleep 0.5
done
logs="$(docker logs "$NAME" 2>&1 || true)"
if [ "$up" != true ]; then
    printf '%s\n' "$logs"
    echo "FAIL: serve did not start, or published no loopback port (port '$port')"
    exit 1
fi
BASE="http://127.0.0.1:$port"
ok "serve started on $BASE"
if printf '%s\n' "$logs" | grep -q 'warning: --listen-all-in-container'; then
    ok "it warns that it serves every interface without authentication"
else
    fail "no --listen-all-in-container warning in the log: $logs"
fi

# post <path> <body file> <out file> [host]: the HTTP status.
post() {
    local host=()
    [ $# -lt 4 ] || host=(-H "Host: $4")
    curl -sS -o "$3" -w '%{http_code}' --max-time 300 -H 'Content-Type: application/json' "${host[@]}" \
        --data-binary "@$2" "$BASE$1"
}

printf '%s' '{"source": {"kind": "recording", "name": "short.jsonl.gz"}}' > "$T/inputs.json"
code="$(post /v1/inputs "$T/inputs.json" "$T/inputs.out")"
id="$(jq -r '.inputsId // empty' "$T/inputs.out" 2>/dev/null || true)"
if [ "$code" = 200 ] && [ "${id#sha256:}" != "$id" ]; then
    ok "POST /v1/inputs: 200, inputsId $id"
else
    fail "POST /v1/inputs: $code, inputsId '$id': $(head -c 400 "$T/inputs.out")"
fi

jq -n --arg id "$id" '{inputsId: $id, assume: {vp: 130000},
    traffic: {classes: {critical: {rate: 1}}},
    slo: {ttft: {statistic: "p90", target: 1}}}' > "$T/plan.json"
code="$(post /v1/plan "$T/plan.json" "$T/plan.out")"
if [ "$code" = 200 ] && jq -e --arg id "$id" \
    '.inputsId == $id and (.scenarioId | type == "string" and length > 0)
     and (.ttft.line.curve | type == "array" and length > 0)
     and (.limits.fleet | type == "object")' "$T/plan.out" >/dev/null 2>&1; then
    ok "POST /v1/plan: 200, $(jq '.ttft.line.curve | length' "$T/plan.out") points on the TTFT curve"
else
    fail "POST /v1/plan: $code: $(head -c 400 "$T/plan.out")"
fi

code="$(post /v1/plan "$T/plan.json" "$T/foreign.out" "planner.example:$port")"
if [ "$code" = 403 ]; then
    ok "a request naming a host other than loopback is refused (403)"
else
    fail "a request with Host planner.example got $code, want 403"
fi

# Without the opt-in, 0.0.0.0 with no authentication is refused, as before.
rc=0
out="$(docker run --rm "$IMG" serve --listen 0.0.0.0:8080 --recordings /recordings 2>&1)" || rc=$?
if [ "$rc" = 2 ] && printf '%s' "$out" | grep -q 'not a loopback address'; then
    ok "serve on 0.0.0.0 without --listen-all-in-container is refused (exit 2)"
else
    fail "serve on 0.0.0.0 without --listen-all-in-container: exit $rc: $out"
fi

echo ""
echo "capacity planner image: $PASSED passed, $FAILED failed"
if [ $((PASSED + FAILED)) -ne "$EXPECT_CASES" ]; then
    echo "FAIL: $((PASSED + FAILED)) case(s) ran, $EXPECT_CASES expected"
    exit 1
fi
[ "$FAILED" -eq 0 ]
