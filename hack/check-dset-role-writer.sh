#!/usr/bin/env bash
#
# Executes the installer's role writer grant handling (deploy/lib) against a
# STUBBED kubectl and the real kustomize. What this pins, each of which
# parses cleanly and is wrong on a cluster:
#
#   revoke        without DSET_ROLE_WRITER=true, the prereqs phase deletes the
#                 grant by the names the install RENDERS (the per-install
#                 suffix in cluster scope), one object per kubectl call; a
#                 Forbidden delete is reported with the admin's command, never
#                 as removed; nothing there says nothing; a notice on stderr
#                 is neither a denial nor the answer, and another failure is
#                 reported by its error line (kc_capture, kc_err_line)
#   watched copy  with it, the Role copied into a namespace managed from
#                 outside is the rendered role writer Role (selected by name), renamed,
#                 labelled and bound to the controller's ServiceAccount;
#                 without it, the copy is deleted, and Forbidden is reported
#   undeploy      the uninstall's render always carries the component, so it
#                 deletes the grant whatever DSET_ROLE_WRITER says now
#   refusal       a value other than true or false stops with an error naming
#                 DSET_ROLE_WRITER
#   render        with it, the install grants exactly get, list and patch on
#                 disaggregatedsets, in one role bound to the controller, and
#                 sets DSET_ROLE_WRITER=true; without it, no role may patch a
#                 set, the variable is unset, and the manager role only reads
#                 sets and role scalers
#
# LIB_DIR overrides deploy/lib, so a negative control can run this against a
# broken copy. Every case asserts something a missing function also fails.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB_DIR="${LIB_DIR:-$ROOT/deploy/lib}"
FAIL=0
CASES=0
EXPECTED_CASES=17
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "  FAIL: $*"; FAIL=$((FAIL + 1)); }
begin() { CASES=$((CASES + 1)); echo "== $*"; }

command -v yq >/dev/null 2>&1 || { echo "FAIL: yq is required (not skipping: a skipped check reads as a pass)"; exit 1; }
# Whatever renders: make passes KUSTOMIZE as bin/kustomize whether or not it
# was downloaded, and CI's deploy-scripts-check has only kubectl. `command`
# keeps the kubectl stub below from answering its own render.
if [ -n "${KUSTOMIZE:-}" ] && [ -x "${KUSTOMIZE}" ]; then
    KBUILD=("$KUSTOMIZE" build)
elif [ -x "$ROOT/bin/kustomize" ]; then
    KBUILD=("$ROOT/bin/kustomize" build)
elif command -v kustomize >/dev/null 2>&1; then
    KBUILD=(command kustomize build)
elif command -v kubectl >/dev/null 2>&1; then
    KBUILD=(command kubectl kustomize)
else
    echo "FAIL: neither kustomize nor kubectl is available to render overlays"
    exit 1
fi
# A renderer that renders nothing would fail every case for the wrong reason.
if ! "${KBUILD[@]}" "$ROOT/config/overlays/cluster-scoped/kubernetes" >/dev/null 2>"$WORK/render.err"; then
    echo "FAIL: ${KBUILD[*]} cannot render the cluster-scoped overlay: $(head -3 "$WORK/render.err")"
    exit 1
fi

for f in constants common infra_scaling_manager cleanup physical_limiter pod_writer scaler_tls; do
    if [ -f "$LIB_DIR/$f.sh" ]; then tr -d '\r' <"$LIB_DIR/$f.sh" >"$WORK/$f.sh"; else : >"$WORK/$f.sh"; fi
done

CTRL=llmsm-ctl
WATCHED=team-a

