#!/usr/bin/env bash
#
# Executes rv_snapshot (test/tools-install/rv_snapshot.sh), which the
# tools-install e2e takes before and after a second install, against a stub
# kubectl, and asserts that a KEDA status write to a ClusterTriggerAuthentication
# leaves the snapshot as it was while an install that rewrites its spec, a
# label or an annotation, or any write to another object, changes it.
#
# It exists because KEDA patches status.scaledobjects of a
# ClusterTriggerAuthentication when it finalizes a ScaledObject that names it.
# The smoke suite deletes its namespaces without waiting, KEDA finalized one
# between the two snapshots, and the e2e failed "a second install changes no
# resourceVersion" on a change no install made.
#
# RV_SNAPSHOT_LIB overrides the file sourced, to run these cases against
# another version of rv_snapshot; the stub serves only "-o json".

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB="${RV_SNAPSHOT_LIB:-$ROOT/test/tools-install/rv_snapshot.sh}"
FAIL=0
PASS=0

for tool in jq sha256sum; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "FATAL: $tool is required by this check, and is missing. Nothing below would be about the code."
        exit 2
    }
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
tr -d '\r' < "$LIB" > "$WORK/lib.sh"
# shellcheck source=/dev/null
. "$WORK/lib.sh"
declare -F rv_snapshot >/dev/null || { echo "FATAL: rv_snapshot is not defined after sourcing $LIB."; exit 2; }

ok() { echo "ok   $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL $1"; FAIL=$((FAIL + 1)); }

# The stub kubectl: "get <kind> <name> [-n <ns>] -o json" prints
# $WORK/live/<kind>.<name>.json, and fails as a NotFound would when the file
# is missing. Any other call is a failure of this check.
kubectl() {
    [ "$1" = get ] || { echo "stub kubectl: unexpected call: $*" >&2; return 99; }
    local f="$WORK/live/$2.$3.json" out=''
    shift 3
    while [ $# -gt 0 ]; do
        case "$1" in
            -n) shift ;;
            -o) out="$2"; shift ;;
        esac
        shift
    done
    [ "$out" = json ] || { echo "stub kubectl: unexpected output: $out" >&2; return 99; }
    [ -f "$f" ] || { echo "Error from server (NotFound): not found" >&2; return 1; }
    cat "$f"
}

mkdir -p "$WORK/live"
CTA="$WORK/live/ClusterTriggerAuthentication.scaling-manager-external-scaler-llm-d-sim.json"
TA="$WORK/live/TriggerAuthentication.scaling-manager-external-scaler.json"
CM="$WORK/live/ConfigMap.scaling-manager-settings.json"
cat > "$CTA" <<'EOF'
{"apiVersion":"keda.sh/v1alpha1","kind":"ClusterTriggerAuthentication",
 "metadata":{"name":"scaling-manager-external-scaler-llm-d-sim","uid":"5b0c6c1e-0000-4000-8000-000000000001",
  "resourceVersion":"2210","generation":1,
  "finalizers":["finalizer.keda.sh"],"labels":{"app.kubernetes.io/name":"scaling-manager"},
  "managedFields":[{"manager":"kubectl","operation":"Apply","time":"2026-11-17T05:58:26Z"}]},
 "spec":{"secretTargetRef":[{"parameter":"caCert","name":"scaling-manager-external-scaler-llm-d-sim","key":"ca.crt"}]},
 "status":{"scaledobjects":"ext-scaler-so,error-test-hpa-so"}}
EOF
cat > "$TA" <<'EOF'
{"apiVersion":"keda.sh/v1alpha1","kind":"TriggerAuthentication",
 "metadata":{"name":"scaling-manager-external-scaler","namespace":"llm-d-sim","uid":"5b0c6c1e-0000-4000-8000-000000000002",
  "resourceVersion":"2215","generation":1,"finalizers":["finalizer.keda.sh"],
  "managedFields":[{"manager":"kubectl","operation":"Apply","time":"2026-11-17T05:58:27Z"}]},
 "spec":{"secretTargetRef":[{"parameter":"caCert","name":"scaling-manager-external-scaler","key":"ca.crt"}]},
 "status":{"scaledobjects":"ext-scaler-so"}}
EOF
cat > "$CM" <<'EOF'
{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"scaling-manager-settings","namespace":"llm-d-sim","resourceVersion":"1916"},"data":{"passive":"true"}}
EOF
cat > "$WORK/objects.txt" <<'EOF'
ClusterTriggerAuthentication - scaling-manager-external-scaler-llm-d-sim
TriggerAuthentication llm-d-sim scaling-manager-external-scaler
ConfigMap llm-d-sim scaling-manager-settings
Secret llm-d-sim scaling-manager-prometheus-ca
EOF
for f in "$CTA" "$TA" "$CM"; do cp "$f" "$f.orig"; done

