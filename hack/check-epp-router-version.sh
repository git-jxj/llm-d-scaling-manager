#!/usr/bin/env bash
# Checks that the EPP installer refuses a router too old for its values.
#
# deploy/lib/epp-*.values.yaml carry an llm-d.ai/v1 EndpointPickerConfig, which
# router v1.0.0 introduced; an older router fails to start on it. So
# deploy/install-epp.sh must refuse LLM_D_ROUTER_VERSION below v1.0.0 before it
# touches the cluster. Runs install-epp.sh against stub kubectl and helm and
# fails if a refused version reaches either.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); echo "ok   $*"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL $*"; }

mkdir -p "$T/bin"
for tool in kubectl helm; do
    printf '#!/usr/bin/env bash\necho "%s $*" >> "%s/calls.log"\nexit 0\n' "$tool" "$T" > "$T/bin/$tool"
    chmod +x "$T/bin/$tool"
done

# run <version>: runs install-epp.sh with the stubs; sets RC and OUT.
run() {
    : > "$T/calls.log"
    OUT="$(cd "$T" && PATH="$T/bin:$PATH" LLM_D_ROUTER_VERSION="$1" NAMESPACE=check-ns \
        bash "$ROOT/deploy/install-epp.sh" 2>&1)"
    RC=$?
}

for v in v0.9.0 v0.11.0 v0.11.2 v0.99.99 main latest 1.0.0 v1.0; do
    run "$v"
    if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "LLM_D_ROUTER_VERSION=.* is not supported" \
        && printf '%s' "$OUT" | grep -q 'llm-d.ai/v1' && [ ! -s "$T/calls.log" ]; then
        ok "refuses LLM_D_ROUTER_VERSION='$v' before touching the cluster"
    else
        fail "LLM_D_ROUTER_VERSION='$v': rc=$RC, cluster calls: $(wc -l < "$T/calls.log"); output: $(printf '%s' "$OUT" | tail -2)"
    fi
done

for v in v1.0.0 v1.0.0-rc.2 v1.0.1 v1.2.0 v2.0.0; do
    run "$v"
    if [ "$RC" -eq 0 ] && grep -q -- "--version $v" "$T/calls.log"; then
        ok "accepts LLM_D_ROUTER_VERSION=$v and installs that chart"
    else
        fail "LLM_D_ROUTER_VERSION=$v: rc=$RC, helm calls: $(grep -c '^helm' "$T/calls.log"); output: $(printf '%s' "$OUT" | tail -2)"
    fi
done

# The default must itself be supported, or a bare install-epp.sh refuses.
default="$(sed -n 's/^LLM_D_ROUTER_VERSION=\${LLM_D_ROUTER_VERSION:-\(.*\)}$/\1/p' "$ROOT/deploy/install-epp.sh")"
run "$default"
if [ -n "$default" ] && [ "$RC" -eq 0 ]; then
    ok "install-epp.sh's own default ($default) is accepted"
else
    fail "install-epp.sh's own default ('$default') is refused or unreadable"
fi

echo "EPP router version: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
