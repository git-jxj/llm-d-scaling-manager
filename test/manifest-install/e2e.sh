#!/usr/bin/env bash
#
# Installs the scaling manager from the one release manifest,
# scaling-manager.yaml, with the commands of docs/guides/kubectl-install
# (proposal PR #404, phase 1), and checks each step.
# `make test-e2e-manifest-install` runs it on a kind cluster whose
# prerequisites (Prometheus, KEDA, the llm-d CRDs, the model servers and the
# EPP) the existing e2e setup installed, without the scaling manager.
#
#   test/manifest-install/e2e.sh <dir with the rendered release files>
#
# It copies the manifest to a scratch directory and works there. The one
# exception is SMOKE_CMD, the existing smoke suite, run against this install.
#
# Environment:
#   MODEL_NS        the namespace with the model servers (default llm-d-sim)
#   KEDA_NS         where KEDA runs (default keda-system)
#   MONITORING_NS   where the e2e Prometheus runs (default scaling-manager-monitoring)
#   PROMETHEUS_URL  default the e2e Prometheus Service in MONITORING_NS
#   PROMETHEUS_TLS_SECRET  the Secret in MONITORING_NS whose tls.crt signs
#                   Prometheus's certificate (default prometheus-web-tls)
#   SMOKE_CMD       a command that runs the smoke suite against this install
#                   (with passive off); unset, that step does not run
#   KEEP_INSTALL    true: stop before the uninstall
#
# Exit 0 only when every step passed. The summary lists each step.

set -uo pipefail

DIST="${1:?usage: $0 <dir with the rendered release files>}"
NS=scaling-manager-system
MODEL_NS="${MODEL_NS:-llm-d-sim}"
KEDA_NS="${KEDA_NS:-keda-system}"
MONITORING_NS="${MONITORING_NS:-scaling-manager-monitoring}"
PROMETHEUS_URL="${PROMETHEUS_URL:-https://kube-prometheus-stack-prometheus.$MONITORING_NS.svc.cluster.local:9090}"
PROMETHEUS_TLS_SECRET="${PROMETHEUS_TLS_SECRET:-prometheus-web-tls}"
SMOKE_CMD="${SMOKE_CMD:-}"
KEEP_INSTALL="${KEEP_INSTALL:-false}"
REPO_DIR="$(pwd)"
# An unlabelled namespace with a ScaledObject on the scaler: the negative
# control for the label opt-in.
OUTSIDE_NS=scaling-manager-e2e-outside
LABEL=scaling-manager.llmd.ai/managed
# The admin label namespaceSelector requires beside the opt-in label: one a
# tenant who can label namespaces does not set (the proposal, "The label
# alone is not consent"). The smoke suite puts both on the namespaces it
# creates (E2E_NAMESPACE_LABELS).
ADMIT=e2e.scaling-manager.llmd.ai/admitted
M=scaling-manager.yaml
STATUS_CM=scaling-manager-controller-status
DEPLOY=scaling-manager-controller-manager
INSTANCE="app.kubernetes.io/instance=$NS"

WORK="$(mktemp -d)"
LOGS="${E2E_LOG_DIR:-$WORK/logs}"
mkdir -p "$LOGS"
SUMMARY="$LOGS/summary.txt"
: > "$SUMMARY"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
pass() { printf 'PASS  %s\n' "$1" | tee -a "$SUMMARY"; }
notrun() { printf 'NOT RUN  %s: %s\n' "$1" "$2" | tee -a "$SUMMARY"; }

fail() {
    printf 'FAIL  %s: %s\n' "$1" "$2" | tee -a "$SUMMARY"
    diagnostics
    echo "=== summary ==="; cat "$SUMMARY"
    echo "Logs and the files as applied: $LOGS, $WORK"
    exit 1
}

