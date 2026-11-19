#!/usr/bin/env bash
#
# Deploys the capacity planner's in-cluster mode for plain Kubernetes
# (config/capacity-planner/kubernetes) and checks what
# docs/guides/capacity-planner/in-cluster.md promises: the planner listens on
# the Pod's loopback with no Service, a user opens it with kubectl
# port-forward, the Role gates who may, and recordings are copied in with the
# loader Pod. `make test-e2e-capacity-planner-in-cluster` runs it on a kind
# cluster with the image loaded.
#
# The users are real identities, never the cluster admin:
#   viewer   a ServiceAccount token in a kubeconfig of its own (tokenFile), so
#            only system:authenticated and its own groups; it is granted nothing
#            but what the shipped RoleBinding gives everyone.
#   carol    impersonated, in the group "planners".
#   dave     impersonated, in no group but system:authenticated.
#
# Environment:
#   PLANNER_IMG   the planner image, already on the cluster's nodes (required)
#   NS            the planner's namespace (default capacity-planner-e2e)
#   KEEP          true leaves the namespaces behind
#
# Exit 0 only when every case passed and the count is EXPECT_CASES.

set -uo pipefail

PLANNER_IMG="${PLANNER_IMG:?set PLANNER_IMG to the planner image loaded into the cluster}"
NS="${NS:-capacity-planner-e2e}"
USERS_NS="${NS}-users"
KEEP="${KEEP:-false}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RECORDING="$ROOT/internal/engines/analyzers/learnreplay/testdata/goldens/ttft-open-loop-short/recording.jsonl.gz"
# One ok or FAIL per case. Change it deliberately with the cases.
EXPECT_CASES=14

for tool in kubectl curl jq; do
    command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: $tool is required"; exit 1; }
done
[ -s "$RECORDING" ] || { echo "FAIL: $RECORDING is missing"; exit 1; }

T="$(mktemp -d)"
PF_PIDS=()
cleanup() {
    local p
    for p in "${PF_PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
    if [ "$KEEP" != true ]; then
        kubectl delete namespace "$NS" "$USERS_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
    rm -rf "$T"
}
trap cleanup EXIT

PASSED=0
FAILED=0
ok()   { printf '  ok   %s\n' "$1"; PASSED=$((PASSED + 1)); }
fail() { printf '  FAIL %s\n' "$1"; FAILED=$((FAILED + 1)); }
die()  { printf 'FATAL %s\n' "$1"; exit 1; }

# The image under test, over the shipped overlay; a kustomization outside the
# tree names the overlay by a relative path.
overlay() { # overlay <dir> <overlay> [extra kustomization lines file]
    local dir="$1" rel
    mkdir -p "$dir"
    rel="$(realpath --relative-to="$dir" "$ROOT/config/capacity-planner/$2")"
    {
        echo "apiVersion: kustomize.config.k8s.io/v1beta1"
        echo "kind: Kustomization"
        echo "resources:"
        echo "  - $rel"
        echo "images:"
        echo "  - name: ghcr.io/llm-d-incubation/llm-d-scaling-manager/capacity-planner"
        echo "    newName: ${PLANNER_IMG%:*}"
        echo "    newTag: ${PLANNER_IMG##*:}"
        [ $# -lt 3 ] || cat "$3"
    } > "$dir/kustomization.yaml"
}

# pf <log> <port> <kubectl args...>: port-forward in the background. 0 once it
# forwards, 1 when kubectl exits first (its log says why), 2 after 30 s.
pf() {
    local log="$1" port="$2"
    shift 2
    kubectl "$@" -n "$NS" port-forward deploy/capacity-planner "$port:8080" >"$log" 2>&1 &
    local pid=$!
    PF_PIDS+=("$pid")
    local i
    for i in $(seq 1 60); do
        grep -q '^Forwarding from' "$log" && return 0
        kill -0 "$pid" 2>/dev/null || return 1
        sleep 0.5
    done
    return 2
}

# get <port> <out> [Host]: the HTTP status of GET /.
get() {
    local host=()
    [ $# -lt 3 ] || host=(-H "Host: $3")
    curl -sS -o "$2" -w '%{http_code}' --max-time 30 "${host[@]}" "http://localhost:$1/" || echo 000
}

# inputs <port> <out>: the HTTP status of POST /v1/inputs for the recording.
inputs() {
    curl -sS -o "$2" -w '%{http_code}' --max-time 300 -H 'Content-Type: application/json' \
        -d '{"source": {"kind": "recording", "name": "short.jsonl.gz"}}' "http://localhost:$1/v1/inputs" || echo 000
}

echo "Deploying the kubernetes overlay with $PLANNER_IMG into $NS"
kubectl create namespace "$NS" >/dev/null || die "cannot create namespace $NS"
overlay "$T/planner" kubernetes
kubectl apply -n "$NS" -k "$T/planner" || die "kubectl apply -k the kubernetes overlay failed"
kubectl -n "$NS" rollout status deploy/capacity-planner --timeout=180s || {
    kubectl -n "$NS" describe pods
    die "the planner did not become ready"
}

# 1. No Service, no Ingress: nothing exposes the planner.
exposed="$(kubectl -n "$NS" get services,ingresses -o name 2>&1)"
if [ -z "$exposed" ]; then ok "nothing exposes the planner: no Service, no Ingress in $NS"; else fail "exposed: $exposed"; fi

# 2. The planner bound the Pod's loopback, with no authentication.
plog="$(kubectl -n "$NS" logs deploy/capacity-planner -c planner 2>&1)"
if printf '%s\n' "$plog" | grep -q 'serving the capacity planner.s API on 127.0.0.1:8080 (auth none)'; then
    ok "the planner serves 127.0.0.1:8080 with auth none"
else
    fail "the planner's log: $plog"
fi

# 3. No token is mounted into the planner.
mounts="$(kubectl -n "$NS" get pods -l app.kubernetes.io/name=capacity-planner -o json \
    | jq -r '[.items[0].spec.containers[].volumeMounts[]?.mountPath] | join(",")')"
case ",$mounts," in
    *,/var/run/secrets/kubernetes.io/serviceaccount,*) fail "a ServiceAccount token is mounted: $mounts" ;;
    *) ok "no ServiceAccount token in the Pod (mounts: $mounts)" ;;
