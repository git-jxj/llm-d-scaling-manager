#!/usr/bin/env bash
#
# Runs smt admin-setup, install, admin-finish and release-files
# (deploy/lib/release_install.sh, through deploy/scaling-manager-tools.sh)
# against rendered release files and a stateful stub kubectl
# (hack/testdata/release-install/kubectl), and asserts what
# docs/proposals/two-command-install.md says they do: the exit codes, that
# --dry-run is the release files with the substitutions made, that files which
# fail their SHA256SUMS or name another release are refused before anything is
# applied, that a FAIL stops the install before the controller, that existing
# settings are never written, and that the KEDA side written by
# llmsm_scaler_keda_auth_apply is scaling-manager-keda-auth.yaml of the release.
# Also runs hack/compare-baked-release-files.sh, the check of the release workflow
# that the files of the image are the uploaded ones, on fixtures.
#
# Offline: kubectl is the stub; the render uses kustomize (KUSTOMIZE).

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
for tool in jq yq sha256sum openssl; do
    command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: $tool is required (not skipping: a skipped check reads as a pass)"; exit 1; }
done

W="$(mktemp -d)"
trap 'rm -rf "$W"' EXIT
PASS=0
FAIL=0
ok()  { printf '  ok   %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf '  FAIL %s\n' "$1"; FAIL=$((FAIL + 1)); }
# expect <label> <want exit> <got exit>
expect_rc() { if [ "$3" = "$2" ]; then ok "$1 (exit $3)"; else bad "$1: exit $3, want $2: $(tail -3 "$W/out" | tr '\n' ' ')"; fi; }

V=v9.9.9-check
IMG="ghcr.io/llm-d-incubation/llm-d-scaling-manager@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
echo "Rendering the release files ($V)"
bash "$ROOT/deploy/release-manifests.sh" --image "$IMG" --version "$V" --out "$W/dist" > "$W/render.log" 2>&1 \
    || { cat "$W/render.log"; echo "FAIL: the release files did not render"; exit 1; }
# Signature bundles: the release signs each file; any bytes stand in here.
for f in "$W"/dist/*.yaml "$W"/dist/scaling-manager-tools-*.tar.gz "$W/dist/SHA256SUMS"; do
    printf '{"bundle":"%s"}\n' "${f##*/}" > "$f.sigstore.json"
done

# The layout of the image: the entry point, lib/, VERSION and the baked files, as
# Dockerfile.tools lays them out.
IMGDIR="$W/image"
mkdir -p "$IMGDIR/release"
cp "$ROOT/deploy/scaling-manager-tools.sh" "$ROOT/deploy/weights.sh" "$IMGDIR/"
cp -R "$ROOT/deploy/lib" "$IMGDIR/lib"
printf '%s\n' "$V" > "$IMGDIR/VERSION"
for f in "$W"/dist/*; do
    case "${f##*/}" in scaling-manager-tools-*) ;; *) cp "$f" "$IMGDIR/release/" ;; esac
done
# A development image: no baked files.
DEVDIR="$W/devimage"
mkdir -p "$DEVDIR"
cp "$ROOT/deploy/scaling-manager-tools.sh" "$ROOT/deploy/weights.sh" "$DEVDIR/"
cp -R "$ROOT/deploy/lib" "$DEVDIR/lib"
printf '%s\n' "$V" > "$DEVDIR/VERSION"

mkdir -p "$W/bin"
cp "$ROOT/hack/testdata/release-install/kubectl" "$W/bin/kubectl"
chmod 0755 "$W/bin/kubectl"
export STUB_DIR="$W/stub"

# reset: an empty cluster with namespace team-a and the KEDA CRDs.
reset() {
    rm -rf "$STUB_DIR"
    mkdir -p "$STUB_DIR/obj/namespace/_"
    for n in team-a keda-system "$@"; do
        printf '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"%s"}}' "$n" > "$STUB_DIR/obj/namespace/_/$n.json"
    done
    : > "$STUB_DIR/calls.log"
    printf 'PASS    settings  ok\n1 PASS, 0 WARN, 0 FAIL, 0 UNKNOWN\n' > "$STUB_DIR/preflight.log"
    echo Complete > "$STUB_DIR/preflight.end"
}
# smt <image dir> <args...>: the entry point of that layout, the stub first on PATH.
smt_in() {
    local d="$1"
    shift
    (cd "$W/work" && PATH="$W/bin:$PATH" SMT_POLL=0 bash "$d/scaling-manager-tools.sh" "$@") > "$W/out" 2>&1
}
smt() { smt_in "$IMGDIR" "$@"; }
obj() { cat "$STUB_DIR/obj/$1/$2/$3.json" 2>/dev/null; }
has_obj() { [ -f "$STUB_DIR/obj/$1/$2/$3.json" ]; }
writes() { grep -cE '^(apply|create)( |$)' "$STUB_DIR/calls.log" || true; }
mkdir -p "$W/work"

# norm <files...>: every object as one sorted, compact JSON line (comments,
# key order and formatting do not count).
norm() {
    yq ea -o json -I 0 'select(.kind != null)' "$@" | jq -c -S '.' | sort
}
# sedded <file> <ns> <model ns> [keda ns]: a release file as the sed of the guide makes it.
sedded() {
    local out="$W/sed-${1}-$2-$3"
    sed -e "s/__NAMESPACE__/$2/g" -e "s/__MODEL_NAMESPACE__/$3/g" "$W/dist/$1" > "$out"
    [ -z "${4:-}" ] || sed -i "s#kubernetes.io/metadata.name: keda\$#kubernetes.io/metadata.name: $4#" "$out"
    printf '%s\n' "$out"
}

# ---------------------------------------------------------------------------
echo "Usage and exit codes"
reset
smt install; expect_rc "install without -n is usage" 2 $?
smt install -n Bad_NS; expect_rc "a namespace that is not a name is usage" 2 $?
smt install -n team-a --bogus x; expect_rc "an unknown flag is usage" 2 $?
smt admin-setup -n team-a --scope cluster
rc=$?
if [ "$rc" = 2 ] && grep -q 'smt install --scope cluster' "$W/out"; then ok "admin-setup --scope cluster is refused, pointing at install --scope cluster"; else bad "admin-setup --scope cluster: exit $rc: $(head -1 "$W/out")"; fi
smt install -n team-a --dry-run --out "$W/x" --prometheus-url https://p:9090 --set noSuchKey=1; expect_rc "--set of a key the settings do not have is usage" 2 $?
smt install -n team-a --dry-run; expect_rc "--dry-run without --out is usage" 2 $?
smt release-files; expect_rc "release-files without --out is usage" 2 $?

# ---------------------------------------------------------------------------
echo "The release files"
reset
smt_in "$DEVDIR" install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 2 ] && grep -q 'carries no release files' "$W/out" && [ ! -s "$STUB_DIR/calls.log" ]; then
    ok "an image without files refuses install without --files, before any kubectl call"
else
    bad "development image: exit $rc, calls: $(wc -l < "$STUB_DIR/calls.log"): $(head -1 "$W/out")"
fi
reset
cp -R "$W/dist" "$W/tampered"
sed -i 's/@sha256:0123/@sha256:9123/' "$W/tampered/scaling-manager-namespaced.yaml"
cmp -s "$W/dist/scaling-manager-namespaced.yaml" "$W/tampered/scaling-manager-namespaced.yaml" && bad "the tamper fixture changed nothing"
smt_in "$DEVDIR" admin-setup -n team-a --keda-namespace keda-system --files "$W/tampered"
rc=$?
if [ "$rc" = 2 ] && grep -q 'do not match their SHA256SUMS' "$W/out" && [ "$(writes)" = 0 ]; then
    ok "files that do not match their SHA256SUMS are refused, nothing applied"
else
    bad "SHA256SUMS mismatch: exit $rc, $(writes) writes: $(head -1 "$W/out")"
fi
reset
bash "$ROOT/deploy/release-manifests.sh" --image "$IMG" --version v9.9.8-check --out "$W/other" > /dev/null 2>&1
smt_in "$DEVDIR" install -n team-a --prometheus-url https://p:9090 --files "$W/other"
rc=$?
if [ "$rc" = 2 ] && grep -q "not $V, the release of this image" "$W/out" && [ ! -s "$STUB_DIR/calls.log" ]; then
    ok "--files of another release is refused, before any kubectl call"
else
    bad "--files of v9.9.8-check: exit $rc: $(head -1 "$W/out")"
fi
reset
smt_in "$DEVDIR" install -n team-a --dry-run --out "$W/devdry" --prometheus-url https://p:9090 --files "$W/dist"
expect_rc "--files of this release installs from a development image" 0 $?

# ---------------------------------------------------------------------------
echo "--dry-run is the release files with the substitutions made"
reset
rm -rf "$W/dry"
smt admin-setup -n team-a --keda-namespace kk --dry-run --out "$W/dry"; rc1=$?
smt install -n team-a --prometheus-url https://prom.example:9090 --quota H200=16 --dry-run --out "$W/dry"; rc2=$?
if [ "$rc1$rc2" = 00 ] && [ ! -s "$STUB_DIR/calls.log" ]; then ok "admin-setup and install --dry-run exit 0 and call no kubectl"; else bad "dry-run: exits $rc1 $rc2, kubectl calls $(wc -l < "$STUB_DIR/calls.log")"; fi
want="$(norm "$(sedded scaling-manager-namespaced.yaml team-a team-a kk)" "$(sedded scaling-manager-preflight.yaml team-a team-a)" \
    "$(sedded scaling-manager-model-namespace.yaml team-a team-a)" "$(sedded scaling-manager-scaler-tls.yaml team-a team-a)")"
got="$(norm "$W"/dry/10-admin.yaml "$W"/dry/11-preflight-admin.yaml "$W"/dry/12-model-namespace.yaml "$W"/dry/30-preflight.yaml "$W"/dry/40-scaler-tls.yaml "$W"/dry/50-controller.yaml)"
nwant="$(printf '%s\n' "$want" | grep -c .)"
if [ "$nwant" -gt 30 ] && [ "$want" = "$got" ]; then
    ok "the dry-run steps are exactly the sed-substituted release files ($nwant objects)"
else
    bad "dry-run differs from the sed-substituted files ($nwant objects wanted): $(diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") | head -4 | cut -c1-200 | tr '\n' ' ')"
fi
if [ "$(norm "$W/dry/10-admin.yaml" | jq -r '.metadata.labels["scaling-manager.llmd.ai/part"]' | sort -u)" = admin ] \
    && [ "$(norm "$W/dry/50-controller.yaml" | jq -r '.metadata.labels["scaling-manager.llmd.ai/part"]' | sort -u)" = controller ]; then
    ok "10-admin.yaml holds part=admin only, 50-controller.yaml part=controller only"
else
    bad "the admin and controller steps mix parts"
fi
s="$W/dry/20-settings.yaml"
if [ "$(yq '.data.prometheusURL + "|" + .data.limiter + "|" + .data.quotas + "|" + .data.passive' "$s")" = 'https://prom.example:9090|quota|H200=16|true' ] \
    && [ "$(yq '.data | del(.prometheusURL, .limiter, .quotas)' "$s")" = "$(yq '.data | del(.prometheusURL, .limiter, .quotas)' "$(sedded scaling-manager-settings.yaml team-a team-a)")" ] \
    && [ "$(yq '.metadata.namespace' "$s")" = team-a ]; then
    ok "20-settings.yaml is the release settings with prometheusURL, limiter quota and quotas from the flags, passive true"
else
    bad "20-settings.yaml: $(yq -o json -I 0 '.data' "$s" | cut -c1-200)"
fi
if [ "$(yq '.metadata.name + "|" + .data.kedaNamespace + "|" + .data.release' "$W/dry/19-admin-setup-stamp.yaml")" = "scaling-manager-admin-setup|kk|$V" ] \
    && yq '.data.adminObjectsSha256' "$W/dry/19-admin-setup-stamp.yaml" | grep -qxE '[0-9a-f]{64}'; then
    ok "19-admin-setup-stamp.yaml names the release, KEDA's namespace and a sha256 of the admin objects"
else
    bad "the stamp: $(yq -o json -I 0 '.data' "$W/dry/19-admin-setup-stamp.yaml")"
fi
rm -rf "$W/dryc"
smt install --scope cluster --keda-namespace kk --prometheus-url https://prom.example:9090 --model-namespace m1 --model-namespace m2 --dry-run --out "$W/dryc"
rc=$?
want="$(norm "$(sedded scaling-manager-cluster.yaml x x kk)" "$W/dist/scaling-manager-preflight-cluster.yaml" \
    "$(sedded scaling-manager-model-namespace.yaml scaling-manager-system m1)" "$(sedded scaling-manager-model-namespace.yaml scaling-manager-system m2)" \
    "$(sedded scaling-manager-scaler-tls.yaml scaling-manager-system x)")"