diagnostics() {
    echo "=== diagnostics ==="
    kubectl get pods -n "$NS" -o wide 2>&1 | head -10
    kubectl get configmap "$STATUS_CM" -n "$NS" -o yaml 2>&1 | grep -v -E 'BEGIN CERT|END CERT|^\s+[A-Za-z0-9+/=]{40,}$' | head -100
    kubectl logs -n "$NS" "deployment/$DEPLOY" --tail=100 2>&1 | tail -100
    kubectl logs -n "$NS" "deployment/$DEPLOY" --previous --tail=30 2>&1 | tail -30
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

# status <key>: one key of the status ConfigMap; dots in a key (preflight.<check>)
# are escaped, or jsonpath reads them as nested fields.
status() { kubectl get configmap "$STATUS_CM" -n "$NS" -o "jsonpath={.data.${1//./\\.}}" 2>/dev/null; }
status_is() { [ "$(status "$1")" = "$2" ]; }
status_has_prefix() { case "$(status "$1")" in "$2"*) return 0 ;; esac; return 1; }
status_contains() { case "$(status "$1")" in *"$2"*) return 0 ;; esac; return 1; }
settings_patch() { kubectl patch configmap scaling-manager-settings -n "$NS" --type merge -p "$1" >/dev/null; }
secret_sha() { kubectl get secret "$2" -n "$1" -o jsonpath='{.data.tls\.crt}' 2>/dev/null | sha256sum | cut -c1-16; }
rollout() { kubectl rollout status "deployment/$DEPLOY" -n "$NS" --timeout=300s; }
ready_ok() { status_is ready ok; }

# --- 0. the file, as an operator downloads it --------------------------------

cp "$DIST/$M" "$DIST/scaling-manager-grants.yaml" "$DIST/scaling-manager-monitoring.yaml" "$DIST/SHA256SUMS" "$WORK"/ \
    || fail files "no $M, its grants and monitoring files, or SHA256SUMS in $DIST"
cd "$WORK" || exit 1
sha256sum -c --ignore-missing --quiet SHA256SUMS > "$LOGS/sums.log" 2>&1 && grep -q "  $M\$" SHA256SUMS \
    || fail files "$M does not match SHA256SUMS: $(cat "$LOGS/sums.log")"
pass "files: $M matches SHA256SUMS (signature: checked on a release with cosign verify-blob, not here)"
log "Kubernetes: $(kubectl version -o json 2>/dev/null | grep -m1 gitVersion | tr -d ' ",')"

# KEDA in another namespace: the edit the file's header gives.
if [ "$KEDA_NS" != keda ]; then
    sedline="$(sed -n '1,6{s/^# KEDA in another namespace than keda: //p}' "$M")"
    [ -n "$sedline" ] || fail "keda namespace" "the header gives no sed for KEDA elsewhere"
    eval "${sedline//<ns>/$KEDA_NS}" || fail "keda namespace" "the header's sed failed"
    left="$(grep -nE '(^|[ :=/])keda$' "$M" || true)"
    [ -z "$left" ] || fail "keda namespace" "keda still named after the sed: $left"
    pass "the header's sed moved every mention of keda to $KEDA_NS"
fi
kubectl get namespace "$MODEL_NS" >/dev/null 2>&1 || fail namespace "no model namespace $MODEL_NS (the e2e setup makes it)"

# --- 1. command 1: the manifest ----------------------------------------------

kubectl apply --server-side -f "$M" > "$LOGS/apply-1.log" 2>&1 || { cat "$LOGS/apply-1.log"; fail "apply" "kubectl apply --server-side -f $M"; }
pass "command 1: kubectl apply --server-side -f $M ($(grep -c . "$LOGS/apply-1.log") objects)"

