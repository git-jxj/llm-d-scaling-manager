#!/usr/bin/env bash
#
# Installs the scaling manager with the tools image, `smt admin-setup`, `smt
# install` and `smt admin-finish` (docs/proposals/two-command-install.md), and
# checks each promise the "Testing" section of the proposal lists. `make
# test-e2e-tools-install` runs it on a kind cluster whose prerequisites
# (Prometheus, KEDA, the llm-d CRDs and EPP) the existing e2e setup installed,
# after building the tools image with the release files baked in.
#
#   test/tools-install/e2e.sh <dir with the rendered release files>
#
# The tools container runs with --network host: kind writes its kubeconfig
# with the API server at 127.0.0.1:<port>, which a container reaches only on
# the host network.
#
# Environment:
#   TOOLS_IMG         the tools image, built with --build-context release=<dir>
#   TOOLS_E2E_SCOPE   namespace (default): admin-setup and admin-finish as the
#                     cluster admin, install as a ServiceAccount bound to the
#                     stock admin ClusterRole in NS and nothing else.
#                     cluster: one install --scope cluster as the cluster admin.
#   NS                the namespace of the controller (default llm-d-sim; with scope
#                     cluster, scaling-manager-system)
#   MODEL_NS          the namespace with the model servers (default llm-d-sim)
#   KEDA_NS           where KEDA runs (default keda-system)
#   MONITORING_NS     where the e2e Prometheus runs (default scaling-manager-monitoring)
#   PROMETHEUS_URL    default the e2e Prometheus Service in MONITORING_NS
#   PROMETHEUS_TLS_SECRET  the Secret whose tls.crt signs the Prometheus certificate
#   SMOKE_CMD         runs the smoke suite against this install; unset, not run
#   PREVIOUS_RELEASE  scope namespace: the release whose files are installed by
#                     hand before the upgrade case (default v0.11.1)
#
# Exit 0 only when every step passed. The summary lists each step.

set -uo pipefail

DIST="${1:?usage: $0 <dir with the rendered release files>}"
SCOPE="${TOOLS_E2E_SCOPE:-namespace}"
TOOLS_IMG="${TOOLS_IMG:?set TOOLS_IMG to the tools image built with the release files}"
case "$SCOPE" in
    namespace) NS="${NS:-llm-d-sim}" ;;
    cluster) NS=scaling-manager-system ;;
    *) echo "TOOLS_E2E_SCOPE must be namespace or cluster, not '$SCOPE'" >&2; exit 2 ;;
esac
MODEL_NS="${MODEL_NS:-llm-d-sim}"
KEDA_NS="${KEDA_NS:-keda-system}"
MONITORING_NS="${MONITORING_NS:-scaling-manager-monitoring}"
PROMETHEUS_URL="${PROMETHEUS_URL:-https://kube-prometheus-stack-prometheus.$MONITORING_NS.svc.cluster.local:9090}"
PROMETHEUS_TLS_SECRET="${PROMETHEUS_TLS_SECRET:-prometheus-web-tls}"
SMOKE_CMD="${SMOKE_CMD:-}"
PREVIOUS_RELEASE="${PREVIOUS_RELEASE:-v0.11.1}"
REPO_DIR="$(pwd)"
NEG_NS=smt-negative
UPG_NS=smt-upgrade
DEPLOY=scaling-manager-controller-manager
STATUS_CM=scaling-manager-controller-status