got="$(norm "$W"/dryc/10-admin.yaml "$W"/dryc/11-preflight-admin.yaml "$W"/dryc/12-model-namespace-m1.yaml "$W"/dryc/12-model-namespace-m2.yaml \
    "$W"/dryc/30-preflight.yaml "$W"/dryc/40-scaler-tls.yaml "$W"/dryc/50-controller.yaml)"
if [ "$rc" = 0 ] && [ "$want" = "$got" ] && [ "$(yq '.kind + "/" + .metadata.name' "$W/dryc/05-namespace.yaml")" = Namespace/scaling-manager-system ]; then
    ok "install --scope cluster --dry-run: the namespace, then the cluster files and one model-namespace file per --model-namespace ($(printf '%s\n' "$want" | grep -c .) objects)"
else
    bad "cluster dry-run: exit $rc: $(diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") | head -4 | cut -c1-200 | tr '\n' ' ')"
fi
rm -rf "$W/dryr"
smt install -n team-a --prometheus-url https://p:9090 --image-repo mirror.example.com/llmsm/controller --dry-run --out "$W/dryr"
imgs="$(yq ea '[select(.kind == "Deployment" or .kind == "Job") | .spec.template.spec.containers[].image] | unique | join(" ")' "$W/dryr/50-controller.yaml" "$W/dryr/30-preflight.yaml")"
if [ "$imgs" = "mirror.example.com/llmsm/controller@${IMG#*@}" ]; then ok "--image-repo moves the controller and the preflight Job to the mirror, keeping the digest"; else bad "--image-repo: images '$imgs'"; fi

rm -rf "$W/dryo"
smt admin-setup -n team-a --platform openshift --dry-run --out "$W/dryo"; rc1=$?
smt install -n team-a --platform openshift --dry-run --out "$W/dryo"; rc2=$?
np="$(yq ea '[select(.kind == "NetworkPolicy") | .spec.ingress[].from[]?.namespaceSelector.matchLabels["kubernetes.io/metadata.name"]] | unique | join(",")' "$W/dryo/10-admin.yaml")"
want="$(norm "$(sedded scaling-manager-namespaced-openshift.yaml team-a team-a)" | jq -c 'select(.metadata.labels["scaling-manager.llmd.ai/part"] == "controller")')"
got="$(norm "$W/dryo/50-controller.yaml")"
if [ "$rc1$rc2" = 00 ] && [ "$np" = openshift-keda ] && [ -n "$want" ] && [ "$want" = "$got" ] \
    && [ "$(yq '.data.prometheusURL' "$W/dryo/20-settings.yaml")" = https://thanos-querier.openshift-monitoring.svc.cluster.local:9091 ]; then
    ok "--platform openshift --dry-run: the -openshift files, KEDA in openshift-keda, Thanos Querier as the default URL"
else
    bad "openshift dry-run: exits $rc1 $rc2, NetworkPolicy admits '$np'"
fi
mkdir -p "$W/failbin"
REAL_YQ="$(command -v yq)"
printf '#!/bin/sh\nfor a in "$@"; do [ "$a" = -i ] && exit 1; done\nexec "%s" "$@"\n' "$REAL_YQ" > "$W/failbin/yq"
chmod 0755 "$W/failbin/yq"
rm -rf "$W/dryf"
PATH="$W/failbin:$PATH" smt admin-setup -n team-a --keda-namespace kk --dry-run --out "$W/dryf"
rc=$?
if [ "$rc" = 2 ] && grep -q 'cannot write KEDA namespace kk into scaling-manager-namespaced.yaml' "$W/out" && [ ! -s "$W/dryf/10-admin.yaml" ]; then
    ok "a render that fails exits 2 and writes no step file"
else
    bad "failed render: exit $rc: $(tail -1 "$W/out")"
fi

# ---------------------------------------------------------------------------
echo "admin-setup"
reset
rm -f "$STUB_DIR/obj/namespace/_/team-a.json"
smt admin-setup -n team-a --keda-namespace keda-system
rc=$?
if [ "$rc" = 4 ] && grep -q 'namespace team-a does not exist' "$W/out" && [ "$(writes)" = 0 ]; then ok "admin-setup in a missing namespace exits 4 and creates nothing"; else bad "missing namespace: exit $rc, $(writes) writes"; fi
reset
smt admin-setup -n team-a
rc=$?
if [ "$rc" = 4 ] && grep -q 'Give --keda-namespace' "$W/out"; then ok "no flag, no NetworkPolicy, no KEDA APIService: exit 4, asking for --keda-namespace"; else bad "no KEDA namespace: exit $rc: $(tail -1 "$W/out")"; fi
reset
echo "keda-system keda-operator-metrics-apiserver" > "$STUB_DIR/apiservice"
smt admin-setup -n team-a
rc=$?
np="$(obj networkpolicy team-a scaling-manager-external-scaler | jq -r '[.spec.ingress[].from[]?.namespaceSelector.matchLabels["kubernetes.io/metadata.name"]] | join(",")')"
parts="$(cat "$STUB_DIR"/obj/*/*/*.json | jq -r '.metadata.labels["scaling-manager.llmd.ai/part"] // "none"' | sort | uniq -c | tr -s ' ' | tr '\n' ';')"
if [ "$rc" = 0 ] && [ "$np" = keda-system ] && ! has_obj deployment team-a scaling-manager-controller-manager \
    && has_obj role team-a scaling-manager-manager-role-team-a && has_obj podmonitor team-a scaling-manager-modelserver-metrics; then
    ok "admin-setup finds KEDA from the APIService, writes it into the NetworkPolicy, applies the admin objects and the model-namespace file, no controller ($parts)"
else
    bad "admin-setup: exit $rc, NetworkPolicy admits '$np', parts $parts: $(tail -2 "$W/out" | tr '\n' ' ')"
fi
stamp="$(obj configmap team-a scaling-manager-admin-setup | jq -r '.data.kedaNamespace + "|" + .data.release')"
if [ "$stamp" = "keda-system|$V" ]; then ok "the stamp names KEDA's namespace and the release"; else bad "stamp: '$stamp'"; fi
touch "$STUB_DIR/forbid-networkpolicies"
smt admin-setup -n team-a
rc=$?
if [ "$rc" = 7 ] && grep -q 'cannot read NetworkPolicy scaling-manager-external-scaler in team-a: Error from server (Forbidden)' "$W/out"; then
    ok "a NetworkPolicy read that fails: exit 7, with the error kubectl printed"
else
    bad "NetworkPolicy read error: exit $rc: $(tail -1 "$W/out")"
fi
rm -f "$STUB_DIR/forbid-networkpolicies"
echo "keda-elsewhere keda-operator-metrics-apiserver" > "$STUB_DIR/apiservice"
smt admin-setup -n team-a
np="$(obj networkpolicy team-a scaling-manager-external-scaler | jq -r '[.spec.ingress[].from[]?.namespaceSelector.matchLabels["kubernetes.io/metadata.name"]] | join(",")')"
if [ "$np" = keda-system ]; then ok "a re-run without the flag keeps the NetworkPolicy's namespace"; else bad "re-run: NetworkPolicy now admits '$np'"; fi
smt admin-setup -n team-a --keda-namespace keda-flag
np="$(obj networkpolicy team-a scaling-manager-external-scaler | jq -r '[.spec.ingress[].from[]?.namespaceSelector.matchLabels["kubernetes.io/metadata.name"]] | join(",")')"
if [ "$np" = keda-flag ]; then ok "--keda-namespace wins over the live NetworkPolicy"; else bad "--keda-namespace: NetworkPolicy admits '$np'"; fi
smt admin-setup -n team-a --keda-namespace keda-system

# ---------------------------------------------------------------------------
echo "install"
save="$W/after-admin"
rm -rf "$save"; cp -R "$STUB_DIR" "$save"
restore() { rm -rf "$STUB_DIR"; cp -R "$save" "$STUB_DIR"; : > "$STUB_DIR/calls.log"; }

reset
smt install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 3 ] && grep -q 'smt admin-setup -n team-a' "$W/out" && [ "$(writes)" = 0 ]; then ok "install before admin-setup exits 3, prints the admin-setup line, writes nothing"; else bad "no stamp: exit $rc, $(writes) writes: $(tail -2 "$W/out" | tr '\n' ' ')"; fi
restore
jq '.data.adminObjectsSha256 = "0000"' "$STUB_DIR/obj/configmap/team-a/scaling-manager-admin-setup.json" > "$W/s.json" && mv "$W/s.json" "$STUB_DIR/obj/configmap/team-a/scaling-manager-admin-setup.json"
smt install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 3 ] && grep -q 'from another release' "$W/out" && grep -q 'smt admin-setup -n team-a --keda-namespace keda-system' "$W/out" && [ "$(writes)" = 0 ]; then
    ok "a stamp with another hash exits 3, naming KEDA's namespace in the admin-setup line"
else
    bad "edited stamp: exit $rc: $(tail -2 "$W/out" | tr '\n' ' ')"
fi
restore
smt install -n team-a
rc=$?
if [ "$rc" = 2 ] && grep -q 'no --prometheus-url' "$W/out" && [ "$(writes)" = 0 ]; then ok "no settings and no --prometheus-url: exit 2 before anything is created"; else bad "no URL: exit $rc, $(writes) writes"; fi
restore
printf 'PASS    settings  ok\nFAIL    prometheus.reachable  https://p:9090: connection refused\n        fix: check the URL\n1 PASS, 0 WARN, 1 FAIL, 0 UNKNOWN\n' > "$STUB_DIR/preflight.log"
echo Failed > "$STUB_DIR/preflight.end"
smt install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 1 ] && grep -q 'prometheus.reachable' "$W/out" && ! has_obj deployment team-a scaling-manager-controller-manager \
    && ! has_obj secret team-a scaling-manager-external-scaler-ca && ! has_obj configmap team-a scaling-manager-settings \
    && grep -q '^create -f .*settings.yaml' "$STUB_DIR/calls.log"; then
    ok "a preflight FAIL exits 1 naming the check, before the scaler Secrets and the controller, and removes the settings it created"
else
    bad "preflight FAIL: exit $rc: $(tail -2 "$W/out" | tr '\n' ' ')"
fi
# The fixed URL now reaches the settings: the failed run left none.
printf 'PASS    settings  ok\n1 PASS, 0 WARN, 0 FAIL, 0 UNKNOWN\n' > "$STUB_DIR/preflight.log"
echo Complete > "$STUB_DIR/preflight.end"
smt install -n team-a --prometheus-url https://right:9090
rc=$?
url="$(obj configmap team-a scaling-manager-settings | jq -r '.data.prometheusURL')"
if [ "$rc" = 0 ] && [ "$url" = https://right:9090 ]; then ok "a re-run with the right --prometheus-url after a FAIL installs with it"; else bad "re-run after a FAIL: exit $rc, prometheusURL '$url'"; fi
# Settings an earlier run left (or a person made) are never removed on a FAIL.
printf 'FAIL    prometheus.reachable  refused\n0 PASS, 0 WARN, 1 FAIL, 0 UNKNOWN\n' > "$STUB_DIR/preflight.log"
echo Failed > "$STUB_DIR/preflight.end"
smt install -n team-a --prometheus-url https://right:9090
rc=$?
if [ "$rc" = 1 ] && has_obj configmap team-a scaling-manager-settings; then ok "a FAIL keeps settings this run did not create"; else bad "FAIL with existing settings: exit $rc"; fi
restore
smt install -n team-a --prometheus-url https://p:9090 --set prometheusURL=https://q:9090
rc=$?
if [ "$rc" = 2 ] && grep -q 'give --prometheus-url' "$W/out"; then ok "--set of a key a flag owns is refused, naming the flag"; else bad "--set prometheusURL: exit $rc"; fi
smt install -n team-a --prometheus-url https://p:9090 --set passive=false
rc=$?
if [ "$rc" = 2 ] && grep -qF "After smt admin-finish, turn it off with:  kubectl patch configmap scaling-manager-settings -n team-a --type merge -p '{\"data\":{\"passive\":\"false\"}}'" "$W/out"; then
    ok "--set passive is refused with the exact patch to run after admin-finish"
else
    bad "--set passive: exit $rc: $(tail -1 "$W/out")"
fi
rm -f "$STUB_DIR/obj/namespace/_/team-a.json"
smt install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 4 ] && grep -q 'namespace team-a does not exist' "$W/out" && [ "$(writes)" = 0 ]; then ok "install in a missing namespace exits 4 before the stamp check"; else bad "install, missing namespace: exit $rc: $(tail -1 "$W/out")"; fi
touch "$STUB_DIR/forbid-namespaces"
smt install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 4 ] && grep -q 'it has no ServiceAccount default' "$W/out"; then ok "an owner who may not read Namespaces: a missing namespace is still exit 4"; else bad "owner, missing namespace: exit $rc: $(tail -1 "$W/out")"; fi
restore
touch "$STUB_DIR/forbid-namespaces"
printf 'PASS    settings  ok\nUNKNOWN keda.crd  cannot read CustomResourceDefinition scaledobjects.keda.sh: forbidden\n1 PASS, 0 WARN, 0 FAIL, 1 UNKNOWN\n' > "$STUB_DIR/preflight.log"
rm -f "$STUB_DIR/obj/clusterrolebinding/_/scaling-manager-preflight-team-a.json"
smt install -n team-a --prometheus-url https://p:9090 --strict
expect_rc "--strict after admin-finish (the preflight binding gone): a refused cluster read is not a stop, and an owner who may not read Namespaces installs" 0 $?
restore
printf 'PASS    settings  ok\nUNKNOWN keda.crd  cannot read CustomResourceDefinition scaledobjects.keda.sh: forbidden\n1 PASS, 0 WARN, 0 FAIL, 1 UNKNOWN\n' > "$STUB_DIR/preflight.log"
smt install -n team-a --prometheus-url https://p:9090 --strict
rc=$?
if [ "$rc" = 6 ] && ! has_obj configmap team-a scaling-manager-settings; then ok "--strict with the preflight binding there: the same refused read stops (exit 6), and the settings it created are removed"; else bad "--strict, binding present: exit $rc"; fi
restore
printf 'PASS    settings  ok\nUNKNOWN rbac.controller  no controller yet\nUNKNOWN metrics.model  no pods\n1 PASS, 0 WARN, 0 FAIL, 2 UNKNOWN\n' > "$STUB_DIR/preflight.log"
smt install -n team-a --prometheus-url https://p:9090 --strict
rc=$?
if [ "$rc" = 6 ] && grep -q 'could not check metrics.model' "$W/out" && ! has_obj deployment team-a scaling-manager-controller-manager; then
    ok "--strict stops on an UNKNOWN the controller does not answer (exit 6), naming it"
