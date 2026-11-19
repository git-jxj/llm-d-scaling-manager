#!/usr/bin/env bash
#
# Renders config/capacity-planner (the planner's in-cluster mode) and asserts
# what docs/guides/capacity-planner/in-cluster.md promises. Every way of
# getting it wrong applies cleanly:
#
#   a Service, an Ingress or a Route on plain Kubernetes   the planner, which has
#                                                          no authentication, is
#                                                          reachable past RBAC
#   a cluster-scoped object by default                     a namespace admin can
#                                                          no longer apply it
#   --listen on anything but 127.0.0.1                     the same, from the Pod IP
#   a RoleBinding to another subject, a Role that grants   who can open it is not
#   more than port-forward                                 who the guide says
#   oauth-proxy passing the Route's Host                   every request is a 403
#   a token mounted into the planner                       an API credential in the
#                                                          one container that needs none
#   a planner image whose tag is not the controller's      the release ships two
#                                                          versions that disagree
#
# Then each check is run against a mutated copy of the render, and must fail
# it: a check that cannot fail is not a check.
#
# Needs yq, and kustomize ($KUSTOMIZE, else kustomize, else kubectl kustomize).
# Missing either is a failure, never a skip.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAIL=0
PASS=0
# Every assertion and negative control below, each one ok. A case dropped, or a
# run that stops early, fails on the count. Change it with the cases.
EXPECT_PASS=73

command -v yq >/dev/null 2>&1 || { echo "FAIL: yq is required (not skipping: a skipped check reads as a pass)"; exit 1; }

render() {
    if [ -n "${KUSTOMIZE:-}" ] && [ -x "${KUSTOMIZE}" ]; then
        "$KUSTOMIZE" build "$1" >"$2"
    elif command -v kustomize >/dev/null 2>&1; then
        command kustomize build "$1" >"$2"
    elif command -v kubectl >/dev/null 2>&1; then
        command kubectl kustomize "$1" >"$2"
    else
        echo "FAIL: neither kustomize nor kubectl is available to render the overlays"
        exit 1
    fi
}

# The controller's newTag: the planner image must carry the same tag.
WANT_TAG="$(awk '$1 == "newTag:" {gsub(/["\r]/, "", $2); print $2; exit}' config/base/manager/kustomization.yaml)"
[ -n "$WANT_TAG" ] || { echo "FAIL: no newTag in config/base/manager/kustomization.yaml"; exit 1; }

DEP='select(.kind == "StatefulSet" and .metadata.name == "capacity-planner")'
PLANNER="$DEP | .spec.template.spec.containers[] | select(.name == \"planner\")"
PROXY="$DEP | .spec.template.spec.containers[] | select(.name == \"oauth-proxy\")"
TOKEN_PATH=/var/run/secrets/kubernetes.io/serviceaccount

# Each v_* function prints one line per violation in the render $1, and
# nothing when it holds.

# Only these kinds, all namespaced, and none names a namespace: kubectl
# apply -n decides where it goes, and a namespace admin may apply it.
v_namespaced_only() {
    yq -N '.kind + "/" + .metadata.name' "$1" | while read -r obj; do
        case "${obj%%/*}" in
            ServiceAccount|StatefulSet|PersistentVolumeClaim|Role|RoleBinding|Service|Route|Pod) ;;
            *) echo "$obj is not one of the namespaced kinds this may ship" ;;
        esac
    done
    yq -N 'select(.metadata.namespace != null) | .kind + "/" + .metadata.name' "$1" | sed 's/$/ names a namespace/'
}

v_not_exposed() {
    yq -N 'select(.kind == "Service" or .kind == "Ingress" or .kind == "Route" or .kind == "Gateway" or .kind == "HTTPRoute") | .kind + "/" + .metadata.name' "$1" \
        | sed 's/$/ exposes the planner on plain Kubernetes/'
}