WORK="$(mktemp -d)"
LOGS="${E2E_LOG_DIR:-$WORK/logs}"
mkdir -p "$LOGS" "$WORK/work"
chmod 0777 "$WORK/work"
SUMMARY="$LOGS/summary.txt"
: > "$SUMMARY"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
pass() { printf 'PASS  %s\n' "$1" | tee -a "$SUMMARY"; }
notrun() { printf 'NOT RUN  %s: %s\n' "$1" "$2" | tee -a "$SUMMARY"; }
fail() {
    printf 'FAIL  %s: %s\n' "$1" "$2" | tee -a "$SUMMARY"
    echo "=== diagnostics ==="
    kubectl get pods,jobs -n "$NS" -o wide 2>&1 | head -30
    kubectl get configmap "$STATUS_CM" -n "$NS" -o jsonpath='{.data.scalerTLS}{"\n"}{.data.scalerTLSAction}{"\n"}' 2>&1 | head -20
    kubectl logs -n "$NS" "deployment/$DEPLOY" --tail=60 2>&1 | tail -60
    echo "=== summary ==="; cat "$SUMMARY"
    echo "Logs: $LOGS"
    exit 1
}

wait_for() {
    local t="$1" what="$2" i=0
    shift 2
    until "$@" >/dev/null 2>&1; do
        i=$((i + 3))
        [ "$i" -lt "$t" ] || { log "timed out after ${t}s waiting for $what"; return 1; }
        sleep 3
    done
    log "ok: $what (${i}s)"
}

# smt_as <admin|owner> <log name> <args...>: the tools container, as an
# operator runs it: their kubeconfig read-only at /kube/config, a working
# directory at /work, the host network. Output in $LOGS/<log name>.log; the
# exit status is that of the tool.
smt_as() {
    local who="$1" name="$2"
    shift 2
    log "smt $* (as $who)"
    docker run --rm --network host --user "$(id -u):$(id -g)" \
        -v "$WORK/$who.kubeconfig:/kube/config:ro" -v "$WORK/work:/work" \
        "$TOOLS_IMG" "$@" > "$LOGS/$name.log" 2>&1
    local rc=$?
    log "  exit $rc; last line: $(tail -1 "$LOGS/$name.log")"
    return "$rc"
}

status() { kubectl get configmap "$STATUS_CM" -n "$NS" -o "jsonpath={.data.$1}" 2>/dev/null; }
status_is() { [ "$(status "$1")" = "$2" ]; }
settings_patch() { kubectl patch configmap scaling-manager-settings -n "$NS" --type merge -p "$1" >/dev/null; }
wait_settings_read() {
    local rv
    rv="$(kubectl get configmap scaling-manager-settings -n "$NS" -o jsonpath='{.metadata.resourceVersion}')"
    wait_for 120 "the controller read settings resourceVersion $rv" status_is settingsResourceVersion "$rv"
}
rv_of() { kubectl get configmap scaling-manager-settings -n "$1" -o jsonpath='{.metadata.resourceVersion}'; }

# controller_pods: the controller pods, by the selector of its Deployment.
controller_pods() {
    local sel
    sel="$(kubectl get deployment "$DEPLOY" -n "$NS" -o json | jq -r '.spec.selector.matchLabels | to_entries | map(.key + "=" + .value) | join(",")')"
    kubectl get pods -n "$NS" -l "$sel" --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}'
}

# objects_of <file...>: "<kind> <namespace> <name>" for every object but the
# preflight Job (made again each run) and the Deployment, whose status the
# controller updates (its .metadata.generation is compared instead).
objects_of() {
    yq e 'select(.kind != null) | [.kind, (.metadata.namespace // "-"), .metadata.name] | join(" ")' "$@" | grep -vE '^(Job|Deployment) '
}
# rv_snapshot <out> <objects file>: the resourceVersion of each object, or for
# a (Cluster)TriggerAuthentication, whose status KEDA writes, a digest of it
# without the status. See rv_snapshot.sh.
# shellcheck source=test/tools-install/rv_snapshot.sh
. "$(dirname "${BASH_SOURCE[0]}")/rv_snapshot.sh"

# preflight_clean <log>: the last preflight summary in an smt log has no FAIL.
preflight_clean() {
    local last
    last="$(grep -E '^ +[0-9]+ PASS, [0-9]+ WARN, [0-9]+ FAIL, [0-9]+ UNKNOWN' "$1" | tail -1)"
    [ -n "$last" ] && printf '%s' "$last" | grep -q ' 0 FAIL,'
}