reset() { for f in "$CTA" "$TA" "$CM"; do cp "$f.orig" "$f"; done; }
# case_of <description> <want: same|changed> <live file> <jq program>:
# snapshots, rewrites the live object with the program, snapshots again.
case_of() {
    local what="$1" want="$2" got
    reset
    rv_snapshot "$WORK/before.txt" "$WORK/objects.txt"
    jq "$4" "$3" > "$3.new" && mv "$3.new" "$3"
    rv_snapshot "$WORK/after.txt" "$WORK/objects.txt"
    if [ "$(grep -c . "$WORK/before.txt")" != 4 ]; then
        fail "$what: the snapshot has $(grep -c . "$WORK/before.txt") lines, want 4"
        return
    fi
    if diff -q "$WORK/before.txt" "$WORK/after.txt" >/dev/null; then got=same; else got=changed; fi
    if [ "$got" = "$want" ]; then
        ok "$what: $got"
    else
        fail "$what: $got, want $want"
        diff "$WORK/before.txt" "$WORK/after.txt" | sed 's/^/       /'
    fi
}

reset
rv_snapshot "$WORK/before.txt" "$WORK/objects.txt"
if grep -qE '^ClusterTriggerAuthentication - scaling-manager-external-scaler-llm-d-sim spec:[0-9a-f]{64}$' "$WORK/before.txt" \
    && grep -qE '^TriggerAuthentication llm-d-sim scaling-manager-external-scaler spec:[0-9a-f]{64}$' "$WORK/before.txt" \
    && grep -qx 'ConfigMap llm-d-sim scaling-manager-settings 1916' "$WORK/before.txt" \
    && grep -qx 'Secret llm-d-sim scaling-manager-prometheus-ca absent' "$WORK/before.txt" \
    && [ "$(awk '/ spec:/ {print $4}' "$WORK/before.txt" | sort -u | wc -l)" = 2 ]; then
    ok "snapshot lines: both authentications by distinct spec digests, the ConfigMap by resourceVersion, a missing Secret absent"
else
    fail "snapshot lines are not as expected:"
    sed 's/^/       /' "$WORK/before.txt"
fi

STATUS_WRITE='.metadata.managedFields += [{"manager":"keda","operation":"Update","subresource":"status","time":"2026-11-17T06:05:17Z"}]'
case_of "nothing written" same "$CTA" '.'
case_of "KEDA status write to the CTA (status, resourceVersion, managedFields move)" same "$CTA" \
    ".status.scaledobjects = \"ext-scaler-so\" | .metadata.resourceVersion = \"3473\" | $STATUS_WRITE"
case_of "KEDA status write that empties the CTA status" same "$CTA" \
    'del(.status) | .metadata.resourceVersion = "3480"'
case_of "KEDA status write to a namespaced TriggerAuthentication" same "$TA" \
    ".status.scaledobjects = \"\" | .metadata.resourceVersion = \"3474\" | $STATUS_WRITE"
case_of "install rewrites the CTA spec" changed "$CTA" \
    '.spec.secretTargetRef[0].key = "tls.crt" | .metadata.resourceVersion = "3473" | .metadata.generation = 2'
case_of "install rewrites the CTA spec without a generation bump" changed "$CTA" \
    '.spec.secretTargetRef[0].name = "other" | .metadata.resourceVersion = "3473"'
case_of "install adds a label to the CTA" changed "$CTA" \
    '.metadata.labels.extra = "x" | .metadata.resourceVersion = "3473"'
case_of "install adds an annotation to the CTA" changed "$CTA" \
    '.metadata.annotations = {"a":"b"} | .metadata.resourceVersion = "3473"'
case_of "the finalizers of the CTA change" changed "$CTA" \
    '.metadata.finalizers = [] | .metadata.resourceVersion = "3473"'
case_of "install adds an owner reference to the CTA" changed "$CTA" \
    '.metadata.ownerReferences = [{"apiVersion":"v1","kind":"ConfigMap","name":"x","uid":"u"}] | .metadata.resourceVersion = "3473"'
case_of "install deletes and recreates the CTA (new uid, same spec)" changed "$CTA" \
    '.metadata.uid = "5b0c6c1e-0000-4000-8000-000000000009" | .metadata.resourceVersion = "3473" | del(.status)'
case_of "install rewrites the spec of a namespaced TriggerAuthentication" changed "$TA" \
    '.spec.secretTargetRef[0].key = "tls.crt" | .metadata.resourceVersion = "3474"'
reset
rv_snapshot "$WORK/before.txt" "$WORK/objects.txt"
rm -f "$CTA"
rv_snapshot "$WORK/after.txt" "$WORK/objects.txt"
if grep -qx 'ClusterTriggerAuthentication - scaling-manager-external-scaler-llm-d-sim absent' "$WORK/after.txt" \
    && ! diff -q "$WORK/before.txt" "$WORK/after.txt" >/dev/null; then
    ok "a CTA that is gone after the install reads absent and changes the snapshot"
else
    fail "a CTA that is gone after the install does not change the snapshot"
fi
case_of "install writes another object (the ConfigMap)" changed "$CM" \
    '.data.passive = "false" | .metadata.resourceVersion = "3500"'

echo
echo "$PASS passed, $FAIL failed"
[ $((PASS + FAIL)) -ge 15 ] || { echo "FAIL: only $((PASS + FAIL)) cases ran, want 15"; exit 1; }
[ "$FAIL" -eq 0 ]
