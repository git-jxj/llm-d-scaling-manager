#!/usr/bin/env bash
#
# The install from the tools image: smt admin-setup, smt install, smt
# admin-finish, smt release-files. Design: docs/proposals/two-command-install.md.
# Sourced by deploy/scaling-manager-tools.sh after common.sh, constants.sh and
# scaler_tls.sh.
#
# The objects are the release files, baked into the image at
# /opt/scaling-manager-tools/release (Dockerfile.tools) or given with --files.
# Nothing here renders an object of its own except the settings values, the
# admin stamp, the Prometheus CA Secret and the namespace of a cluster-wide
# install; the KEDA side of mutual TLS is llmsm_scaler_keda_auth_apply
# (scaler_tls.sh), the function make setup-prereqs runs.
#
# Exit codes, the same for every command (docs/reference/tools-install.md):
#   0 done                     4 the cluster is not ready
#   1 the preflight has a FAIL 5 timed out
#   2 usage, or unusable files 6 --strict: an UNKNOWN the install cannot wait out
#   3 needs the cluster admin  7 a kubectl call failed
#
# Requires funcs: log_* and kc_capture (common.sh), llmsm_keda_apiservice_ns and
# llmsm_scaler_np_keda_ns (common.sh), llmsm_scaler_* (scaler_tls.sh). kubectl,
# jq, yq and sha256sum on PATH.

SMT_PART_LABEL=scaling-manager.llmd.ai/part
SMT_STAMP=scaling-manager-admin-setup
SMT_SETTINGS=scaling-manager-settings
SMT_PROM_CA_SECRET=scaling-manager-prometheus-ca
SMT_PROM_CA_PATH=/etc/scaling-manager-prometheus-ca/ca.crt
SMT_DEPLOY=scaling-manager-controller-manager
SMT_STATUS_CM=scaling-manager-controller-status
SMT_PREFLIGHT_JOB=scaling-manager-preflight
# On the preflight Job with --allow-http: the controller is applied with
# PROMETHEUS_TOKEN_PATH empty (internal/preflight TokenPathEmptiedEnv).
SMT_TOKEN_EMPTIED_ENV=LLMSM_PREFLIGHT_TOKEN_PATH_EMPTIED
SMT_CLUSTER_NS=scaling-manager-system
SMT_THANOS_URL=https://thanos-querier.openshift-monitoring.svc.cluster.local:9091
# The checks the preflight cannot make before the controller runs: UNKNOWN in
# the first run is expected, and the run after the install makes them.
SMT_NEEDS_CONTROLLER="rbac.controller keda.mtls scaler.tls"
# The migration section named when the live controller is not from the release
# files (an install made with make, before it renders from them).
SMT_MIGRATION_DOC="docs/proposals/install-from-release.md#phases"

# smt_exit <code> <message>: the message on stderr, then the exit code.
smt_exit() {
    echo "smt: $2" >&2
    exit "$1"
}
smt_usage() { smt_exit 2 "$1"; }

# smt_poll_step echoes the seconds one poll counts against a timeout: the
# sleep, SMT_POLL (default 3), and at least 1, so every wait ends.
smt_poll_step() {
    local p="${SMT_POLL:-3}"
    case "$p" in ""|*[!0-9]*) p=3 ;; esac
    [ "$p" -gt 0 ] || p=1
    printf '%s\n' "$p"
}

# smt_call <function> <args...>: a deploy/lib reader that echoes its answer,
# run in this shell (not in $( )), so the KC_ERR it leaves names the failure.
# The answer is in SMT_CALL_OUT; the status is the function's.
smt_call() {
    local rc=0
    "$@" > "$SMT_TMP/call.out" || rc=$?
    SMT_CALL_OUT="$(cat "$SMT_TMP/call.out")"
    return "$rc"
}

# smt_kubectl_failed <what>: a kubectl call failed; exit 7 naming it and why.
smt_kubectl_failed() {
    smt_exit 7 "kubectl $1 failed: $(kc_err_line "$KC_ERR")"
}

# smt_kc <args...>: kubectl, or exit 7. Output in KC_OUT.
smt_kc() {
    kc_capture "$@" || smt_kubectl_failed "$*"
}

# smt_get <resource> <name> [-n ns]: KC_OUT holds the object as JSON and it
# returns 0; returns 1 when it does not exist; exits 7 on any other failure
# (Forbidden is not absent).
smt_get() {
    local rc=0
    kc_capture get "$@" -o json || rc=$?
    [ "$rc" = 0 ] && return 0
    printf '%s' "$KC_ERR" | grep -qF '(NotFound)' && return 1
    smt_kubectl_failed "get $*"
}

# smt_check_ns <flag> <value>: a namespace name, or exit 2.
smt_check_ns() {
    case "$2" in
        ""|*[!a-z0-9-]*|-*|*-) smt_usage "$1: '$2' is not a namespace name" ;;
    esac
    [ "${#2}" -le 63 ] || smt_usage "$1: '$2' is longer than 63 characters"
}

# --- Flags -------------------------------------------------------------------

# smt_parse <command> <args...>: the flags every command shares, into SMT_*.
# A flag the command does not take is a usage error.
smt_parse() {
    local cmd="$1" allowed
    shift
    SMT_NS="" SMT_KEDA_NS="" SMT_PROM_URL="" SMT_PROM_CA="" SMT_QUOTA="" SMT_SETS=() SMT_FILES=""
    SMT_DRY_RUN=false SMT_OUT="" SMT_SCOPE=namespace SMT_MODEL_NS=() SMT_PLATFORM="" SMT_STRICT=false
    SMT_RESTART_KEDA=false SMT_IMAGE_REPO="" SMT_ALLOW_HTTP=false
    case "$cmd" in
        admin-setup) allowed=" -n --keda-namespace --files --dry-run --out --platform --scope " ;;
        install) allowed=" -n --keda-namespace --prometheus-url --prometheus-ca --quota --set --files --dry-run --out --scope --model-namespace --platform --strict --restart-keda --image-repo --allow-http " ;;
        admin-finish) allowed=" -n --keda-namespace --files --platform --strict --restart-keda --scope " ;;
        release-files) allowed=" --out " ;;
    esac
    while [ $# -gt 0 ]; do
        local flag="$1"
        [ "$flag" = --namespace ] && flag=-n
        case " $allowed " in
            *" $flag "*) ;;
            *) smt_usage "$cmd: unknown option $1 (see $cmd --help)" ;;
        esac
        case "$flag" in
            --dry-run|--strict|--restart-keda|--allow-http) ;;
            *) [ $# -ge 2 ] && [ -n "$2" ] || smt_usage "$cmd: $1 needs a value" ;;
        esac
        case "$flag" in
            -n) SMT_NS="$2"; shift 2 ;;
            --keda-namespace) SMT_KEDA_NS="$2"; shift 2 ;;
            --prometheus-url) SMT_PROM_URL="$2"; shift 2 ;;
            --prometheus-ca) SMT_PROM_CA="$2"; shift 2 ;;
            --quota) SMT_QUOTA="$2"; shift 2 ;;
            --set) SMT_SETS+=("$2"); shift 2 ;;
            --files) SMT_FILES="$2"; shift 2 ;;
            --out) SMT_OUT="$2"; shift 2 ;;
            --scope) SMT_SCOPE="$2"; shift 2 ;;
            --model-namespace) SMT_MODEL_NS+=("$2"); shift 2 ;;
            --platform) SMT_PLATFORM="$2"; shift 2 ;;
            --image-repo) SMT_IMAGE_REPO="$2"; shift 2 ;;
            --dry-run) SMT_DRY_RUN=true; shift ;;
            --strict) SMT_STRICT=true; shift ;;
            --restart-keda) SMT_RESTART_KEDA=true; shift ;;
            --allow-http) SMT_ALLOW_HTTP=true; shift ;;
        esac
    done
    case "$SMT_SCOPE" in
        namespace|cluster) ;;
        *) smt_usage "$cmd: --scope is namespace or cluster, not '$SMT_SCOPE'" ;;
    esac
    case "$SMT_PLATFORM" in
        ""|kubernetes|openshift) ;;
        *) smt_usage "$cmd: --platform is kubernetes or openshift, not '$SMT_PLATFORM'" ;;
    esac
    if [ "$SMT_SCOPE" = cluster ]; then
        case "$cmd" in
            admin-setup|admin-finish)
                smt_usage "$cmd does not take --scope cluster: a cluster-wide install has one actor, so one command does it all: smt install --scope cluster --keda-namespace <KEDA namespace> --prometheus-url <url> --model-namespace <ns>" ;;
        esac
        [ -z "$SMT_NS" ] || [ "$SMT_NS" = "$SMT_CLUSTER_NS" ] \
            || smt_usage "$cmd --scope cluster installs in $SMT_CLUSTER_NS, which the cluster-wide files name; leave out -n"
        SMT_NS="$SMT_CLUSTER_NS"
    elif [ "$cmd" != release-files ]; then
        [ ${#SMT_MODEL_NS[@]} -eq 0 ] || smt_usage "$cmd: --model-namespace is for --scope cluster; a namespaced install manages its own namespace"
        [ -n "$SMT_NS" ] || smt_usage "$cmd needs -n <namespace>: the namespace the model servers run in"
    fi
    [ "$cmd" = release-files ] || smt_check_ns "$cmd -n" "$SMT_NS"
    [ -z "$SMT_KEDA_NS" ] || smt_check_ns "$cmd --keda-namespace" "$SMT_KEDA_NS"
    local m
    for m in "${SMT_MODEL_NS[@]}"; do smt_check_ns "$cmd --model-namespace" "$m"; done
    if [ "$SMT_DRY_RUN" = true ]; then
        [ -n "$SMT_OUT" ] || smt_usage "$cmd --dry-run needs --out <dir>: the rendered objects are written there"
    elif [ "$cmd" != release-files ] && [ -n "$SMT_OUT" ]; then
        smt_usage "$cmd: --out is for --dry-run"
    fi
    if [ -n "$SMT_IMAGE_REPO" ]; then
        case "$SMT_IMAGE_REPO" in
            *@*|*/|/*) smt_usage "$cmd --image-repo: give REGISTRY/PATH, without a tag or digest" ;;
        esac
        case "${SMT_IMAGE_REPO##*/}" in *:*) smt_usage "$cmd --image-repo: give REGISTRY/PATH, without a tag" ;; esac
    fi
    if [ -n "$SMT_PROM_CA" ] && [ ! -r "$SMT_PROM_CA" ]; then
        smt_usage "$cmd --prometheus-ca: cannot read '$SMT_PROM_CA' (a path under the mounted /work)"
    fi
    if [ -n "$SMT_QUOTA" ]; then
        printf '%s' "$SMT_QUOTA" | grep -qE '^[A-Za-z0-9._-]+=-?[0-9]+(,[A-Za-z0-9._-]+=-?[0-9]+)*$' \
            || smt_usage "$cmd --quota: '$SMT_QUOTA' is not GPU=N[,GPU=N]"
    fi
    if [ -n "$SMT_PROM_URL" ]; then
        smt_check_scheme "$SMT_PROM_URL" "--prometheus-url $SMT_PROM_URL"
    fi
    if [ "$SMT_ALLOW_HTTP" = true ] && [ -n "$SMT_PROM_CA" ]; then
        smt_usage "$cmd --allow-http with --prometheus-ca: a plain HTTP Prometheus has no certificate, and the controller refuses TLS settings beside an http URL"
    fi
}