run_smoke() {
    if [ -z "$SMOKE_CMD" ]; then
        notrun "smoke suite" "SMOKE_CMD unset"
        return 0
    fi
    settings_patch '{"data":{"passive":"false"}}'
    wait_settings_read || fail smoke "settings not read"
    wait_for 60 "status passiveDefault=false" status_is passiveDefault false || fail smoke "passiveDefault is '$(status passiveDefault)'"
    log "running the smoke suite against this install: $SMOKE_CMD"
    (cd "$REPO_DIR" && bash -c "$SMOKE_CMD") > "$LOGS/smoke.log" 2>&1
    local rc=$? plain ran counts spec_n
    plain="$(sed 's/\x1b\[[0-9;]*m//g' "$LOGS/smoke.log")"
    ran="$(printf '%s\n' "$plain" | grep -oE 'Ran [0-9]+ of [0-9]+ Specs' | tail -1)"
    counts="$(printf '%s\n' "$plain" | grep -oE '[0-9]+ Passed \| [0-9]+ Failed \| [0-9]+ Pending \| [0-9]+ Skipped' | tail -1)"
    log "smoke: exit $rc, ${ran:-no Ran line}, ${counts:-no counts}"
    spec_n="$(printf '%s' "$ran" | awk '{print $2}')"
    [ "${spec_n:-0}" -gt 0 ] || fail smoke "ran 0 specs"
    [ "$rc" -eq 0 ] || fail smoke "exit $rc ($ran: $counts); see $LOGS/smoke.log"
    pass "smoke suite against the tools install: $ran: $counts, exit 0"
    settings_patch '{"data":{"passive":"true"}}'
    wait_settings_read || fail "back to passive" "settings not read"
}

# --- kubeconfigs and inputs --------------------------------------------------

# Without jq or sha256sum every object of the resourceVersion check would read
# absent on both sides, and absent equals absent.
for tool in jq sha256sum; do
    command -v "$tool" >/dev/null 2>&1 || fail setup "$tool is not installed"
done
kubectl config view --minify --flatten > "$WORK/admin.kubeconfig" || fail setup "cannot read the current kubeconfig"
chmod 0600 "$WORK/admin.kubeconfig"
server="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
case "$server" in https://127.0.0.1:*|https://localhost:*) ;; *) log "note: the API server is $server" ;; esac
kubectl get secret "$PROMETHEUS_TLS_SECRET" -n "$MONITORING_NS" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$WORK/work/prometheus-ca.crt"
[ -s "$WORK/work/prometheus-ca.crt" ] || fail setup "no tls.crt in Secret $PROMETHEUS_TLS_SECRET in $MONITORING_NS"
chmod 0644 "$WORK/work/prometheus-ca.crt"
docker image inspect "$TOOLS_IMG" >/dev/null 2>&1 || fail setup "no image $TOOLS_IMG"
log "scope $SCOPE: controller in $NS, models in $MODEL_NS, KEDA in $KEDA_NS; tools image $TOOLS_IMG"
smt_as admin version version || fail setup "the tools image does not run"
pass "setup: admin kubeconfig ($server), Prometheus CA, tools image $(head -1 "$LOGS/version.log")"

INSTALL_FLAGS=(--prometheus-url "$PROMETHEUS_URL" --prometheus-ca prometheus-ca.crt)

# Every object the install writes or the admin wrote, for the resourceVersion check.
mkdir -p "$WORK/files"
for f in "$DIST"/scaling-manager-*.yaml; do
    sed -e "s/__NAMESPACE__/$NS/g" -e "s/__MODEL_NAMESPACE__/$MODEL_NS/g" "$f" > "$WORK/files/${f##*/}"
done
if [ "$SCOPE" = cluster ]; then
    objects_of "$WORK/files/scaling-manager-cluster.yaml" "$WORK/files/scaling-manager-preflight-cluster.yaml" \
        "$WORK/files/scaling-manager-model-namespace.yaml" > "$WORK/objects.txt"
