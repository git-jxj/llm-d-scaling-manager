# Capacity planner: run it in a cluster

Run one planner in a namespace and let people open it from their browser.
They need nothing but `kubectl` (or an OpenShift login). The planner serves
the recordings you copy into it, and it never changes the cluster.

## Who does what

| Step | Who | How often |
| --- | --- | --- |
| [Install it](#install-it) | an admin of one namespace; no cluster admin needed | once, and again on each upgrade |
| [Load recordings](#load-recordings) | anyone who may create Pods in that namespace: its admin, or a user with `edit` there | each time there is new data |
| [Open it](#open-it) | anyone you allow: by default, everyone who can log in to the cluster | any time |

- **Plain Kubernetes.** Nothing is exposed. Each person runs
  `kubectl port-forward`, and Kubernetes RBAC decides who may.
- **OpenShift.** A Route behind the cluster's login (and so its SSO).

## Install it

For the namespace admin.

### Prerequisites

- [ ] Admin of a namespace that holds nothing but the planner. You create it
  below, or your cluster admin creates it for you.
- [ ] `kubectl` 1.27 or later (or `oc`), and `git`: `kubectl apply -k` fetches
  the manifests from GitHub.
- [ ] A default StorageClass, for the planner's 1 GiB recordings volume. The
  loader writes it as uid 65532, so the StorageClass must apply `fsGroup`, or
  the volume root must be writable by that uid.

```bash
# The namespace, and the release: v0.12.0 or later.
export NS=capacity-planner V=<release, v0.12.0 or later>
export SRC=https://github.com/llm-d-incubation/llm-d-scaling-manager//config/capacity-planner
```

### Plain Kubernetes

```bash
# Skip the first command if your cluster admin made the namespace. The planner listens on 127.0.0.1 inside its Pod;
# a Role and a RoleBinding let every authenticated user port-forward to that Pod, capacity-planner-0, and no other.
kubectl create namespace ${NS}
kubectl apply -n ${NS} -k "${SRC}/kubernetes?ref=${V}"
kubectl -n ${NS} rollout status statefulset/capacity-planner
```

To let only one group open it, see [Narrow who can open it](#narrow-who-can-open-it).

### OpenShift

The Route sends people to the cluster's login page. After login, an
oauth-proxy sidecar forwards them to the planner on the Pod's 127.0.0.1. The
proxy image is Red Hat's, from `registry.redhat.io`, which the cluster's
global pull secret covers.

```bash
# Skip the first command if your cluster admin made the namespace. The secret signs the proxy's session cookie.
oc new-project ${NS}
oc -n ${NS} create secret generic capacity-planner-proxy --from-literal=session_secret="$(openssl rand -hex 16)"
# Any user who can log in to the cluster may open it. Use openshift-sar in place of openshift to narrow that (below).
oc apply -n ${NS} -k "${SRC}/openshift?ref=${V}"
oc -n ${NS} rollout status statefulset/capacity-planner
```

### Long recordings load slowly

The shipped limit, 2 GiB, is enough for day-long recordings. The planner reads
a recording from disk in passes when someone first picks it, and keeps only the
state it needs. Measured on a 24-hour recording of a 16-replica P/D fleet
(95 MB compressed): under 80 MiB while it loads and after, and about 80 seconds
for the first load. Picking another cycle of the same recording loads it again.
Wait for the first load; asking again starts a second one.

### Let someone else load recordings

Loading needs Pod create in the namespace. To let a user do it without making
them its admin:

```bash
kubectl -n ${NS} create rolebinding capacity-planner-loaders --clusterrole=edit --user=<user>
```

`edit` also lets them read the recordings volume. Give it only to people who
may see every recording in the namespace.

## Load recordings

For whoever loads data. A recording comes from
[Record a window](../record-for-capacity-planner/). Name each file after what
it holds, such as `qwen3-32b.jsonl.gz`: people pick a recording by its file
name. If the people who open the planner should not see your namespace, model
and Pod names, load an exported copy instead
([Real names in recordings](#real-names-in-recordings)).

```bash
# Use oc and the openshift/loader overlay on OpenShift. The loader is a short-lived Pod that mounts the recordings
# volume read-write; the planner sees new files without a restart. Copy as many files as you like in one go.
kubectl apply -n ${NS} -k "${SRC}/loader?ref=${V}"
kubectl -n ${NS} wait --for=condition=Ready pod/capacity-planner-loader
kubectl -n ${NS} cp qwen3-32b.jsonl.gz capacity-planner-loader:/recordings/qwen3-32b.jsonl.gz
kubectl -n ${NS} exec capacity-planner-loader -- ls -l /recordings
kubectl -n ${NS} delete pod capacity-planner-loader
```

- **Replace a recording:** copy a file of the same name over it.
- **Remove one:** `kubectl -n ${NS} exec capacity-planner-loader -- rm /recordings/<file>`
  while the loader runs.
- **Permission denied on `kubectl cp`:** the StorageClass ignores `fsGroup`
  (`kubectl get csidriver <driver> -o jsonpath='{.spec.fsGroupPolicy}'`). Ask
  your admin for one that applies it, or a volume whose root uid 65532 can
  write.
- The loader stops by itself after an hour. A stopped loader stays until it
  is deleted: delete it before you load again.

## Open it

Send this to the people who use it, with your namespace in it.

### Plain Kubernetes

You need `kubectl` and a kubeconfig for the cluster.

```bash
# Leave it running, then open http://localhost:8080. Use localhost or 127.0.0.1: the planner refuses other names.
# If port 8080 is busy on your machine, use 9090:8080 and open http://localhost:9090.
kubectl -n <namespace> port-forward pod/capacity-planner-0 8080:8080
```

In **Source**, pick a recording by its file name.

- **Forbidden:** you are not in the group allowed to open it; ask the
  namespace admin.
- **The page stops answering** after an upgrade: the Pod was replaced, which
  ends every port-forward. Run the command again.

### OpenShift

Open the address the admin gets from this command, and log in:

```bash
echo "https://$(oc -n ${NS} get route capacity-planner -o jsonpath='{.spec.host}')"
```

## Narrow who can open it

### Plain Kubernetes

By default the RoleBinding names the group `system:authenticated`: everyone
who can log in to the cluster, ServiceAccounts included. It lets them
port-forward to the planner's Pod and nothing else. To allow one group, apply
your own kustomization over the shipped one:

```bash
# Replace planners with your group. Keep the directory, and apply it again after each upgrade, with the new V.
cd "$(mktemp -d)"
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

### OpenShift

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
exported copy. It renames every name to a pseudonym such as `variant-03`,
and refuses a recording that holds anything it cannot place. The export is
not in the released planner: run it from a clone of the release, with Go 1.26
or later:

```bash
# The export reads an uncompressed recording: gunzip -k keeps the original.
gunzip -k /path/to/qwen3-32b.jsonl.gz
git clone https://github.com/llm-d-incubation/llm-d-scaling-manager.git && cd llm-d-scaling-manager && git checkout ${V}
go run ./cmd/learnergolden export -in /path/to/qwen3-32b.jsonl -out /path/to/qwen3-32b-export.jsonl.gz
```

Load the export, not the original.

## Upgrade

Apply the same overlay with the new release, then tell people to run their
port-forward again. The recordings stay.

```bash
export V=<new release>
kubectl apply -n ${NS} -k "${SRC}/kubernetes?ref=${V}"
kubectl -n ${NS} rollout status statefulset/capacity-planner
```

## Remove it

```bash
# Name the overlay you applied (kubernetes, openshift or openshift-sar). Deletes the recordings claim. If your StorageClass's
# reclaim policy is Retain, its volume stays (kubectl get pv | grep capacity-planner-recordings) for your cluster admin to delete.
kubectl delete -n ${NS} -k "${SRC}/kubernetes?ref=${V}"
# OpenShift only: the cookie secret.
kubectl -n ${NS} delete secret capacity-planner-proxy
```

## Details

- **Why port-forward is safe.** The planner has no authentication of its own
  (`--auth=none`) and binds `127.0.0.1:8080` in its Pod, so nothing outside the
  Pod reaches it, and there is no Service. `kubectl port-forward` arrives on
  the Pod's loopback after the API server checks RBAC: `get` on the Pod
  `capacity-planner-0`, and `create` (or `get`, for kubectl's WebSocket
  upgrade) on its `pods/portforward`. The Role names that one Pod, which is
  why the planner is a StatefulSet of one. The planner also refuses a request
  whose `Host` is not a loopback name ([Requests](api.md#requests)), so a web
  page cannot reach it through your port-forward by rebinding its own name.
- **A namespace of its own** still matters: anyone who may create Pods in the
  namespace can mount the recordings volume and read the recordings.
- **On OpenShift**, the proxy sends the planner `Host: 127.0.0.1:8080`
  (`--pass-host-header=false`); with the Route's name the planner would answer
  403. The Route re-encrypts to the proxy's service-CA certificate. The
  planner container gets no ServiceAccount token; the proxy does, as its OAuth
  client secret. Users with `edit` in the namespace can `oc port-forward`
  past the proxy; that grants them nothing new, since `edit` already lets them
  run Pods there and read the volume.
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
  Loading from the page is planned (#195).
- **Kubernetes authentication.** `serve --auth=kubernetes`
  ([Kubernetes authentication](api.md#kubernetes-authentication)) is for API
  clients that send a bearer token. A browser sends none, which is why these
  overlays use port-forward or oauth-proxy in front of `--auth=none`.