else
    bad "--strict: exit $rc: $(tail -1 "$W/out")"
fi
restore
printf 'PASS    settings  ok\nUNKNOWN rbac.controller  no controller yet\nWARN    accelerators  none labelled\n1 PASS, 1 WARN, 0 FAIL, 1 UNKNOWN\n' > "$STUB_DIR/preflight.log"
printf 'ca\n' > "$W/work/prom-ca.crt"
smt install -n team-a --prometheus-url https://p:9090 --quota H200=16 --prometheus-ca prom-ca.crt --strict
rc=$?
st="$(obj configmap team-a scaling-manager-settings | jq -r '.data.prometheusURL + "|" + .data.limiter + "|" + .data.quotas + "|" + .data.prometheusCACertPath + "|" + .data.passive')"
nsec="$(ls "$STUB_DIR/obj/secret/team-a/" 2>/dev/null | grep -c 'scaling-manager-external-scaler')"
if [ "$rc" = 0 ] && [ "$st" = "https://p:9090|quota|H200=16|/etc/scaling-manager-prometheus-ca/ca.crt|true" ] && [ "$nsec" = 3 ] \
    && has_obj deployment team-a scaling-manager-controller-manager && has_obj secret team-a scaling-manager-prometheus-ca \
    && [ "$(grep -c '^apply -f .*scaling-manager-preflight.yaml -l scaling-manager.llmd.ai/part=controller' "$STUB_DIR/calls.log")" = 2 ] \
    && ! grep -qE '^apply -f .*-l scaling-manager.llmd.ai/part=admin' "$STUB_DIR/calls.log"; then
    ok "install: settings from the flags, the CA Secret, 3 scaler Secrets, the controller, the preflight twice, no admin object (--strict passes an UNKNOWN the controller answers)"
else
    bad "install: exit $rc, settings '$st', $nsec scaler Secrets: $(tail -3 "$W/out" | tr '\n' ' ')"
fi
if grep -q 'smt admin-finish -n team-a' "$W/out" && grep -q 'WARN    accelerators' "$W/out" && grep -q 'Not checked' "$W/out"; then
    ok "install ends with the admin-finish line, the WARN again and the UNKNOWN as not checked"
else
    bad "install summary: $(tail -6 "$W/out" | tr '\n' ' ')"
fi
before="$(obj configmap team-a scaling-manager-settings)"
: > "$STUB_DIR/calls.log"
smt install -n team-a --prometheus-url https://other:9090 --quota H200=16 --prometheus-ca prom-ca.crt
rc=$?
if [ "$rc" = 0 ] && [ "$(obj configmap team-a scaling-manager-settings)" = "$before" ] \
    && grep -qF "kubectl patch configmap scaling-manager-settings -n team-a --type merge -p '{\"data\":{\"prometheusURL\":\"https://other:9090\"}}'" "$W/out" \
    && ! grep -qE '^create ' "$STUB_DIR/calls.log" && ! grep -q 'secret/scaling-manager-prometheus-ca' "$STUB_DIR/calls.log"; then
    ok "a re-run with another --prometheus-url leaves the settings as they were, prints the patch, creates nothing"
else
    bad "re-run: exit $rc: $(grep -m1 'kubectl patch' "$W/out")"
fi
jq '.metadata.labels = {"app.kubernetes.io/name": "scaling-manager"}' "$STUB_DIR/obj/deployment/team-a/scaling-manager-controller-manager.json" > "$W/d.json"
cp "$STUB_DIR/obj/deployment/team-a/scaling-manager-controller-manager.json" "$W/d.orig.json"
mv "$W/d.json" "$STUB_DIR/obj/deployment/team-a/scaling-manager-controller-manager.json"
smt install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 4 ] && grep -q 'not installed from the release files' "$W/out"; then ok "a live controller not from the release files (make) is not adopted: exit 4"; else bad "make-installed controller: exit $rc"; fi
mv "$W/d.orig.json" "$STUB_DIR/obj/deployment/team-a/scaling-manager-controller-manager.json"
touch "$STUB_DIR/rollout-timeout"
smt install -n team-a --prometheus-url https://p:9090
expect_rc "a rollout that times out exits 5" 5 $?
rm -f "$STUB_DIR/rollout-timeout"
touch "$STUB_DIR/unreachable"
smt install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 7 ] && grep -q 'kubectl api-versions failed: The connection to the server' "$W/out"; then ok "an unreachable API server exits 7, naming the call"; else bad "unreachable: exit $rc: $(tail -1 "$W/out")"; fi
rm -f "$STUB_DIR/unreachable"
echo none > "$STUB_DIR/preflight.end"
SMT_PREFLIGHT_TIMEOUT=0 smt install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 5 ] && grep -q 'the preflight Job did not finish within 0s' "$W/out"; then ok "a preflight Job that never ends: exit 5"; else bad "preflight timeout: exit $rc: $(tail -1 "$W/out")"; fi
echo Complete > "$STUB_DIR/preflight.end"
touch "$STUB_DIR/job-vanishes"
smt install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 7 ] && grep -q 'the preflight Job disappeared while it ran' "$W/out"; then ok "a preflight Job that disappears: exit 7"; else bad "vanished Job: exit $rc: $(tail -1 "$W/out")"; fi
rm -f "$STUB_DIR/job-vanishes"
QV="team-x'quoted"
smt install -n team-a --prometheus-url https://p:9090 --set "kueueNamespace=$QV"
rc=$?
cmd="$(grep -m1 'kubectl patch configmap scaling-manager-settings -n team-a --type merge -p .*kueueNamespace' "$W/out" | sed 's/^ *//')"
arg="$(bash -c "kubectl() { printf '%s' \"\$9\"; }; $cmd" 2>/dev/null)"
if [ "$rc" = 0 ] && [ -n "$cmd" ] && [ "$(printf '%s' "$arg" | jq -r .data.kueueNamespace 2>/dev/null)" = "$QV" ]; then
    ok "the printed patch for a value with a single quote runs as printed and sets that value"
else
    bad "patch with a quote: exit $rc, printed [$cmd], -p [$arg]"
fi
installed="$W/after-install"
rm -rf "$installed"; cp -R "$STUB_DIR" "$installed"

# ---------------------------------------------------------------------------
echo "admin-finish"
restore_installed() { rm -rf "$STUB_DIR"; cp -R "$installed" "$STUB_DIR"; : > "$STUB_DIR/calls.log"; printf 'PASS    settings  ok\n1 PASS, 0 WARN, 0 FAIL, 0 UNKNOWN\n' > "$STUB_DIR/preflight.log"; echo Complete > "$STUB_DIR/preflight.end"; }
restore_installed
SMT_CERT_TIMEOUT=0 smt admin-finish -n team-a
expect_rc "admin-finish before the controller filled the client certificate times out" 5 $?
# What the controller does: fill the client certificate, report mutual TLS
# once restarted.
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$W/k.pem" -out "$W/c.pem" -days 2 -subj /CN=scaling-manager-keda-client \
    -addext subjectAltName=DNS:scaling-manager-keda-client >/dev/null 2>&1
fill_cert() {
    jq --arg c "$(base64 < "$1" | tr -d '\n')" --arg k "$(base64 < "$W/k.pem" | tr -d '\n')" \
        '.data = {"tls.crt": $c, "tls.key": $k, "ca.crt": $c}' "$STUB_DIR/obj/secret/team-a/scaling-manager-external-scaler-client-tls.json" > "$W/sec.json"
    mv "$W/sec.json" "$STUB_DIR/obj/secret/team-a/scaling-manager-external-scaler-client-tls.json"
}
fill_cert "$W/c.pem"
printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"scaling-manager-controller-status","namespace":"team-a"},"data":{"scalerTLS":"mutual TLS (client CA sha256 ab)"}}' \
    > "$STUB_DIR/obj/configmap/team-a/scaling-manager-controller-status.json"
filled="$W/filled"
rm -rf "$filled"; cp -R "$STUB_DIR" "$filled"
jq '.data.kedaNamespace = "keda-other"' "$STUB_DIR/obj/configmap/team-a/scaling-manager-admin-setup.json" > "$W/s.json" && mv "$W/s.json" "$STUB_DIR/obj/configmap/team-a/scaling-manager-admin-setup.json"
smt admin-finish -n team-a
rc=$?
if [ "$rc" = 4 ] && grep -q "says KEDA runs in 'keda-other'" "$W/out" && [ "$(writes)" = 0 ]; then ok "a stamp and a NetworkPolicy that disagree on KEDA's namespace: exit 4, nothing written"; else bad "disagreement: exit $rc: $(tail -1 "$W/out")"; fi
rm -rf "$STUB_DIR"; cp -R "$filled" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
smt admin-finish -n team-a
rc=$?
cta="scaling-manager-external-scaler-team-a"
if [ "$rc" = 0 ] && has_obj clustertriggerauthentication _ "$cta" && has_obj secret keda-system "$cta" \
    && has_obj configmap team-a scaling-manager-scaler-keda-auth && [ "$(grep -c '^rollout restart' "$STUB_DIR/calls.log")" = 1 ] \
    && ! has_obj clusterrolebinding _ scaling-manager-preflight-team-a && grep -q 'scalerTLS: mutual TLS' "$W/out"; then
    ok "admin-finish: KEDA's side written by llmsm_scaler_keda_auth_apply, one controller restart, mutual TLS, the preflight binding removed"
else
    bad "admin-finish: exit $rc, restarts $(grep -c '^rollout restart' "$STUB_DIR/calls.log"): $(tail -3 "$W/out" | tr '\n' ' ')"
fi
finished="$W/finished"
rm -rf "$finished"; cp -R "$STUB_DIR" "$finished"
: > "$STUB_DIR/calls.log"
smt admin-finish -n team-a
rc=$?
if [ "$rc" = 0 ] && [ "$(grep -c '^rollout restart' "$STUB_DIR/calls.log")" = 0 ] && ! grep -qE '^apply .*--server-side' "$STUB_DIR/calls.log"; then
    ok "a second admin-finish writes nothing of KEDA's side and restarts nothing"
else
    bad "second admin-finish: exit $rc, restarts $(grep -c '^rollout restart' "$STUB_DIR/calls.log")"