else
    objects_of "$WORK/files/scaling-manager-namespaced.yaml" "$WORK/files/scaling-manager-preflight.yaml" \
        "$WORK/files/scaling-manager-model-namespace.yaml" > "$WORK/objects.txt"
    printf 'ConfigMap %s scaling-manager-admin-setup\n' "$NS" >> "$WORK/objects.txt"
fi
objects_of "$WORK/files/scaling-manager-settings.yaml" "$WORK/files/scaling-manager-scaler-tls.yaml" "$WORK/files/scaling-manager-keda-auth.yaml" \
    | sed "s/__KEDA_NAMESPACE__/$KEDA_NS/" >> "$WORK/objects.txt"
printf 'Secret %s scaling-manager-prometheus-ca\nSecret %s scaling-manager-external-scaler-%s\n' "$NS" "$KEDA_NS" "$NS" >> "$WORK/objects.txt"
[ "$(grep -c . "$WORK/objects.txt")" -gt 30 ] || fail setup "only $(grep -c . "$WORK/objects.txt") objects listed for the resourceVersion check"

# make_owner <namespace> <name>: the namespace owner of <namespace>, a
# ServiceAccount bound to the stock admin ClusterRole there and nothing else,
# as a token kubeconfig $WORK/<name>.kubeconfig (smt_as <name> ...). Fails the
# run unless that account may not create ClusterRoleBindings and may create
# Deployments in the namespace.
make_owner() {
    local ns="$1" name="$2" token ca can_crb can_dep
    kubectl create serviceaccount smt-owner -n "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null || fail owner "cannot create ServiceAccount smt-owner in $ns"
    kubectl create rolebinding smt-owner-admin -n "$ns" --clusterrole=admin --serviceaccount="$ns:smt-owner" --dry-run=client -o yaml \
        | kubectl apply -f - >/dev/null || fail owner "cannot bind admin to smt-owner in $ns"
    token="$(kubectl create token smt-owner -n "$ns" --duration=4h)" || fail owner "no token for smt-owner in $ns"
    ca="$(kubectl config view --minify --flatten -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
    printf 'apiVersion: v1\nkind: Config\nclusters:\n- name: kind\n  cluster:\n    server: %s\n    certificate-authority-data: %s\ncontexts:\n- name: owner\n  context:\n    cluster: kind\n    user: smt-owner\n    namespace: %s\ncurrent-context: owner\nusers:\n- name: smt-owner\n  user:\n    token: %s\n' \
        "$server" "$ca" "$ns" "$token" > "$WORK/$name.kubeconfig"
    chmod 0600 "$WORK/$name.kubeconfig"
    can_crb="$(kubectl --kubeconfig "$WORK/$name.kubeconfig" auth can-i create clusterrolebindings 2>/dev/null)"
    can_dep="$(kubectl --kubeconfig "$WORK/$name.kubeconfig" auth can-i create deployments -n "$ns" 2>/dev/null)"
    [ "$can_crb" = no ] || fail owner "the owner of $ns may create clusterrolebindings ('$can_crb'): the run would prove nothing about least privilege"
    [ "$can_dep" = yes ] || fail owner "the owner of $ns may not create deployments there ('$can_dep'): the token does not work"
}

# drop_cluster_objects <namespace>: the ClusterRoles and bindings a namespaced
# install made, which carry the namespace in their names.
drop_cluster_objects() {
    kubectl get clusterrole,clusterrolebinding -o name 2>/dev/null | grep -E "/scaling-manager-.*-$1\$" \
        | xargs -r kubectl delete --ignore-not-found >/dev/null 2>&1
}

# --- namespace scope ---------------------------------------------------------

