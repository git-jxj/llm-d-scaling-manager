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

# refused <version> <reason-pattern>: the install stops before any cluster call,
# says why in those words, names the way forward, and links the upgrade notes.
refused() {
    run "$1"
    if [ "$RC" -ne 0 ] && [ ! -s "$T/calls.log" ] \
        && printf '%s' "$OUT" | grep -q -- "LLM_D_ROUTER_VERSION=$1 $2" \
        && printf '%s' "$OUT" | grep -q 'llm-d.ai/v1' \
        && printf '%s' "$OUT" | grep -q 'run install-epp.sh from the v0.11.1 checkout' \
        && printf '%s' "$OUT" | grep -q 'upgrade-and-uninstall.md#the-llm-d-router-moves-to-v100'; then
        ok "refuses LLM_D_ROUTER_VERSION='$1' ($2) before touching the cluster"
    else
        fail "LLM_D_ROUTER_VERSION='$1': rc=$RC, cluster calls: $(wc -l < "$T/calls.log"); output: $(printf '%s' "$OUT" | tail -2)"
    fi
}
for v in v0.9.0 v0.11.0 v0.11.2 v0.99.99 v0.11.0-rc.1; do refused "$v" "is older than v1.0.0"; done
for v in main latest 1.0.0 v1.0; do refused "$v" 'is not a release tag (vX.Y.Z)'; done

# The anchor the message links must exist.
if grep -q '^#### The llm-d router moves to v1.0.0$' "$ROOT/docs/reference/upgrade-and-uninstall.md"; then
    ok "the refusal's link target exists"
else
    fail "docs/reference/upgrade-and-uninstall.md has no '#### The llm-d router moves to v1.0.0' heading for the refusal to link"
fi

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

# Every EPP config this repo ships as llm-d.ai/v1 must reach a router that reads
# it: a benchmark scenario through its own routerEndpointPicker pin, or, with
# no pin, through the harness default patch_harness.sh fix 6 writes; the
# installer's values through the LLM_D_ROUTER_VERSION defaults above.
# llmsm_router_version_supported is the installer's own comparison.
LLMSM_MIN_ROUTER_VERSION="$(sed -n 's/^LLMSM_MIN_ROUTER_VERSION="\(.*\)"$/\1/p' "$ROOT/deploy/lib/infra_epp.sh")"
if [ "$LLMSM_MIN_ROUTER_VERSION" = "v1.0.0" ]; then
    ok "LLMSM_MIN_ROUTER_VERSION is v1.0.0, the first router that reads llm-d.ai/v1"
else
    fail "LLMSM_MIN_ROUTER_VERSION is '$LLMSM_MIN_ROUTER_VERSION', want v1.0.0"
fi
eval "$(sed -n '/^llmsm_router_version_supported() {$/,/^}$/p' "$ROOT/deploy/lib/infra_epp.sh")"
type llmsm_router_version_supported >/dev/null 2>&1 || fail "could not read llmsm_router_version_supported from infra_epp.sh"

harness_epp="$(tr -d '\r' < "$ROOT/hack/benchmark/patch_harness.sh" | sed -n 's/^ROUTER_EPP_IMAGE = "\(.*\)"$/\1/p')"
for f in "$ROOT"/hack/benchmark/scenarios/guides/*.yaml "$ROOT"/hack/benchmark/scenarios/guides/variants/*.yaml; do
    [ -f "$f" ] || continue
    body="$(tr -d '\r' < "$f" | grep -vE '^[[:space:]]*#')"
    printf '%s\n' "$body" | grep -qE '^[[:space:]]+apiVersion: llm-d\.ai/v1$' || continue
    name="${f#"$ROOT"/hack/benchmark/scenarios/guides/}"
    pin="$(printf '%s\n' "$body" | grep -A1 -E '^[[:space:]]+routerEndpointPicker:$' | sed -n 's/^[[:space:]]*tag: *\([^[:space:]]*\)$/\1/p' | head -1)"
    if [ -n "$pin" ]; then
        if llmsm_router_version_supported "$pin"; then
            ok "$name: llm-d.ai/v1 with routerEndpointPicker pinned at $pin"
        else
            fail "$name: llm-d.ai/v1 but routerEndpointPicker pinned at $pin, below $LLMSM_MIN_ROUTER_VERSION"
        fi
    elif llmsm_router_version_supported "$harness_epp"; then
        ok "$name: llm-d.ai/v1, no pin, harness default EPP is $harness_epp"
    else
        fail "$name: llm-d.ai/v1, no pin, and the harness default EPP ('$harness_epp') is below $LLMSM_MIN_ROUTER_VERSION"
    fi
done

for f in "$ROOT"/deploy/lib/epp-*.values.yaml; do
    name="${f#"$ROOT"/}"
    if ! tr -d '\r' < "$f" | grep -q 'kind: EndpointPickerConfig'; then
        continue
    fi
    if tr -d '\r' < "$f" | grep -qE '^[[:space:]]+apiVersion: llm-d\.ai/v1$'; then
        ok "$name carries llm-d.ai/v1"
    else
        fail "$name has an EndpointPickerConfig that is not llm-d.ai/v1"
    fi
done
mk_default="$(tr -d '\r' < "$ROOT/Makefile" | sed -n 's/^LLM_D_ROUTER_VERSION *?= *\([^[:space:]]*\).*/\1/p')"
if llmsm_router_version_supported "$mk_default"; then
    ok "the Makefile's LLM_D_ROUTER_VERSION ($mk_default) loads the values"
else
    fail "the Makefile's LLM_D_ROUTER_VERSION ('$mk_default') is below $LLMSM_MIN_ROUTER_VERSION"
fi

echo "EPP router version: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
