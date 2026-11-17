#!/usr/bin/env bash
# Executes hack/benchmark/patch_harness.sh fix 6 -- the router pins it writes
# into the llm-d-benchmark clone's defaults.yaml: the modelservice chart, the
# routing sidecar that must match it, and the default EPP image -- against
# fixtures carrying the upstream anchors.
#
# Each case is a clone state the fix must handle: pristine upstream, a clone
# patched by an older version of this fix (either note prefix), a clone whose
# pins are right but which still carries an old note, and one missing an anchor.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PY=${PYTHON:-python3}
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); echo "ok   $*"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL $*"; }

tr -d '\r' < "$ROOT/hack/benchmark/patch_harness.sh" \
    | sed -n '/fix 6 (sidecar\/chart\/EPP pins) failed"/,/^PYEOF$/p' | sed '1d;$d' > "$T/fix6.py"
[ -s "$T/fix6.py" ] || { echo "FAIL could not extract fix 6 from patch_harness.sh"; exit 1; }

want_chart="$(sed -n 's/^MODELSERVICE_CHART = "\(.*\)"$/\1/p' "$T/fix6.py")"
want_sidecar="$(sed -n 's/^ROUTING_SIDECAR = "\(.*\)"$/\1/p' "$T/fix6.py")"
want_epp="$(sed -n 's/^ROUTER_EPP_IMAGE = "\(.*\)"$/\1/p' "$T/fix6.py")"
if [ -z "$want_chart" ] || [ -z "$want_sidecar" ] || [ -z "$want_epp" ]; then
    echo "FAIL fix 6 does not declare MODELSERVICE_CHART, ROUTING_SIDECAR and ROUTER_EPP_IMAGE"
    exit 1
fi

# fixture <file> <chart> <sidecar> <epp> [note-prefix]: a defaults.yaml cut down
# to the anchors fix 6 reads, in upstream's spacing.
fixture() {
    {
        echo "anchors:"
        echo "  llm-d-router-endpoint-picker_version: &llm-d-router-endpoint-picker_version $4"
        echo "  llm-d-routing-sidecar_version:     &llm-d-routing-sidecar_version     $3"
        echo "chartVersions:"
        echo "  llmDModelservice:"
        echo "    url: https://example.invalid/"
        if [ -n "${5:-}" ]; then
            echo "  # $5-patch: pinned. v0.4.16 emits --model-server-port, which the"
            echo "  # llm-d-routing-sidecar version pinned above does not accept."
        fi
        echo "  llmDModelservice: $2"
    } > "$1"
}

# pinned <file>: the three pins are the wanted values and no old note is left.
pinned() {
    grep -qE "^  llmDModelservice: ${want_chart}\$" "$1" \
        && grep -qE "&llm-d-routing-sidecar_version +${want_sidecar}\$" "$1" \
        && grep -qE "&llm-d-router-endpoint-picker_version +${want_epp}\$" "$1" \
        && ! grep -qE 'pinned\. v0\.4\.16 emits' "$1" \
        && [ "$(grep -c 'scaling-manager-patch: pinned with the routing sidecar' "$1")" -eq 1 ]
}

check_case() {
    local name="$1" file="$2"
    local out rc
    out="$("$PY" "$T/fix6.py" "$file" 2>&1)"; rc=$?
    if [ "$rc" -ne 0 ] || ! pinned "$file"; then
        fail "$name: rc=$rc ($out)"
        return
    fi
    cp "$file" "$file.first"
    out="$("$PY" "$T/fix6.py" "$file" 2>&1)"; rc=$?
    if [ "$rc" -eq 0 ] && cmp -s "$file" "$file.first" && printf '%s' "$out" | grep -q 'already applied'; then
        ok "$name: pinned, and a second run changes nothing"
    else
        fail "$name: second run rc=$rc, changed=$(cmp -s "$file" "$file.first" && echo no || echo yes) ($out)"
    fi
}

fixture "$T/pristine.yaml" auto v0.9.0 v0.9.0
check_case "pristine upstream (chart auto, sidecar and EPP v0.9.0)" "$T/pristine.yaml"

fixture "$T/old-sm.yaml" v0.4.15 v0.9.0 v0.9.0 scaling-manager
check_case "patched by the old fix (scaling-manager note, v0.4.15)" "$T/old-sm.yaml"

fixture "$T/old-wva.yaml" v0.4.15 v0.9.0 v0.9.0 wva
check_case "patched under the older wva prefix" "$T/old-wva.yaml"

# Pins already right; only the stale note differs. The fix must still write.
fixture "$T/note-only.yaml" "$want_chart" "$want_sidecar" "$want_epp" wva
check_case "pins right, stale note left (must still write)" "$T/note-only.yaml"

# Both upstream starting points end in the same file.
if cmp -s "$T/pristine.yaml" "$T/old-wva.yaml" && cmp -s "$T/pristine.yaml" "$T/note-only.yaml"; then
    ok "every starting state converges on one file"
else
    fail "starting states end in different files"
fi

fixture "$T/missing.yaml" auto v0.9.0 v0.9.0
grep -v '^  llmDModelservice: ' "$T/missing.yaml" > "$T/missing2.yaml"
if out="$("$PY" "$T/fix6.py" "$T/missing2.yaml" 2>&1)"; then
    fail "a missing anchor was not an error ($out)"
elif printf '%s' "$out" | grep -q 'anchor missing'; then
    ok "a missing anchor is a hard error that names it"
else
    fail "a missing anchor failed without saying so ($out)"
fi

echo "harness router pins: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