# Before the settings exist the controller waits for them, live, and says
# why in its status, instead of exiting into the kubelet's crash back-off.
pod_restarts() { kubectl get pods -n "$NS" -l control-plane=controller-manager -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' | awk '{s += $1} END {print s + 0}'; }
wait_for 240 "ready says the settings are missing" status_contains ready "no ConfigMap scaling-manager-settings" \
    || fail "waiting" "ready is '$(status ready)' before the settings exist"
sleep 15
[ "$(pod_restarts)" = 0 ] || fail "waiting" "the controller restarted $(pod_restarts) times while waiting for its settings"
pass "with no settings the controller waits, live (0 restarts): ready '$(status ready)'"

# --- 2. command 2: the settings ----------------------------------------------
#
# The e2e Prometheus has a private CA: its certificate goes in the fixed Secret
# the Deployment mounts, and caFromSecret points the controller at it.
kubectl get secret "$PROMETHEUS_TLS_SECRET" -n "$MONITORING_NS" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$WORK/prometheus-ca.crt"
[ -s "$WORK/prometheus-ca.crt" ] || fail settings "no tls.crt in Secret $PROMETHEUS_TLS_SECRET in $MONITORING_NS"
kubectl create secret generic scaling-manager-prometheus-ca -n "$NS" --from-file=ca.crt="$WORK/prometheus-ca.crt" >/dev/null \
    || fail settings "could not create Secret scaling-manager-prometheus-ca"
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: scaling-manager-settings\n  namespace: %s\ndata:\n  prometheusURL: %s\n  caFromSecret: "true"\n  namespaceSelector: "%s=true,%s=true"\n' \
    "$NS" "$PROMETHEUS_URL" "$LABEL" "$ADMIT" > settings.yaml
kubectl apply --server-side -f settings.yaml > "$LOGS/apply-settings.log" 2>&1 || { cat "$LOGS/apply-settings.log"; fail settings "kubectl apply --server-side -f settings.yaml"; }
pass "command 2: kubectl apply --server-side -f settings.yaml (prometheusURL, caFromSecret, namespaceSelector)"

# --- 3. command 3: the label -------------------------------------------------

kubectl label namespace "$MODEL_NS" "$LABEL=true" "$ADMIT=true" --overwrite >/dev/null || fail label "kubectl label namespace $MODEL_NS"
# Nothing in the e2e setup scrapes the model servers: the release's
# monitoring file, in the model namespace (appendix D).
kubectl apply -n "$MODEL_NS" -f scaling-manager-monitoring.yaml -l scaling-manager.llmd.ai/monitor=modelserver \
    > "$LOGS/podmonitor.log" 2>&1 || { cat "$LOGS/podmonitor.log"; fail label "the model-server PodMonitor"; }
kubectl get podmonitor scaling-manager-modelserver-metrics -n "$MODEL_NS" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/instance}' | grep -qx "$NS" \
    || fail label "the PodMonitor in $MODEL_NS does not carry the install's instance label"
pass "command 3: kubectl label namespace $MODEL_NS $LABEL=true (and scaling-manager-monitoring.yaml there)"

# --- 4. the controller starts in place once the settings exist, and gets ready

rollout > "$LOGS/rollout-1.log" 2>&1 || fail controller "the Deployment did not become Ready: $(cat "$LOGS/rollout-1.log")"
# prometheusURL and caFromSecret are read at start: the waiting controller
# reads them and starts, with no restart (--wait-for-settings).
wait_for 300 "the controller read prometheusURL" status_contains settingsSources "prometheusURL=settings" \
    || fail controller "settingsSources: '$(status settingsSources)'"
[ "$(pod_restarts)" = 0 ] || fail controller "the controller restarted $(pod_restarts) times on its way to reading the settings; it should start in place"
wait_for 300 "ready=ok" ready_ok || { kubectl get configmap "$STATUS_CM" -n "$NS" -o yaml | grep -E '^\s+(ready|preflight\.)' ; fail ready "ready is '$(status ready)'"; }
pass "ready ok; preflight: $(kubectl get configmap "$STATUS_CM" -n "$NS" -o json | jq -r '[.data | to_entries[] | select(.key | startswith("preflight.")) | .value | split(":")[0]] | group_by(.) | map("\(length) \(.[0])") | join(", ")')"
kubectl get configmap "$STATUS_CM" -n "$NS" -o json | jq -r '.data | to_entries[] | select(.key | startswith("preflight.")) | select(.value | startswith("ok") | not) | "      \(.key) \(.value)"' | cut -c1-220 | tee -a "$SUMMARY"
kubectl get events -n "$NS" --field-selector "involvedObject.name=$STATUS_CM" -o name | grep -q . \
    || fail ready "no Event on $STATUS_CM for the preflight checks"
