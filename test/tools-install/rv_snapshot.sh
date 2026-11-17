#!/usr/bin/env bash
#
# rv_snapshot, sourced by test/tools-install/e2e.sh and executed by
# hack/check-tools-install-rv-snapshot.sh. Needs kubectl, jq and sha256sum.

# rv_snapshot <out> <objects file>: one line per "<kind> <namespace> <name>"
# in the objects file, with what a second install must leave as it is: the
# resourceVersion, or absent when the object cannot be read.
#
# (Cluster)TriggerAuthentication is the exception. KEDA patches its
# status.scaledobjects through the status subresource whenever a ScaledObject
# that names it is reconciled or finalized, which moves the resourceVersion
# with no install anywhere near it: the smoke suite deletes its namespaces
# without waiting, and KEDA finalized one of its ScaledObjects between the
# two snapshots around a second install. For those kinds the line holds "spec:<sha256>" of the object
# without status, resourceVersion and managedFields, so an install that
# rewrites the spec, a label, an annotation or a finalizer still shows up.
rv_snapshot() {
    local kind ns name json value
    while read -r kind ns name; do
        [ -n "$kind" ] || continue
        if [ "$ns" = - ]; then
            json="$(kubectl get "$kind" "$name" -o json 2>/dev/null)" || json=''
        else
            json="$(kubectl get "$kind" "$name" -n "$ns" -o json 2>/dev/null)" || json=''
        fi
        value=''
        if [ -n "$json" ]; then
            case "$kind" in
                ClusterTriggerAuthentication|TriggerAuthentication)
                    value="$(printf '%s' "$json" | jq -cS 'del(.status, .metadata.resourceVersion, .metadata.managedFields)' 2>/dev/null | sha256sum | cut -d' ' -f1)"
                    # An empty jq output hashes to a value too; never compare that.
                    [ -n "$(printf '%s' "$json" | jq -r '.metadata.name // empty' 2>/dev/null)" ] && value="spec:$value" || value=''
                    ;;
                *)
                    value="$(printf '%s' "$json" | jq -r '.metadata.resourceVersion // empty' 2>/dev/null)"
                    ;;
            esac
        fi
        printf '%s %s %s %s\n' "$kind" "$ns" "$name" "${value:-absent}"
    done < "$2" > "$1"
}