# The planner: loopback, no authentication of its own, the recordings.
v_planner_loopback() {
    local args
    args="$(yq -N "$PLANNER | .args[]" "$1" | tr '\n' ' ')"
    [ -n "$args" ] || { echo "no planner container, or it has no args"; return; }
    case " $args " in *" --listen=127.0.0.1:8080 "*) ;; *) echo "the planner does not --listen=127.0.0.1:8080: $args" ;; esac
    case " $args " in *" --auth=none "*) ;; *) echo "the planner does not say --auth=none: $args" ;; esac
    case " $args " in *listen-all-in-container*|*0.0.0.0*|*" --listen "*) echo "the planner may listen beyond loopback: $args" ;; esac
    case " $args " in *" --recordings=/recordings "*) ;; *) echo "the planner does not serve /recordings: $args" ;; esac
}

v_recordings_readonly() {
    [ "$(yq -N "$PLANNER | .volumeMounts[] | select(.mountPath == \"/recordings\") | .readOnly" "$1")" = true ] \
        || echo "the planner does not mount /recordings read-only"
    [ "$(yq -N "$DEP | .spec.template.spec.volumes[] | select(.name == \"recordings\") | .persistentVolumeClaim.readOnly" "$1")" = true ] \
        || echo "the recordings volume is not a read-only claim"
    [ "$(yq -N "$DEP | .spec.template.spec.volumes[] | select(.name == \"recordings\") | .persistentVolumeClaim.claimName" "$1")" = capacity-planner-recordings ] \
        || echo "the recordings volume does not claim capacity-planner-recordings"
}

# Non-root, read-only root, no escalation, no capabilities, memory bounded,
# in every container of every Pod template and Pod.
v_hardened() {
    local f="$1" spec
    for spec in '.spec.template.spec' '.spec'; do
        local sel
        if [ "$spec" = .spec ]; then sel='select(.kind == "Pod")'; else sel='select(.kind == "StatefulSet")'; fi
        yq -N "$sel | select($spec.securityContext.runAsNonRoot != true) | .kind + \"/\" + .metadata.name" "$f" | sed 's/$/: runAsNonRoot is not true/'
        yq -N "$sel | $spec.containers[] | select(.securityContext.readOnlyRootFilesystem != true) | .name" "$f" | sed 's/$/: readOnlyRootFilesystem is not true/'
        yq -N "$sel | $spec.containers[] | select(.securityContext.allowPrivilegeEscalation != false) | .name" "$f" | sed 's/$/: allowPrivilegeEscalation is not false/'
        yq -N "$sel | $spec.containers[] | select((.securityContext.capabilities.drop // []) | contains([\"ALL\"]) | not) | .name" "$f" | sed 's/$/: does not drop ALL capabilities/'
        yq -N "$sel | $spec.containers[] | select(.resources.requests.memory == null or .resources.limits.memory == null or .resources.requests.cpu == null) | .name" "$f" | sed 's/$/: resources are not set/'
    done
}

# Every image names a registry host, and none is Docker Hub (AGENTS.md).
v_images() {
    yq -N '.. | select(has("image")) | .image' "$1" 2>/dev/null | while read -r img; do
        local host="${img%%/*}"
        case "$img" in */*) ;; *) echo "image $img has no registry host"; continue ;; esac
        case "$host" in
            docker.io|index.docker.io|registry-1.docker.io) echo "image $img is on Docker Hub" ;;
            *.*|*:*|localhost) ;;
            *) echo "image $img has no registry host" ;;
        esac
    done
}

v_planner_tag() {
    local img
    img="$(yq -N "$PLANNER | .image" "$1")"
    [ "$img" = "ghcr.io/llm-d-incubation/llm-d-scaling-manager/capacity-planner:$WANT_TAG" ] \
        || echo "the planner image is '$img', want the controller's newTag $WANT_TAG"
}

v_no_token_k8s() {
    [ "$(yq -N "$DEP | .spec.template.spec.automountServiceAccountToken" "$1")" = false ] \
        || echo "the planner Pod mounts a ServiceAccount token"
    [ "$(yq -N 'select(.kind == "ServiceAccount" and .metadata.name == "capacity-planner") | .automountServiceAccountToken' "$1")" = false ] \
        || echo "the ServiceAccount does not set automountServiceAccountToken: false"
}

# The RBAC: the Role is what port-forward needs and nothing more, bound to
# every authenticated user.
v_rbac_k8s() {
    local f="$1" ROLE='select(.kind == "Role" and .metadata.name == "capacity-planner-viewer")'
    local RB='select(.kind == "RoleBinding" and .metadata.name == "capacity-planner-viewer")'
    [ "$(yq -N "$RB | .roleRef.kind + \"/\" + .roleRef.name" "$f")" = Role/capacity-planner-viewer ] \
        || echo "the RoleBinding does not bind the Role capacity-planner-viewer"
    [ "$(yq -N "$RB | .subjects | length" "$f")" = 1 ] && [ "$(yq -N "$RB | .subjects[0].kind + \"/\" + .subjects[0].name" "$f")" = Group/system:authenticated ] \
        || echo "the RoleBinding subject is not exactly Group system:authenticated: $(yq -o=json -I=0 "$RB | .subjects" "$f")"
    local got want
    got="$(yq -N "$ROLE | .rules[] | ((.apiGroups | join(\",\")) + \"|\" + (.resources | join(\",\")) + \"|\" + ((.resourceNames // []) | join(\",\")) + \"|\" + (.verbs | sort | join(\",\")))" "$f" | sort | tr '\n' ';')"
    # Both rules name the planner's one Pod: no other Pod, and no list.
    want='|pods/portforward|capacity-planner-0|create,get;|pods|capacity-planner-0|get;'
    [ "$got" = "$want" ] || echo "the Role's rules are '$got', want '$want'"
    # The name the Role holds is the StatefulSet's only Pod.
    [ "$(yq -N "$DEP | .spec.replicas" "$f")" = 1 ] \
        || echo "the StatefulSet has $(yq -N "$DEP | .spec.replicas" "$f") replicas; the Role names capacity-planner-0 alone"
}

# OpenShift: the proxy forwards to the planner's loopback with a loopback
# Host, and the Route reaches the proxy only.
v_proxy() {
    local f="$1" args sa
    args="$(yq -N "$PROXY | .args[]" "$f" | tr '\n' ' ')"
    [ -n "$args" ] || { echo "no oauth-proxy container"; return; }
    sa="$(yq -N "$DEP | .spec.template.spec.serviceAccountName" "$f")"
    for want in --provider=openshift --upstream=http://127.0.0.1:8080 --pass-host-header=false \
        "--openshift-service-account=$sa" --https-address=:8443 --cookie-secret-file=/etc/proxy/secrets/session_secret; do
        case " $args " in *" $want "*) ;; *) echo "oauth-proxy lacks $want: $args" ;; esac
    done
    case " $args " in *openshift-delegate-urls*) echo "oauth-proxy delegates (--openshift-delegate-urls needs system:auth-delegator)" ;; esac
    [ "$(yq -N "$PROXY | .ports[] | select(.name == \"https\") | .containerPort" "$f")" = 8443 ] || echo "oauth-proxy has no https port 8443"
    local svc='select(.kind == "Service" and .metadata.name == "capacity-planner")'
    [ "$(yq -N "$svc | .spec.ports | length" "$f")" = 1 ] && [ "$(yq -N "$svc | .spec.ports[0].targetPort" "$f")" = https ] \
        || echo "the Service does not target the proxy's https port alone"
    [ "$(yq -N "$svc | .spec.selector.\"app.kubernetes.io/name\"" "$f")" = "$(yq -N "$DEP | .spec.template.metadata.labels.\"app.kubernetes.io/name\"" "$f")" ] \
        || echo "the Service does not select the planner Pod"
    [ -n "$(yq -N "$svc | .metadata.annotations.\"service.beta.openshift.io/serving-cert-secret-name\"" "$f" | grep -v '^null$')" ] \
        && [ "$(yq -N "$svc | .metadata.annotations.\"service.beta.openshift.io/serving-cert-secret-name\"" "$f")" = "$(yq -N "$DEP | .spec.template.spec.volumes[] | select(.name == \"proxy-tls\") | .secret.secretName" "$f")" ] \
        || echo "the proxy does not mount the Service's serving certificate"
    local route='select(.kind == "Route" and .metadata.name == "capacity-planner")'
    case "$(yq -N "$route | .spec.tls.termination" "$f")" in
        reencrypt|edge) ;;
        *) echo "the Route does not terminate TLS (reencrypt or edge): $(yq -N "$route | .spec.tls.termination" "$f")" ;;
    esac
    [ "$(yq -N "$route | .spec.to.kind + \"/\" + .spec.to.name" "$f")" = Service/capacity-planner ] || echo "the Route does not go to the Service capacity-planner"
    local ann
    ann="$(yq -N "select(.kind == \"ServiceAccount\" and .metadata.name == \"$sa\") | .metadata.annotations.\"serviceaccounts.openshift.io/oauth-redirectreference.primary\"" "$f")"
    [ "$(printf '%s' "$ann" | yq -p=json -N '.reference.kind + "/" + .reference.name' 2>/dev/null)" = Route/capacity-planner ] \
        || echo "the ServiceAccount's oauth-redirectreference does not name the Route: $ann"
}

# The token the proxy needs is not mounted into the planner: an existing
# mount at the token's path stops the ServiceAccount admission from adding it.
v_token_masked() {
    local vol
    vol="$(yq -N "$PLANNER | .volumeMounts[] | select(.mountPath == \"$TOKEN_PATH\") | .name" "$1")"
    [ -n "$vol" ] || { echo "the planner container does not mask $TOKEN_PATH"; return; }
    [ "$(yq -N "$DEP | .spec.template.spec.volumes[] | select(.name == \"$vol\") | has(\"emptyDir\")" "$1")" = true ] \
        || echo "the planner masks $TOKEN_PATH with something other than an emptyDir"
}

# OpenShift's restricted SCC refuses fixed IDs.
v_no_fixed_ids() {
    yq -N '((select(.kind == "StatefulSet") | .spec.template.spec), (select(.kind == "Pod") | .spec)) | .securityContext | select(has("runAsUser") or has("runAsGroup") or has("fsGroup")) | "fixed IDs: " + (. | @json)' "$1"
}

# The SAR is asked in the planner's own namespace, and the shipped Role
# grants exactly what it asks.
v_sar() {
    local f="$1" sar
    sar="$(yq -N "$PROXY | .args[] | select(test(\"^--openshift-sar=\"))" "$f" | sed 's/^--openshift-sar=//')"
    [ -n "$sar" ] || { echo "oauth-proxy has no --openshift-sar"; return; }
    [ "$(printf '%s' "$sar" | yq -p=json -N '.namespace + "|" + .resource + "|" + .resourceName + "|" + .verb' 2>/dev/null)" = '$(POD_NAMESPACE)|services|capacity-planner|get' ] \
        || echo "--openshift-sar is not get services/capacity-planner in the Pod's namespace: $sar"
    [ "$(yq -N "$PROXY | .env[] | select(.name == \"POD_NAMESPACE\") | .valueFrom.fieldRef.fieldPath" "$f")" = metadata.namespace ] \
        || echo "POD_NAMESPACE is not the Pod's namespace"
    [ "$(yq -N 'select(.kind == "Role" and .metadata.name == "capacity-planner-viewer") | .rules[] | (.resources | join(",")) + "|" + (.resourceNames | join(",")) + "|" + (.verbs | join(","))' "$f")" = 'services|capacity-planner|get' ] \
        || echo "the Role does not grant exactly get services/capacity-planner"
}

# The loader: on the planner's node (the claim is ReadWriteOnce), writing the
# planner's claim.
v_loader() {
    local f="$1" POD='select(.kind == "Pod" and .metadata.name == "capacity-planner-loader")'
    [ "$(yq -N "$POD | .spec.volumes[] | select(.name == \"recordings\") | .persistentVolumeClaim.claimName" "$f")" = capacity-planner-recordings ] \
        || echo "the loader does not mount capacity-planner-recordings"
    [ "$(yq -N "$POD | .spec.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].labelSelector.matchLabels.\"app.kubernetes.io/name\"" "$f")" = capacity-planner ] \
        && [ "$(yq -N "$POD | .spec.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].topologyKey" "$f")" = kubernetes.io/hostname ] \
        || echo "the loader is not required onto the planner's node"
    [ "$(yq -N "$POD | .spec.automountServiceAccountToken" "$f")" = false ] || echo "the loader mounts a token"
}

v_loader_user() { # the Kubernetes loader writes as the planner's user
    local want got
    want="$(yq -N "$DEP | .spec.template.spec.securityContext.runAsUser" "$2")"
    got="$(yq -N 'select(.kind == "Pod") | .spec.securityContext.runAsUser' "$1")"
    [ "$got" = "$want" ] || echo "the loader runs as $got, the planner as $want"
}

# expect <label> <violations>: none expected.
expect_clean() {
    local label="$1" out="$2"
    if [ -z "$out" ]; then
        PASS=$((PASS + 1)); echo "  ok   $label"
    else
        FAIL=$((FAIL + 1)); echo "  FAIL $label"; printf '%s\n' "$out" | sed 's/^/         /'
    fi
}

# caught <label> <check> <file> <yq mutation | append:file>: the check must
# report the mutated copy.
caught() {
    local label="$1" check="$2" src="$3" mut="$4" m="$WORK/mutated.yaml"
    cp "$src" "$m"
    case "$mut" in
        append:*) { echo "---"; cat "${mut#append:}"; } >>"$m" ;;
        *) yq -i "$mut" "$m" || { FAIL=$((FAIL + 1)); echo "  FAIL negative control '$label': the mutation did not apply"; return; } ;;
    esac
    if cmp -s "$src" "$m"; then FAIL=$((FAIL + 1)); echo "  FAIL negative control '$label': the mutation changed nothing"; return; fi
    local out
    out="$("$check" "$m" "${5:-}")"
    if [ -n "$out" ]; then
        PASS=$((PASS + 1)); echo "  ok   caught: $label ($(printf '%s\n' "$out" | head -1))"
    else
        FAIL=$((FAIL + 1)); echo "  FAIL not caught: $label"
    fi
}

for o in kubernetes openshift openshift-sar loader openshift/loader; do
    render "config/capacity-planner/$o" "$WORK/${o//\//-}.yaml" || { echo "FAIL: config/capacity-planner/$o does not render"; exit 1; }
    [ -s "$WORK/${o//\//-}.yaml" ] || { echo "FAIL: config/capacity-planner/$o rendered nothing"; exit 1; }
done
K="$WORK/kubernetes.yaml"
O="$WORK/openshift.yaml"
S="$WORK/openshift-sar.yaml"
L="$WORK/loader.yaml"
OL="$WORK/openshift-loader.yaml"

echo "== config/capacity-planner/kubernetes"
expect_clean "nothing but namespaced objects, none naming a namespace" "$(v_namespaced_only "$K")"
expect_clean "no Service, Ingress or Route" "$(v_not_exposed "$K")"
expect_clean "the planner listens on 127.0.0.1:8080 with --auth=none" "$(v_planner_loopback "$K")"
expect_clean "the recordings claim is mounted read-only" "$(v_recordings_readonly "$K")"
expect_clean "non-root, read-only root, no capabilities, resources set" "$(v_hardened "$K")"
expect_clean "no ServiceAccount token" "$(v_no_token_k8s "$K")"
expect_clean "the Role is port-forward only, bound to system:authenticated" "$(v_rbac_k8s "$K")"
expect_clean "the planner image is :$WANT_TAG, the controller's newTag" "$(v_planner_tag "$K")"
expect_clean "every image names a registry, none Docker Hub" "$(v_images "$K")"

for pair in "openshift:$O" "openshift-sar:$S"; do
    label="${pair%%:*}" f="${pair#*:}"
    echo "== config/capacity-planner/$label"
    expect_clean "nothing but namespaced objects, none naming a namespace" "$(v_namespaced_only "$f")"
    expect_clean "the planner listens on 127.0.0.1:8080 with --auth=none" "$(v_planner_loopback "$f")"
    expect_clean "the recordings claim is mounted read-only" "$(v_recordings_readonly "$f")"
    expect_clean "non-root, read-only root, no capabilities, resources set" "$(v_hardened "$f")"
    expect_clean "oauth-proxy forwards to 127.0.0.1:8080 with its Host; a reencrypt Route; the redirect reference" "$(v_proxy "$f")"
    expect_clean "the token is not mounted into the planner" "$(v_token_masked "$f")"
    expect_clean "no fixed user, group or fsGroup" "$(v_no_fixed_ids "$f")"
    expect_clean "the planner image is :$WANT_TAG" "$(v_planner_tag "$f")"
    expect_clean "every image names a registry, none Docker Hub" "$(v_images "$f")"
done
expect_clean "openshift-sar: the SAR is get services/capacity-planner in the Pod's namespace" "$(v_sar "$S")"

echo "== config/capacity-planner/loader and openshift/loader"
expect_clean "loader: namespaced only" "$(v_namespaced_only "$L")"
expect_clean "loader: on the planner's node, the planner's claim" "$(v_loader "$L")"
expect_clean "loader: hardened" "$(v_hardened "$L")"
expect_clean "loader: images" "$(v_images "$L")"
expect_clean "loader: runs as the planner's user" "$(v_loader_user "$L" "$K")"
expect_clean "openshift/loader: on the planner's node, the planner's claim" "$(v_loader "$OL")"
expect_clean "openshift/loader: no fixed IDs" "$(v_no_fixed_ids "$OL")"

echo "== negative controls: each check fails a broken render"
cat >"$WORK/svc.yaml" <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: capacity-planner
spec:
  ports:
    - port: 8080
EOF
cat >"$WORK/crb.yaml" <<'EOF'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: capacity-planner-auth-delegator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:auth-delegator
subjects:
  - kind: ServiceAccount
    name: capacity-planner
    namespace: capacity-planner
EOF
PA='(select(.kind == "StatefulSet") | .spec.template.spec.containers[] | select(.name == "planner"))'
XA='(select(.kind == "StatefulSet") | .spec.template.spec.containers[] | select(.name == "oauth-proxy"))'
caught "a Service on plain Kubernetes" v_not_exposed "$K" "append:$WORK/svc.yaml"
caught "a ClusterRoleBinding" v_namespaced_only "$K" "append:$WORK/crb.yaml"
caught "a ClusterRoleBinding in openshift-sar" v_namespaced_only "$S" "append:$WORK/crb.yaml"
caught "an object naming a namespace" v_namespaced_only "$K" '(select(.kind == "Role") | .metadata.namespace) = "default"'
caught "--listen=0.0.0.0:8080" v_planner_loopback "$K" "$PA.args[1] = \"--listen=0.0.0.0:8080\""
caught "--listen-all-in-container" v_planner_loopback "$K" "$PA.args += [\"--listen-all-in-container\"]"
caught "--auth dropped" v_planner_loopback "$K" "$PA.args |= map(select(. != \"--auth=none\"))"
caught "the recordings mount writable" v_recordings_readonly "$K" "$PA.volumeMounts[0].readOnly = false"
caught "a writable root filesystem" v_hardened "$K" "$PA.securityContext.readOnlyRootFilesystem = false"
caught "no memory limit" v_hardened "$K" "del($PA.resources.limits)"
caught "runAsNonRoot dropped" v_hardened "$K" 'del(select(.kind == "StatefulSet") | .spec.template.spec.securityContext.runAsNonRoot)'
caught "a token mounted on plain Kubernetes" v_no_token_k8s "$K" '(select(.kind == "StatefulSet") | .spec.template.spec.automountServiceAccountToken) = true'
caught "the RoleBinding to another group" v_rbac_k8s "$K" '(select(.kind == "RoleBinding") | .subjects[0].name) = "system:unauthenticated"'
caught "a second RoleBinding subject" v_rbac_k8s "$K" '(select(.kind == "RoleBinding") | .subjects) += [{"kind": "User", "name": "eve"}]'
caught "the Role grants Secrets" v_rbac_k8s "$K" '(select(.kind == "Role") | .rules) += [{"apiGroups": [""], "resources": ["secrets"], "verbs": ["get"]}]'
caught "the Role grants exec" v_rbac_k8s "$K" '(select(.kind == "Role") | .rules[1].resources) = ["pods/portforward", "pods/exec"]'
caught "the Role loses pods/portforward create" v_rbac_k8s "$K" '(select(.kind == "Role") | .rules[1].verbs) = ["get"]'
caught "the Role lists Pods" v_rbac_k8s "$K" '(select(.kind == "Role") | .rules[0].verbs) = ["get", "list"]'
caught "the Role names no Pod" v_rbac_k8s "$K" 'del(select(.kind == "Role") | .rules[1].resourceNames)'
caught "the Role names another Pod" v_rbac_k8s "$K" '(select(.kind == "Role") | .rules[0].resourceNames) = ["capacity-planner-1"]'
caught "a second planner replica" v_rbac_k8s "$K" '(select(.kind == "StatefulSet") | .spec.replicas) = 2'
caught "a planner tag other than newTag" v_planner_tag "$K" "$PA.image = \"ghcr.io/llm-d-incubation/llm-d-scaling-manager/capacity-planner:v0.0.1\""
caught "a Docker Hub image" v_images "$K" "$PA.image = \"docker.io/library/busybox:1.36\""
caught "an image with no registry host" v_images "$L" '(select(.kind == "Pod") | .spec.containers[0].image) = "busybox:1.36"'
caught "oauth-proxy passes the Route's Host" v_proxy "$O" "$XA.args |= map(select(. != \"--pass-host-header=false\"))"
caught "oauth-proxy upstream on another port" v_proxy "$O" "$XA.args |= map(select(. != \"--upstream=http://127.0.0.1:8080\")) + [\"--upstream=http://127.0.0.1:9090\"]"
caught "oauth-proxy delegating" v_proxy "$O" "$XA.args += [\"--openshift-delegate-urls={}\"]"
caught "a passthrough Route" v_proxy "$O" '(select(.kind == "Route") | .spec.tls.termination) = "passthrough"'
caught "a Route with no TLS" v_proxy "$O" 'del(select(.kind == "Route") | .spec.tls)'
caught "the redirect reference names another Route" v_proxy "$O" '(select(.kind == "ServiceAccount") | .metadata.annotations."serviceaccounts.openshift.io/oauth-redirectreference.primary") = "{\"kind\":\"OAuthRedirectReference\",\"apiVersion\":\"v1\",\"reference\":{\"kind\":\"Route\",\"name\":\"other\"}}"'
caught "a Service port to the planner" v_proxy "$O" '(select(.kind == "Service") | .spec.ports) += [{"name": "planner", "port": 8080, "targetPort": 8080}]'
caught "the token mounted into the planner" v_token_masked "$O" "$PA.volumeMounts |= map(select(.mountPath != \"$TOKEN_PATH\"))"
caught "a fixed runAsUser on OpenShift" v_no_fixed_ids "$O" '(select(.kind == "StatefulSet") | .spec.template.spec.securityContext.runAsUser) = 65532'
caught "a fixed runAsUser in the OpenShift loader" v_no_fixed_ids "$OL" '(select(.kind == "Pod") | .spec.securityContext.runAsUser) = 65532'
caught "the SAR in no namespace" v_sar "$S" "$XA.args |= map(select(test(\"^--openshift-sar=\") | not)) + [\"--openshift-sar={\\\"resource\\\":\\\"services\\\",\\\"resourceName\\\":\\\"capacity-planner\\\",\\\"verb\\\":\\\"get\\\"}\"]"
caught "the SAR Role grants another Service" v_sar "$S" '(select(.kind == "Role") | .rules[0].resourceNames) = ["other"]'
caught "the loader off the planner's node" v_loader "$L" 'del(select(.kind == "Pod") | .spec.affinity)'
caught "the loader as another user" v_loader_user "$L" '(select(.kind == "Pod") | .spec.securityContext.runAsUser) = 1000' "$K"

echo
echo "capacity planner in-cluster manifests: $PASS passed, $FAIL failed (expected $EXPECT_PASS)"
[ "$FAIL" = 0 ] && [ "$PASS" = "$EXPECT_PASS" ]