pass "the preflight checks are Events on $STATUS_CM"

wait_for 120 "namespacesManaged=$MODEL_NS" status_is namespacesManaged "$MODEL_NS" || fail namespaces "namespacesManaged is '$(status namespacesManaged)'"
pass "namespacesManaged: $(status namespacesManaged)"

# --- 5. KEDA's side, written by the controller -------------------------------

wait_for 120 "scalerTLS mutual TLS" status_has_prefix scalerTLS "mutual TLS" || fail "scaler TLS" "scalerTLS is '$(status scalerTLS)'"
copy="scaling-manager-external-scaler-$NS"
[ -n "$(kubectl get secret "$copy" -n "$KEDA_NS" -o jsonpath='{.data.tls\.crt}')" ] || fail "scaler TLS" "KEDA's copy $copy in $KEDA_NS is empty"
[ "$(secret_sha "$KEDA_NS" "$copy")" = "$(secret_sha "$NS" scaling-manager-scaler-client)" ] \
    || fail "scaler TLS" "KEDA's copy does not hold the client certificate"
status_is kedaCopyStale false || fail "scaler TLS" "kedaCopyStale is '$(status kedaCopyStale)'"
kubectl get configmap scaling-manager-scaler-keda-auth -n "$NS" >/dev/null 2>&1 && fail "scaler TLS" "a marker ConfigMap exists; nothing on this path writes one"
# The in-controller keda.mtls judges this path by the controller's status,
# not by a marker; its first run can precede the status ConfigMap.
wait_for 150 "preflight.keda.mtls ok" status_has_prefix preflight.keda.mtls ok \
    || fail "scaler TLS" "preflight.keda.mtls is '$(status preflight.keda.mtls)'"
pass "mutual TLS with no step by a person: KEDA's copy in $KEDA_NS holds the client certificate; kedaCopyStale false; preflight.keda.mtls ok"

# --- 5b. a changed restart setting restarts the controller -------------------

restarts_more_than() { [ "$(pod_restarts)" -gt "$1" ]; }
before="$(pod_restarts)"
settings_patch '{"data":{"logVerbosity":"3"}}'
wait_for 180 "the controller restarted itself" restarts_more_than "$before" || fail "self-restart" "restartCount stayed $before after a restart setting changed"
rollout > "$LOGS/rollout-restart.log" 2>&1 || fail "self-restart" "the restarted controller did not become Ready"
wait_for 180 "settingsSources logVerbosity=settings" status_contains settingsSources "logVerbosity=settings" \
    || fail "self-restart" "settingsSources: '$(status settingsSources)'"
wait_for 180 "ready=ok again" ready_ok || fail "self-restart" "ready is '$(status ready)'"
pass "a changed restart setting (logVerbosity) restarted the controller once ($before -> $(pod_restarts)); ready ok again"

# --- 6. a re-apply keeps what the controller and the operator wrote ----------

ca_before="$(secret_sha "$NS" scaling-manager-scaler-ca)"
copy_before="$(secret_sha "$KEDA_NS" "$copy")"
settings_patch '{"data":{"utilizationShare":"shadow"}}'
kubectl apply --server-side -f "$M" > "$LOGS/apply-2.log" 2>&1 || { cat "$LOGS/apply-2.log"; fail "re-apply" "a second kubectl apply --server-side -f $M"; }
kubectl apply --server-side -f settings.yaml > "$LOGS/apply-settings-2.log" 2>&1 || { cat "$LOGS/apply-settings-2.log"; fail "re-apply" "a second apply of settings.yaml"; }
[ "$(secret_sha "$NS" scaling-manager-scaler-ca)" = "$ca_before" ] || fail "re-apply" "the scaler CA changed on a re-apply"
[ "$(secret_sha "$KEDA_NS" "$copy")" = "$copy_before" ] || fail "re-apply" "KEDA's copy changed on a re-apply"
[ "$(kubectl get configmap scaling-manager-settings -n "$NS" -o jsonpath='{.data.utilizationShare}')" = shadow ] \
    || fail "re-apply" "the live utilizationShare patch did not survive a re-apply of settings.yaml"