# smt_is_http <url>: the URL is plain HTTP.
smt_is_http() {
    case "$(printf '%s' "$1" | tr 'A-Z' 'a-z')" in http://*) return 0 ;; esac
    return 1
}

# smt_check_scheme <url> <what names it>: --allow-http is given exactly when
# the Prometheus URL the controller reads is plain HTTP, or exit 2. With it,
# the settings allow HTTP and the controller is applied without a token (the
# controller refuses to send one over HTTP); without it, it is applied with
# its ServiceAccount token, which an http URL would make it refuse.
smt_check_scheme() {
    if smt_is_http "$1"; then
        [ "$SMT_ALLOW_HTTP" = true ] \
            || smt_usage "$2 is plain HTTP, which the controller refuses by default. Add --allow-http to read it unencrypted (the controller then sends Prometheus no token), or give an https URL"
    elif [ "$SMT_ALLOW_HTTP" = true ]; then
        smt_usage "--allow-http is for a plain HTTP Prometheus, and $2 is not one: leave --allow-http out"
    fi
}

# --- The release files -------------------------------------------------------

# smt_version echoes the release this image is, or empty in a checkout.
smt_version() {
    [ -f "$HERE/VERSION" ] && cat "$HERE/VERSION"
}

# smt_baked_dir echoes where the image carries the release files.
smt_baked_dir() {
    printf '%s\n' "${SMT_BAKED_DIR:-$HERE/release}"
}

# smt_load_files: copies the release files (--files, else those of the image) to a
# scratch directory, SMT_DIR, after checking SHA256SUMS and the version.
# Exits 2 when there are none, or they do not check out. Applies nothing.
smt_load_files() {
    local src="$SMT_FILES" v bad f n
    if [ -z "$src" ]; then
        src="$(smt_baked_dir)"
        if [ ! -f "$src/SHA256SUMS" ]; then
            smt_usage "this image carries no release files (a development build). Give --files <dir> with the release files and their SHA256SUMS, or run the release's tools image"
        fi
    fi
    [ -d "$src" ] || smt_usage "--files $src: no such directory"
    [ -f "$src/SHA256SUMS" ] || smt_usage "--files $src: no SHA256SUMS there; download it with the files"
    # --ignore-missing: SHA256SUMS lists the tools tarball, which the image
    # does not carry. sha256sum still fails when it verified no file at all.
    if ! (cd "$src" && sha256sum -c --ignore-missing --quiet SHA256SUMS) > "$SMT_TMP/sums.out" 2>&1; then
        smt_usage "the release files in $src do not match their SHA256SUMS; refusing them: $(tr '\n' ' ' < "$SMT_TMP/sums.out")"
    fi
    SMT_DIR="$SMT_TMP/files"
    mkdir -p "$SMT_DIR"
    n=0
    for f in "$src"/scaling-manager-*.yaml; do
        [ -f "$f" ] || continue
        # Listed in SHA256SUMS, or it was not checked.
        grep -qE "  ${f##*/}\$" "$src/SHA256SUMS" || smt_usage "${f##*/} in $src is not listed in its SHA256SUMS"
        cp "$f" "$SMT_DIR/"
        n=$((n + 1))
    done
    [ "$n" -gt 0 ] || smt_usage "no release files in $src"
    v="$(smt_version)"
    if [ -n "$v" ]; then
        bad="$(cd "$SMT_DIR" && for f in scaling-manager-*.yaml; do
            yq ea '[.metadata.labels["app.kubernetes.io/version"] // ""] | .[]' "$f" 2>/dev/null | grep -vxF -- "$v" | sed "s|^|$f: |" | head -1
        done | head -3)"
        [ -z "$bad" ] || smt_usage "these release files are not $v, the release of this image (${bad}). The install logic and the files ship together: use the tools image of the files' release"
    fi
    SMT_FILES_FROM="$src"
}

# smt_need <file...>: each release file is present, or exit 2.
smt_need() {
    local f
    for f in "$@"; do
        [ -f "$SMT_DIR/$f" ] || smt_usage "the release files lack $f"
    done
}

# smt_files_for: the file names for SMT_SCOPE and SMT_PLATFORM.
smt_files_for() {
    local suffix=""
    [ "$SMT_PLATFORM" = openshift ] && suffix=-openshift
    if [ "$SMT_SCOPE" = cluster ]; then
        SMT_MAIN="scaling-manager-cluster$suffix.yaml"
        SMT_PREFLIGHT=scaling-manager-preflight-cluster.yaml
        SMT_PREFLIGHT_CRB=scaling-manager-preflight
    else
        SMT_MAIN="scaling-manager-namespaced$suffix.yaml"
        SMT_PREFLIGHT=scaling-manager-preflight.yaml
        SMT_PREFLIGHT_CRB="scaling-manager-preflight-$SMT_NS"
    fi
}

# smt_admin_hash echoes the hash of the admin objects as shipped: the
# part=admin objects of the main and preflight files and the model-namespace
# file, before any substitution, without the version label (a release that does
# not change them needs no admin). Read from the unsubstituted copy.
smt_admin_hash() {
    local f
    for f in "$SMT_MAIN" scaling-manager-preflight.yaml scaling-manager-model-namespace.yaml; do
        [ "$SMT_SCOPE" = cluster ] && [ "$f" = scaling-manager-preflight.yaml ] && f="$SMT_PREFLIGHT"
        PART="$SMT_PART_LABEL" yq ea -o json -I 0 \
            'select(.kind != null and .metadata.labels[strenv(PART)] == "admin") | del(.metadata.labels["app.kubernetes.io/version"])' \
            "$SMT_SHIPPED/$f"
    done | sha256sum | awk '{print $1}'
}