if [ "$SCOPE" = namespace ]; then
    make_owner "$NS" owner
    pass "owner: ServiceAccount smt-owner with the admin ClusterRole in $NS; auth can-i create clusterrolebindings: no"

    # install before admin-setup: exit 3, nothing created.
    smt_as owner install-before-admin install -n "$NS" "${INSTALL_FLAGS[@]}"
    rc=$?
    [ "$rc" = 3 ] || fail "install before admin-setup" "exit $rc, want 3"
    grep -q "smt admin-setup -n $NS" "$LOGS/install-before-admin.log" || fail "install before admin-setup" "no admin-setup line printed"
    kubectl get configmap scaling-manager-settings -n "$NS" >/dev/null 2>&1 && fail "install before admin-setup" "the settings were created"
    pass "install before admin-setup exits 3, prints the admin-setup line, creates nothing"

    smt_as admin admin-setup admin-setup -n "$NS" || fail admin-setup "exit $?"
    np="$(kubectl get networkpolicy scaling-manager-external-scaler -n "$NS" -o json | jq -r '[.spec.ingress[].from[]?.namespaceSelector.matchLabels["kubernetes.io/metadata.name"] // empty] | unique | join(",")')"
    [ "$np" = "$KEDA_NS" ] || fail admin-setup "the NetworkPolicy admits '$np', not $KEDA_NS"
    kubectl get deployment "$DEPLOY" -n "$NS" >/dev/null 2>&1 && fail admin-setup "admin-setup created the controller"
    pass "admin-setup: KEDA found in $KEDA_NS without --keda-namespace, written into the NetworkPolicy; no controller"

    kubectl patch configmap scaling-manager-admin-setup -n "$NS" --type merge -p '{"data":{"adminObjectsSha256":"edited"}}' >/dev/null
    smt_as owner install-edited-stamp install -n "$NS" "${INSTALL_FLAGS[@]}"
    rc=$?
    [ "$rc" = 3 ] || fail "edited stamp" "exit $rc, want 3"
    smt_as admin admin-setup-again admin-setup -n "$NS" || fail "admin-setup again" "exit $?"
    pass "a stamp edited to another hash makes install exit 3; admin-setup again restores it"

    smt_as owner install install -n "$NS" "${INSTALL_FLAGS[@]}" || { rc=$?; cat "$LOGS/install.log"; fail install "exit $rc"; }
    grep -q "smt admin-finish -n $NS" "$LOGS/install.log" || fail install "install does not print the admin-finish line"
    preflight_clean "$LOGS/install.log" || fail install "the preflight after the install has a FAIL"
    pass "install as the owner: exit 0, the preflight after it has no FAIL, prints the admin-finish line"

    smt_as admin admin-finish admin-finish -n "$NS" || { rc=$?; cat "$LOGS/admin-finish.log"; fail admin-finish "exit $rc"; }
    case "$(status scalerTLS)" in "mutual TLS"*) ;; *) fail admin-finish "scalerTLS is '$(status scalerTLS)'" ;; esac
    preflight_clean "$LOGS/admin-finish.log" || fail admin-finish "the last preflight has a FAIL"
    kubectl get clusterrolebinding "scaling-manager-preflight-$NS" >/dev/null 2>&1 && fail admin-finish "the preflight binding is still there"
    pass "admin-finish: scalerTLS $(status scalerTLS | cut -c1-60); preflight without FAIL; ClusterRoleBinding scaling-manager-preflight-$NS removed"
else
    smt_as admin admin-setup-cluster admin-setup -n "$NS" --scope cluster
    rc=$?
    [ "$rc" = 2 ] && grep -q 'smt install --scope cluster' "$LOGS/admin-setup-cluster.log" || fail "admin-setup --scope cluster" "exit $rc, want 2 pointing at install --scope cluster"
    pass "admin-setup --scope cluster is refused (exit 2) and points at install --scope cluster"

    smt_as admin install install --scope cluster --keda-namespace "$KEDA_NS" --model-namespace "$MODEL_NS" "${INSTALL_FLAGS[@]}" \
        || { rc=$?; cat "$LOGS/install.log"; fail "install --scope cluster" "exit $rc"; }
    case "$(status scalerTLS)" in "mutual TLS"*) ;; *) fail "install --scope cluster" "scalerTLS is '$(status scalerTLS)'" ;; esac
    preflight_clean "$LOGS/install.log" || fail "install --scope cluster" "the last preflight has a FAIL"
    kubectl get clusterrolebinding scaling-manager-preflight >/dev/null 2>&1 && fail "install --scope cluster" "the preflight binding is still there"
    kubectl get podmonitors.monitoring.coreos.com scaling-manager-modelserver-metrics -n "$MODEL_NS" >/dev/null || fail "install --scope cluster" "no PodMonitor in $MODEL_NS"
    pass "install --scope cluster: scalerTLS $(status scalerTLS | cut -c1-60); preflight without FAIL; the PodMonitor in $MODEL_NS; ClusterRoleBinding scaling-manager-preflight removed"