fi
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$W/k.pem" -out "$W/c2.pem" -days 2 -subj /CN=scaling-manager-keda-client >/dev/null 2>&1
fill_cert "$W/c2.pem"
: > "$STUB_DIR/calls.log"
smt admin-finish -n team-a
rc=$?
if [ "$rc" = 0 ] && [ "$(grep -c '^rollout restart' "$STUB_DIR/calls.log")" = 1 ]; then ok "a renewed client certificate is copied again, and the controller restarted once"; else bad "renewed certificate: exit $rc, restarts $(grep -c '^rollout restart' "$STUB_DIR/calls.log")"; fi
rm -rf "$STUB_DIR"; cp -R "$filled" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
touch "$STUB_DIR/obj/clusterrolebinding/_/scaling-manager-preflight-team-a.json" 2>/dev/null || { mkdir -p "$STUB_DIR/obj/clusterrolebinding/_"; echo '{}' > "$STUB_DIR/obj/clusterrolebinding/_/scaling-manager-preflight-team-a.json"; }
printf 'FAIL    keda.mtls  KEDA presents no certificate\n0 PASS, 0 WARN, 1 FAIL, 0 UNKNOWN\n' > "$STUB_DIR/preflight.log"
echo Failed > "$STUB_DIR/preflight.end"
smt admin-finish -n team-a
rc=$?
if [ "$rc" = 1 ] && has_obj clusterrolebinding _ scaling-manager-preflight-team-a && grep -q 'kubectl delete clusterrolebinding scaling-manager-preflight-team-a' "$W/out"; then
    ok "a FAIL in the last preflight keeps the preflight binding, prints its delete, exits 1"
else
    bad "admin-finish with a FAIL: exit $rc"
fi
# Refusals inside llmsm_scaler_keda_auth_apply are exit 4 with a fix the tools
# image can follow, never 1 (a preflight FAIL); a write the API server
# refuses is exit 7.
rm -rf "$STUB_DIR"; cp -R "$filled" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
mkdir -p "$STUB_DIR/obj/secret/keda-system"
printf '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"%s","namespace":"keda-system"},"data":{}}' "$cta" > "$STUB_DIR/obj/secret/keda-system/$cta.json"
smt admin-finish -n team-a
rc=$?
if [ "$rc" = 4 ] && grep -qF "kubectl delete secret $cta -n keda-system  then run smt admin-finish -n team-a again" "$W/out" \
    && ! grep -q 'LLMSM_SCALER_TLS=false' "$W/out" && ! has_obj clustertriggerauthentication _ "$cta"; then
    ok "a foreign Secret where the copy goes: exit 4, the exact delete and the re-run, no LLMSM_SCALER_TLS advice, nothing written"
else
    bad "foreign copy: exit $rc: $(grep -m1 ERROR "$W/out")"
fi
rm -rf "$STUB_DIR"; cp -R "$filled" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"scaling-manager-scaler-keda-auth","namespace":"team-a"},"data":{}}' > "$STUB_DIR/obj/configmap/team-a/scaling-manager-scaler-keda-auth.json"
smt admin-finish -n team-a
rc=$?
if [ "$rc" = 4 ] && grep -qF "kubectl delete configmap scaling-manager-scaler-keda-auth -n team-a" "$W/out"; then ok "a foreign marker ConfigMap: exit 4 with its delete"; else bad "foreign marker: exit $rc: $(grep -m1 ERROR "$W/out")"; fi
rm -rf "$STUB_DIR"; cp -R "$filled" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
touch "$STUB_DIR/apply-fails"
smt admin-finish -n team-a
rc=$?
if [ "$rc" = 7 ] && grep -q 'Could not write' "$W/out"; then ok "a write of KEDA's side the API server refuses: exit 7"; else bad "refused write: exit $rc: $(grep -m1 ERROR "$W/out")"; fi
rm -rf "$STUB_DIR"; cp -R "$save" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
SMT_CERT_TIMEOUT=180 smt admin-finish -n team-a
rc=$?
if [ "$rc" = 4 ] && grep -q 'has not run smt install -n team-a yet' "$W/out"; then ok "admin-finish before smt install: exit 4 at once, naming the step"; else bad "admin-finish before install: exit $rc: $(tail -1 "$W/out")"; fi
rm -rf "$STUB_DIR"; cp -R "$filled" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
jq '.data.scalerTLS = "plaintext"' "$STUB_DIR/obj/configmap/team-a/scaling-manager-controller-status.json" > "$W/st.json" && mv "$W/st.json" "$STUB_DIR/obj/configmap/team-a/scaling-manager-controller-status.json"
# SMT_POLL=2 against a 5 s budget: reads at 0, 2, 4 and 6 s, then the timeout.
: > "$STUB_DIR/calls.log"
(cd "$W/work" && PATH="$W/bin:$PATH" SMT_POLL=2 SMT_MTLS_TIMEOUT=5 bash "$IMGDIR/scaling-manager-tools.sh" admin-finish -n team-a) > "$W/out" 2>&1
rc=$?
reads="$(grep -c '^get configmap scaling-manager-controller-status -n team-a' "$STUB_DIR/calls.log")"
if [ "$rc" = 5 ] && grep -q "scalerTLS is 'plaintext', not mutual TLS" "$W/out" && [ "$reads" = 4 ]; then
    ok "scalerTLS never mutual TLS: exit 5, the budget counted in SMT_POLL steps ($reads reads)"
else
    bad "mTLS timeout: exit $rc, $reads reads: $(tail -1 "$W/out")"
fi
# KEDA still holds a connection from before a reinstall: the controller
# refused its old certificate, and the refused address is a KEDA pod.
stale_setup() {
    rm -rf "$STUB_DIR"; cp -R "$filled" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
    printf '%s\n' '{"level":"info","msg":"refused a TLS handshake","error":"tls: failed to verify certificate: x509: certificate signed by unknown authority","remote":"10.0.0.5:41234"}' > "$STUB_DIR/controller.log"
    mkdir -p "$STUB_DIR/obj/pod/keda-system"
    printf '{"apiVersion":"v1","kind":"Pod","metadata":{"name":"keda-operator-1","namespace":"keda-system"},"status":{"podIP":"10.0.0.5"}}' > "$STUB_DIR/obj/pod/keda-system/keda-operator-1.json"
}
stale_setup
smt admin-finish -n team-a
rc=$?
if [ "$rc" = 0 ] && grep -q 'still presents an OLD client certificate' "$W/out" && grep -q 'run this again with --restart-keda' "$W/out" \
    && ! grep -q '^rollout restart deployment/keda-operator' "$STUB_DIR/calls.log"; then
    ok "a stale KEDA connection is named with the restart, and KEDA is not restarted unasked"
else
    bad "stale connection: exit $rc: $(grep -m1 -i 'keda' "$W/out")"
fi
stale_setup
smt admin-finish -n team-a --restart-keda
rc=$?
if [ "$rc" = 0 ] && grep -q '^rollout restart deployment/keda-operator -n keda-system' "$STUB_DIR/calls.log"; then ok "--restart-keda restarts the KEDA operator on a stale connection"; else bad "--restart-keda: exit $rc"; fi
rm -rf "$STUB_DIR"; cp -R "$filled" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
printf 'PASS    settings  ok\nUNKNOWN metrics.model  no pods\n1 PASS, 0 WARN, 0 FAIL, 1 UNKNOWN\n' > "$STUB_DIR/preflight.log"
smt admin-finish -n team-a --strict
rc=$?
if [ "$rc" = 6 ] && has_obj clusterrolebinding _ scaling-manager-preflight-team-a && grep -q 'could not check metrics.model' "$W/out"; then
    ok "admin-finish --strict: an UNKNOWN in the last preflight exits 6 and keeps the preflight binding"
else
    bad "admin-finish --strict: exit $rc: $(tail -1 "$W/out")"
fi

# ---------------------------------------------------------------------------
echo "KEDA's side is the release's scaling-manager-keda-auth.yaml"
# keda_auth_same <release keda-auth file> <dir of what admin-finish wrote>
keda_auth_same() {
    local file="$1" d="$2" fcta fcm wcta wcm wsec why=""
    fcta="$(yq ea -o json -I 0 'select(.kind == "ClusterTriggerAuthentication")' "$file")"
    fcm="$(yq ea -o json -I 0 'select(.kind == "ConfigMap")' "$file")"
    wcta="$(cat "$d/obj/clustertriggerauthentication/_/$cta.json")"
    wcm="$(cat "$d/obj/configmap/team-a/scaling-manager-scaler-keda-auth.json")"
    wsec="$(cat "$d/obj/secret/keda-system/$cta.json")"
    [ -n "$fcta" ] && [ -n "$fcm" ] || why="the release file holds no ClusterTriggerAuthentication or ConfigMap"
    [ "$(jq -c -S '[.metadata.name, .spec]' <<< "$fcta")" = "$(jq -c -S '[.metadata.name, .spec]' <<< "$wcta")" ] || why="$why; the ClusterTriggerAuthentication differs"
    [ "$(jq -r '.metadata.labels["scaling-manager.llmd.ai/scaler-tls"]' <<< "$fcta")" = "$(jq -r '.metadata.labels["scaling-manager.llmd.ai/scaler-tls"]' <<< "$wcta")" ] || why="$why; the scaler-tls label differs"
    [ "$(jq -c '[.metadata.name, .metadata.namespace]' <<< "$fcm")" = "$(jq -c '[.metadata.name, .metadata.namespace]' <<< "$wcm")" ] || why="$why; the marker is another object"
    [ "$(jq -c -S '.data' <<< "$fcm")" = "$(jq -c -S --argjson f "$fcm" '.data | with_entries(select(.key as $k | $f.data | has($k)))' <<< "$wcm")" ] || why="$why; the marker data differs"
    [ "$(jq -r '[.spec.secretTargetRef[].name] | unique | join(",")' <<< "$wcta")" = "$(jq -r '.metadata.name' <<< "$wsec")" ] \
        && [ "$(jq -r '.metadata.namespace' <<< "$wsec")" = "$(jq -r '.data.secretNamespace' <<< "$fcm")" ] \
        && [ "$(jq -r '.data | keys | join(",")' <<< "$wsec")" = "ca.crt,tls.crt,tls.key" ] || why="$why; the Secret copy is not the one the ClusterTriggerAuthentication reads"
    [ -z "$why" ] || { echo "${why#; }"; return 1; }
}
kf="$(sedded scaling-manager-keda-auth.yaml team-a team-a)"
sed -i 's/__KEDA_NAMESPACE__/keda-system/g' "$kf"
if why="$(keda_auth_same "$kf" "$finished")"; then ok "the ClusterTriggerAuthentication, the marker and the Secret copy match the release file for the same namespaces"; else bad "keda-auth: $why"; fi
# The comparison fails on a release file that differs: it compares.
cp "$kf" "$W/keda-auth-broken.yaml"
sed -i 's/parameter: tlsClientKey/parameter: tlsClientKeyX/' "$W/keda-auth-broken.yaml"
if why="$(keda_auth_same "$W/keda-auth-broken.yaml" "$finished")"; then bad "the keda-auth comparison passed a changed release file"; else ok "the keda-auth comparison fails on a changed release file ($why)"; fi

# ---------------------------------------------------------------------------
echo "install --scope cluster"
reset m1
echo "keda-system keda-operator-metrics-apiserver" > "$STUB_DIR/apiservice"
printf '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"scaling-manager-controller-status","namespace":"scaling-manager-system"},"data":{"scalerTLS":"mutual TLS"}}' > "$W/status.json"
# What the controller reports once it serves mutual TLS. The client certificate is filled below, as the controller would fill it.
mkdir -p "$STUB_DIR/obj/configmap/scaling-manager-system"
cp "$W/status.json" "$STUB_DIR/obj/configmap/scaling-manager-system/scaling-manager-controller-status.json"
smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1 --model-namespace nosuch
rc=$?
if [ "$rc" = 4 ] && grep -q 'model namespace nosuch does not exist' "$W/out" && [ "$(writes)" = 0 ] && ! has_obj namespace _ scaling-manager-system; then
    ok "install --scope cluster refuses a model namespace that does not exist (exit 4), before it writes anything"
else
    bad "cluster, missing model namespace: exit $rc: $(tail -1 "$W/out")"
fi
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1
rc=$?
if [ "$rc" = 5 ] && has_obj namespace _ scaling-manager-system && has_obj deployment scaling-manager-system scaling-manager-controller-manager \
    && has_obj podmonitor m1 scaling-manager-modelserver-metrics && has_obj rolebinding m1 scaling-manager-manager-rolebinding-scaling-manager-system \
    && ! has_obj configmap scaling-manager-system scaling-manager-admin-setup; then
    ok "install --scope cluster creates the namespace, the admin objects, the model-namespace file in m1, no stamp, the controller; then waits for the client certificate (exit 5 here)"
else
    bad "cluster install: exit $rc: $(tail -2 "$W/out" | tr '\n' ' ')"
fi
mkdir -p "$STUB_DIR/obj/clusterrolebinding/_"
echo '{}' > "$STUB_DIR/obj/clusterrolebinding/_/scaling-manager-preflight.json"
jq --arg c "$(base64 < "$W/c.pem" | tr -d '\n')" --arg k "$(base64 < "$W/k.pem" | tr -d '\n')" \
    '.data = {"tls.crt": $c, "tls.key": $k, "ca.crt": $c}' "$STUB_DIR/obj/secret/scaling-manager-system/scaling-manager-external-scaler-client-tls.json" > "$W/sec.json"