# smt_render: SMT_SHIPPED keeps the files as shipped; the copies in SMT_DIR get
# the namespaces, the KEDA namespace in the scaler NetworkPolicy, and
# --image-repo.
smt_render() {
    local f model="${1:-$SMT_NS}"
    SMT_SHIPPED="$SMT_TMP/shipped"
    if [ ! -d "$SMT_SHIPPED" ]; then
        mkdir -p "$SMT_SHIPPED"
        cp "$SMT_DIR"/scaling-manager-*.yaml "$SMT_SHIPPED/"
    fi
    for f in "$SMT_DIR"/scaling-manager-*.yaml; do
        sed -e "s/__NAMESPACE__/$SMT_NS/g" -e "s/__MODEL_NAMESPACE__/$model/g" "$SMT_SHIPPED/${f##*/}" > "$f" \
            || smt_exit 2 "cannot render ${f##*/} for namespace $SMT_NS"
    done
    if [ -n "${SMT_KEDA_NS:-}" ]; then
        # The same rewrite deploy/release-manifests.sh makes when it writes the
        # platform default, here with the namespace KEDA runs in.
        KEDA_NS="$SMT_KEDA_NS" yq -i '(select(.kind == "NetworkPolicy" and .metadata.name == "scaling-manager-external-scaler") | .spec.ingress[] | select(has("from")) | .from[] | select(has("namespaceSelector")) | .namespaceSelector.matchLabels | select(has("kubernetes.io/metadata.name")) | .["kubernetes.io/metadata.name"]) = strenv(KEDA_NS)' \
            "$SMT_DIR/$SMT_MAIN" || smt_exit 2 "cannot write KEDA namespace $SMT_KEDA_NS into $SMT_MAIN"
    fi
    if [ "${SMT_ALLOW_HTTP:-false}" = true ]; then
        # The controller refuses to send its token over plain HTTP, and env
        # beats the settings: empty PROMETHEUS_TOKEN_PATH by name, as
        # llmsm_reconcile_prometheus_scheme does for make. Rendered rather than
        # patched, so a re-run with the same flags applies the same object.
        D="$SMT_DEPLOY" yq -i '(select(.kind == "Deployment" and .metadata.name == strenv(D)) | .spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == "PROMETHEUS_TOKEN_PATH") | .value) = ""' \
            "$SMT_DIR/$SMT_MAIN" || smt_exit 2 "cannot empty PROMETHEUS_TOKEN_PATH in $SMT_MAIN"
        # The preflight runs before this Deployment is applied: tell it, so the
        # token path of a live controller (one crash-looping on it, say) is
        # not held against the install that removes it.
        J="$SMT_PREFLIGHT_JOB" E="$SMT_TOKEN_EMPTIED_ENV" yq -i '(select(.kind == "Job" and .metadata.name == strenv(J)) | .spec.template.spec.containers[] | select(.name == "preflight") | .env) += [{"name": strenv(E), "value": "true"}]' \
            "$SMT_DIR/$SMT_PREFLIGHT" || smt_exit 2 "cannot set $SMT_TOKEN_EMPTIED_ENV in $SMT_PREFLIGHT"
    fi
    if [ -n "$SMT_IMAGE_REPO" ]; then
        for f in "$SMT_MAIN" "$SMT_PREFLIGHT"; do
            REPO="$SMT_IMAGE_REPO" yq -i '(select(.kind == "Deployment" or .kind == "Job") | .spec.template.spec.containers[] | select(.image | test("@sha256:")) | .image) |= (strenv(REPO) + "@" + sub("^[^@]*@", ""))' \
                "$SMT_DIR/$f" || smt_exit 2 "cannot move the images of $f to $SMT_IMAGE_REPO"
        done
    fi
}

# smt_model_file <model namespace>: the model-namespace file for that
# namespace, written to SMT_TMP; echoes its path.
smt_model_file() {
    local out="$SMT_TMP/model-namespace-$1.yaml"
    sed -e "s/__NAMESPACE__/$SMT_NS/g" -e "s/__MODEL_NAMESPACE__/$1/g" "$SMT_SHIPPED/scaling-manager-model-namespace.yaml" > "$out" \
        || smt_exit 2 "cannot render scaling-manager-model-namespace.yaml for $1"
    printf '%s\n' "$out"
}

# smt_select <part> <file>: the objects of that part.
smt_select() {
    PART="$SMT_PART_LABEL" P="$1" yq ea 'select(.kind != null and .metadata.labels[strenv(PART)] == strenv(P))' "$2"
}

# --- The cluster -------------------------------------------------------------

# smt_detect_platform sets SMT_PLATFORM from --platform, else API discovery
# (security.openshift.io), which any authenticated user may read.
smt_detect_platform() {
    [ -z "$SMT_PLATFORM" ] || return 0
    if [ "$SMT_DRY_RUN" = true ]; then
        SMT_PLATFORM=kubernetes
        return 0
    fi
    smt_kc api-versions
    if printf '%s\n' "$KC_OUT" | grep -q '^security\.openshift\.io/'; then
        SMT_PLATFORM=openshift
    else
        SMT_PLATFORM=kubernetes
    fi
}

# smt_require_namespace: the namespace exists, or exit 4. Never creates it.
smt_require_namespace() {
    local rc=0
    kc_capture get namespace "$SMT_NS" -o name || rc=$?
    [ "$rc" = 0 ] && return 0
    if printf '%s' "$KC_ERR" | grep -qF '(NotFound)'; then
        smt_exit 4 "namespace $SMT_NS does not exist. Create it, or name the namespace the model servers run in"
    fi
    printf '%s' "$KC_ERR" | grep -qF '(Forbidden)' || smt_kubectl_failed "get namespace $SMT_NS"
    # A namespace owner may not read Namespaces. Every namespace has the
    # ServiceAccount default, which an owner may read in their own.
    rc=0
    kc_capture get serviceaccount default -n "$SMT_NS" -o name || rc=$?
    [ "$rc" = 0 ] && return 0
    if printf '%s' "$KC_ERR" | grep -qF '(Forbidden)'; then
        smt_exit 4 "you may not read namespace $SMT_NS: is it the name of the namespace the model servers run in, and is it yours? ($(kc_err_line "$KC_ERR"))"
    fi
    printf '%s' "$KC_ERR" | grep -qF '(NotFound)' \
        && smt_exit 4 "namespace $SMT_NS does not exist (it has no ServiceAccount default). Name the namespace the model servers run in"
    smt_kubectl_failed "get serviceaccount default -n $SMT_NS"
}

# smt_resolve_keda_ns <command>: sets SMT_KEDA_NS. The flag; else the live
# source namespace of the scaler NetworkPolicy; else the namespace serving the
# external-metrics APIService when KEDA serves it (the detection make
# setup-prereqs uses); on OpenShift, openshift-keda. None: exit 4.
smt_resolve_keda_ns() {
    local found rc
    [ -n "$SMT_KEDA_NS" ] && { SMT_KEDA_FROM="--keda-namespace"; return 0; }
    if [ "$SMT_DRY_RUN" = true ]; then
        SMT_KEDA_NS=keda
        [ "$SMT_PLATFORM" = openshift ] && SMT_KEDA_NS=openshift-keda
        SMT_KEDA_FROM="the platform default (--dry-run reads no cluster)"
        return 0
    fi
    rc=0
    smt_call llmsm_scaler_np_keda_ns "$SMT_NS" || rc=$?
    found="$SMT_CALL_OUT"
    case "$rc" in
        0) SMT_KEDA_NS="$found"; SMT_KEDA_FROM="the scaler NetworkPolicy already in $SMT_NS"; return 0 ;;
        2) smt_exit 7 "cannot read NetworkPolicy scaling-manager-external-scaler in $SMT_NS: $(kc_err_line "$KC_ERR")" ;;
    esac
    rc=0
    smt_call llmsm_keda_apiservice_ns || rc=$?
    found="$SMT_CALL_OUT"
    case "$rc" in
        0) SMT_KEDA_NS="$found"; SMT_KEDA_FROM="the namespace serving the external-metrics APIService"; return 0 ;;
        2) smt_exit 7 "cannot read APIService v1beta1.external.metrics.k8s.io: $(kc_err_line "$KC_ERR")" ;;
    esac
    if [ "$SMT_PLATFORM" = openshift ]; then
        SMT_KEDA_NS=openshift-keda
        SMT_KEDA_FROM="the OpenShift default"
        return 0
    fi
    smt_exit 4 "cannot find the namespace KEDA runs in (no scaler NetworkPolicy in $SMT_NS, and the external-metrics APIService is not served by KEDA${found:+: $found}). Give --keda-namespace <namespace>"
}