settings_patch '{"data":{"utilizationShare":"off"}}'
pass "re-apply of both files: scaler CA, KEDA's copy and a live switch kept"

# --- 6b. the per-namespace grants ----------------------------------------------

SA="system:serviceaccount:$NS:$DEPLOY"
can() { kubectl auth can-i "$1" "$2" -n "$MODEL_NS" --as="$SA" 2>/dev/null; }
[ "$(can patch pods)" = no ] || fail grants "the controller may patch pods in $MODEL_NS before the grant"
[ "$(can patch disaggregatedsets.disaggregatedset.x-k8s.io)" = no ] || fail grants "the controller may patch disaggregatedsets before the grant"
kubectl apply -n "$MODEL_NS" -f scaling-manager-grants.yaml -l scaling-manager.llmd.ai/grant=pod-writer > "$LOGS/grant.log" 2>&1 \
    || { cat "$LOGS/grant.log"; fail grants "applying the pod-writer grant"; }
[ "$(can patch pods)" = yes ] || fail grants "the controller may not patch pods in $MODEL_NS after the pod-writer grant"
[ "$(can patch disaggregatedsets.disaggregatedset.x-k8s.io)" = no ] || fail grants "the pod-writer grant also grants the role writer"
kubectl apply -n "$MODEL_NS" -f scaling-manager-grants.yaml -l scaling-manager.llmd.ai/grant=dset-role-writer > "$LOGS/grant-dset.log" 2>&1 \
    || { cat "$LOGS/grant-dset.log"; fail grants "applying the dset-role-writer grant"; }
[ "$(can patch disaggregatedsets.disaggregatedset.x-k8s.io)" = yes ] || fail grants "the controller may not patch disaggregatedsets after its grant"
pass "grants: patch on pods and on disaggregatedsets in $MODEL_NS only after each labelled grant is applied there"

# --- 7. negative control: a namespace that did not opt in --------------------

kubectl create namespace "$OUTSIDE_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
printf 'apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: outside\n  namespace: %s\nspec:\n  replicas: 1\n  selector:\n    matchLabels: {app: outside}\n  template:\n    metadata:\n      labels: {app: outside}\n    spec:\n      containers:\n      - name: pause\n        image: registry.k8s.io/pause:3.10\n---\napiVersion: keda.sh/v1alpha1\nkind: ScaledObject\nmetadata:\n  name: outside\n  namespace: %s\nspec:\n  scaleTargetRef: {name: outside}\n  minReplicaCount: 1\n  maxReplicaCount: 2\n  triggers:\n  - type: external\n    authenticationRef: {kind: ClusterTriggerAuthentication, name: %s}\n    metadata:\n      scalerAddress: scaling-manager-external-scaler.%s.svc:9090\n' \
    "$OUTSIDE_NS" "$OUTSIDE_NS" "$copy" "$NS" > outside.yaml
kubectl apply -f outside.yaml > "$LOGS/outside.log" 2>&1 || { cat "$LOGS/outside.log"; fail "outside" "applying the unlabelled namespace's ScaledObject"; }
wait_for 180 "namespacesRefused names $OUTSIDE_NS" status_contains namespacesRefused "$OUTSIDE_NS" \
    || fail "outside" "the scaler did not refuse $OUTSIDE_NS: namespacesRefused '$(status namespacesRefused)'"
pass "an unlabelled namespace is refused at the scaler: $(status namespacesRefused | grep "$OUTSIDE_NS" | cut -c1-160)"
kubectl label namespace "$OUTSIDE_NS" "$LABEL=true" --overwrite >/dev/null
wait_for 120 "the selector refuses the labelled $OUTSIDE_NS" status_contains namespacesRefused "$OUTSIDE_NS: the namespaceSelector" \
    || fail "outside" "labelled but outside the selector, $OUTSIDE_NS is not refused by it: '$(status namespacesRefused)'"