fi

# --- the smoke suite, then re-runs --------------------------------------------

run_smoke

rv_snapshot "$WORK/rv-before.txt" "$WORK/objects.txt"
gen_before="$(kubectl get deployment "$DEPLOY" -n "$NS" -o jsonpath="{.metadata.generation}")"
pods_before="$(controller_pods)"
[ -n "$pods_before" ] || fail rerun "no running controller pod"
if [ "$SCOPE" = namespace ]; then
    smt_as owner install-again install -n "$NS" "${INSTALL_FLAGS[@]}" || fail "second install" "exit $?"
else
    smt_as admin install-again install --scope cluster --keda-namespace "$KEDA_NS" --model-namespace "$MODEL_NS" "${INSTALL_FLAGS[@]}" || fail "second install" "exit $?"
fi
rv_snapshot "$WORK/rv-after.txt" "$WORK/objects.txt"
gen_after="$(kubectl get deployment "$DEPLOY" -n "$NS" -o jsonpath="{.metadata.generation}")"
[ -n "$gen_before" ] && [ "$gen_before" = "$gen_after" ] || fail "second install" "the controller Deployment generation moved from $gen_before to $gen_after"
changed="$(diff "$WORK/rv-before.txt" "$WORK/rv-after.txt" | grep '^>' || true)"
[ -z "$changed" ] || fail "second install" "resourceVersion (or TriggerAuthentication spec digest) changed: $(echo $changed)"
present="$(grep -vc ' absent$' "$WORK/rv-after.txt")"
listed="$(grep -c . "$WORK/objects.txt")"
# A read that fails is absent before and after, and passes; most must be readable.
[ "$present" -gt $((listed / 2)) ] || fail "second install" "only $present of $listed listed objects could be read; the check compared absent with absent"
digests="$(grep -c ' spec:[0-9a-f]\{64\}$' "$WORK/rv-after.txt")"
[ "$digests" -gt 0 ] || fail "second install" "no ClusterTriggerAuthentication was compared by its spec digest; the KEDA auth went unchecked"
pass "a second install changes no resourceVersion ($present objects, $digests TriggerAuthentication by spec digest, the preflight Job aside) and not the controller generation ($gen_after)"

if [ "$SCOPE" = namespace ]; then
    smt_as admin admin-finish-again admin-finish -n "$NS" || fail "second admin-finish" "exit $?"
fi
pods_after="$(controller_pods)"
[ "$pods_after" = "$pods_before" ] || fail "re-run restarts nothing" "controller pods were '$pods_before', now '$pods_after'"
pass "the re-run restarts nothing: the controller pod is the same ($pods_after)"