# smt_apply <file> [part]: kubectl apply, the part only when given. Exit 7.
smt_apply() {
    local sel=()
    [ -z "${2:-}" ] || sel=(-l "$SMT_PART_LABEL=$2")
    smt_kc apply -f "$1" "${sel[@]}"
    printf '%s\n' "$KC_OUT" | sed '/^$/d; s/^/  /'
}

# smt_stamp_yaml: the admin stamp, a ConfigMap in SMT_NS.
smt_stamp_yaml() {
    jq -n --arg ns "$SMT_NS" --arg name "$SMT_STAMP" --arg part "$SMT_PART_LABEL" \
        --arg v "$(smt_version)" --arg keda "$SMT_KEDA_NS" --arg hash "$(smt_admin_hash)" --arg p "$SMT_PLATFORM" \
        '{apiVersion: "v1", kind: "ConfigMap",
          metadata: {name: $name, namespace: $ns,
                     labels: {"app.kubernetes.io/part-of": "scaling-manager", ($part): "admin"}},
          data: {release: $v, kedaNamespace: $keda, adminObjectsSha256: $hash, platform: $p}}' | yq -P
}

# smt_admin_line echoes the admin-setup command for this namespace.
smt_admin_line() {
    printf 'smt admin-setup -n %s --keda-namespace %s\n' "$SMT_NS" "${1:-<the namespace KEDA runs in>}"
}

# --- Settings ----------------------------------------------------------------

# smt_settings_pairs: SMT_PAIRS, the settings keys the flags set, KEY=VALUE.
# One table: --prometheus-url prometheusURL, --prometheus-ca
# prometheusCACertPath, --quota limiter=quota and quotas, --allow-http
# prometheusAllowHTTP=true, --set KEY=VALUE.
smt_settings_pairs() {
    local kv key known
    SMT_PAIRS=()
    [ -z "$SMT_PROM_URL" ] || SMT_PAIRS+=("prometheusURL=$SMT_PROM_URL")
    [ -z "$SMT_PROM_CA" ] || SMT_PAIRS+=("prometheusCACertPath=$SMT_PROM_CA_PATH")
    [ -z "$SMT_QUOTA" ] || SMT_PAIRS+=("limiter=quota" "quotas=$SMT_QUOTA")
    [ "$SMT_ALLOW_HTTP" != true ] || SMT_PAIRS+=("prometheusAllowHTTP=true")
    known="$(yq '.data | keys | .[]' "$SMT_SHIPPED/scaling-manager-settings.yaml")"
    for kv in "${SMT_SETS[@]}"; do
        key="${kv%%=*}"
        [ "$key" != "$kv" ] || smt_usage "--set $kv: give KEY=VALUE"
        printf '%s\n' "$known" | grep -qxF -- "$key" || smt_usage "--set $key: not a key of scaling-manager-settings ($(printf '%s' "$known" | tr '\n' ' '))"
        [ "$key" != passive ] || smt_usage "--set passive: the install is passive. After smt admin-finish, turn it off with:  $(smt_patch_cmd passive "${kv#*=}")"
        case "$key" in
            prometheusURL) smt_usage "--set prometheusURL: give --prometheus-url" ;;
            prometheusCACertPath) smt_usage "--set prometheusCACertPath: give --prometheus-ca <file>; it creates the Secret the path reads" ;;
            quotas) smt_usage "--set quotas: give --quota \"GPU=N\"" ;;
            prometheusAllowHTTP) smt_usage "--set prometheusAllowHTTP: give --allow-http; it also applies the controller without PROMETHEUS_TOKEN_PATH, which a plain HTTP Prometheus needs" ;;
            limiter) [ -z "$SMT_QUOTA" ] || smt_usage "--set limiter with --quota: --quota sets limiter to quota" ;;
        esac
        SMT_PAIRS+=("$kv")
    done
}

# smt_settings_file: the settings file with the values of the flags, in SMT_TMP.
smt_settings_file() {
    local kv out="$SMT_TMP/settings.yaml"
    cp "$SMT_DIR/scaling-manager-settings.yaml" "$out"
    for kv in "${SMT_PAIRS[@]}"; do
        K="${kv%%=*}" V="${kv#*=}" yq -i '.data[strenv(K)] = strenv(V)' "$out" \
            || smt_exit 2 "cannot set ${kv%%=*} in the settings"
    done
    printf '%s\n' "$out"
}

# smt_patch_cmd <key> <value>: the kubectl patch that sets one settings key.
smt_patch_cmd() {
    local j
    j="$(jq -cn --arg k "$1" --arg v "$2" '{data: {($k): $v}}')"
    # Inside single quotes a quote is written as: quote, backslash, quote, quote.
    j="$(printf '%s' "$j" | sed "s/'/'\\\\''/g")"
    printf "kubectl patch configmap %s -n %s --type merge -p '%s'\n" "$SMT_SETTINGS" "$SMT_NS" "$j"
}

# smt_settings: creates the settings from the flags when there are none, and
# never writes existing ones: a flag that differs is reported with its patch.
smt_settings() {
    local kv live cur f
    smt_settings_pairs
    if smt_get configmap "$SMT_SETTINGS" -n "$SMT_NS"; then
        live="$KC_OUT"
        echo "Settings: $SMT_SETTINGS exists; left as it is."
        for kv in "${SMT_PAIRS[@]}"; do
            cur="$(printf '%s' "$live" | jq -r --arg k "${kv%%=*}" '.data[$k] // ""')"
            if [ "$cur" != "${kv#*=}" ]; then
                echo "  ${kv%%=*} is \"$cur\" there, not \"${kv#*=}\" as given. Not changed; to change it:"
                echo "    $(smt_patch_cmd "${kv%%=*}" "${kv#*=}")"
            fi
        done
        return 0
    fi
    f="$(smt_settings_file)"
    smt_kc create -f "$f"
    SMT_SETTINGS_CREATED=true
    echo "Settings: created $SMT_SETTINGS ($(printf '%s ' "${SMT_PAIRS[@]%%=*}")from the flags; passive)."
}

# smt_settings_preconditions: with no settings, a Prometheus URL is needed
# before anything is created. Run before the first write.
#
# The URL the controller will read decides --allow-http (smt_check_scheme): the
# one in the live settings when they exist, since they are never written, else
# the one the flag gives. A flag URL of the other scheme than the live one is
# a switch the settings must make first: the refusal prints every patch it
# needs (smt_switch_patches).
smt_settings_preconditions() {
    local live allow want=false had=false
    if smt_get configmap "$SMT_SETTINGS" -n "$SMT_NS"; then
        live="$(printf '%s' "$KC_OUT" | jq -r '.data.prometheusURL // ""')"
        allow="$(printf '%s' "$KC_OUT" | jq -r '.data.prometheusAllowHTTP // ""')"
        smt_is_http "$live" && had=true
        [ -n "$SMT_PROM_URL" ] && smt_is_http "$SMT_PROM_URL" && want=true
        if [ -n "$live" ] && [ -n "$SMT_PROM_URL" ] && [ "$had" != "$want" ]; then
            smt_usage "prometheusURL in the live $SMT_SETTINGS is $live, and --prometheus-url $SMT_PROM_URL changes its scheme. install never writes existing settings: run these, then the same command again right away (until then a controller restart fails):
$(smt_switch_patches "$want" | sed 's/^/  /')"
        fi
        [ -z "$live" ] || smt_check_scheme "$live" "prometheusURL in the live $SMT_SETTINGS ($live)"
        if [ "$SMT_ALLOW_HTTP" = true ] && [ "$allow" != true ]; then
            smt_usage "prometheusAllowHTTP in the live $SMT_SETTINGS is \"$allow\", not \"true\", so the controller would refuse the http URL. install never writes existing settings: run this, then the same command again:
  $(smt_patch_cmd prometheusAllowHTTP true)"
        fi
        return 0
    fi
    if [ -z "$SMT_PROM_URL" ]; then
        if [ "$SMT_PLATFORM" = openshift ]; then
            SMT_PROM_URL="$SMT_THANOS_URL"
            smt_check_scheme "$SMT_PROM_URL" "the OpenShift default, $SMT_PROM_URL,"
            echo "No --prometheus-url: the OpenShift default, $SMT_PROM_URL."
        else
            smt_usage "no $SMT_SETTINGS in $SMT_NS yet, and no --prometheus-url: give the Prometheus base URL, https (http with --allow-http)"
        fi
    fi
}

# smt_switch_patches <to http: true|false>: the kubectl patches, one a line,
# that switch the live settings (KC_OUT) to SMT_PROM_URL. To http: allow it,
# and clear the TLS settings the controller refuses beside an http URL. To
# https: stop allowing http, and with --prometheus-ca point at its CA.
smt_switch_patches() {
    local to_http="$1" v
    smt_patch_cmd prometheusURL "$SMT_PROM_URL"
    smt_patch_cmd prometheusAllowHTTP "$to_http"
    if [ "$to_http" = true ]; then
        v="$(printf '%s' "$KC_OUT" | jq -r '.data.prometheusInsecureSkipVerify // ""' | tr 'A-Z' 'a-z')"
        case "$v" in true|t|1) smt_patch_cmd prometheusInsecureSkipVerify false ;; esac
        for v in prometheusCACertPath prometheusServerName; do
            [ -z "$(printf '%s' "$KC_OUT" | jq -r --arg k "$v" '.data[$k] // ""')" ] || smt_patch_cmd "$v" ""
        done
    elif [ -n "$SMT_PROM_CA" ] && [ "$(printf '%s' "$KC_OUT" | jq -r '.data.prometheusCACertPath // ""')" != "$SMT_PROM_CA_PATH" ]; then
        smt_patch_cmd prometheusCACertPath "$SMT_PROM_CA_PATH"
    fi
}