# stub_kubectl records every call in $WORK/calls and answers:
#   kustomize DIR      the real render
#   apply -f -         stdin appended to $WORK/applied.yaml
#   delete -f -        stdin appended to $WORK/deleted.yaml
#   delete KIND NAME   $DELETE_MODE: deleted | forbidden | notice | error | absent
#   get ...            NotFound
# anything else succeeds silently.
stub_kubectl() {
    printf '%s\n' "$*" >>"$WORK/calls"
    case "$1" in
        kustomize) "${KBUILD[@]}" "$2" ;;
        apply) cat >>"$WORK/applied.yaml"; printf -- '---\n' >>"$WORK/applied.yaml" ;;
        delete)
            if [ "${2:-}" = "-f" ]; then cat >>"$WORK/deleted.yaml"; return 0; fi
            case "${DELETE_MODE:-absent}" in
                deleted) echo "$2.rbac.authorization.k8s.io \"$3\" deleted" ;;
                forbidden) echo "Error from server (Forbidden): $2.rbac.authorization.k8s.io \"$3\" is forbidden: User \"tenant\" cannot delete resource" >&2; return 1 ;;
                # A success with a notice that names a forbidden thing on stderr.
                notice) echo "Warning: policy notice: a forbidden-words list is deprecated" >&2
                        echo "$2.rbac.authorization.k8s.io \"$3\" deleted" ;;
                # A failure that is not a denial, after a notice.
                error) echo "I1113 09:55:01.123456   1 request.go:700] Waited for 1s due to client-side throttling" >&2
                       echo "Error from server (InternalError): an error on the server has prevented the request" >&2; return 1 ;;
            esac ;;
        get) echo 'Error from server (NotFound): not found' >&2; return 1 ;;
    esac
    return 0
}

# in_lib runs "$@" in a subshell with the libraries sourced and kubectl stubbed,
# output to $WORK/out.
in_lib() {
    : >"$WORK/calls"; : >"$WORK/applied.yaml"; : >"$WORK/deleted.yaml"
    (
        BLUE='' GREEN='' YELLOW='' RED='' NC=''
        for f in constants common infra_scaling_manager cleanup physical_limiter pod_writer scaler_tls; do
            # shellcheck source=/dev/null
            source "$WORK/$f.sh"
        done
        kubectl() { stub_kubectl "$@"; }
        LLMSM_PROJECT="$ROOT"
        LLMSM_NS="$CTRL"
        "$@"
    ) >"$WORK/out" 2>&1
}

# rendered_names <scope> echoes "kind name" of the role writer objects the
# install renders for $CTRL: the component plus the per-install renames.
rendered_names() {
    local dir
    dir="$(mktemp -d "$WORK/ovl.XXXXXX")"
    ln -s "$ROOT/config/overlays/$1-scoped/kubernetes" "$dir/base"
    printf 'namespace: %s\nresources:\n- ./base\n' "$CTRL" >"$dir/kustomization.yaml"
    (
        BLUE='' GREEN='' YELLOW='' RED='' NC=''
        # shellcheck source=/dev/null
        source "$WORK/constants.sh"; source "$WORK/common.sh"
        LLMSM_SCOPE="$1" LLMSM_PROJECT="$ROOT" llmsm_append_dset_role_writer_component "$dir"
        llmsm_append_crb_name_patches "$dir/kustomization.yaml" "$CTRL"
    ) >/dev/null 2>&1
    "${KBUILD[@]}" "$dir" 2>/dev/null | yq -N -r 'select(.metadata.name | test("dset-role-writer")) | (.kind | downcase) + " " + .metadata.name' | sort
}

# deleted_names echoes "kind name" of every single-object delete in $WORK/calls.
deleted_names() {
    awk '$1 == "delete" && $2 != "-f" { print $2 " " $3 }' "$WORK/calls" | sort
}

for scope in cluster namespace; do
    want="$(rendered_names "$scope")"
    begin "revoke, $scope scope: deletes exactly the rendered names"
    [ -n "$want" ] || fail "the component rendered nothing for $scope scope"
    DELETE_MODE=deleted LLMSM_SCOPE="$scope" in_lib llmsm_revoke_dset_role_writer
    got="$(deleted_names)"
    [ "$got" = "$want" ] || fail "deleted [$(echo "$got" | paste -sd,)], rendered [$(echo "$want" | paste -sd,)]"
    grep -q 'removed the DisaggregatedSet grant' "$WORK/out" || fail "a delete that removed the grant was not reported: $(cat "$WORK/out")"
    grep -q 'INSTALL_PHASE=scaling-manager' "$WORK/out" || fail "the split-install step is not named: $(cat "$WORK/out")"