mv "$W/sec.json" "$STUB_DIR/obj/secret/scaling-manager-system/scaling-manager-external-scaler-client-tls.json"
smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1
rc=$?
if [ "$rc" = 0 ] && has_obj clustertriggerauthentication _ scaling-manager-external-scaler-scaling-manager-system \
    && ! has_obj clusterrolebinding _ scaling-manager-preflight; then
    ok "a re-run of install --scope cluster finishes: KEDA's side written, the preflight binding scaling-manager-preflight removed"
else
    bad "cluster finish: exit $rc: $(tail -2 "$W/out" | tr '\n' ' ')"
fi

# ---------------------------------------------------------------------------
echo "the DisaggregatedSet role writer (--dset-role-writer)"
# dset_grants: the Roles and ClusterRoles in the stub cluster that grant more
# than get on disaggregatedsets. Only the opt-in may make one.
dset_grants() {
    cat "$STUB_DIR"/obj/role/*/*.json "$STUB_DIR"/obj/clusterrole/_/*.json 2>/dev/null \
        | jq -r 'select(any(.rules[]?; ((.resources // []) | (index("disaggregatedsets") or index("*"))) and (((.verbs // []) - ["get"]) | length > 0)))
            | .kind + "/" + .metadata.name' | sort | tr '\n' ' ' | sed 's/ $//'
}
# dset_env <namespace>: DSET_ROLE_WRITER on the controller there, empty when unset.
dset_env() {
    jq -r '[.spec.template.spec.containers[] | select(.name == "manager") | .env[]? | select(.name == "DSET_ROLE_WRITER") | .value] | join(",")' \
        "$STUB_DIR/obj/deployment/$1/scaling-manager-controller-manager.json" 2>/dev/null
}
dset_stamp() { obj configmap team-a scaling-manager-admin-setup | jq -r '.data.dsetRoleWriter // "unset"'; }
reset
echo "keda-system keda-operator-metrics-apiserver" > "$STUB_DIR/apiservice"
smt admin-setup -n team-a
rc=$?
if [ "$rc" = 0 ] && [ -z "$(dset_grants)" ] && [ "$(dset_stamp)" = false ] && has_obj role team-a scaling-manager-manager-role-team-a; then
    ok "admin-setup without --dset-role-writer grants no list or patch on disaggregatedsets, and the stamp says dsetRoleWriter false"
else
    bad "default admin-setup: exit $rc, grants '$(dset_grants)', stamp '$(dset_stamp)'"
fi
: > "$STUB_DIR/calls.log"
smt install -n team-a --prometheus-url https://p:9090 --dset-role-writer
rc=$?
if [ "$rc" = 3 ] && grep -q 'smt admin-setup -n team-a --keda-namespace keda-system --dset-role-writer' "$W/out" && [ "$(writes)" = 0 ]; then
    ok "install --dset-role-writer before the admin granted it: exit 3, the admin-setup line with --dset-role-writer, nothing written"
else
    bad "install --dset-role-writer, no grant: exit $rc, $(writes) writes: $(tail -2 "$W/out" | tr '\n' ' ')"
fi
smt install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 0 ] && has_obj deployment team-a scaling-manager-controller-manager && [ -z "$(dset_env team-a)" ] && [ -z "$(dset_grants)" ]; then
    ok "the default install: no DSET_ROLE_WRITER on the controller, no grant"
else
    bad "default install: exit $rc, DSET_ROLE_WRITER '$(dset_env team-a)', grants '$(dset_grants)'"
fi
smt install -n team-a --prometheus-url https://p:9090 --set DSET_ROLE_WRITER=true
rc=$?
if [ "$rc" = 2 ] && grep -q 'give --dset-role-writer' "$W/out"; then ok "--set DSET_ROLE_WRITER is refused, naming the flag"; else bad "--set DSET_ROLE_WRITER: exit $rc: $(tail -1 "$W/out")"; fi
smt admin-finish -n team-a --dset-role-writer
expect_rc "admin-finish does not take --dset-role-writer" 2 $?
smt admin-setup -n team-a --dset-role-writer
rc=$?
sub="$(obj rolebinding team-a scaling-manager-dset-role-writer-rolebinding | jq -r '[.roleRef.kind, .roleRef.name, (.subjects[] | .namespace + "/" + .name)] | join(" ")')"
if [ "$rc" = 0 ] && [ "$(dset_grants)" = Role/scaling-manager-dset-role-writer-role ] && [ "$(dset_stamp)" = true ] \
    && [ "$sub" = "Role scaling-manager-dset-role-writer-role team-a/scaling-manager-controller-manager" ] \
    && grep -q 'smt install -n team-a --prometheus-url <https://prometheus:9090> --dset-role-writer' "$W/out"; then
    ok "admin-setup --dset-role-writer: the Role in team-a bound to the controller, the stamp says so, the install line names the flag"
else
    bad "admin-setup --dset-role-writer: exit $rc, grants '$(dset_grants)', stamp '$(dset_stamp)', binding '$sub'"
fi
gen="$(obj deployment team-a scaling-manager-controller-manager | jq -r '.metadata.generation')"
smt install -n team-a --prometheus-url https://p:9090 --dset-role-writer
rc=$?
if [ "$rc" = 0 ] && [ "$(dset_env team-a)" = true ] && [ "$(obj deployment team-a scaling-manager-controller-manager | jq -r '.metadata.generation')" != "$gen" ] \
    && [ "$(obj configmap team-a scaling-manager-settings | jq -r '.data.passive')" = true ] && grep -q 'writes no replicas while a model is passive' "$W/out"; then
    ok "install --dset-role-writer: DSET_ROLE_WRITER=true on the controller (a new rollout), the install still passive"
else
    bad "install --dset-role-writer: exit $rc, DSET_ROLE_WRITER '$(dset_env team-a)': $(tail -2 "$W/out" | tr '\n' ' ')"
fi
smt install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 0 ] && [ -z "$(dset_env team-a)" ]; then ok "a re-run of install without the flag turns the writer off on the controller"; else bad "re-run without the flag: exit $rc, DSET_ROLE_WRITER '$(dset_env team-a)'"; fi
jq '.data.adminObjectsSha256 = "0000"' "$STUB_DIR/obj/configmap/team-a/scaling-manager-admin-setup.json" > "$W/s.json" && cp "$STUB_DIR/obj/configmap/team-a/scaling-manager-admin-setup.json" "$W/s.orig.json" && mv "$W/s.json" "$STUB_DIR/obj/configmap/team-a/scaling-manager-admin-setup.json"
smt install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 3 ] && grep -q 'smt admin-setup -n team-a --keda-namespace keda-system --dset-role-writer' "$W/out"; then
    ok "an upgrade that needs admin-setup keeps --dset-role-writer in the line it prints, when the stamp has it"
else
    bad "stamp of another release with the grant: exit $rc: $(tail -1 "$W/out")"
fi
mv "$W/s.orig.json" "$STUB_DIR/obj/configmap/team-a/scaling-manager-admin-setup.json"
: > "$STUB_DIR/calls.log"
smt admin-setup -n team-a
rc=$?
if [ "$rc" = 0 ] && [ -z "$(dset_grants)" ] && ! has_obj rolebinding team-a scaling-manager-dset-role-writer-rolebinding && [ "$(dset_stamp)" = false ] \
    && grep -q '^delete rolebinding scaling-manager-dset-role-writer-rolebinding -n team-a' "$STUB_DIR/calls.log" \
    && grep -q 'Removed Role/scaling-manager-dset-role-writer-role' "$W/out"; then
    ok "admin-setup without the flag removes the grant an earlier run made, binding first, and says so"
else
    bad "revoke: exit $rc, grants '$(dset_grants)', stamp '$(dset_stamp)': $(grep -m1 -i 'role writer' "$W/out")"
fi
rm -rf "$W/dryd"
smt admin-setup -n team-a --keda-namespace kk --dset-role-writer --dry-run --out "$W/dryd"; rc1=$?
smt install -n team-a --prometheus-url https://p:9090 --dset-role-writer --dry-run --out "$W/dryd"; rc2=$?
want="$(norm "$(sedded scaling-manager-dset-role-writer.yaml team-a team-a)")"
if [ "$rc1$rc2" = 00 ] && [ -n "$want" ] && [ "$(norm "$W/dryd/13-dset-role-writer.yaml")" = "$want" ] && [ ! -e "$W/dry/13-dset-role-writer.yaml" ] \
    && [ "$(yq ea '[select(.kind == "Deployment") | .spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == "DSET_ROLE_WRITER") | .value] | join(",")' "$W/dryd/50-controller.yaml")" = true ] \
    && [ "$(yq ea '[select(.kind == "Deployment") | .spec.template.spec.containers[] | .env[]? | select(.name == "DSET_ROLE_WRITER")] | length' "$W/dry/50-controller.yaml")" = 0 ]; then
    ok "--dry-run --dset-role-writer: 13-dset-role-writer.yaml is the release file, the controller has DSET_ROLE_WRITER=true; without the flag neither"
else
    bad "dry-run --dset-role-writer: exits $rc1 $rc2: $(ls "$W/dryd" | tr '\n' ' ')"
fi
reset m1
echo "keda-system keda-operator-metrics-apiserver" > "$STUB_DIR/apiservice"
mkdir -p "$STUB_DIR/obj/configmap/scaling-manager-system"
cp "$W/status.json" "$STUB_DIR/obj/configmap/scaling-manager-system/scaling-manager-controller-status.json"
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1 --no-weights-labeller
if [ -z "$(dset_grants)" ] && [ -z "$(dset_env scaling-manager-system)" ] && has_obj deployment scaling-manager-system scaling-manager-controller-manager; then
    ok "install --scope cluster without the flag: no grant, no DSET_ROLE_WRITER"
else
    bad "default cluster install: grants '$(dset_grants)', DSET_ROLE_WRITER '$(dset_env scaling-manager-system)'"
fi
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1 --no-weights-labeller --dset-role-writer
sub="$(obj clusterrolebinding _ scaling-manager-dset-role-writer-rolebinding | jq -r '[.roleRef.name, (.subjects[] | .namespace + "/" + .name)] | join(" ")')"
if [ "$(dset_grants)" = ClusterRole/scaling-manager-dset-role-writer-role ] && [ "$(dset_env scaling-manager-system)" = true ] \
    && [ "$sub" = "scaling-manager-dset-role-writer-role scaling-manager-system/scaling-manager-controller-manager" ]; then
    ok "install --scope cluster --dset-role-writer: the ClusterRole bound to the controller, DSET_ROLE_WRITER=true"
else
    bad "cluster --dset-role-writer: grants '$(dset_grants)', DSET_ROLE_WRITER '$(dset_env scaling-manager-system)', binding '$sub'"
fi
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1 --no-weights-labeller
if [ -z "$(dset_grants)" ] && ! has_obj clusterrolebinding _ scaling-manager-dset-role-writer-rolebinding && [ -z "$(dset_env scaling-manager-system)" ]; then
    ok "a cluster re-run without the flag removes the grant and turns the writer off"
else
    bad "cluster re-run without the flag: grants '$(dset_grants)', DSET_ROLE_WRITER '$(dset_env scaling-manager-system)'"
fi

# ---------------------------------------------------------------------------
echo "the weights labeller"
LNS=scaling-manager-weights-labeller
largs() { jq -c '.spec.template.spec.containers[0].args' "$STUB_DIR/obj/deployment/$LNS/weights-labeller.json" 2>/dev/null; }
limg() { jq -r '.spec.template.spec.containers[0].image' "$STUB_DIR/obj/deployment/$LNS/weights-labeller.json" 2>/dev/null; }
# creset: reset, with the APIService and the controller status a cluster install reads
creset() {
    reset "$@"
    echo "keda-system keda-operator-metrics-apiserver" > "$STUB_DIR/apiservice"
    mkdir -p "$STUB_DIR/obj/configmap/scaling-manager-system"
    cp "$W/status.json" "$STUB_DIR/obj/configmap/scaling-manager-system/scaling-manager-controller-status.json"
}
# wds NS: a weights DaemonSet in NS, as weights.sh apply leaves one.
wds() {
    mkdir -p "$STUB_DIR/obj/daemonset/$1"
    jq -n --arg ns "$1" '{apiVersion: "apps/v1", kind: "DaemonSet", metadata: {name: "weights-x", namespace: $ns,
        labels: {"app.kubernetes.io/component": "node-local-weights", "app.kubernetes.io/managed-by": "scaling-manager-weights"}}}' \
        > "$STUB_DIR/obj/daemonset/$1/weights-x.json"
}
creset m1 m2 team-b
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1 --model-namespace m2
if ! has_obj deployment "$LNS" weights-labeller && ! has_obj clusterrole _ scaling-manager-weights-labeller \
    && grep -q 'Weights labeller: not applied: no model namespace keeps weights on the node (no weights.sh apply DaemonSet in m1, m2)' "$W/out" \
    && grep -q "After weights apply, run this install again, or: smt weights labeller -n <model namespace> --image ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools:$V --merge" "$W/out" \
    && grep -q "KEDA's client certificate" "$W/out"; then
    ok "install --scope cluster with no weights DaemonSet in any model namespace applies no labeller, and says how to add it"
else
    bad "no weights, no labeller: $(grep -A1 -i 'labeller' "$W/out" | head -3 | tr '\n' ' ')"
fi
touch "$STUB_DIR/forbid-daemonsets"
wds m1
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1
rm -f "$STUB_DIR/forbid-daemonsets"
if ! has_obj deployment "$LNS" weights-labeller && grep -q 'Weights labeller: not applied: could not read the DaemonSets in m1 (Error from server (Forbidden)' "$W/out" \
    && grep -q "KEDA's client certificate" "$W/out"; then
    ok "DaemonSets that cannot be read: no labeller, the namespace and kubectl's reason named, and the install goes on (Forbidden is not absent)"
else
    bad "Forbidden DaemonSets: $(grep -i 'labeller' "$W/out" | head -2 | tr '\n' ' ')"
fi
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1
if [ "$(largs)" = '["weights","label","-n","m1","--watch","30","--label-prefix","llm-d.ai"]' ] \
    && [ "$(limg)" = "ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools:$V" ] \
    && has_obj rolebinding m1 scaling-manager-weights-labeller-reader && has_obj clusterrole _ scaling-manager-weights-labeller \
    && jq -e '.rules == [{apiGroups: [""], resources: ["nodes"], verbs: ["get", "list", "patch"]}, {apiGroups: [""], resources: ["namespaces"], verbs: ["get"]}]' "$STUB_DIR/obj/clusterrole/_/scaling-manager-weights-labeller.json" >/dev/null; then
    ok "install --scope cluster applies the weights labeller, on this release's tools image, watching its --model-namespace"
else
    bad "cluster install labeller: args $(largs), image $(limg): $(grep -i labeller "$W/out" | head -2 | tr '\n' ' ')"
fi
smt admin-setup -n team-b --keda-namespace keda-system
rc=$?
if [ "$rc" = 0 ] && [ "$(largs)" = '["weights","label","-n","m1,team-b","--watch","30","--label-prefix","llm-d.ai"]' ] \
    && has_obj rolebinding team-b scaling-manager-weights-labeller-reader && grep -q 'Added team-b to the weights labeller' "$W/out"; then
    ok "admin-setup adds its namespace to the running labeller, with namespaced writes (a RoleBinding, the Deployment)"
else
    bad "admin-setup add: exit $rc, args $(largs): $(grep -A2 'Weights labeller' "$W/out" | tr '\n' ' ')"
fi
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m2
if [ "$(largs)" = '["weights","label","-n","m1,team-b,m2","--watch","30","--label-prefix","llm-d.ai"]' ] && has_obj rolebinding m2 scaling-manager-weights-labeller-reader; then
    ok "a re-run of install --scope cluster keeps every namespace the labeller watches, in its order, and adds its own after them"
else
    bad "cluster re-run merge: args $(largs)"
fi
rm -f "$STUB_DIR/obj/namespace/_/team-b.json"
# (the exit code is not the point here, as in the cluster cases above: the stub install ends non-zero)
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m2
rc=$?
if ! grep -q "weights labeller was not applied" "$W/out" && [ "$(largs)" = '["weights","label","-n","m1,m2","--watch","30","--label-prefix","llm-d.ai"]' ] && grep -q 'team-b, which is gone or being deleted; it is dropped' "$W/out"; then
    ok "a re-run drops a watched namespace that was deleted since, and says so"
else
    bad "cluster re-run, deleted namespace: exit $rc, args $(largs): $(grep -i 'labeller\|team-b' "$W/out" | head -3 | tr '\n' ' ')"
fi
# A re-run with a namespace already watched renders the same arguments: a
# changed list would restart the labeller on every install.
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1
if [ "$(largs)" = '["weights","label","-n","m1,m2","--watch","30","--label-prefix","llm-d.ai"]' ]; then
    ok "a re-run with a namespace it already watches leaves the labeller's arguments as they are"
else
    bad "cluster re-run, same namespaces: args $(largs)"
fi
# weights.sh labeller on its own, over the running one.
wl() { (cd "$W/work" && PATH="$W/bin:$PATH" bash "$IMGDIR/weights.sh" "$@") > "$W/out" 2>&1; }
wl labeller -n m1 --image mirror.example.com/tools:v1
rc=$?
if [ "$rc" -ne 0 ] && grep -q 'the running labeller also watches m2' "$W/out" && grep -q 'Add --merge' "$W/out" \
    && [ "$(largs)" = '["weights","label","-n","m1,m2","--watch","30","--label-prefix","llm-d.ai"]' ] && [ "$(limg)" = "ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools:$V" ]; then
    ok "labeller -n with fewer namespaces than the running one watches is refused, and changes nothing"
else
    bad "labeller narrowing: exit $rc, args $(largs), image $(limg): $(tail -1 "$W/out")"
fi
wl labeller -n m1 --delete
rc=$?
if [ "$rc" = 0 ] && ! has_obj rolebinding m1 scaling-manager-weights-labeller-reader && ! has_obj rolebinding m2 scaling-manager-weights-labeller-reader; then
    ok "labeller --delete -n m1 also removes the RoleBinding of every namespace the running one watched"
else
    bad "labeller --delete: exit $rc, RoleBinding in m2 left: $(has_obj rolebinding m2 scaling-manager-weights-labeller-reader && echo yes || echo no)"
fi
wl labeller --add -n "$LNS"
rc=$?
[ "$rc" -ne 0 ] && grep -q "is the labeller's own namespace" "$W/out" && ok "labeller --add refuses the labeller's own namespace" || bad "labeller --add of its own namespace: exit $rc: $(tail -1 "$W/out")"
creset m1 team-c
wds m1
smt admin-setup -n team-c --keda-namespace keda-system
rc=$?
if [ "$rc" = 0 ] && grep -q 'No weights labeller runs in' "$W/out" && grep -q 'smt install --scope cluster' "$W/out" && ! has_obj deployment "$LNS" weights-labeller; then
    ok "admin-setup with no labeller says how to add one, creates none, and still succeeds"
else
    bad "admin-setup, no labeller: exit $rc: $(grep -A3 'Weights labeller' "$W/out" | tr '\n' ' ')"
fi
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1
printf 'create rolebindings team-c\n' > "$STUB_DIR/can-i-no"
smt admin-setup -n team-c --keda-namespace keda-system
rc=$?
if [ "$rc" = 0 ] && grep -q 'smt weights labeller --add -n team-c' "$W/out" && [ "$(largs)" = '["weights","label","-n","m1","--watch","30","--label-prefix","llm-d.ai"]' ] \
    && ! has_obj rolebinding team-c scaling-manager-weights-labeller-reader; then
    ok "admin-setup without the rights to add its namespace prints the command, changes nothing, and still succeeds"
else
    bad "admin-setup, no rights: exit $rc, args $(largs): $(grep -A3 'Weights labeller' "$W/out" | tr '\n' ' ')"
fi
rm -f "$STUB_DIR/can-i-no"
creset m1
wds m1
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1 --no-weights-labeller
if ! has_obj deployment "$LNS" weights-labeller && grep -q 'Weights labeller: not applied (--no-weights-labeller)' "$W/out"; then
    ok "--no-weights-labeller applies no labeller, and says so"
else
    bad "--no-weights-labeller: $(grep -i labeller "$W/out" | head -2 | tr '\n' ' ')"
fi
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1 --weights-labeller-image mirror.example.com/tools:v1
[ "$(limg)" = mirror.example.com/tools:v1 ] && ok "--weights-labeller-image sets its image" || bad "--weights-labeller-image: $(limg)"
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1 --image-repo mirror.example.com/sm
if grep -q 'Weights labeller: not applied: no tools image to run it with (a development image, or --image-repo)' "$W/out" && [ "$(limg)" = mirror.example.com/tools:v1 ]; then
    ok "--image-repo without --weights-labeller-image: the labeller is left as it is, and why is said"
else
    bad "--image-repo labeller: image $(limg): $(grep -i 'labeller' "$W/out" | head -2 | tr '\n' ' ')"
fi
smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1 --weights-labeller-image 'bad image'
rc=$?
[ "$rc" = 2 ] && grep -q "is not an image reference" "$W/out" && ok "a malformed --weights-labeller-image is refused (usage)" || bad "bad labeller image: exit $rc: $(tail -1 "$W/out")"
# A labeller Deployment this script did not write: weights.sh refuses to merge
# into it, and the install says so and exits 7.
jq '.spec.template.spec.containers[0].args = ["sh", "-c", "sleep 1d"]' "$STUB_DIR/obj/deployment/$LNS/weights-labeller.json" > "$W/foreign.json"
cp "$W/foreign.json" "$STUB_DIR/obj/deployment/$LNS/weights-labeller.json"
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url https://p:9090 --model-namespace m1
rc=$?
if [ "$rc" = 7 ] && grep -q 'the weights labeller was not applied (weights.sh labeller exited non-zero, above)' "$W/out" \
    && grep -q 'does not carry the arguments weights.sh writes' "$W/out" && [ "$(largs)" = '["sh","-c","sleep 1d"]' ]; then
    ok "a labeller Deployment weights.sh did not write is not merged into: the install exits 7 and names it"
else
    bad "foreign labeller: exit $rc, args $(largs): $(grep -i 'labeller' "$W/out" | head -2 | tr '\n' ' ')"
fi
rm -rf "$W/drywl"
smt install --scope cluster --keda-namespace kk --prometheus-url https://prom.example:9090 --model-namespace m1 --dry-run --out "$W/drywl"
if [ "$(yq ea -o json -I 0 '[select(.kind == "Deployment") | .spec.template.spec.containers[0].args[3]]' "$W/drywl/60-weights-labeller.yaml" 2>/dev/null)" = '["m1"]' ] \
    && grep -q 'it watches exactly --model-namespace and, applied, replaces a running labeller.s list: keep the full list in git, and do not use labeller --add' "$W/out" \
    && ! grep -q 'keeps every namespace the running labeller already watches' "$W/out"; then
    ok "install --scope cluster --dry-run renders the labeller as 60-weights-labeller.yaml"
else
    bad "dry-run labeller: $(ls "$W/drywl" | tr '\n' ' ')"
fi
smt install -n team-a --prometheus-url https://p:9090 --no-weights-labeller --dry-run --out "$W/x"
rc=$?
if [ "$rc" = 2 ] && grep -q 'are for --scope cluster' "$W/out"; then ok "a namespaced install refuses the labeller flags (usage)"; else bad "namespaced labeller flag: exit $rc: $(tail -1 "$W/out")"; fi

# ---------------------------------------------------------------------------
echo "A plain HTTP Prometheus: --allow-http"
# The controller refuses an http URL unless allowed, and refuses to send a
# bearer token over one; the release Deployment sets PROMETHEUS_TOKEN_PATH.
DEP=scaling-manager-controller-manager
gen() { obj deployment "$1" "$DEP" | jq -r '.metadata.generation'; }
# tokv <namespace>: the PROMETHEUS_TOKEN_PATH values of the stored controller, as a JSON list.
tokv() { obj deployment "$1" "$DEP" | jq -c '[.spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == "PROMETHEUS_TOKEN_PATH") | .value]'; }
# tokf <file>: the same, read from a rendered file.
tokf() { yq ea -o json -I 0 'select(.kind == "Deployment")' "$1" | jq -c '[.spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == "PROMETHEUS_TOKEN_PATH") | .value]'; }
SA_TOKEN=/var/run/secrets/kubernetes.io/serviceaccount/token
TOKEN_EMPTIED_ENV=LLMSM_PREFLIGHT_TOKEN_PATH_EMPTIED
# jobenv <namespace>: TOKEN_EMPTIED_ENV on the stored preflight Job, empty when it has none.
jobenv() { obj job "$1" scaling-manager-preflight | jq -r --arg e "$TOKEN_EMPTIED_ENV" '[.spec.template.spec.containers[].env[]? | select(.name == $e) | .value] | join(",")'; }

reset
rm -rf "$W/dryh"
smt install -n team-a --prometheus-url http://prom.example:9090 --allow-http --dry-run --out "$W/dryh"
rc=$?
yq ea '(select(.kind == "Deployment") | .spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == "PROMETHEUS_TOKEN_PATH") | .value) = ""' "$W/dry/50-controller.yaml" > "$W/dry-emptied.yaml"
if [ "$rc" = 0 ] && [ "$(tokf "$W/dry/50-controller.yaml")" = "[\"$SA_TOKEN\"]" ] && [ "$(tokf "$W/dryh/50-controller.yaml")" = '[""]' ] \
    && [ "$(norm "$W/dryh/50-controller.yaml")" = "$(norm "$W/dry-emptied.yaml")" ] \
    && [ "$(yq '.data.prometheusURL + "|" + .data.prometheusAllowHTTP' "$W/dryh/20-settings.yaml")" = 'http://prom.example:9090|true' ]; then
    ok "--allow-http --dry-run: prometheusAllowHTTP true in the settings, and the controller is the release's with PROMETHEUS_TOKEN_PATH emptied, nothing else changed"
else
    bad "--allow-http dry-run: exit $rc, token https $(tokf "$W/dry/50-controller.yaml") http $(tokf "$W/dryh/50-controller.yaml"): $(tail -1 "$W/out")"
fi
smt install -n team-a --prometheus-url http://p:9090
rc=$?
if [ "$rc" = 2 ] && grep -q 'is plain HTTP, which the controller refuses by default. Add --allow-http' "$W/out" && [ ! -s "$STUB_DIR/calls.log" ]; then
    ok "an http --prometheus-url without --allow-http is usage (exit 2) naming the flag, before any kubectl call"
else
    bad "http without --allow-http: exit $rc: $(tail -1 "$W/out")"
fi
smt install -n team-a --prometheus-url https://p:9090 --allow-http
rc=$?
if [ "$rc" = 2 ] && grep -q -- '--allow-http is for a plain HTTP Prometheus' "$W/out" && [ ! -s "$STUB_DIR/calls.log" ]; then ok "--allow-http with an https URL is usage"; else bad "--allow-http with https: exit $rc: $(tail -1 "$W/out")"; fi
smt install -n team-a --prometheus-url http://p:9090 --allow-http --prometheus-ca prom-ca.crt
rc=$?
if [ "$rc" = 2 ] && grep -q -- '--allow-http with --prometheus-ca' "$W/out"; then ok "--allow-http with --prometheus-ca is usage"; else bad "--allow-http with a CA: exit $rc: $(tail -1 "$W/out")"; fi
smt install -n team-a --prometheus-url http://p:9090 --allow-http --set prometheusAllowHTTP=true --dry-run --out "$W/dryx"
rc=$?
if [ "$rc" = 2 ] && grep -q -- '--set prometheusAllowHTTP: give --allow-http' "$W/out"; then ok "--set prometheusAllowHTTP is refused, naming --allow-http"; else bad "--set prometheusAllowHTTP: exit $rc: $(tail -1 "$W/out")"; fi

# The generation the stub keeps: a re-apply that changes the controller bumps it.
# What a hand patch does: an https install, PROMETHEUS_TOKEN_PATH emptied with
# kubectl set env, then a re-run that applies the release Deployment again.
restore
smt install -n team-a --prometheus-url https://p:9090 > /dev/null 2>&1
g1="$(gen team-a)"
jq '(.spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == "PROMETHEUS_TOKEN_PATH") | .value) = ""' \
    "$STUB_DIR/obj/deployment/team-a/$DEP.json" > "$W/d.json" && mv "$W/d.json" "$STUB_DIR/obj/deployment/team-a/$DEP.json"
smt install -n team-a --prometheus-url https://p:9090
if [ "$(gen team-a)" = $((g1 + 1)) ] && [ "$(tokv team-a)" = "[\"$SA_TOKEN\"]" ]; then
    ok "control: a re-run over a hand-emptied token path puts the token back and bumps the generation ($g1 -> $(gen team-a))"
else
    bad "control: generation $g1 -> $(gen team-a), token $(tokv team-a)"
fi

restore
smt install -n team-a --prometheus-url http://p:9090 --allow-http
rc=$?
g1="$(gen team-a)"
st="$(obj configmap team-a scaling-manager-settings | jq -r '.data.prometheusURL + "|" + .data.prometheusAllowHTTP')"
if [ "$rc" = 0 ] && [ "$st" = 'http://p:9090|true' ] && [ "$(tokv team-a)" = '[""]' ]; then
    ok "install --allow-http: the settings allow HTTP and the controller is applied with PROMETHEUS_TOKEN_PATH empty"
else
    bad "install --allow-http: exit $rc, settings '$st', token $(tokv team-a): $(tail -2 "$W/out" | tr '\n' ' ')"
fi
before="$(obj configmap team-a scaling-manager-settings)"
: > "$STUB_DIR/calls.log"
smt install -n team-a --prometheus-url http://p:9090 --allow-http
rc=$?
if [ "$rc" = 0 ] && [ "$(gen team-a)" = "$g1" ] && [ "$(tokv team-a)" = '[""]' ] && [ "$(obj configmap team-a scaling-manager-settings)" = "$before" ] \
    && ! grep -q 'kubectl patch' "$W/out" && ! grep -qE '^create ' "$STUB_DIR/calls.log"; then
    ok "a re-run with the same flags changes nothing: controller generation $g1, settings as they were, no patch printed"
else
    bad "re-run --allow-http: exit $rc, generation $g1 -> $(gen team-a), token $(tokv team-a): $(grep -m1 'kubectl patch' "$W/out")"
fi
: > "$STUB_DIR/calls.log"
smt install -n team-a
rc=$?
if [ "$rc" = 2 ] && grep -q 'prometheusURL in the live scaling-manager-settings (http://p:9090) is plain HTTP' "$W/out" && grep -q -- '--allow-http' "$W/out" \
    && [ "$(writes)" = 0 ] && [ "$(gen team-a)" = "$g1" ]; then
    ok "a re-run without --allow-http on live http settings is refused before any write: it would apply the token again"
else
    bad "re-run without --allow-http: exit $rc, $(writes) writes, generation $(gen team-a): $(tail -1 "$W/out")"
fi
# A live controller crash-looping on the token path the release files set:
# --allow-http repairs it. The preflight Job is told the Deployment it is about
# to apply has no token path, so the live one is not held against it.
jq --arg t "$SA_TOKEN" '(.spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == "PROMETHEUS_TOKEN_PATH") | .value) = $t' \
    "$STUB_DIR/obj/deployment/team-a/$DEP.json" > "$W/d.json" && mv "$W/d.json" "$STUB_DIR/obj/deployment/team-a/$DEP.json"
: > "$STUB_DIR/calls.log"
smt install -n team-a --prometheus-url http://p:9090 --allow-http
rc=$?
if [ "$rc" = 0 ] && [ "$(tokv team-a)" = '[""]' ] && [ "$(jobenv team-a)" = true ]; then
    ok "--allow-http over a live controller with the token path: the preflight Job says the token path is emptied, and the emptied Deployment is applied"
else
    bad "repair: exit $rc, token $(tokv team-a), preflight Job $TOKEN_EMPTIED_ENV '$(jobenv team-a)': $(tail -2 "$W/out" | tr '\n' ' ')"
fi
saved_http="$W/after-http-install"
rm -rf "$saved_http"; cp -R "$STUB_DIR" "$saved_http"
# set_setting <namespace> <key> <value>: what the printed kubectl patch does.
set_setting() {
    jq --arg k "$2" --arg v "$3" '.data[$k] = $v' "$STUB_DIR/obj/configmap/$1/scaling-manager-settings.json" > "$W/s.json" \
        && mv "$W/s.json" "$STUB_DIR/obj/configmap/$1/scaling-manager-settings.json"
}
set_setting team-a prometheusAllowHTTP ""
: > "$STUB_DIR/calls.log"
smt install -n team-a --prometheus-url http://p:9090 --allow-http
rc=$?
if [ "$rc" = 2 ] && grep -qF "kubectl patch configmap scaling-manager-settings -n team-a --type merge -p '{\"data\":{\"prometheusAllowHTTP\":\"true\"}}'" "$W/out" && [ "$(writes)" = 0 ]; then
    ok "--allow-http over live http settings without prometheusAllowHTTP: refused before any write, with the patch"
else
    bad "--allow-http, the setting missing: exit $rc, $(writes) writes: $(tail -2 "$W/out" | tr '\n' ' ')"
fi
# From http to https: refused with both patches; after them the same flags install.
rm -rf "$STUB_DIR"; cp -R "$saved_http" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
smt install -n team-a --prometheus-url https://p:9090
rc=$?
if [ "$rc" = 2 ] && [ "$(writes)" = 0 ] && grep -q 'changes its scheme' "$W/out" \
    && grep -qF "kubectl patch configmap scaling-manager-settings -n team-a --type merge -p '{\"data\":{\"prometheusURL\":\"https://p:9090\"}}'" "$W/out" \
    && grep -qF "kubectl patch configmap scaling-manager-settings -n team-a --type merge -p '{\"data\":{\"prometheusAllowHTTP\":\"false\"}}'" "$W/out"; then
    set_setting team-a prometheusURL https://p:9090
    set_setting team-a prometheusAllowHTTP false
    smt install -n team-a --prometheus-url https://p:9090
    rc=$?
    if [ "$rc" = 0 ] && [ "$(tokv team-a)" = "[\"$SA_TOKEN\"]" ] && [ "$(jobenv team-a)" = "" ]; then
        ok "http to https: refused with the two patches, and after them the same flags install the controller with its token"
    else
        bad "http to https after the patches: exit $rc, token $(tokv team-a): $(tail -2 "$W/out" | tr '\n' ' ')"
    fi
else
    bad "http to https: exit $rc, $(writes) writes: $(tail -3 "$W/out" | tr '\n' ' ')"
fi
# From https to http: the same.
rm -rf "$STUB_DIR"; cp -R "$installed" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
smt install -n team-a --prometheus-url http://p:9090 --allow-http
rc=$?
if [ "$rc" = 2 ] && [ "$(writes)" = 0 ] && grep -q 'changes its scheme' "$W/out" \
    && grep -qF "kubectl patch configmap scaling-manager-settings -n team-a --type merge -p '{\"data\":{\"prometheusURL\":\"http://p:9090\"}}'" "$W/out" \
    && grep -qF "kubectl patch configmap scaling-manager-settings -n team-a --type merge -p '{\"data\":{\"prometheusAllowHTTP\":\"true\"}}'" "$W/out"; then
    set_setting team-a prometheusURL http://p:9090
    set_setting team-a prometheusAllowHTTP true
    smt install -n team-a --prometheus-url http://p:9090 --allow-http
    rc=$?
    if [ "$rc" = 0 ] && [ "$(tokv team-a)" = '[""]' ]; then
        ok "https to http: refused with the two patches, before any write; after them the same flags install the controller without a token path"
    else
        bad "https to http after the patches: exit $rc, token $(tokv team-a): $(tail -2 "$W/out" | tr '\n' ' ')"
    fi
else
    bad "https to http: exit $rc, $(writes) writes: $(tail -3 "$W/out" | tr '\n' ' ')"
fi
# A switch prints every patch it needs: to http, the TLS settings the
# controller refuses beside an http URL; to https with --prometheus-ca, its CA.
rm -rf "$STUB_DIR"; cp -R "$installed" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
set_setting team-a prometheusCACertPath /etc/scaling-manager-prometheus-ca/ca.crt
set_setting team-a prometheusInsecureSkipVerify True
smt install -n team-a --prometheus-url http://p:9090 --allow-http
rc=$?
if [ "$rc" = 2 ] && [ "$(writes)" = 0 ] && grep -q 'the same command again right away' "$W/out" \
    && grep -qF "kubectl patch configmap scaling-manager-settings -n team-a --type merge -p '{\"data\":{\"prometheusCACertPath\":\"\"}}'" "$W/out" \
    && grep -qF "kubectl patch configmap scaling-manager-settings -n team-a --type merge -p '{\"data\":{\"prometheusInsecureSkipVerify\":\"false\"}}'" "$W/out" \
    && ! grep -qF '"prometheusServerName"' "$W/out"; then
    ok "https to http with a CA path and insecureSkipVerify set: the switch also prints the patches that clear them, and none for an unset key"
else
    bad "https to http, TLS settings: exit $rc: $(tail -6 "$W/out" | tr '\n' ' ')"
fi
rm -rf "$STUB_DIR"; cp -R "$saved_http" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
smt install -n team-a --prometheus-url https://p:9090 --prometheus-ca prom-ca.crt
rc=$?
if [ "$rc" = 2 ] && [ "$(writes)" = 0 ] \
    && grep -qF "kubectl patch configmap scaling-manager-settings -n team-a --type merge -p '{\"data\":{\"prometheusCACertPath\":\"/etc/scaling-manager-prometheus-ca/ca.crt\"}}'" "$W/out"; then
    ok "http to https with --prometheus-ca: the switch also prints the prometheusCACertPath patch"
else
    bad "http to https with a CA: exit $rc: $(tail -5 "$W/out" | tr '\n' ' ')"
fi
# --allow-http without --prometheus-url.
restore
smt install -n team-a --allow-http
rc=$?
if [ "$rc" = 2 ] && grep -q 'no --prometheus-url' "$W/out" && [ "$(writes)" = 0 ]; then ok "--allow-http, no settings, no --prometheus-url: exit 2 before any write"; else bad "--allow-http, no URL: exit $rc: $(tail -1 "$W/out")"; fi
rm -rf "$W/dryo2"
smt install -n team-a --platform openshift --allow-http --dry-run --out "$W/dryo2"
rc=$?
if [ "$rc" = 2 ] && grep -q -- '--allow-http is for a plain HTTP Prometheus, and the OpenShift default' "$W/out" && [ ! -s "$W/dryo2/20-settings.yaml" ]; then
    ok "--allow-http on OpenShift with no --prometheus-url: the https default is refused, nothing rendered"
else
    bad "--allow-http, OpenShift default: exit $rc: $(tail -1 "$W/out")"
fi
rm -rf "$STUB_DIR"; cp -R "$installed" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
smt install -n team-a --allow-http
rc=$?
if [ "$rc" = 2 ] && grep -q 'prometheusURL in the live scaling-manager-settings (https://p:9090) is not one' "$W/out" && [ "$(writes)" = 0 ]; then
    ok "--allow-http, no --prometheus-url, live https settings: refused before any write"
else
    bad "--allow-http over live https, no URL: exit $rc: $(tail -1 "$W/out")"
fi
rm -rf "$STUB_DIR"; cp -R "$saved_http" "$STUB_DIR"; : > "$STUB_DIR/calls.log"
smt install -n team-a --allow-http
rc=$?
if [ "$rc" = 0 ] && [ "$(tokv team-a)" = '[""]' ]; then ok "--allow-http, no --prometheus-url, live http settings: installs, the token path empty"; else bad "--allow-http over live http, no URL: exit $rc, token $(tokv team-a): $(tail -1 "$W/out")"; fi
# A scheme in capitals is http too.
reset
smt install -n team-a --prometheus-url HTTP://P:9090 --dry-run --out "$W/dryu"
rc1=$?
rm -rf "$W/dryu"
smt install -n team-a --prometheus-url HTTP://P:9090 --allow-http --dry-run --out "$W/dryu"
rc2=$?
if [ "$rc1$rc2" = 20 ] && [ "$(tokf "$W/dryu/50-controller.yaml")" = '[""]' ] && [ "$(yq '.data.prometheusAllowHTTP' "$W/dryu/20-settings.yaml")" = true ]; then
    ok "HTTP:// is plain HTTP: refused without --allow-http, rendered without the token path with it"
else
    bad "HTTP://: exits $rc1 $rc2, token $(tokf "$W/dryu/50-controller.yaml" 2>/dev/null)"
fi
# --files: the render and the Job flag apply to files given on the command line.
rm -rf "$W/dryf2"
smt_in "$DEVDIR" install -n team-a --prometheus-url http://p:9090 --allow-http --files "$W/dist" --dry-run --out "$W/dryf2"
rc=$?
fenv="$(yq ea -o json -I 0 'select(.kind == "Job")' "$W/dryf2/30-preflight.yaml" | jq -r --arg e "$TOKEN_EMPTIED_ENV" '[.spec.template.spec.containers[].env[]? | select(.name == $e) | .value] | join(",")')"
if [ "$rc" = 0 ] && [ "$(tokf "$W/dryf2/50-controller.yaml")" = '[""]' ] && [ "$fenv" = true ]; then
    ok "--files <dir> --allow-http: the controller without the token path, the preflight Job told so"
else
    bad "--files --allow-http: exit $rc, token $(tokf "$W/dryf2/50-controller.yaml"), Job '$fenv': $(tail -1 "$W/out")"
fi
# Files whose Deployment sets no PROMETHEUS_TOKEN_PATH: nothing to empty, nothing added.
rm -rf "$W/notoken"; cp -R "$W/dist" "$W/notoken"
yq -i 'del(select(.kind == "Deployment") | .spec.template.spec.containers[].env[] | select(.name == "PROMETHEUS_TOKEN_PATH"))' "$W/notoken/scaling-manager-namespaced.yaml"
(cd "$W/notoken" && sha256sum scaling-manager-*.yaml > SHA256SUMS)
rm -rf "$W/dryn"
smt_in "$DEVDIR" install -n team-a --prometheus-url http://p:9090 --allow-http --files "$W/notoken" --dry-run --out "$W/dryn"
rc=$?
if [ "$rc" = 0 ] && [ "$(tokf "$W/notoken/scaling-manager-namespaced.yaml")" = '[]' ] && [ "$(tokf "$W/dryn/50-controller.yaml")" = '[]' ]; then
    ok "a Deployment with no PROMETHEUS_TOKEN_PATH entry: --allow-http renders it as it is, adding none"
else
    bad "no token path entry: exit $rc, files $(tokf "$W/notoken/scaling-manager-namespaced.yaml"), rendered $(tokf "$W/dryn/50-controller.yaml" 2>/dev/null): $(tail -1 "$W/out")"
fi
# The Job variable smt sets is the one the preflight reads.
goenv="$(sed -n 's/^const TokenPathEmptiedEnv = "\([A-Z_]*\)"$/\1/p' "$ROOT/internal/preflight/settings.go" | tr -d '\r')"
shenv="$(sed -n 's/^SMT_TOKEN_EMPTIED_ENV=\([A-Z_]*\)$/\1/p' "$ROOT/deploy/lib/release_install.sh")"
if [ -n "$goenv" ] && [ "$goenv" = "$shenv" ] && [ "$goenv" = "$TOKEN_EMPTIED_ENV" ]; then
    ok "release_install.sh sets the Job variable the preflight reads: $goenv"
else
    bad "the Job variable: Go '$goenv', release_install.sh '$shenv', this check '$TOKEN_EMPTIED_ENV'"
fi
restore
smt admin-setup -n team-a --keda-namespace keda-system
rc=$?
if [ "$rc" = 0 ] && grep -q 'an http:// Prometheus: add --allow-http' "$W/out"; then ok "admin-setup ends with the install line and the --allow-http hint"; else bad "admin-setup closing line: exit $rc: $(tail -1 "$W/out")"; fi

reset m1
echo "keda-system keda-operator-metrics-apiserver" > "$STUB_DIR/apiservice"
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url http://p:9090 --allow-http --model-namespace m1
rc=$?
g1="$(gen scaling-manager-system)"
st="$(obj configmap scaling-manager-system scaling-manager-settings | jq -r '.data.prometheusURL + "|" + .data.prometheusAllowHTTP')"
SMT_CERT_TIMEOUT=0 smt install --scope cluster --prometheus-url http://p:9090 --allow-http --model-namespace m1
rc2=$?
if [ "$rc$rc2" = 55 ] && [ "$st" = 'http://p:9090|true' ] && [ "$(tokv scaling-manager-system)" = '[""]' ] && [ -n "$g1" ] && [ "$(gen scaling-manager-system)" = "$g1" ]; then
    ok "install --scope cluster --allow-http: the settings allow HTTP, the controller has no token path, and a re-run keeps generation $g1"
else
    bad "cluster --allow-http: exits $rc $rc2, settings '$st', token $(tokv scaling-manager-system), generation $g1 -> $(gen scaling-manager-system): $(tail -1 "$W/out")"
fi

# ---------------------------------------------------------------------------
echo "release-files and llmsm_make_cmd"
rm -rf "$W/rf"
smt release-files --out "$W/rf"
rc=$?
if [ "$rc" = 0 ] && cmp -s "$W/rf/SHA256SUMS" "$W/dist/SHA256SUMS" && [ -f "$W/rf/scaling-manager-namespaced.yaml.sigstore.json" ] \
    && ! ls "$W/rf" | grep -q 'scaling-manager-tools-' && (cd "$W/rf" && sha256sum -c --ignore-missing --quiet SHA256SUMS) \
    && grep -q 'is on the GitHub release' "$W/out"; then
    ok "release-files writes the manifests, the bundles and SHA256SUMS, which check; the tarball is named as on the GitHub release"
else
    bad "release-files: exit $rc: $(tail -1 "$W/out")"
fi
got="$(BLUE='' NC='' bash -c 'source "$1/deploy/lib/common.sh"; LLMSM_NS=team-a LLMSM_TOOLS_MODE=true llmsm_make_cmd verify-deployment; LLMSM_NS=team-a llmsm_make_cmd verify-deployment' _ "$ROOT")"
if [ "$got" = "smt admin-finish -n team-a
NAMESPACE=team-a make verify-deployment" ]; then ok "llmsm_make_cmd names smt admin-finish in tools mode, make otherwise"; else bad "llmsm_make_cmd: $got"; fi

# ---------------------------------------------------------------------------
echo "hack/compare-baked-release-files.sh"
cmpb() { bash "$ROOT/hack/compare-baked-release-files.sh" "$@" > "$W/out" 2>&1; }
cmpb "$IMGDIR/release" "$W/dist"; expect_rc "the baked files equal the dist: accepted" 0 $?
rm -rf "$W/b2"; cp -R "$IMGDIR/release" "$W/b2"
echo '{"bundle":"re-signed"}' > "$W/b2/scaling-manager-namespaced.yaml.sigstore.json"
cmpb "$W/b2" "$W/dist"
rc=$?
if [ "$rc" = 1 ] && grep -q 'scaling-manager-namespaced.yaml.sigstore.json in the image differs' "$W/out"; then ok "a re-signed bundle is refused"; else bad "re-signed bundle: exit $rc"; fi
rm -rf "$W/b3"; cp -R "$IMGDIR/release" "$W/b3"; rm "$W/b3/SHA256SUMS.sigstore.json"
cmpb "$W/b3" "$W/dist"; expect_rc "a bundle missing from the image is refused" 1 $?
rm -rf "$W/b4"; cp -R "$IMGDIR/release" "$W/b4"; cp "$W"/dist/scaling-manager-tools-*.tar.gz "$W/b4/"
cmpb "$W/b4" "$W/dist"; expect_rc "a tools tarball inside the image is refused" 1 $?
rm -rf "$W/b5"; mkdir -p "$W/b5"
cmpb "$W/b5" "$W/dist"
rc=$?
if [ "$rc" = 1 ] && grep -q 'built without --build-context release' "$W/out"; then ok "an image built without the release files is refused"; else bad "empty image: exit $rc"; fi
# The release also attaches the capacity planner binaries, which SHA256SUMS
# lists and the tools image does not carry.
rm -rf "$W/distp" "$W/bakedp"; cp -R "$W/dist" "$W/distp"
pb="capacity-planner-$V-linux-amd64"
printf 'planner\n' > "$W/distp/$pb"
(cd "$W/distp" && sha256sum "$pb" >> SHA256SUMS)
for f in "$W/distp/$pb" "$W/distp/SHA256SUMS"; do printf '{"bundle":"%s"}\n' "${f##*/}" > "$f.sigstore.json"; done
mkdir -p "$W/bakedp"
for f in "$W"/distp/*; do case "${f##*/}" in scaling-manager-tools-*|capacity-planner-*) ;; *) cp "$f" "$W/bakedp/" ;; esac; done
cmpb "$W/bakedp" "$W/distp"; expect_rc "a release with planner binaries: the image without them, its SHA256SUMS listing them, is accepted" 0 $?
cp "$W/distp/$pb" "$W/bakedp/"
cmpb "$W/bakedp" "$W/distp"
rc=$?
if [ "$rc" = 1 ] && grep -q "the image carries $pb" "$W/out"; then ok "a planner binary inside the tools image is refused"; else bad "planner binary baked: exit $rc"; fi
rm -f "$W/bakedp/$pb"
rm -rf "$W/imagep"; cp -R "$IMGDIR" "$W/imagep"; rm -rf "$W/imagep/release"; cp -R "$W/bakedp" "$W/imagep/release"
rm -rf "$W/dryp"
smt_in "$W/imagep" install -n team-a --prometheus-url https://p:9090 --dry-run --out "$W/dryp"
expect_rc "install checks a baked SHA256SUMS that lists the planner binaries the image does not carry" 0 $?

EXPECT_CASES=136
echo
echo "release install ($(yq --version 2>&1)): $PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
    exit 1
fi
if [ "$PASS" -ne "$EXPECT_CASES" ]; then
    echo "FAIL ran $PASS cases, expected $EXPECT_CASES: the run ended early"
    exit 1
fi