# smt_prometheus_ca: with --prometheus-ca, creates or updates Secret
# scaling-manager-prometheus-ca. SMT_CA_CHANGED=true when an existing one
# changed (the controller reads it at start).
smt_prometheus_ca() {
    local want cur
    SMT_CA_CHANGED=false
    [ -n "$SMT_PROM_CA" ] || return 0
    want="$(base64 < "$SMT_PROM_CA" | tr -d '\n')"
    if smt_get secret "$SMT_PROM_CA_SECRET" -n "$SMT_NS"; then
        cur="$(printf '%s' "$KC_OUT" | jq -r '.data["ca.crt"] // ""')"
        if [ "$cur" = "$want" ]; then
            echo "Prometheus CA: Secret $SMT_PROM_CA_SECRET unchanged."
            return 0
        fi
        SMT_CA_CHANGED=true
    fi
    smt_prometheus_ca_yaml > "$SMT_TMP/prometheus-ca.yaml"
    smt_kc apply --server-side --force-conflicts --field-manager=scaling-manager-tools -f "$SMT_TMP/prometheus-ca.yaml"
    echo "Prometheus CA: Secret $SMT_PROM_CA_SECRET $([ "$SMT_CA_CHANGED" = true ] && echo updated || echo written)."
}

smt_prometheus_ca_yaml() {
    jq -n --arg ns "$SMT_NS" --arg n "$SMT_PROM_CA_SECRET" --rawfile ca "$SMT_PROM_CA" \
        '{apiVersion: "v1", kind: "Secret", type: "Opaque",
          metadata: {name: $n, namespace: $ns, labels: {"app.kubernetes.io/part-of": "scaling-manager"}},
          data: {"ca.crt": ($ca | @base64)}}' | yq -P
}

# --- The preflight -----------------------------------------------------------

# smt_preflight <label>: delete and create the preflight Job, wait for it,
# print its log. Sets SMT_PF_FAIL, SMT_PF_WARN, SMT_PF_UNKNOWN (check names)
# and keeps the log in SMT_TMP/preflight-<label>.log. Exit 5 on a timeout.
smt_preflight() {
    local label="$1" end="" i=0 t="${SMT_PREFLIGHT_TIMEOUT:-600}" log="$SMT_TMP/preflight-$1.log"
    echo "Preflight ($label):"
    smt_kc delete job "$SMT_PREFLIGHT_JOB" -n "$SMT_NS" --ignore-not-found --wait=true
    smt_kc apply -f "$SMT_DIR/$SMT_PREFLIGHT" -l "$SMT_PART_LABEL=controller"
    while :; do
        smt_get job "$SMT_PREFLIGHT_JOB" -n "$SMT_NS" || smt_exit 7 "the preflight Job disappeared while it ran"
        end="$(printf '%s' "$KC_OUT" | jq -r '[.status.conditions[]? | select(.status == "True") | .type | select(. == "Complete" or . == "Failed")] | first // ""')"
        [ -n "$end" ] && break
        [ "$i" -lt "$t" ] || smt_exit 5 "the preflight Job did not finish within ${t}s: kubectl logs -n $SMT_NS job/$SMT_PREFLIGHT_JOB"
        sleep "${SMT_POLL:-3}"
        i=$((i + $(smt_poll_step)))
    done
    smt_kc logs -n "$SMT_NS" "job/$SMT_PREFLIGHT_JOB"
    printf '%s\n' "$KC_OUT" > "$log"
    sed 's/^/  /' "$log"
    SMT_PF_FAIL="$(awk '$1 == "FAIL" {print $2}' "$log" | sort -u | tr '\n' ' ')"
    SMT_PF_WARN="$(awk '$1 == "WARN" {print $2}' "$log" | sort -u | tr '\n' ' ')"
    SMT_PF_UNKNOWN="$(awk '$1 == "UNKNOWN" {print $2}' "$log" | sort -u | tr '\n' ' ')"
    # The checks its own account was refused: the cluster reads admin-finish
    # removes with ClusterRoleBinding SMT_PREFLIGHT_CRB.
    SMT_PF_FORBIDDEN="$(awk '$1 == "UNKNOWN" && /forbidden/ {print $2}' "$log" | sort -u | tr '\n' ' ')"
    if [ "$end" = Failed ] && [ -z "$SMT_PF_FAIL" ]; then
        smt_exit 1 "the preflight Job failed without a FAIL line: kubectl logs -n $SMT_NS job/$SMT_PREFLIGHT_JOB"
    fi
    SMT_PF_LAST="$log"
}

# smt_unknown_other echoes the UNKNOWN checks the controller does not answer.
smt_unknown_other() {
    local c out="" excused
    excused="$(smt_unknown_excused)"
    for c in $SMT_PF_UNKNOWN; do
        case " $SMT_NEEDS_CONTROLLER " in *" $c "*) continue ;; esac
        case " $excused " in *" $c "*) continue ;; esac
        out="$out $c"
    done
    printf '%s\n' "${out# }"
}

# smt_unknown_excused echoes the UNKNOWN checks refused to the preflight
# account once admin-finish removed its cluster reads: expected after it, so
# --strict does not stop on them. Only while that ClusterRoleBinding is not
# there; a namespace owner may not read it, which counts as not there, since
# its reads are what the refusals show.
smt_unknown_excused() {
    [ -n "${SMT_PF_FORBIDDEN:-}" ] || return 0
    kc_capture get clusterrolebinding "$SMT_PREFLIGHT_CRB" -o name && return 0
    printf '%s\n' "$SMT_PF_FORBIDDEN"
}

# smt_preflight_gate <what stops>: exit 1 on a FAIL, 6 on --strict and an
# UNKNOWN the install cannot wait out.
smt_preflight_gate() {
    local other
    if [ -n "$SMT_PF_FAIL" ]; then
        smt_exit 1 "the preflight has a FAIL (${SMT_PF_FAIL% }): $1. Each FAIL above prints its fix; then run this again"
    fi
    other="$(smt_unknown_other)"
    if [ "$SMT_STRICT" = true ] && [ -n "$other" ]; then
        smt_exit 6 "--strict: the preflight could not check $other: $1"
    fi
}

# smt_preflight_first: the preflight before the scaler TLS Secrets and the
# controller. When it stops the install and this run created the settings,
# it removes them, so the next run creates them again from its flags: a wrong
# --prometheus-url must not stick in settings no controller has read yet.
smt_preflight_first() {
    local why="stopped before the scaler TLS Secrets and the controller; nothing more was created"
    smt_preflight before-install
    if [ "$SMT_SETTINGS_CREATED" = true ] && { [ -n "$SMT_PF_FAIL" ] || { [ "$SMT_STRICT" = true ] && [ -n "$(smt_unknown_other)" ]; }; }; then
        smt_kc delete configmap "$SMT_SETTINGS" -n "$SMT_NS" --ignore-not-found
        why="stopped before the scaler TLS Secrets and the controller, and removed $SMT_SETTINGS, which this run created: the next run creates it from its flags"
    fi
    smt_preflight_gate "$why"
}

# smt_summary_lines: the WARN and UNKNOWN lines of the last preflight, again.
smt_summary_lines() {
    [ -n "${SMT_PF_LAST:-}" ] || return 0
    local other
    if [ -n "$SMT_PF_WARN" ]; then
        echo "Warnings (the install went on):"
        grep -E '^WARN ' "$SMT_PF_LAST" | sed 's/^/  /'
    fi
    other="$(smt_unknown_other)"
    if [ -n "$SMT_PF_UNKNOWN" ]; then
        echo "Not checked (UNKNOWN is not a pass):"
        grep -E '^UNKNOWN ' "$SMT_PF_LAST" | sed 's/^/  /'
        case " $SMT_PF_UNKNOWN " in
            *" scaler.tls "*) echo "  scaler.tls stays UNKNOWN: the scaler NetworkPolicy admits KEDA only, so the preflight Job cannot dial it. scalerTLS in $SMT_STATUS_CM shows mutual TLS instead." ;;
        esac
    fi
}