esac

echo "Copying the recording in with the loader Pod"
kubectl apply -n "$NS" -k "$ROOT/config/capacity-planner/loader" >/dev/null || die "cannot create the loader Pod"
kubectl -n "$NS" wait --for=condition=Ready pod/capacity-planner-loader --timeout=120s >/dev/null \
    || { kubectl -n "$NS" describe pod capacity-planner-loader; die "the loader Pod did not start"; }
# 0600, as the record guide writes a recording.
cp "$RECORDING" "$T/short.jsonl.gz"
chmod 0600 "$T/short.jsonl.gz"
kubectl -n "$NS" cp "$T/short.jsonl.gz" capacity-planner-loader:/recordings/short.jsonl.gz || die "kubectl cp into the loader failed"
kubectl -n "$NS" delete pod capacity-planner-loader --wait=true >/dev/null

echo "A ServiceAccount token, in a kubeconfig of its own"
kubectl create namespace "$USERS_NS" >/dev/null || die "cannot create namespace $USERS_NS"
kubectl -n "$USERS_NS" create serviceaccount viewer >/dev/null || die "cannot create the viewer"
( umask 077; kubectl -n "$USERS_NS" create token viewer --duration=1h > "$T/viewer.token" ) || die "cannot create a token"
server="$(kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.server}')"
kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > "$T/ca.crt"
VK="$T/viewer.kubeconfig"
KUBECONFIG="$VK" kubectl config set-cluster e2e --server="$server" --certificate-authority="$T/ca.crt" >/dev/null
# tokenFile, never the token inline: set-credentials has no flag for it.
KUBECONFIG="$VK" kubectl config set-credentials viewer >/dev/null
KUBECONFIG="$VK" kubectl config set users.viewer.tokenFile "$T/viewer.token" >/dev/null
KUBECONFIG="$VK" kubectl config set-context e2e --cluster=e2e --user=viewer >/dev/null
KUBECONFIG="$VK" kubectl config use-context e2e >/dev/null
grep -q 'token: ' "$VK" && die "the viewer kubeconfig holds a token inline"
VIEWER=(--kubeconfig "$VK")

# 4. The viewer has nothing but the Role.
if [ "$(kubectl "${VIEWER[@]}" auth can-i get secrets -n "$NS" 2>/dev/null)" = no ] \
    && [ "$(kubectl "${VIEWER[@]}" auth can-i create pods/exec -n "$NS" 2>/dev/null)" = no ]; then
    ok "the viewer may not read Secrets or exec in $NS"
else
    fail "the viewer has more than the Role in $NS"
fi