done

begin "revoke: one object per kubectl call (render mode records one target per call)"
LLMSM_SCOPE=cluster DELETE_MODE=deleted in_lib llmsm_revoke_dset_role_writer
n="$(awk '$1 == "delete"' "$WORK/calls" | awk '{ t = 0; for (i = 2; i <= NF; i++) if ($i !~ /^-/ && $(i-1) != "-n") t++; if (t != 2) bad++ } END { print bad + 0 }')"
[ "$n" = 0 ] || fail "$n delete call(s) name more than one object: $(cat "$WORK/calls")"

begin "revoke: Forbidden is reported with the admin's command, never as removed"
LLMSM_SCOPE=cluster DELETE_MODE=forbidden in_lib llmsm_revoke_dset_role_writer
grep -q 'Forbidden' "$WORK/out" || fail "a Forbidden delete was not reported: $(cat "$WORK/out")"
grep -q 'kubectl delete clusterrole scaling-manager-dset-role-writer-role-' "$WORK/out" || fail "the admin's command is missing: $(cat "$WORK/out")"
! grep -q 'removed the DisaggregatedSet grant' "$WORK/out" || fail "a Forbidden delete was reported as removed"

begin "revoke: a notice on stderr beside a delete is neither a denial nor the answer"
LLMSM_SCOPE=cluster DELETE_MODE=notice in_lib llmsm_revoke_dset_role_writer
grep -q 'removed the DisaggregatedSet grant' "$WORK/out" || fail "a delete with a notice was not reported as removed: $(cat "$WORK/out")"
! grep -q 'Forbidden' "$WORK/out" || fail "a notice naming forbidden was taken for a denial: $(cat "$WORK/out")"

begin "revoke: another failure is reported with its error line, not the notice before it"
LLMSM_SCOPE=cluster DELETE_MODE=error in_lib llmsm_revoke_dset_role_writer
grep -q 'could not be deleted (Error from server (InternalError)' "$WORK/out" || fail "the failure was not reported with its error line: $(cat "$WORK/out")"
! grep -q 'removed the DisaggregatedSet grant\|Forbidden' "$WORK/out" || fail "a failed delete was reported as removed or denied"

begin "revoke: nothing to delete says nothing"
LLMSM_SCOPE=cluster DELETE_MODE=absent in_lib llmsm_revoke_dset_role_writer
[ ! -s "$WORK/out" ] || fail "said something with nothing to delete: $(cat "$WORK/out")"

# The watched-namespace copy needs an overlay that already carries the
# namespace-scoped component, as the deploy's tmp overlay does.
OVL="$WORK/deploy-ovl"
mkdir -p "$OVL"
ln -s "$ROOT/config/overlays/namespace-scoped/kubernetes" "$OVL/base"
printf 'namespace: %s\nresources:\n- ./base\n' "$CTRL" >"$OVL/kustomization.yaml"
(
    BLUE='' GREEN='' YELLOW='' RED='' NC=''
    # shellcheck source=/dev/null
    source "$WORK/constants.sh"; source "$WORK/common.sh"
    LLMSM_SCOPE=namespace LLMSM_PROJECT="$ROOT" llmsm_append_dset_role_writer_component "$OVL"
) >/dev/null 2>&1

begin "watched copy, on: the rendered role writer Role, renamed, labelled, bound to the controller"
DSET_ROLE_WRITER=true LLMSM_SCOPE=namespace LLMSM_WATCH_NS="$WATCHED" in_lib llmsm_apply_watch_namespace_dset_role_writer "$OVL"
role="$(yq -N -r 'select(.kind == "Role") | .metadata.namespace + "/" + .metadata.name + " " + ([.rules[].resources[]] | join(",")) + " " + ([.rules[].verbs[]] | join(","))' "$WORK/applied.yaml")"
case "$role" in
    "$WATCHED/scaling-manager-dset-role-writer-role-"*" disaggregatedsets get,list,patch") ;;
    *) fail "applied Role [$role]; out: $(cat "$WORK/out")" ;;