# --- install steps -----------------------------------------------------------

# smt_check_stamp: the admin stamp exists and names these admin objects, or
# exit 3 with the admin-setup line.
smt_check_stamp() {
    local have want keda
    want="$(smt_admin_hash)"
    if ! smt_get configmap "$SMT_STAMP" -n "$SMT_NS"; then
        echo "The cluster admin has not run admin-setup for $SMT_NS (no ConfigMap $SMT_STAMP). Ask them to run, with this tools image:" >&2
        echo "  $(smt_admin_line)" >&2
        exit 3
    fi
    have="$(printf '%s' "$KC_OUT" | jq -r '.data.adminObjectsSha256 // ""')"
    keda="$(printf '%s' "$KC_OUT" | jq -r '.data.kedaNamespace // ""')"
    if [ "$have" != "$want" ]; then
        echo "The admin objects in $SMT_NS are from another release ($(printf '%s' "$KC_OUT" | jq -r '.data.release // "unknown"')): this release changes them, and only a cluster admin may apply them. Ask them to run, with this tools image ($(smt_version)):" >&2
        echo "  $(smt_admin_line "$keda")" >&2
        exit 3
    fi
    SMT_STAMP_KEDA="$keda"
}

# smt_check_adoptable: a live controller Deployment must come from the release
# files (their part label); an install made with make is not adopted yet.
smt_check_adoptable() {
    smt_get deployment "$SMT_DEPLOY" -n "$SMT_NS" || { SMT_HAD_CONTROLLER=false; return 0; }
    SMT_HAD_CONTROLLER=true
    printf '%s' "$KC_OUT" | jq -e --arg l "$SMT_PART_LABEL" '.metadata.labels[$l] // empty' >/dev/null && return 0
    smt_exit 4 "Deployment $SMT_DEPLOY in $SMT_NS was not installed from the release files (no $SMT_PART_LABEL label): an install made with make deploy-scaling-manager. Upgrade it with make, or move it to the release files first: $SMT_MIGRATION_DOC"
}

# smt_scaler_secrets: the scaler TLS Secrets, created when absent, never
# applied (that would empty Secrets the controller filled).
smt_scaler_secrets() {
    local n name
    n="$(yq ea '[select(.kind == "Secret")] | length' "$SMT_DIR/scaling-manager-scaler-tls.yaml")"
    [ "$n" -gt 0 ] || smt_usage "scaling-manager-scaler-tls.yaml holds no Secret"
    for name in $(yq e -N 'select(.kind == "Secret") | .metadata.name' "$SMT_DIR/scaling-manager-scaler-tls.yaml"); do
        if smt_get secret "$name" -n "$SMT_NS"; then
            echo "  secret/$name exists; kept"
            continue
        fi
        N="$name" yq ea 'select(.kind == "Secret" and .metadata.name == strenv(N))' "$SMT_DIR/scaling-manager-scaler-tls.yaml" > "$SMT_TMP/secret-$name.yaml"
        smt_kc create -f "$SMT_TMP/secret-$name.yaml"
        echo "  secret/$name created, empty: the controller fills it"
    done
}

# smt_controller: the part=controller objects, then the rollout.
smt_controller() {
    echo "Controller:"
    smt_apply "$SMT_DIR/$SMT_MAIN" controller
    if [ "${SMT_CA_CHANGED:-false}" = true ] && [ "${SMT_HAD_CONTROLLER:-false}" = true ]; then
        echo "  the Prometheus CA changed, and the controller reads it at start: restarting it"
        smt_kc rollout restart "deployment/$SMT_DEPLOY" -n "$SMT_NS"
    fi
    smt_rollout
}

smt_rollout() {
    local rc=0
    kc_capture rollout status "deployment/$SMT_DEPLOY" -n "$SMT_NS" --timeout="${SMT_ROLLOUT_TIMEOUT:-300s}" || rc=$?
    if [ "$rc" != 0 ]; then
        case "$KC_ERR" in
            *"timed out"*|*"exceeded its progress deadline"*) smt_exit 5 "the controller did not become ready within ${SMT_ROLLOUT_TIMEOUT:-300s}: kubectl describe deployment/$SMT_DEPLOY -n $SMT_NS" ;;
        esac
        smt_kubectl_failed "rollout status deployment/$SMT_DEPLOY -n $SMT_NS"
    fi
    echo "  deployment/$SMT_DEPLOY ready"
}

# --- admin-finish steps ------------------------------------------------------

# smt_wait_client_cert: until the controller filled the KEDA client certificate.
smt_wait_client_cert() {
    local i=0 t="${SMT_CERT_TIMEOUT:-180}" crt
    smt_get deployment "$SMT_DEPLOY" -n "$SMT_NS" \
        || smt_exit 4 "no controller in $SMT_NS (Deployment $SMT_DEPLOY): the namespace owner has not run smt install -n $SMT_NS yet"
    while :; do
        if smt_get secret "$LLMSM_SCALER_CLIENT_SECRET" -n "$SMT_NS"; then
            crt="$(printf '%s' "$KC_OUT" | jq -r '.data["tls.crt"] // ""')"
            [ -n "$crt" ] && return 0
        fi
        [ "$i" -lt "$t" ] || smt_exit 5 "the controller has not filled Secret $LLMSM_SCALER_CLIENT_SECRET in $SMT_NS within ${t}s. Has the namespace owner run smt install? kubectl logs -n $SMT_NS deployment/$SMT_DEPLOY"
        sleep "${SMT_POLL:-3}"
        i=$((i + $(smt_poll_step)))
    done
}

# smt_finish_keda_ns: sets SMT_KEDA_NS for admin-finish. The flag; else the
# stamp admin-setup wrote, which must agree with the live NetworkPolicy; else
# the APIService detection. None, or a disagreement: exit 4.
smt_finish_keda_ns() {
    local stamp np rc=0 found
    [ -n "$SMT_KEDA_NS" ] && { SMT_KEDA_FROM="--keda-namespace"; return 0; }
    if [ "$SMT_SCOPE" = namespace ] && smt_get configmap "$SMT_STAMP" -n "$SMT_NS"; then
        stamp="$(printf '%s' "$KC_OUT" | jq -r '.data.kedaNamespace // ""')"
        smt_call llmsm_scaler_np_keda_ns "$SMT_NS" || rc=$?
        np="$SMT_CALL_OUT"
        [ "$rc" != 2 ] || smt_exit 7 "cannot read NetworkPolicy scaling-manager-external-scaler in $SMT_NS: $(kc_err_line "$KC_ERR")"
        if [ -n "$stamp" ] && [ "$rc" = 0 ] && [ "$np" = "$stamp" ]; then
            SMT_KEDA_NS="$stamp"
            SMT_KEDA_FROM="ConfigMap $SMT_STAMP, which the scaler NetworkPolicy agrees with"
            return 0
        fi
        smt_exit 4 "ConfigMap $SMT_STAMP says KEDA runs in '${stamp}', and the scaler NetworkPolicy in $SMT_NS admits '${np:-nothing it can name}'. Run smt admin-setup -n $SMT_NS --keda-namespace <namespace> again, or give --keda-namespace"
    fi
    rc=0
    smt_call llmsm_keda_apiservice_ns || rc=$?
    found="$SMT_CALL_OUT"
    case "$rc" in
        0) SMT_KEDA_NS="$found"; SMT_KEDA_FROM="the namespace serving the external-metrics APIService"; return 0 ;;
        2) smt_exit 7 "cannot read APIService v1beta1.external.metrics.k8s.io: $(kc_err_line "$KC_ERR")" ;;
    esac
    smt_exit 4 "cannot find the namespace KEDA runs in. Give --keda-namespace <namespace>"
}

# smt_library_exits: in a subshell that runs a deploy/lib function, make its
# refusals (log_error, an exit 1 there) exit with the codes of this tool: 7 for
# a write kubectl could not make, 4 for anything else (a refusal a person
# must resolve: a foreign or unreadable object, KEDA not ready). Exit 1 here
# means a preflight FAIL, which a refusal is not.
smt_library_exits() {
    log_error() {
        echo -e "${RED}[ERROR]${NC} $1" >&2
        case "$1" in
            "Could not write"*) exit 7 ;;
            *) exit 4 ;;
        esac
    }
}

