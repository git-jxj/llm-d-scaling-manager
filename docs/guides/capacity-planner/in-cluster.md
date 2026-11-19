# Capacity planner: run it in a cluster

Run one planner in the cluster and let people open it from their browser,
with no install on their side but `kubectl` (or `oc`). It serves the
recordings you copy into it. It never changes the cluster.

- **Plain Kubernetes.** Nothing is exposed. Each person opens it with
  `kubectl port-forward`; Kubernetes RBAC decides who may.
- **OpenShift.** A Route, behind the cluster's login (and so its SSO).

Both need only admin of one namespace. Neither creates a cluster-scoped
object.

## Prerequisites

- [ ] Admin of a namespace that holds nothing but the planner. On plain
  Kubernetes, whoever may open the planner may port-forward to any Pod in that
  namespace.
- [ ] `kubectl` 1.27 or later (or `oc`), and `git`: `kubectl apply -k` fetches
  the manifests from GitHub.
- [ ] A default StorageClass, for the planner's 1 GiB recordings volume.
- [ ] One or more recordings, from
  [Record a window](../record-for-capacity-planner/). They may hold real
  names: see [Real names in recordings](#real-names-in-recordings).

```bash
# The namespace, and the release: v0.12.1 or later.
export NS=capacity-planner V=<release, v0.12.1 or later>
export SRC=https://github.com/llm-d-incubation/llm-d-scaling-manager//config/capacity-planner
```

## Plain Kubernetes

### 1. Deploy it

```bash
# The planner listens on 127.0.0.1 inside its Pod. A Role and a RoleBinding let every authenticated user port-forward to it.
kubectl apply -n ${NS} -k "${SRC}/kubernetes?ref=${V}"
kubectl -n ${NS} rollout status deploy/capacity-planner
```

### 2. Copy recordings in

```bash
# A short-lived Pod that mounts the recordings volume read-write. The planner sees new files without a restart.
kubectl apply -n ${NS} -k "${SRC}/loader?ref=${V}"
kubectl -n ${NS} wait --for=condition=Ready pod/capacity-planner-loader
kubectl -n ${NS} cp run.jsonl.gz capacity-planner-loader:/recordings/run.jsonl.gz
kubectl -n ${NS} delete pod capacity-planner-loader
```

### 3. Open it

Send people this, with your namespace in it:

```bash
# Leave it running, then open http://localhost:8080. Use localhost or 127.0.0.1: the planner refuses other names.
kubectl -n <namespace> port-forward deploy/capacity-planner 8080:8080
```

In **Source**, pick a recording by its file name.

### Narrow who can open it

By default the RoleBinding names the group `system:authenticated`: everyone
who can log in to the cluster, ServiceAccounts included. To allow one group,
apply your own kustomization over the shipped one:

```bash
# Replace planners with your group. Run it again after each upgrade, with the new V.
mkdir -p capacity-planner && cd capacity-planner
cat > kustomization.yaml <<EOF
resources:
  - ${SRC}/kubernetes?ref=${V}
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
kubectl apply -n ${NS} -k .
```

Reading the namespace (`view`) is not enough to open the planner; the Role
`capacity-planner-viewer` is.

## OpenShift

The Route sends people to the cluster's login page. After login, an
oauth-proxy sidecar forwards them to the planner on the Pod's 127.0.0.1.
The proxy image is Red Hat's, from `registry.redhat.io`, which the cluster's
global pull secret covers.

### 1. Create the proxy's cookie secret

```bash
# Signs the proxy's session cookie. Create it once.
oc -n ${NS} create secret generic capacity-planner-proxy --from-literal=session_secret="$(openssl rand -hex 16)"
```

### 2. Deploy it

```bash
# Any user who can log in to the cluster may open it. Use openshift-sar in place of openshift to narrow that (below).
oc apply -n ${NS} -k "${SRC}/openshift?ref=${V}"
oc -n ${NS} rollout status deploy/capacity-planner
```

### 3. Copy recordings in

```bash
# As on plain Kubernetes, with the loader made for OpenShift's restricted SCC.
oc apply -n ${NS} -k "${SRC}/openshift/loader?ref=${V}"
oc -n ${NS} wait --for=condition=Ready pod/capacity-planner-loader
oc -n ${NS} cp run.jsonl.gz capacity-planner-loader:/recordings/run.jsonl.gz
oc -n ${NS} delete pod capacity-planner-loader
```

### 4. Open it

```bash
# The address to send people.
echo "https://$(oc -n ${NS} get route capacity-planner -o jsonpath='{.spec.host}')"
```

### Narrow who can open it

Deploy the `openshift-sar` overlay in place of `openshift`. Then only users
who may `get` the Service `capacity-planner` in the namespace get in: anyone
with `view`, `edit` or `admin` there, and anyone you bind to the shipped
Role:

```bash
oc apply -n ${NS} -k "${SRC}/openshift-sar?ref=${V}"
oc -n ${NS} create rolebinding capacity-planner-viewer --role=capacity-planner-viewer --group=<group>
```

The proxy asks with the user's own token, so neither overlay needs a cluster
admin.

## Real names in recordings

A recording holds the namespaces, models, variants and Pods it saw. Where
the people you share the planner with should not see them, serve an
exported copy, made from a clone of the release you run. It renames every
name to a pseudonym such as `variant-03`, and refuses a recording that holds
anything it cannot place:

```bash
go run ./cmd/learnergolden export -in run.jsonl.gz -out run-export.jsonl.gz
```

Copy `run-export.jsonl.gz` in, and not the original.

## Remove it

```bash
# Name the overlay you applied (kubernetes, openshift or openshift-sar). Deletes the recordings volume too.
kubectl delete -n ${NS} -k "${SRC}/kubernetes?ref=${V}"
# OpenShift only: the cookie secret.
kubectl -n ${NS} delete secret capacity-planner-proxy
```

## Details

- **Why port-forward is safe.** The planner has no authentication of its own
  (`--auth=none`) and binds `127.0.0.1:8080` in its Pod, so nothing outside the
  Pod reaches it, and there is no Service. `kubectl port-forward` arrives on
  the Pod's loopback after the API server checks RBAC: `get` on the
  Deployment, `get` and `list` on Pods, and `create` on `pods/portforward`.
  The planner also refuses a request whose `Host` is not a loopback name
  ([Requests](api.md#requests)), so a web page cannot reach it through your
  port-forward by rebinding its own name.
- **On OpenShift**, the proxy sends the planner `Host: 127.0.0.1:8080`
  (`--pass-host-header=false`); with the Route's name the planner would answer
  403. The Route re-encrypts to the proxy's service-CA certificate. The
  planner container gets no ServiceAccount token; the proxy does, as its OAuth
  client secret.
- **The proxy image.** The overlay names
  `registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.19`. To match your
  cluster's minor version, add an `images:` entry with another `newTag` in a
  kustomization of your own, as in [Narrow who can open
  it](#narrow-who-can-open-it).
- **The planner image** is the release's own
  (`ghcr.io/llm-d-incubation/llm-d-scaling-manager/capacity-planner:${V}`): the
  `?ref=` you apply picks it.
- **Hardening.** Non-root, read-only root filesystem, no capabilities, no
  privilege escalation, memory limited, and the recordings mounted
  read-only.
- **Recordings only.** The in-cluster planner does not read Prometheus. To
  plan from a live window, run it [on your machine](README.md#start-the-planner).
- **Kubernetes authentication.** `serve --auth=kubernetes`
  ([Kubernetes authentication](api.md#kubernetes-authentication)) is for API
  clients that send a bearer token. A browser sends none, which is why these
  overlays use port-forward or oauth-proxy in front of `--auth=none`.