esac
[ "$(yq -N -r 'select(.kind == "Role") | .metadata.labels."llm-d.ai/watch-grant-for"' "$WORK/applied.yaml")" != "" ] \
    || fail "the copied Role carries no watch-grant label, so the revoke would not find it"
binding="$(yq -N -r 'select(.kind == "RoleBinding") | .metadata.namespace + " " + .roleRef.name + " " + .subjects[0].namespace + "/" + .subjects[0].name' "$WORK/applied.yaml")"
case "$binding" in
    "$WATCHED scaling-manager-dset-role-writer-role-"*" $CTRL/scaling-manager-controller-manager") ;;
    *) fail "applied RoleBinding [$binding]" ;;
esac

begin "watched copy, off: the copy is deleted"
DSET_ROLE_WRITER=false LLMSM_SCOPE=namespace LLMSM_WATCH_NS="$WATCHED" DELETE_MODE=deleted in_lib llmsm_apply_watch_namespace_dset_role_writer "$OVL"
grep -q "^delete role scaling-manager-dset-role-writer-role-.* -n $WATCHED" "$WORK/calls" || fail "the Role copy was not deleted: $(cat "$WORK/calls")"
grep -q "^delete rolebinding scaling-manager-dset-role-writer-rolebinding-.* -n $WATCHED" "$WORK/calls" || fail "the RoleBinding copy was not deleted"
[ ! -s "$WORK/applied.yaml" ] || fail "applied something with the repair off"

begin "watched copy, off: Forbidden is reported"
DSET_ROLE_WRITER=false LLMSM_SCOPE=namespace LLMSM_WATCH_NS="$WATCHED" DELETE_MODE=forbidden in_lib llmsm_apply_watch_namespace_dset_role_writer "$OVL"
grep -q "Forbidden" "$WORK/out" || fail "a Forbidden delete in $WATCHED was not reported: $(cat "$WORK/out")"

begin "undeploy: deletes the grant though DSET_ROLE_WRITER is unset"
unset DSET_ROLE_WRITER
LLMSM_SCOPE=cluster in_lib undeploy_scaling_manager_controller
got="$(yq -N -r 'select(.metadata.name | test("dset-role-writer")) | .kind' "$WORK/deleted.yaml" | sort | paste -sd, -)"
[ "$got" = "ClusterRole,ClusterRoleBinding" ] || fail "the uninstall deletes [$got] of the grant, want ClusterRole,ClusterRoleBinding"

begin "refusal: a value other than true or false names DSET_ROLE_WRITER"
DSET_ROLE_WRITER=yes in_lib llmsm_dset_role_writer_on
rc=0; grep -q "DSET_ROLE_WRITER must be true or false (got 'yes')" "$WORK/out" || rc=1
[ "$rc" = 0 ] || fail "no refusal naming the variable: $(cat "$WORK/out")"
DSET_ROLE_WRITER=true in_lib llmsm_dset_role_writer_on && [ ! -s "$WORK/out" ] || fail "true was refused or noisy"

# render_install <scope> <on|off> renders what the install applies for $CTRL,
# with the component when on, as llmsm_render_manifests does.
render_install() {
    local dir
    dir="$(mktemp -d "$WORK/inst.XXXXXX")"
    ln -s "$ROOT/config/overlays/$1-scoped/kubernetes" "$dir/base"
    printf 'namespace: %s\nresources:\n- ./base\n' "$CTRL" >"$dir/kustomization.yaml"
    (
        BLUE='' GREEN='' YELLOW='' RED='' NC=''
        # shellcheck source=/dev/null
        source "$WORK/constants.sh"; source "$WORK/common.sh"
        if [ "$2" = on ]; then LLMSM_SCOPE="$1" LLMSM_PROJECT="$ROOT" llmsm_append_dset_role_writer_component "$dir"; fi
        llmsm_append_crb_name_patches "$dir/kustomization.yaml" "$CTRL"
    ) >/dev/null 2>&1
    "${KBUILD[@]}" "$dir" 2>/dev/null
}