# smt_keda_side: the KEDA side of mutual TLS, by llmsm_scaler_keda_auth_apply
# (deploy/lib/scaler_tls.sh), the function make setup-prereqs runs. It exits
# on a refusal (log_error), so it runs in a subshell that reports back.
# Sets SMT_KEDA_WROTE.
smt_keda_side() {
    local state="$SMT_TMP/keda-side" rc=0
    : > "$state"
    (
        smt_library_exits
        export LLMSM_SCALER_TLS_WANTED=true LLMSM_NS="$SMT_NS" ENVIRONMENT="$SMT_PLATFORM"
        export KEDA_NAMESPACE="$SMT_KEDA_NS" KEDA_NAMESPACE_EXPLICIT=true LLMSM_TOOLS_MODE=true LLMSM_SCOPE="$SMT_SCOPE"
        LLMSM_SCALER_KEDA_READY=false
        LLMSM_SCALER_KEDA_WROTE=false
        llmsm_scaler_keda_auth_apply
        printf '%s %s %s\n' "$LLMSM_SCALER_KEDA_READY" "$LLMSM_SCALER_KEDA_WROTE" "${LLMSM_SCALER_KEDA_OBJ_NS:-}" > "$state"
    ) || rc=$?
    case "$rc" in
        0) ;;
        4) smt_exit 4 "KEDA's side of mutual TLS was refused; the error above says why and what to do" ;;
        *) smt_exit 7 "writing KEDA's side failed (the line above says which call)" ;;
    esac
    read -r SMT_KEDA_READY SMT_KEDA_WROTE SMT_KEDA_OBJ_NS < "$state"
    [ "$SMT_KEDA_READY" = true ] || smt_exit 4 "KEDA's side of mutual TLS was not written (the warning above says why)"
}

# smt_wait_mtls: until the controller reports scalerTLS: mutual TLS.
smt_wait_mtls() {
    local i=0 t="${SMT_MTLS_TIMEOUT:-180}" v=""
    while :; do
        if smt_get configmap "$SMT_STATUS_CM" -n "$SMT_NS"; then
            v="$(printf '%s' "$KC_OUT" | jq -r '.data.scalerTLS // ""')"
            case "$v" in "mutual TLS"*) echo "  scalerTLS: $v"; return 0 ;; esac
        fi
        [ "$i" -lt "$t" ] || smt_exit 5 "scalerTLS is '${v}', not mutual TLS, after ${t}s: kubectl get configmap $SMT_STATUS_CM -n $SMT_NS -o jsonpath='{.data.scalerTLSAction}'"
        sleep "${SMT_POLL:-3}"
        i=$((i + $(smt_poll_step)))
    done
}

# smt_keda_connection: the stale-connection check make verify-deployment runs
# (llmsm_scaler_keda_conn_state). KEDA is shared: restarted only with
# --restart-keda.
smt_keda_connection() {
    (
        smt_library_exits
        export LLMSM_NS="$SMT_NS" KEDA_NAMESPACE="$SMT_KEDA_NS" KEDA_NAMESPACE_EXPLICIT=true LLMSM_TOOLS_MODE=true LLMSM_SCOPE="$SMT_SCOPE"
        LLMSM_SCALER_KEDA_RUN_NS=""
        llmsm_scaler_keda_conn_state
        case "$LLMSM_SCALER_KEDA_CONN" in
            none) echo "  KEDA holds no old connection to the scaler" ;;
            stale)
                llmsm_scaler_keda_conn_report || true
                if [ "$SMT_RESTART_KEDA" = true ]; then
                    kubectl rollout restart deployment/keda-operator -n "$SMT_KEDA_NS" >/dev/null \
                        || { echo "smt: restarting KEDA's operator in $SMT_KEDA_NS failed" >&2; exit 7; }
                    echo "  --restart-keda: restarted KEDA's operator in $SMT_KEDA_NS"
                else
                    echo "  KEDA's operator is shared, so it was not restarted. Restart it, or run this again with --restart-keda: $(llmsm_scaler_keda_restart_cmd)"
                fi ;;
            *) llmsm_scaler_keda_conn_report || true ;;
        esac
    ) || exit $?
}

# smt_finish <what>: the admin-finish steps, for both scopes.
smt_finish() {
    echo "KEDA's client certificate:"
    smt_wait_client_cert
    echo "  filled by the controller"
    [ "$SMT_SCOPE" = cluster ] || smt_finish_keda_ns
    echo "KEDA's side of mutual TLS (KEDA in $SMT_KEDA_NS, from ${SMT_KEDA_FROM:-the flag}):"
    smt_keda_side
    if [ "$SMT_KEDA_WROTE" = true ]; then
        echo "  written; restarting the controller, which reads it at start"
        smt_kc rollout restart "deployment/$SMT_DEPLOY" -n "$SMT_NS"
        smt_rollout
    else
        echo "  already in place; nothing restarted"
    fi
    smt_wait_mtls
    # The check reads the controller log by its label, which the preflight
    # Job pods carry too; the Job is created again below.
    smt_kc delete job "$SMT_PREFLIGHT_JOB" -n "$SMT_NS" --ignore-not-found --cascade=foreground --wait=true
    smt_keda_connection
    smt_preflight final
    if [ -n "$SMT_PF_FAIL" ] || { [ "$SMT_STRICT" = true ] && [ -n "$(smt_unknown_other)" ]; }; then
        echo "Kept ClusterRoleBinding $SMT_PREFLIGHT_CRB, so the preflight can run again. After the fix:" >&2
        echo "  kubectl delete clusterrolebinding $SMT_PREFLIGHT_CRB --ignore-not-found" >&2
        smt_preflight_gate "kept ClusterRoleBinding $SMT_PREFLIGHT_CRB"
    fi
    smt_kc delete clusterrolebinding "$SMT_PREFLIGHT_CRB" --ignore-not-found
    echo "Removed ClusterRoleBinding $SMT_PREFLIGHT_CRB: the preflight's cluster reads are needed only while it runs."
}

# --- dry-run -----------------------------------------------------------------

# smt_dry_write <file> [part] <source>: one step file in SMT_OUT.
smt_dry_write() {
    local name="$1" part="$2" src="$3"
    if [ -n "$part" ]; then
        smt_select "$part" "$src" > "$SMT_OUT/$name"
    else
        yq ea 'select(.kind != null)' "$src" > "$SMT_OUT/$name"
    fi
    echo "  $SMT_OUT/$name"
}

smt_dry_admin() {
    local m
    smt_dry_write 10-admin.yaml admin "$SMT_DIR/$SMT_MAIN"
    smt_dry_write 11-preflight-admin.yaml admin "$SMT_DIR/$SMT_PREFLIGHT"
    if [ "$SMT_SCOPE" = cluster ]; then
        for m in "${SMT_MODEL_NS[@]}"; do
            smt_dry_write "12-model-namespace-$m.yaml" "" "$(smt_model_file "$m")"
        done
    else
        smt_dry_write 12-model-namespace.yaml "" "$SMT_DIR/scaling-manager-model-namespace.yaml"
    fi
}

smt_dry_install() {
    smt_settings_pairs
    [ -n "$SMT_PROM_URL" ] || [ "$SMT_PLATFORM" = openshift ] \
        || smt_usage "--dry-run renders a fresh install: give --prometheus-url"
    [ -n "$SMT_PROM_URL" ] || { SMT_PROM_URL="$SMT_THANOS_URL"; smt_check_scheme "$SMT_PROM_URL" "the OpenShift default, $SMT_PROM_URL,"; smt_settings_pairs; }
    smt_dry_write 20-settings.yaml "" "$(smt_settings_file)"
    if [ -n "$SMT_PROM_CA" ]; then
        smt_prometheus_ca_yaml > "$SMT_OUT/25-prometheus-ca.yaml"
        echo "  $SMT_OUT/25-prometheus-ca.yaml"
    fi
    smt_dry_write 30-preflight.yaml controller "$SMT_DIR/$SMT_PREFLIGHT"
    smt_dry_write 40-scaler-tls.yaml "" "$SMT_DIR/scaling-manager-scaler-tls.yaml"
    smt_dry_write 50-controller.yaml controller "$SMT_DIR/$SMT_MAIN"
}

smt_dry_begin() {
    mkdir -p "$SMT_OUT" || smt_usage "--out $SMT_OUT: cannot create it"
    echo "Rendered for $SMT_PLATFORM, namespace $SMT_NS (--dry-run: no cluster was read; apply in this order):"
}

# --- The commands ------------------------------------------------------------

