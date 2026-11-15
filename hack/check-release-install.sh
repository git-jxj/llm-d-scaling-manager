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
cp "$ROOT/deploy/scaling-manager-tools.sh" "$IMGDIR/"
cp -R "$ROOT/deploy/lib" "$IMGDIR/lib"
printf '%s\n' "$V" > "$IMGDIR/VERSION"
for f in "$W"/dist/*; do
    case "${f##*/}" in scaling-manager-tools-*) ;; *) cp "$f" "$IMGDIR/release/" ;; esac
done
# A development image: no baked files.
DEVDIR="$W/devimage"
mkdir -p "$DEVDIR"
cp "$ROOT/deploy/scaling-manager-tools.sh" "$DEVDIR/"
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
    && ! has_obj secret team-a scaling-manager-external-scaler-ca && has_obj configmap team-a scaling-manager-settings; then
    ok "a preflight FAIL exits 1 naming the check, before the scaler Secrets and the controller"
else
    bad "preflight FAIL: exit $rc: $(tail -2 "$W/out" | tr '\n' ' ')"
fi
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

EXPECT_CASES=53
echo
echo "release install ($(yq --version 2>&1)): $PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
    exit 1
fi
if [ "$PASS" -ne "$EXPECT_CASES" ]; then
    echo "FAIL ran $PASS cases, expected $EXPECT_CASES: the run ended early"
    exit 1
fi
