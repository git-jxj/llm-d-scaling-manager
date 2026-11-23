#!/usr/bin/env bash
#
# Renders the release files (deploy/release-manifests.sh) and asserts the
# rules of the one manifest, scaling-manager.yaml, which
# proposal PR #404 sets: applied with
# `kubectl apply --server-side -f`, again on every upgrade, so
#
#   the image by tag                    installs whatever the tag is today
#   a Secret with data                  a re-apply empties the certificates
#                                       the controller wrote
#   the settings ConfigMap, a           a re-apply resets what an operator set,
#   ScaledObject                        or scales before anyone reviewed it
#   a Secret read without resourceNames reads every Secret in reach
#   create on Secrets                   a service-account token under our name
#   a switch missing on the Deployment  the install runs as another path
#   a keda mention the guide's sed      KEDA elsewhere is half wired
#   misses
#   a placeholder                       nothing fills it on this path
#
# Then it runs the same assertions on broken copies, one fault each, and
# requires each to fail with the message of the rule it breaks.
#
# Needs yq v4, jq, sha256sum, GNU tar and kustomize (or kubectl); reads no
# cluster.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0
PASS=0
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "  FAIL: $*"; FAIL=$((FAIL + 1)); }
pass() { PASS=$((PASS + 1)); }

for tool in yq jq sha256sum; do
    command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: $tool is required (not skipping: a skipped check reads as a pass)"; exit 1; }
done

REPO="ghcr.io/llm-d-incubation/llm-d-scaling-manager"
DIGEST="sha256:$(printf 'manifest-install-check' | sha256sum | cut -c1-64)"
VERSION="v0.13.0-check.1"
M=scaling-manager.yaml
NS=scaling-manager-system
FLAGS="--namespace-opt-in=label --keda-namespace=keda --self-check-interval=1m --restart-on-settings-change=true --external-scaler-tls-bootstrap=true"

GOOD="$WORK/good"
if ! bash "$ROOT/deploy/release-manifests.sh" --image "$REPO@$DIGEST" --version "$VERSION" --out "$GOOD" > "$WORK/render.log" 2>&1; then
    echo "FAIL: the render failed:"; tail -5 "$WORK/render.log"; exit 1
fi

# keda_sed <file> <namespace>: the KEDA namespace edit as the file's own
# header gives it, with <ns> filled in, run on <file>.
keda_sed() {
    local line prog
    line="$(sed -n '1,6{s/^# KEDA in another namespace than keda: //p}' "$1")"
    [ -n "$line" ] || return 2
    prog="${line//<ns>/$2}"
    prog="${prog% $M}"
    (cd "$(dirname "$1")" && eval "$prog $(basename "$1")")
}