status_is namespacesManaged "$MODEL_NS" || fail "outside" "namespacesManaged is '$(status namespacesManaged)'"
pass "labelled but outside namespaceSelector, $OUTSIDE_NS is still refused: the label alone is not consent"
kubectl delete namespace "$OUTSIDE_NS" --wait=false >/dev/null

# --- 8. a model scales, through KEDA and the scaler over mutual TLS ----------

if [ -n "$SMOKE_CMD" ]; then
    settings_patch '{"data":{"passive":"false"}}'
    wait_for 60 "passiveDefault=false" status_is passiveDefault false || fail smoke "passiveDefault is '$(status passiveDefault)'"
    log "running the smoke suite against this install: $SMOKE_CMD"
    (cd "$REPO_DIR" && bash -c "$SMOKE_CMD") > "$LOGS/smoke.log" 2>&1
    rc=$?
    plain="$(sed 's/\x1b\[[0-9;]*m//g' "$LOGS/smoke.log")"
    ran="$(printf '%s\n' "$plain" | grep -oE 'Ran [0-9]+ of [0-9]+ Specs' | tail -1)"
    counts="$(printf '%s\n' "$plain" | grep -oE '[0-9]+ Passed \| [0-9]+ Failed \| [0-9]+ Pending \| [0-9]+ Skipped' | tail -1)"
    log "smoke: exit $rc, ${ran:-no Ran line}, ${counts:-no counts}"
    printf '%s\n' "$plain" | grep -E '^\s*\[FAIL\]' | sort -u | head -20
    [ "$(printf '%s' "$ran" | awk '{print $2}')" -gt 0 ] 2>/dev/null || fail smoke "ran 0 specs"
    [ "$rc" -eq 0 ] || fail smoke "exit $rc ($ran: $counts); see $LOGS/smoke.log"
    wait_for 120 "kedaConnection=current" status_is kedaConnection current || fail smoke "kedaConnection is '$(status kedaConnection)'"
    pass "smoke suite against the manifest install: $ran: $counts; kedaConnection current"
    settings_patch '{"data":{"passive":"true"}}'
else
    notrun "smoke suite" "SMOKE_CMD unset"
fi

if [ "$KEEP_INSTALL" = true ]; then
    notrun uninstall "KEEP_INSTALL=true"
    echo "=== summary ==="; cat "$SUMMARY"
    exit 0
fi

# --- 9. uninstall (appendix F) -----------------------------------------------

{
    kubectl delete scaledobject -A -l "$INSTANCE,app.kubernetes.io/managed-by=llm-d-scaling-manager" --ignore-not-found
    kubectl delete rolebinding,role,podmonitor -A -l "$INSTANCE" --ignore-not-found
    kubectl label namespace -l "$LABEL=true" "$LABEL-" "$ADMIT-"
    kubectl delete -f "$M" --ignore-not-found --wait=true --timeout=300s
} > "$LOGS/uninstall.log" 2>&1 || { cat "$LOGS/uninstall.log"; fail uninstall "a delete failed"; }
left=""
for r in $(kubectl api-resources --verbs=list -o name 2>/dev/null | grep -v -E '^(events|events.events.k8s.io)$'); do
    o="$(kubectl get "$r" -A -l "$INSTANCE" -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null | sed "s|^|$r |")"
    [ -z "$o" ] || left="$left$o"$'\n'
done
left="$(printf '%s' "$left" | grep . | sort -u)"
printf '%s\n' "$left" > "$LOGS/left-after-uninstall.txt"
[ -z "$left" ] || fail uninstall "left behind: $(echo $left)"
kubectl get namespace "$NS" >/dev/null 2>&1 && fail uninstall "namespace $NS is still there"
[ -z "$(kubectl get namespace -l "$LABEL=true" -o name)" ] || fail uninstall "a namespace still carries $LABEL"
pass "uninstall: nothing labelled $INSTANCE left, namespace $NS gone, no namespace labelled"

echo "=== summary ==="
cat "$SUMMARY"
echo "Logs: $LOGS"