rv="$(rv_of "$NS")"
url="$(kubectl get configmap scaling-manager-settings -n "$NS" -o jsonpath='{.data.prometheusURL}')"
other=(--prometheus-url https://prometheus.other.example:9090 --prometheus-ca prometheus-ca.crt)
if [ "$SCOPE" = namespace ]; then
    smt_as owner install-other-url install -n "$NS" "${other[@]}" || fail "another --prometheus-url" "exit $?"
else
    smt_as admin install-other-url install --scope cluster --keda-namespace "$KEDA_NS" --model-namespace "$MODEL_NS" "${other[@]}" || fail "another --prometheus-url" "exit $?"
fi
[ "$(rv_of "$NS")" = "$rv" ] || fail "another --prometheus-url" "the settings resourceVersion changed"
[ "$(kubectl get configmap scaling-manager-settings -n "$NS" -o jsonpath='{.data.prometheusURL}')" = "$url" ] || fail "another --prometheus-url" "prometheusURL changed"
grep -q "kubectl patch configmap scaling-manager-settings -n $NS --type merge" "$LOGS/install-other-url.log" || fail "another --prometheus-url" "no kubectl patch printed"
pass "a re-run with another --prometheus-url leaves scaling-manager-settings as it was and prints the patch"

# --- negative control: a wrong Prometheus in a fresh namespace ------------------

kubectl create namespace "$NEG_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
smt_as admin neg-admin-setup admin-setup -n "$NEG_NS" --keda-namespace "$KEDA_NS" || fail "negative control" "admin-setup in $NEG_NS: exit $?"
make_owner "$NEG_NS" owner-neg
smt_as owner-neg neg-install install -n "$NEG_NS" --prometheus-url https://prometheus.invalid.svc.cluster.local:9090
rc=$?
[ "$rc" = 1 ] || fail "negative control" "a wrong --prometheus-url: exit $rc, want 1"
grep -qE '^ +FAIL +prometheus.reachable' "$LOGS/neg-install.log" || fail "negative control" "the output does not name prometheus.reachable"
kubectl get deployment "$DEPLOY" -n "$NEG_NS" >/dev/null 2>&1 && fail "negative control" "a controller Deployment was created"
kubectl get configmap scaling-manager-settings -n "$NEG_NS" >/dev/null 2>&1 && fail "negative control" "the settings with the wrong URL were left behind"
pass "negative control: a wrong --prometheus-url in $NEG_NS, as its owner, exits 1, names prometheus.reachable, creates no controller and leaves no settings"
# The wrong URL does not stick: the same command with the right one installs.
# Namespace scope only: beside a cluster-wide controller a namespaced one is
# an overlap, which the preflight FAILs.
if [ "$SCOPE" = namespace ]; then
    smt_as owner-neg neg-install-fixed install -n "$NEG_NS" "${INSTALL_FLAGS[@]}" || fail "negative control" "the run with the right --prometheus-url: exit $?"
    [ "$(kubectl get configmap scaling-manager-settings -n "$NEG_NS" -o jsonpath='{.data.prometheusURL}')" = "$PROMETHEUS_URL" ] \
        || fail "negative control" "the settings do not hold the right URL"
    pass "after the failed run, the right --prometheus-url installs (exit 0) and is the one in the settings"
fi
kubectl delete namespace "$NEG_NS" --wait=false >/dev/null 2>&1
drop_cluster_objects "$NEG_NS"

# --- upgrade from a hand install of the previous release ------------------------

if [ "$SCOPE" = namespace ]; then
    up="$WORK/previous"
    mkdir -p "$up"
    url="https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$PREVIOUS_RELEASE"
    for f in namespaced settings preflight scaler-tls model-namespace; do
        curl -fsSL --retry 5 --max-time 120 -o "$up/scaling-manager-$f.yaml" "$url/scaling-manager-$f.yaml" || fail upgrade "cannot download scaling-manager-$f.yaml of $PREVIOUS_RELEASE"
    done
    curl -fsSL --retry 5 --max-time 120 -o "$up/SHA256SUMS" "$url/SHA256SUMS" || fail upgrade "cannot download SHA256SUMS of $PREVIOUS_RELEASE"
    (cd "$up" && sha256sum -c --ignore-missing --quiet SHA256SUMS) || fail upgrade "the $PREVIOUS_RELEASE files do not match their SHA256SUMS"
    kubectl create namespace "$UPG_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    for f in "$up"/scaling-manager-*.yaml; do
        sed -i -e "s/__NAMESPACE__/$UPG_NS/g" -e "s/__MODEL_NAMESPACE__/$UPG_NS/g" "$f"
    done
    sed -i "s#kubernetes.io/metadata.name: keda\$#kubernetes.io/metadata.name: $KEDA_NS#" "$up/scaling-manager-namespaced.yaml"
    sed -i -e "s|^  prometheusURL: \"\"|  prometheusURL: \"$PROMETHEUS_URL\"|" \
        -e "s|^  prometheusCACertPath: \"\"|  prometheusCACertPath: \"/etc/scaling-manager-prometheus-ca/ca.crt\"|" "$up/scaling-manager-settings.yaml"
    {
        kubectl apply -f "$up/scaling-manager-namespaced.yaml" -l scaling-manager.llmd.ai/part=admin \
            && kubectl apply -f "$up/scaling-manager-preflight.yaml" -l scaling-manager.llmd.ai/part=admin \
            && kubectl apply -f "$up/scaling-manager-model-namespace.yaml" \
            && kubectl create secret generic scaling-manager-prometheus-ca -n "$UPG_NS" --from-file=ca.crt="$WORK/work/prometheus-ca.crt" \
            && kubectl create -f "$up/scaling-manager-settings.yaml" \
            && kubectl create -f "$up/scaling-manager-scaler-tls.yaml" \
            && kubectl apply -f "$up/scaling-manager-namespaced.yaml" -l scaling-manager.llmd.ai/part=controller \
            && kubectl rollout status "deployment/$DEPLOY" -n "$UPG_NS" --timeout=300s
    } > "$LOGS/upgrade-hand-install.log" 2>&1 || { tail -20 "$LOGS/upgrade-hand-install.log"; fail upgrade "the hand install of $PREVIOUS_RELEASE failed"; }
    prev_image="$(kubectl get deployment "$DEPLOY" -n "$UPG_NS" -o jsonpath='{.spec.template.spec.containers[0].image}')"
    rv="$(rv_of "$UPG_NS")"
    pass "upgrade: the $PREVIOUS_RELEASE files installed by hand in $UPG_NS ($prev_image)"

    make_owner "$UPG_NS" owner-upg
    smt_as owner-upg upgrade-no-stamp install -n "$UPG_NS" "${INSTALL_FLAGS[@]}"
    rc=$?
    [ "$rc" = 3 ] || fail upgrade "install over a hand install without admin-setup: exit $rc, want 3"
    smt_as admin upgrade-admin-setup admin-setup -n "$UPG_NS" --keda-namespace "$KEDA_NS" || fail upgrade "admin-setup: exit $?"
    smt_as owner-upg upgrade-install install -n "$UPG_NS" "${INSTALL_FLAGS[@]}" || { rc=$?; tail -30 "$LOGS/upgrade-install.log"; fail upgrade "install as the owner: exit $rc"; }
    [ "$(rv_of "$UPG_NS")" = "$rv" ] || fail upgrade "the settings changed"
    new_version="$(kubectl get deployment "$DEPLOY" -n "$UPG_NS" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/version}')"
    new_image="$(kubectl get deployment "$DEPLOY" -n "$UPG_NS" -o jsonpath='{.spec.template.spec.containers[0].image}')"
    want_version="$(yq 'select(.kind == "Deployment") | .metadata.labels["app.kubernetes.io/version"]' "$WORK/files/scaling-manager-namespaced.yaml")"
    [ "$new_version" = "$want_version" ] || fail upgrade "the controller is labelled '$new_version', not $want_version"
    [ "$new_image" != "$prev_image" ] || fail upgrade "the controller image did not change"
    pass "upgrade, as the owner of $UPG_NS: install exits 3 until admin-setup, then upgrades $PREVIOUS_RELEASE to $new_version ($new_image); settings unchanged"
    kubectl delete namespace "$UPG_NS" --wait=false >/dev/null 2>&1
    drop_cluster_objects "$UPG_NS"
else
    notrun upgrade "namespace scope only: the stamp and admin-setup are namespace-scope steps"
fi

echo "=== summary ==="
cat "$SUMMARY"
echo "Logs: $LOGS"