# check <dir>: every rule on <dir>/scaling-manager.yaml. Prints a line per
# failure; returns how many.
check() {
    local d="$1" f="$1/$M" js="$WORK/m.json" bad=0 n k
    say() { echo "  FAIL: $*"; bad=$((bad + 1)); }
    [ -s "$f" ] || { say "$M was not written"; return "$bad"; }
    grep -q "  $M\$" "$d/SHA256SUMS" || say "SHA256SUMS does not list $M"
    yq ea -o=json -I=0 '[.] | map(select(.kind != null))' "$f" > "$js" 2>"$WORK/yerr" || { say "$M does not parse: $(head -1 "$WORK/yerr")"; return "$bad"; }
    q() { jq -r --arg img "$REPO@$DIGEST" --arg v "$VERSION" --arg ns "$NS" "$1" "$js"; }

    [ "$(q '[.[] | select(.kind == "Namespace")] | map(.metadata.name) | join(",")')" = "$NS" ] \
        || say "$M must hold exactly one Namespace, $NS; holds: $(q '[.[] | select(.kind == "Namespace") | .metadata.name] | join(",")')"
    [ "$(q '.[0].kind + "/" + .[0].metadata.name')" = "Namespace/$NS" ] || say "the Namespace is not the first object, so the apply of the rest can fail on a fresh cluster"
    n="$(q '.[] | select(.kind == "Deployment" or .kind == "Job") | .spec.template.spec.containers[] | select(.image != $img) | .image')"
    [ -z "$n" ] && [ "$(q '[.[] | select(.kind == "Deployment")] | length')" = 1 ] || say "the controller image is not pinned to $REPO@$DIGEST, or not one Deployment: $n"
    grep -qE 'llm-d-scaling-manager:[A-Za-z0-9]' "$f" && say "$M names the image by tag"
    n="$(q '.[] | select((.kind == "ConfigMap" and (.metadata.name | test("settings|policy|manager-config"))) or .kind == "ScaledObject") | .kind + "/" + .metadata.name')"
    [ -z "$n" ] || say "$M holds what an operator owns (settings, policy, a ScaledObject): $n"
    n="$(q '.[] | select(.kind == "Secret" and .type != "kubernetes.io/service-account-token") | select(.type != "Opaque" or has("data") or has("stringData")) | .metadata.namespace + "/" + .metadata.name + " " + (.type // "-")')"
    [ -z "$n" ] || say "a Secret that is not Opaque with no data, which a re-apply would empty: $n"
    for k in scaling-manager-external-scaler-ca scaling-manager-external-scaler-tls scaling-manager-external-scaler-client-tls; do
        [ "$(jq -r --arg k "$k" --arg ns "$NS" '[.[] | select(.kind == "Secret" and .metadata.name == $k and .metadata.namespace == $ns)] | length' "$js")" = 1 ] \
            || say "the scaler TLS Secret $k is not in $NS"
    done
    [ "$(jq -r '[.[] | select(.kind == "Secret" and .metadata.namespace == "keda" and .metadata.name == "scaling-manager-external-scaler-scaling-manager-system")] | length' "$js")" = 1 ] \
        || say "KEDA's copy is not pre-created in namespace keda"
    n="$(q '.[] | select(.kind == "Role" and .metadata.namespace == "keda") | .rules[] | select((.resources != ["secrets"]) or (.resourceNames != ["scaling-manager-external-scaler-scaling-manager-system"]) or ((.verbs | sort) != ["get", "update"])) | tostring')"
    [ -z "$n" ] || say "the Role in KEDA's namespace grants more than get and update on KEDA's copy by name: $n"
    n="$(q '.[] | select(.kind == "Role" or .kind == "ClusterRole") | select(any(.rules[]?; ((.resources // []) | (index("secrets") or index("*"))) and ((.verbs // []) | (index("get") or index("list") or index("watch") or index("*"))) and ((.resourceNames // []) | length) == 0)) | .kind + "/" + .metadata.name')"
    [ -z "$n" ] || say "reads Secrets with no resourceNames: $n"
    n="$(q '.[] | select(.kind == "Role" or .kind == "ClusterRole") | select(any(.rules[]?; ((.resources // []) | (index("secrets") or index("*"))) and ((.verbs // []) | (index("create") or index("*"))))) | .kind + "/" + .metadata.name')"
    [ -z "$n" ] || say "grants create on Secrets: $n"
    for k in $FLAGS; do
        [ "$(jq -r --arg k "$k" '[.[] | select(.kind == "Deployment") | .spec.template.spec.containers[] | select(.name == "manager") | .args[] | select(. == $k)] | length' "$js")" = 1 ] \
            || say "the Deployment does not pass $k"
    done
    n="$(q '.[] | select(.metadata.labels["app.kubernetes.io/part-of"] != "scaling-manager" or .metadata.labels["app.kubernetes.io/instance"] != $ns or .metadata.labels["app.kubernetes.io/version"] != $v) | .kind + "/" + .metadata.name')"
    [ -z "$n" ] || say "objects missing part-of, instance $NS or version (the uninstall selects by them): $n"
    n="$(q '.[] | select(.metadata.namespace != null and .metadata.namespace != $ns and .metadata.namespace != "keda") | .kind + "/" + .metadata.namespace + "/" + .metadata.name')"
    [ -z "$n" ] || say "an object outside $NS and KEDA's namespace: $n"
    grep -nE '__[A-Z_]+__' "$f" >/dev/null && say "$M holds a placeholder nothing fills: $(grep -oE '__[A-Z_]+__' "$f" | sort -u | tr '\n' ' ')"

    # KEDA elsewhere: the header's sed must reach every mention of keda.
    local moved="$WORK/moved"
    rm -rf "$moved"; mkdir -p "$moved"; cp "$f" "$moved/$M"
    if ! keda_sed "$moved/$M" keda-system 2>"$WORK/sederr"; then
        say "the KEDA namespace sed in the header did not run: $(head -1 "$WORK/sederr")"
    else
        n="$(grep -nE '(^|[ :=/])keda$' "$moved/$M" | grep -v '^[0-9]*:#' || true)"
        [ -z "$n" ] || say "after the KEDA namespace sed, keda is still named: $(echo "$n" | head -3 | tr '\n' ' ')"
        local before after
        before="$(grep -cE '(^|[ :=/])keda$' "$f")"
        after="$(grep -cE '(^|[ :=/])keda-system$' "$moved/$M")"
        [ "$before" -ge 4 ] && [ "$before" = "$after" ] || say "the KEDA namespace sed moved $after of $before mentions of keda"
    fi
    return "$bad"
}