# 5-8. Through the shipped RoleBinding (system:authenticated).
if pf "$T/pf-viewer.log" 18081 "${VIEWER[@]}"; then
    ok "the viewer port-forwards with the shipped RoleBinding"
    code="$(get 18081 "$T/page.html")"
    if [ "$code" = 200 ] && grep -qi '<html' "$T/page.html"; then ok "GET / on localhost: 200, the page"; else fail "GET /: $code $(head -c 300 "$T/page.html")"; fi
    code="$(get 18081 "$T/host.out" planner.example.com)"
    if [ "$code" = 403 ]; then ok "GET / with Host planner.example.com: 403 (the Host check holds in the Pod)"; else fail "GET / with a foreign Host: $code"; fi
    code="$(inputs 18081 "$T/inputs.out")"
    id="$(jq -r '.inputsId // empty' "$T/inputs.out" 2>/dev/null || true)"
    if [ "$code" = 200 ] && [ -n "$id" ]; then ok "POST /v1/inputs on the copied recording: 200, inputsId $id"; else fail "POST /v1/inputs: $code $(head -c 300 "$T/inputs.out")"; fi
else
    fail "the viewer cannot port-forward: $(cat "$T/pf-viewer.log")"
    fail "GET / not tried"; fail "foreign Host not tried"; fail "POST /v1/inputs not tried"
fi

echo "Narrowing the RoleBinding to the group planners, as the guide does"
cat > "$T/narrow.yaml" <<'EOF'
patches:
  - target:
      kind: RoleBinding
      name: capacity-planner-viewer
    patch: |-
      - op: replace
        path: /subjects
        value:
          - apiGroup: rbac.authorization.k8s.io
            kind: Group
            name: planners
EOF
overlay "$T/narrow" kubernetes "$T/narrow.yaml"
kubectl apply -n "$NS" -k "$T/narrow" >/dev/null || die "cannot apply the narrowed overlay"

# 9. In the group: allowed.
if pf "$T/pf-carol.log" 18082 --as=carol --as-group=planners; then
    code="$(get 18082 "$T/carol.html")"
    if [ "$code" = 200 ]; then ok "carol (group planners) opens the page: 200"; else fail "carol: GET / $code"; fi
else
    fail "carol (group planners) cannot port-forward: $(cat "$T/pf-carol.log")"
fi

# 10. Not in the group: denied before any Pod is reached.
if pf "$T/pf-dave.log" 18083 --as=dave; then
    fail "dave (no group) port-forwards after the narrowing"
elif grep -qi 'forbidden' "$T/pf-dave.log"; then
    ok "dave (no group) is denied: $(grep -i -m1 forbidden "$T/pf-dave.log")"
else
    fail "dave failed, but not as forbidden: $(cat "$T/pf-dave.log")"
fi

# 11. The viewer lost it with the narrowing.
if pf "$T/pf-viewer2.log" 18084 "${VIEWER[@]}"; then
    fail "the viewer port-forwards after the narrowing"
elif grep -qi 'forbidden' "$T/pf-viewer2.log"; then
    ok "the viewer is denied after the narrowing"
else
    fail "the viewer failed, but not as forbidden: $(cat "$T/pf-viewer2.log")"
fi

# 12. Reading the namespace is not enough: view grants Pods and Deployments,
# not pods/portforward.
kubectl -n "$NS" create rolebinding viewer-view --clusterrole=view --serviceaccount="$USERS_NS:viewer" >/dev/null
if pf "$T/pf-viewer3.log" 18085 "${VIEWER[@]}"; then
    fail "the viewer port-forwards with view alone"
elif grep -qi 'pods/portforward' "$T/pf-viewer3.log"; then
    ok "with view alone, pods/portforward is denied: $(grep -i -m1 forbidden "$T/pf-viewer3.log")"
else
    fail "with view alone, a failure that does not name pods/portforward: $(cat "$T/pf-viewer3.log")"
fi

# 13-14. The Role is exactly enough: add the viewer to the narrowed binding by
# a binding of the Role alone.
kubectl -n "$NS" delete rolebinding viewer-view >/dev/null
kubectl -n "$NS" create rolebinding viewer-role --role=capacity-planner-viewer --serviceaccount="$USERS_NS:viewer" >/dev/null
if pf "$T/pf-viewer4.log" 18086 "${VIEWER[@]}"; then
    ok "the viewer, bound to the Role alone, port-forwards"
    code="$(inputs 18086 "$T/inputs2.out")"
    if [ "$code" = 200 ]; then ok "and POST /v1/inputs: 200"; else fail "POST /v1/inputs with the Role alone: $code"; fi
else
    fail "the viewer, bound to the Role alone, cannot port-forward: $(cat "$T/pf-viewer4.log")"
    fail "POST /v1/inputs not tried"
fi

echo
echo "capacity-planner in-cluster e2e: $PASSED passed, $FAILED failed (expected $EXPECT_CASES cases)"
[ "$FAILED" = 0 ] && [ "$((PASSED + FAILED))" = "$EXPECT_CASES" ]