# writer_env echoes the controller's DSET_ROLE_WRITER value, empty when unset.
writer_env() {
    yq -N -r 'select(.kind == "Deployment" and .metadata.name == "scaling-manager-controller-manager") | .spec.template.spec.containers[0].env[] | select(.name == "DSET_ROLE_WRITER") | .value'
}

# set_patchers echoes "Kind/name:verbs" of every role that may patch
# disaggregatedsets.
set_patchers() {
    yq -N -r 'select(.kind == "Role" or .kind == "ClusterRole") | select([.rules[] | select((.resources // []) | contains(["disaggregatedsets"])) | .verbs[]] | contains(["patch"])) | .kind + "/" + .metadata.name + ":" + ([.rules[] | select((.resources // []) | contains(["disaggregatedsets"])) | .verbs[]] | join(","))'
}

for scope in cluster namespace; do
    begin "render, $scope scope: on grants exactly get, list, patch on disaggregatedsets and turns the writer on"
    out="$(render_install "$scope" on)"
    [ -n "$out" ] || fail "the $scope install with the component rendered nothing"
    [ "$(printf '%s\n' "$out" | writer_env)" = true ] || fail "DSET_ROLE_WRITER is not true on the Deployment"
    got="$(printf '%s\n' "$out" | set_patchers)"
    case "$scope" in
        cluster) want="ClusterRole/scaling-manager-dset-role-writer-role-$(printf %s "$CTRL" | sha256sum | cut -c1-8):get,list,patch" ;;
        namespace) want="Role/scaling-manager-dset-role-writer-role:get,list,patch" ;;
    esac
    [ "$got" = "$want" ] || fail "roles that patch sets: [$got], want [$want]"
    subject="$(printf '%s\n' "$out" | yq -N -r 'select((.kind == "RoleBinding" or .kind == "ClusterRoleBinding") and (.roleRef.name | test("dset-role-writer"))) | .subjects[0].namespace + "/" + .subjects[0].name')"
    [ "$subject" = "$CTRL/scaling-manager-controller-manager" ] || fail "the grant is bound to [$subject]"

    begin "render, $scope scope: off grants no patch on sets; the manager role only reads them"
    out="$(render_install "$scope" off)"
    [ -n "$out" ] || fail "the $scope install rendered nothing"
    [ -z "$(printf '%s\n' "$out" | writer_env)" ] || fail "DSET_ROLE_WRITER is set without the component"
    [ -z "$(printf '%s\n' "$out" | set_patchers)" ] || fail "a role patches sets without the component: $(printf '%s\n' "$out" | set_patchers)"
    reads="$(printf '%s\n' "$out" | yq -N -r 'select(.kind == "Role" or .kind == "ClusterRole") | select(.metadata.name | test("manager-role")) | .rules[] | select(.apiGroups[0] == "disaggregatedset.x-k8s.io") | (.resources | join(",")) + ":" + (.verbs | join(","))' | sort -u)"
    [ "$reads" = "disaggregatedsetrolescalers,disaggregatedsetrolescalers/scale,disaggregatedsets:get" ] \
        || fail "the manager role's DisaggregatedSet rules are [$reads], want get on the sets, role scalers and their scale"
done

begin "revoke: says that the replicas it wrote stay"
LLMSM_SCOPE=cluster DELETE_MODE=deleted in_lib llmsm_revoke_dset_role_writer
grep -q 'A model it parked stays at zero' "$WORK/out" || fail "the parked-model warning is missing: $(cat "$WORK/out")"

if [ "$CASES" -ne "$EXPECTED_CASES" ]; then
    echo "FAIL: $CASES cases ran, not $EXPECTED_CASES"
    FAIL=$((FAIL + 1))
fi
if [ "$FAIL" -gt 0 ]; then
    echo "check-dset-role-writer: $FAIL failure(s) in $CASES cases"
    exit 1
fi
echo "check-dset-role-writer: $CASES cases passed"