if check "$GOOD" > "$WORK/good.log" 2>&1; then
    pass
    echo "  ok: $M: $(jq 'length' "$WORK/m.json") objects, every rule holds"
else
    cat "$WORK/good.log"
    fail "the render breaks the rules above"
fi

# A server-side re-apply keeps what the controller wrote only while the file
# holds no data for it: the same render twice is the same bytes.
if bash "$ROOT/deploy/release-manifests.sh" --image "$REPO@$DIGEST" --version "$VERSION" --out "$WORK/again" >/dev/null 2>&1 \
    && cmp -s "$GOOD/$M" "$WORK/again/$M"; then
    pass
else
    fail "a second render of $M differs"
fi

# Negative controls: one fault per copy; each must fail for its own rule.
mutate() {
    local name="$1" expect="$2" expr="$3" d="$WORK/m-$1"
    rm -rf "$d"; cp -r "$GOOD" "$d"
    case "$expr" in
        append-settings)
            printf -- '---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: scaling-manager-settings\n  namespace: %s\ndata:\n  passive: "true"\n' "$NS" >> "$d/$M" ;;
        *) yq -i "$expr" "$d/$M" ;;
    esac
    (cd "$d" && sha256sum scaling-manager*.yaml > SHA256SUMS)
    if check "$d" > "$WORK/m-$name.log" 2>&1; then
        fail "negative control '$name' was not caught"
    elif grep -q -- "$expect" "$WORK/m-$name.log"; then
        pass
        echo "  ok: caught '$name': $(grep -m1 -- "$expect" "$WORK/m-$name.log" | sed 's/^ *FAIL: //' | cut -c1-160)"
    else
        fail "negative control '$name' failed, but not for '$expect': $(grep -m1 FAIL "$WORK/m-$name.log")"
    fi
}
mutate image-by-tag "not pinned" \
    '(select(.kind == "Deployment") | .spec.template.spec.containers[0].image) = "ghcr.io/llm-d-incubation/llm-d-scaling-manager:main"'
mutate tls-secret-data "Opaque with no data" \
    '(select(.kind == "Secret" and .metadata.name == "scaling-manager-external-scaler-ca") | .data) = {"tls.crt": ""}'
mutate keda-copy-typed "Opaque with no data" \
    '(select(.kind == "Secret" and .metadata.namespace == "keda") | .type) = "kubernetes.io/tls"'
mutate settings-in-file "what an operator owns" append-settings
mutate no-opt-in "does not pass --namespace-opt-in=label" \
    '(select(.kind == "Deployment") | .spec.template.spec.containers[0].args) |= map(select(. != "--namespace-opt-in=label"))'
mutate no-keda-copy "does not pass --keda-namespace=keda" \
    '(select(.kind == "Deployment") | .spec.template.spec.containers[0].args) |= map(select(. != "--keda-namespace=keda"))'
mutate keda-role-wider "more than get and update" \
    '(select(.kind == "Role" and .metadata.namespace == "keda") | .rules[0].verbs) += ["list"]'
mutate secret-read-unscoped "no resourceNames" \
    '(select(.kind == "Role" and .metadata.name == "scaling-manager-scaler-tls-role") | .rules[0]) |= del(.resourceNames)'
mutate keda-unmoved "keda is still named" \
    '(select(.kind == "Deployment") | .spec.template.spec.containers[0].env) += [{"name": "KEDA_NS", "value": "keda"}]'
mutate placeholder-left "placeholder" \
    '(select(.kind == "Role" and .metadata.namespace == "keda") | .rules[0].resourceNames) = ["scaling-manager-external-scaler-__NAMESPACE__"]'
mutate unlabelled "missing part-of" \
    'del(select(.kind == "ClusterTriggerAuthentication") | .metadata.labels["app.kubernetes.io/instance"])'

echo ""
echo "manifest install: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