smt_begin() {
    SMT_TMP="$(mktemp -d)" || smt_exit 7 "cannot create a scratch directory"
    trap 'rm -rf "$SMT_TMP"' EXIT
    # The libraries print in colour on a terminal and read these names.
    # shellcheck disable=SC2034
    if [ -t 2 ]; then
        BLUE='\033[0;34m' GREEN='\033[0;32m' YELLOW='\033[1;33m' RED='\033[0;31m' NC='\033[0m'
    else
        BLUE='' GREEN='' YELLOW='' RED='' NC=''
    fi
    SMT_SETTINGS_CREATED=false
}

smt_header() {
    echo "smt $1 $(smt_version) (release files: ${SMT_FILES_FROM}; $SMT_PLATFORM; namespace $SMT_NS)"
    [ "$SMT_PLATFORM" != openshift ] || echo "  The OpenShift files are not yet tested end to end."
}

# smt_admin_setup -n NS [--keda-namespace KNS]: the part of the cluster admin.
smt_admin_setup() {
    smt_begin
    smt_parse admin-setup "$@"
    smt_load_files
    smt_detect_platform
    smt_files_for
    smt_need "$SMT_MAIN" "$SMT_PREFLIGHT" scaling-manager-model-namespace.yaml
    if [ "$SMT_DRY_RUN" = true ]; then
        smt_resolve_keda_ns admin-setup
        smt_render
        smt_dry_begin
        smt_dry_admin
        smt_stamp_yaml > "$SMT_OUT/19-admin-setup-stamp.yaml"
        echo "  $SMT_OUT/19-admin-setup-stamp.yaml"
        [ "$SMT_PLATFORM" != openshift ] || echo "  then: kubectl label namespace $SMT_NS openshift.io/user-monitoring=true --overwrite"
        echo "KEDA's namespace in the scaler NetworkPolicy: $SMT_KEDA_NS ($SMT_KEDA_FROM)."
        return 0
    fi
    smt_header admin-setup
    smt_require_namespace
    smt_resolve_keda_ns admin-setup
    echo "KEDA runs in $SMT_KEDA_NS ($SMT_KEDA_FROM)."
    smt_render
    echo "Admin objects:"
    smt_apply "$SMT_DIR/$SMT_MAIN" admin
    echo "Preflight RBAC:"
    smt_apply "$SMT_DIR/$SMT_PREFLIGHT" admin
    echo "Model namespace:"
    smt_apply "$SMT_DIR/scaling-manager-model-namespace.yaml"
    if [ "$SMT_PLATFORM" = openshift ]; then
        smt_kc label namespace "$SMT_NS" openshift.io/user-monitoring=true --overwrite
        echo "Labelled namespace $SMT_NS openshift.io/user-monitoring=true."
    fi
    smt_stamp_yaml > "$SMT_TMP/stamp.yaml"
    smt_kc apply -f "$SMT_TMP/stamp.yaml"
    echo "Stamp: ConfigMap $SMT_STAMP ($(smt_version), KEDA in $SMT_KEDA_NS)."
    echo
    echo "Done. The namespace owner runs next, with their own kubeconfig:"
    echo "  smt install -n $SMT_NS --prometheus-url <https://prometheus:9090>   (an http:// Prometheus: add --allow-http)"
}

# smt_install: the part of the namespace owner, or with --scope cluster everything.
smt_install() {
    smt_begin
    smt_parse install "$@"
    smt_load_files
    smt_detect_platform
    smt_files_for
    smt_need "$SMT_MAIN" "$SMT_PREFLIGHT" scaling-manager-settings.yaml scaling-manager-scaler-tls.yaml scaling-manager-model-namespace.yaml
    if [ "$SMT_DRY_RUN" = true ]; then
        [ "$SMT_SCOPE" = namespace ] || smt_resolve_keda_ns install
        smt_render
        smt_dry_begin
        if [ "$SMT_SCOPE" = cluster ]; then
            jq -n --arg n "$SMT_NS" '{apiVersion: "v1", kind: "Namespace", metadata: {name: $n}}' | yq -P > "$SMT_OUT/05-namespace.yaml"
            echo "  $SMT_OUT/05-namespace.yaml"
            smt_dry_admin
        fi
        smt_dry_install
        [ "$SMT_SCOPE" = namespace ] || echo "KEDA's side of mutual TLS is not rendered: it copies the client certificate the controller makes. After the apply, run: smt install --scope cluster ... again."
        return 0
    fi
    smt_header install
    if [ "$SMT_SCOPE" = cluster ]; then
        smt_install_cluster
        return
    fi
    smt_render
    smt_require_namespace
    smt_check_stamp
    smt_check_adoptable
    smt_settings_preconditions
    smt_settings
    smt_prometheus_ca
    smt_preflight_first
    echo "Scaler TLS Secrets:"
    smt_scaler_secrets
    smt_controller
    smt_preflight after-install
    smt_preflight_gate "the controller is installed"
    echo
    echo "Installed in $SMT_NS, passive."
    smt_summary_lines
    echo
    echo "Left for the cluster admin (KEDA's client certificate, then drop the preflight's cluster reads):"
    echo "  smt admin-finish -n $SMT_NS"
    echo "Then this prints \"mutual TLS\":"
    echo "  kubectl get configmap $SMT_STATUS_CM -n $SMT_NS -o jsonpath='{.data.scalerTLS}{\"\\n\"}'"
}

# smt_install_cluster: admin-setup, install and admin-finish in one, for the
# cluster-wide files in scaling-manager-system. No stamp: the one who would
# read it wrote the admin objects in this run.
smt_install_cluster() {
    local m f
    # Everything that can refuse, before the first write.
    for m in "${SMT_MODEL_NS[@]}"; do
        smt_get namespace "$m" || smt_exit 4 "model namespace $m does not exist"
    done
    smt_resolve_keda_ns install
    echo "KEDA runs in $SMT_KEDA_NS ($SMT_KEDA_FROM)."
    smt_render
    smt_check_adoptable
    smt_settings_preconditions
    if ! smt_get namespace "$SMT_NS"; then
        smt_kc create namespace "$SMT_NS"
        echo "Created namespace $SMT_NS."
    fi
    echo "Admin objects:"
    smt_apply "$SMT_DIR/$SMT_MAIN" admin
    echo "Preflight RBAC:"
    smt_apply "$SMT_DIR/$SMT_PREFLIGHT" admin
    [ ${#SMT_MODEL_NS[@]} -gt 0 ] || echo "No --model-namespace: no model-server PodMonitor is applied. Add each namespace with model servers with --model-namespace, and run this again."
    for m in "${SMT_MODEL_NS[@]}"; do
        echo "Model namespace $m:"
        f="$(smt_model_file "$m")"
        smt_apply "$f"
        if [ "$SMT_PLATFORM" = openshift ]; then
            smt_kc label namespace "$m" openshift.io/user-monitoring=true --overwrite
        fi
    done
    smt_settings
    smt_prometheus_ca
    smt_preflight_first
    echo "Scaler TLS Secrets:"
    smt_scaler_secrets
    smt_controller
    smt_finish
    echo
    echo "Installed in $SMT_NS for the cluster, passive, serving the scaler over mutual TLS."
    smt_summary_lines
}

# smt_admin_finish -n NS: the cluster admin, after the install.
smt_admin_finish() {
    smt_begin
    smt_parse admin-finish "$@"
    smt_load_files
    smt_detect_platform
    smt_files_for
    smt_need "$SMT_PREFLIGHT"
    smt_header admin-finish
    smt_require_namespace
    smt_render
    smt_finish
    echo
    echo "Done: $SMT_NS serves the scaler over mutual TLS."
    smt_summary_lines
}

# smt_release_files --out DIR: the release files of the image, to verify or apply by hand.
smt_release_files() {
    local src f n=0
    smt_begin
    smt_parse release-files "$@"
    [ -n "$SMT_OUT" ] || smt_usage "release-files needs --out <dir>"
    src="$(smt_baked_dir)"
    [ -f "$src/SHA256SUMS" ] || smt_usage "this image carries no release files (a development build)"
    mkdir -p "$SMT_OUT" || smt_usage "--out $SMT_OUT: cannot create it"
    for f in "$src"/scaling-manager-*.yaml "$src"/*.sigstore.json "$src/SHA256SUMS"; do
        [ -f "$f" ] || continue
        cp "$f" "$SMT_OUT/"
        n=$((n + 1))
    done
    echo "Wrote $n files of release $(smt_version) to $SMT_OUT."
    echo "scaling-manager-tools-$(smt_version).tar.gz is on the GitHub release, not in this image: SHA256SUMS lists it, so check with:"
    echo "  sha256sum -c SHA256SUMS --ignore-missing"
}
