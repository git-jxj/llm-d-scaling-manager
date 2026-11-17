#!/usr/bin/env bash
# Keep a model's weights on every accelerator node's local disk, so a replica
# reads them at the node's own speed instead of a shared volume's.
#
# The shared RWX volume that llm-d deployments mount the model from is a
# download cache, not a start-time measure: every replica reads the whole
# model through the network filesystem, and two replicas starting together
# share that pipe. Measured on one cluster, two nodes reading one 700 GB
# model from the shared volume took 1403 s against ~104 s from the node's
# NVMe -- past the engine's own 600 s startup timeout. This puts the weights
# on the node: one static hostPath PersistentVolume, one claim bound to it,
# and one DaemonSet that downloads the model onto every selected node and
# then reports Ready. The model servers mount the claim exactly as they
# mount any other; each node reads its own copy.
#
#   weights.sh apply  -n NS --model HFID --path DIR --image IMG [--dry-run]   download HFID under DIR on every accelerator node
#   weights.sh status -n NS [--model HFID] [--node-selector KEY=VALUE[,...]]  which nodes have it, which are still downloading
#   weights.sh delete -n NS (--model HFID | --all) [--dry-run]                drop the claim, volume and downloader (files stay)
#   weights.sh label (-n NS[,NS...] | -A) [--watch SECONDS] [--dry-run] [--clear]
#                                                                             label each node that holds a verified copy, unlabel the rest
#   weights.sh labeller (-n NS[,NS...] | -A) --image TOOLS_IMAGE [--labeller-namespace LNS] [--watch SECONDS] [--dry-run]
#                                                                             run `label --watch` in the cluster, in a namespace of its own
#   weights.sh labeller --add -n NS [--labeller-namespace LNS] [--dry-run]
#                                                                             add NS to a running labeller's watch list, with
#                                                                             rights in NS and LNS only; else print the command
#   weights.sh labeller --remove -n NS [--labeller-namespace LNS] [--dry-run]
#                                                                             take NS off the watch list (one namespace's
#                                                                             cleanup); the labeller and the other
#                                                                             namespaces' labels stay
#   weights.sh labeller --delete [-n NS[,NS...] | -A] [--labeller-namespace LNS] [--dry-run]
#                                                                             remove the labeller for the whole cluster
#
# Exits of labeller --add and --remove: 0 NS is watched (--add) or not
# (--remove), now or already; 3 the caller lacks the rights in NS or LNS, or
# the change cannot be made this way (what to run is printed); 4 there is
# no labeller (how to add one is printed). With --merge, labeller apply keeps
# every namespace the running labeller already watches (smt install --scope
# cluster uses it, so a namespace admin-setup added is not dropped).
#
#   -n, --namespace NS          the namespace the claim and downloader live in
#                               (label, labeller: the namespaces whose
#                               downloaders are trusted to label nodes)
#   -A, --all-namespaces        label, labeller: every namespace
#
# Options:
#   --model HFID                Hugging Face id, e.g. Qwen/Qwen3-32B
#   --path DIR                  directory on the node, e.g. /mnt/local/weights/<ns>
#                               (/var/mnt/weights/<ns> on RHCOS, under /var;
#                               /var/mnt itself is the root disk); the model
#                               lands under DIR/models/HFID
#   --image IMG                 image to download with: any image carrying
#                               huggingface_hub -- the engine image itself
#                               is the natural choice, and it is then held
#                               on the node as a side effect
#   --hf-token-secret NAME[/KEY]  Secret with a Hugging Face token for gated
#                               models (key defaults to HF_TOKEN)
#   --capacity SIZE             the volume's declared capacity (default 1Ti;
#                               it is a declaration, hostPath has no quota).
#                               A re-apply keeps an existing claim's size
#                               when this is not given, and refuses a SIZE
#                               that differs from it: a claim on a static
#                               volume cannot be resized (delete first).
#                               --dry-run reads no cluster, so it renders
#                               SIZE or the default
#   --memory SIZE               the downloader's memory limit (default 16Gi;
#                               the request stays 256Mi, so a node reserves
#                               nothing for it). 2Gi was OOMKilled on every
#                               node fetching an 8B model's safetensors
#   --node-selector KEY=VALUE[,KEY=VALUE...]
#                               which nodes count as accelerator nodes. The
#                               default is any node carrying a known GPU
#                               product label (deploy/lib/accelerator_nodes.sh);
#                               set this to what your model servers select
#                               on when they select on something. `status`
#                               lists the nodes each DaemonSet was applied
#                               for; this overrides that.
#                               One or more KEY=VALUE pairs, comma
#                               separated, no spaces: a node must carry
#                               every one (k1=v1,k2=v2 is k1=v1 AND
#                               k2=v2, as in a nodeSelector)
#   --toleration KEY            tolerate a taint with KEY (any value, any
#                               effect); nvidia.com/gpu is always tolerated
#   --node-label KEY            apply: the node label that says a node holds
#                               this model, as PREFIX/NAME-cache (default
#                               llm-d.ai/<model name, lower-cased>-cache,
#                               the label some clusters' node-preparation
#                               jobs already set, so both agree on a node)
#   --dry-run                   apply: print the manifests instead of
#                               applying them; delete: print what would go;
#                               label: print the node changes, make none;
#                               labeller: print the manifests
#   --watch SECONDS             label: reconcile every SECONDS, forever
#   --clear                     label: remove every label this command set
#                               (after the labeller is deleted)
#   --label-prefix PREFIX       label, labeller: the only label prefix the
#                               labeller writes or removes (default
#                               llm-d.ai); a DaemonSet asking for another
#                               is skipped
#   --labeller-namespace LNS    labeller: where it runs (default
#                               scaling-manager-weights-labeller); never a
#                               model namespace, whose pod creators could
#                               borrow its node rights
#   --delete                    labeller: remove it for the whole cluster
#                               (the labels stay; run label --clear for
#                               them). With no -n or -A, its RoleBindings
#                               are found by their labels in every namespace
#   --add                       labeller: add -n NS to the running labeller
#   --remove                    labeller: take -n NS off the running
#                               labeller; its next pass removes the labels
#                               only NS's copies held
#   --merge                     labeller: also watch what it watches now.
#                               Without it, an apply that would watch
#                               fewer namespaces than the running labeller
#                               is refused
#
# The node gate. A replica scheduled onto a node whose copy is missing or
# still being written reads a partial model, so the downloader writes its
# marker only after the last file, with the size of every file in it, and
# its readiness probe re-checks those files (not just the marker) every
# 15 s. `label` reads that readiness -- the files, through the kubelet --
# and sets <key>=ready on every node where each downloader for the key is
# Ready, and removes it where one is not: a node that loses its copy stops
# attracting replicas, and a node added for scale-out gets the label only
# when its own download completes. It removes only labels it set itself
# (it records them in the node annotation scaling-manager.llmd.ai/
# weights-node-labels), so a label another tool set is left alone. It
# patches nodes, which is cluster-scoped: a cluster-admin piece, like apply.
# A namespace it cannot read holds the labels it asked for when last read
# (Forbidden is not absent); the others are labelled as usual. The labeller
# records its health in ConfigMap weights-labeller-status, in its own
# namespace. The model servers then prefer (or require) the label, and wait in an init
# container for the marker; `make workload-patch` writes both. Design and
# failure modes: docs/proposals/weights-node-gate.md.
#
# The claim is named weights-<model>-<hash> and printed by apply; a model
# server mounts the weights at pvc://<claim>/models/HFID. The volume is
# cluster-scoped: apply needs leave to create PersistentVolumes, which a
# namespace tenant does not have -- ask the cluster admin to run apply, or
# to create the volume. Nothing here uses a hostPath volume in a Pod (the
# downloader mounts the claim), so Pod Security "baseline" admits it and
# "restricted" does not (no runAsNonRoot: on Kubernetes the image runs as
# root, which is what writing a root-owned node directory takes). The node
# directory is cluster-shared, root-writable state: every namespace pointed
# at the same DIR shares it and trusts its marker, and nothing charges what
# is written there to a quota (the claim's --capacity IS charged to a
# storage quota) -- one DIR per trust domain, on a disk that is not the
# node's own. The downloader runs as its own ServiceAccount,
# weights-downloader, so a grant for it does not land on the namespace's
# default ServiceAccount -- but an SCC bound to it reaches every pod in
# the namespace that names it, which anyone with pods/create can do: it is
# a namespace-wide root grant, not a private one.
#
# On OpenShift -- NOT YET RUN THERE -- restricted-v2 admits the downloader
# and runs it as the project's range UID with GID 0, which cannot write a
# root-owned directory, and the kubelet does not relabel a hostPath, so
# container_t cannot write it either way. Before apply, on each node (oc
# debug node/<n> -- chroot /host): mkdir -p DIR && chgrp 0 DIR && chmod
# 2775 DIR && chcon -t container_file_t DIR; or mount the disk at DIR with
# a MachineConfig mount unit carrying context=system_u:object_r:
# container_file_t:s0. No SCC grant is then needed. Files land at the
# project's SELinux level, so one DIR per project (a context= mount labels
# everything s0 and loses that separation). `status` says when a
# downloader died on a permission error. The other way is an SCC that runs
# it as root -- a copy of anyuid that also allows the runtime/default
# seccomp profile, which stock anyuid does not -- granted to
# weights-downloader; it removes the chgrp/chmod, not the chcon (the
# kubelet still does not relabel the hostPath), and it is root for every
# pod creator in the namespace.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BLUE='\033[0;34m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'
# shellcheck source=lib/common.sh
source "$HERE/lib/common.sh"
# shellcheck source=lib/accelerator_nodes.sh
source "$HERE/lib/accelerator_nodes.sh"
# shellcheck source=lib/nodedir.sh
source "$HERE/lib/nodedir.sh"
# NOTE: log_error EXITS. Nothing may follow it that needs to run.

NAMESPACE=""
MODEL=""
NODE_PATH=""
IMAGE=""
HF_SECRET=""
CAPACITY="1Ti"
CAPACITY_GIVEN=false
# The downloader's memory LIMIT. huggingface_hub fetches several files at once
# (max_workers=8) and buffers each, and at 2Gi the download of an 8B model
# (four or more multi-GB safetensors) was OOMKilled on every node and
# restarted from its last file, never finishing. A limit, not a request: the
# scheduler reserves only the 256Mi request.
MEMORY="16Gi"
NODE_SELECTOR=""          # empty: every node carrying a known GPU product label
NODE_SELECTOR_GIVEN=false
TOLERATIONS=("nvidia.com/gpu")
DRY_RUN=false
ALL=false
# Both labels: component alone is a generic value another tool could carry;
# managed-by is what says this script made it.
LABEL_COMPONENT="app.kubernetes.io/component=node-local-weights,app.kubernetes.io/managed-by=scaling-manager-weights"
STORAGE_CLASS="node-local-weights"
MARKER=".download-complete"
SA_NAME="weights-downloader"
NODE_LABEL=""
ALL_NAMESPACES=false
WATCH=""
CLEAR=false
DELETE=false
ADD=false
REMOVE=false
MERGE=false
LABEL_PREFIX="llm-d.ai"
LABELLER_NS="scaling-manager-weights-labeller"
LABELLER_NAME="weights-labeller"
# The node annotation that lists, comma-separated, the labels `label` set on
# that node: the only ones it ever removes.
OWNED_ANNOTATION="scaling-manager.llmd.ai/weights-node-labels"
LABEL_ANNOTATION="scaling-manager.llmd.ai/weights-node-label"

# VERIFY_PY defines verify(d, marker): "" when directory d holds a complete
# copy, else why not. Complete is the marker present and listing at least
# one file, a config.json (params.json for a Mistral-format checkpoint), and
# every listed file there at its listed size -- the files, not the marker:
# a disk that failed or was remounted empty leaves no marker, and a file
# deleted or truncated under a marker fails its size. The downloader and the
# readiness probe run the same text. Python, not sh: no command
# substitution, which Kubernetes would rewrite, and no `stat` whose flags
# differ between images.
VERIFY_PY='import os, sys
def verify(d, marker):
    try:
        with open(os.path.join(d, marker)) as fh:
            lines = [l for l in fh.read().splitlines() if l.strip()]
    except OSError:
        return "no completion marker"
    if not lines:
        return "the completion marker lists no files"
    if not any(os.path.isfile(os.path.join(d, c)) for c in ("config.json", "params.json")):
        return "no config.json"
    for line in lines:
        size, _, rel = line.partition("\t")
        try:
            if os.path.getsize(os.path.join(d, rel)) != int(size):
                return "wrong size: " + rel
        except (OSError, ValueError):
            return "missing: " + rel
    return ""'

usage() {
    sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; $d'
    exit 0
}

# name_for turns a model id into the claim's (and DaemonSet's) name: the id
# lower-cased with everything that is not [a-z0-9-] replaced, truncated to
# leave room for a hash of the FULL id -- two ids that differ only in a
# character the scrub folds must not collide.
name_for() {
    local model="$1"
    local base hash
    base="$(printf '%s' "$model" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9-]/-/g; s/^-*//; s/-*$//')"
    hash="$(llmsm_ns_suffix "$model")"
    printf 'weights-%s-%s' "${base:0:40}" "$hash" | sed 's/-\{2,\}/-/g'
}

# pv_for is the volume's name. PersistentVolumes are cluster-scoped, so the
# namespace is part of the hash: two namespaces holding the same model under
# the same directory each get their own volume, bound to their own claim.
pv_for() {
    printf '%s-%s' "$(name_for "$MODEL")" "$(llmsm_ns_suffix "${NAMESPACE}/${NODE_PATH}")"
}

# check_model refuses an id that is not one: [org/]name of the characters
# Hugging Face allows, no '..' or '--', no segment starting or ending with
# '.' or '-' -- the Hub's own rule, held here too because the id becomes a
# directory (/weights/models/<id>) and the probe's path before the library
# ever sees it.
check_model() {
    case "$1" in
        "") log_error "--model must not be empty" ;;
        */*/*|/*|*/) log_error "not a Hugging Face model id: '$1' (expected org/name)" ;;
        *[!A-Za-z0-9._/-]*) log_error "not a Hugging Face model id: '$1' (letters, digits, . _ - and one /)" ;;
        *..*|*--*) log_error "not a Hugging Face model id: '$1' (no '..' or '--')" ;;
        .*|-*|*/.*|*/-*|*.|*-|*./*|*-/*) log_error "not a Hugging Face model id: '$1' (a segment must not start or end with '.' or '-')" ;;
    esac
}

# check_path refuses a node directory the volume must not point at
# (deploy/lib/nodedir.sh says which and why).
check_path() {
    local why
    why="$(nodedir_ok "$1")" || log_error "--path: ${why}"
}

# node_label_why says why $1 is not a node label weights.sh writes, or
# nothing: a label key (PREFIX/NAME) whose NAME ends in -cache. The suffix
# is the convention, and it keeps the labeller off every other label a node
# carries -- a role, a zone, a GPU product -- whatever a DaemonSet asks.
node_label_why() {
    local why
    case "$1" in
        */*-cache) ;;
        *) echo "not a node label of the form PREFIX/NAME-cache: '$1'"; return 0 ;;
    esac
    why="$(accelerator_label_key_ok "$1")" || { echo "$why"; return 0; }
}

# check_node_label refuses $1 (given as option $2) unless node_label_why
# accepts it.
check_node_label() {
    local why
    why="$(node_label_why "$1")"
    [ -z "$why" ] || log_error "$2: ${why}"
}

check_image() {
    case "$1" in
        "") log_error "--image must not be empty" ;;
        *[!A-Za-z0-9._:/@-]*) log_error "not an image reference: '$1' (registry/path[:tag|@sha256:digest], no spaces or quotes)" ;;
    esac
}

# render prints the volume, the claim and the downloader for MODEL.
render() {
    local name pv
    name="$(name_for "$MODEL")"
    pv="$(pv_for)"
    local secret_name="${HF_SECRET%%/*}"
    local secret_key="${HF_SECRET#*/}"
    [ "$secret_key" != "$HF_SECRET" ] || secret_key="HF_TOKEN"
    cat <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${SA_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: node-local-weights
    app.kubernetes.io/component: node-local-weights
    app.kubernetes.io/managed-by: scaling-manager-weights
# The downloader's own identity: what an admin grants for it (an SCC on
# OpenShift) does not land on the namespace's default ServiceAccount --
# though any pod in the namespace may name this one, so a grant is still
# namespace-wide. It makes no API call, so no token.
automountServiceAccountToken: false
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: ${pv}
  labels:
    app.kubernetes.io/name: node-local-weights
    app.kubernetes.io/component: node-local-weights
    app.kubernetes.io/managed-by: scaling-manager-weights
  annotations:
    scaling-manager.llmd.ai/weights-model: "${MODEL}"
    scaling-manager.llmd.ai/weights-path: "${NODE_PATH}"
spec:
  capacity:
    storage: ${CAPACITY}
  accessModes:
    - ReadWriteMany
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ${STORAGE_CLASS}
  # Bound to this claim and no other: a static volume without a claimRef is
  # taken by the first claim that asks for its class.
  claimRef:
    namespace: ${NAMESPACE}
    name: ${name}
  hostPath:
    path: ${NODE_PATH}
    type: DirectoryOrCreate
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: node-local-weights
    app.kubernetes.io/component: node-local-weights
    app.kubernetes.io/managed-by: scaling-manager-weights
  annotations:
    scaling-manager.llmd.ai/weights-model: "${MODEL}"
    scaling-manager.llmd.ai/weights-path: "${NODE_PATH}"
    scaling-manager.llmd.ai/weights-node-label: "${NODE_LABEL}"
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${STORAGE_CLASS}
  volumeName: ${pv}
  resources:
    requests:
      storage: ${CAPACITY}
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: node-local-weights
    app.kubernetes.io/component: node-local-weights
    app.kubernetes.io/managed-by: scaling-manager-weights
  annotations:
    scaling-manager.llmd.ai/weights-model: "${MODEL}"
    scaling-manager.llmd.ai/weights-path: "${NODE_PATH}"
    scaling-manager.llmd.ai/weights-node-label: "${NODE_LABEL}"
    scaling-manager.llmd.ai/weights-node-selector: "${NODE_SELECTOR}"
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: node-local-weights
      scaling-manager.llmd.ai/weights: ${name}
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 100%
  template:
    metadata:
      labels:
        app.kubernetes.io/name: node-local-weights
        app.kubernetes.io/component: node-local-weights
        app.kubernetes.io/managed-by: scaling-manager-weights
        scaling-manager.llmd.ai/weights: ${name}
      annotations:
        scaling-manager.llmd.ai/weights-model: "${MODEL}"
    spec:
      serviceAccountName: ${SA_NAME}
      automountServiceAccountToken: false
      securityContext:
        seccompProfile:
          type: RuntimeDefault
$(accelerator_placement_yaml "$NODE_SELECTOR")
      tolerations:
$(for t in "${TOLERATIONS[@]}"; do printf '        - key: "%s"\n          operator: Exists\n' "$t"; done)
      terminationGracePeriodSeconds: 1
      volumes:
        - name: weights
          persistentVolumeClaim:
            claimName: ${name}
      containers:
        - name: download
          image: "${IMAGE}"
          imagePullPolicy: IfNotPresent
          # The marker is written after the last file, atomically, and lists
          # every file with its size; a restart resumes what huggingface_hub
          # left (it skips complete files). A copy that fails verification
          # -- a file gone or short under the marker -- loses its marker
          # and is downloaded again. Then the pod stays, re-checking every
          # 30 s; when the copy breaks it removes the marker and exits (so
          # the kubelet restarts it and it repairs); Ready is the probe below, so
          # the DaemonSet's numberReady is the count of nodes that hold
          # the model. No command substitution and no double dollar in
          # the command: Kubernetes rewrites both.
          command: ["python3", "-c"]
          args:
            - |
$(printf '%s\n' "$VERIFY_PY" | sed 's/^/              /')
              import json, signal, time
              signal.signal(signal.SIGTERM, lambda *a: sys.exit(0))
              d = os.environ["TARGET_DIR"]
              mk = os.environ["MARKER"]
              m = os.path.join(d, mk)
              def manifest():
                  out = []
                  for root, dirs, files in os.walk(d):
                      if root == d:
                          dirs[:] = [x for x in dirs if x != ".cache"]
                      for f in files:
                          rel = os.path.relpath(os.path.join(root, f), d)
                          if rel not in (mk, mk + ".tmp"):
                              out.append("%d\t%s" % (os.path.getsize(os.path.join(d, rel)), rel))
                  return sorted(out)
              def write_marker():
                  t = m + ".tmp"
                  with open(t, "w") as fh:
                      fh.write("\n".join(manifest()) + "\n")
                      fh.flush()
                      os.fsync(fh.fileno())
                  os.replace(t, m)
              def legacy_complete():
                  # An empty marker is an earlier downloader's, written after a
                  # complete download; it is upgraded to a list only when the
                  # files still look whole: a config and every shard the index
                  # names.
                  if not any(os.path.isfile(os.path.join(d, c)) for c in ("config.json", "params.json")):
                      return False
                  for f in os.listdir(d):
                      if f.endswith(".index.json"):
                          with open(os.path.join(d, f)) as fh:
                              shards = set(json.load(fh).get("weight_map", {}).values())
                          if not shards or not all(os.path.isfile(os.path.join(d, s)) for s in shards):
                              return False
                  return True
              if os.path.isfile(m) and os.path.getsize(m) == 0:
                  if legacy_complete():
                      write_marker()
                  else:
                      os.remove(m)
              why = verify(d, mk)
              if why == "":
                  print("weights: present at", d, flush=True)
              else:
                  if os.path.exists(m):
                      print("weights: the copy at", d, "is broken (" + why + "); downloading again", flush=True)
                      os.remove(m)
                  print("weights: downloading", os.environ["MODEL_ID"], "to", d, flush=True)
                  from huggingface_hub import snapshot_download
                  snapshot_download(repo_id=os.environ["MODEL_ID"], local_dir=d,
                                    token=os.environ.get("HF_TOKEN") or None,
                                    ignore_patterns=["original/*", "*.msgpack", "*.h5"],
                                    max_workers=8)
                  write_marker()
                  why = verify(d, mk)
                  if why:
                      sys.exit("weights: downloaded, but the copy does not verify: " + why)
                  print("weights: complete", flush=True)
              # The marker goes first, so the wait-for-weights init container
              # of a replica placed here meanwhile stops passing at once.
              every = int(os.environ.get("RECHECK_SECONDS") or "30")
              while True:
                  time.sleep(every)
                  why = verify(d, mk)
                  if why:
                      try:
                          os.remove(m)
                      except OSError:
                          pass
                      sys.exit("weights: the copy at " + d + " broke (" + why + "); its marker is removed, and the restart downloads it again")
          env:
            - name: MODEL_ID
              value: "${MODEL}"
            - name: TARGET_DIR
              value: "/weights/models/${MODEL}"
            - name: MARKER
              value: "${MARKER}"
            - name: HOME
              value: /tmp
            # Engine images bake in NVIDIA_VISIBLE_DEVICES=all, and the NVIDIA
            # runtime honours it from a container that requested no GPU.
            - name: NVIDIA_VISIBLE_DEVICES
              value: "void"
$(if [ -n "$HF_SECRET" ]; then printf '            - name: HF_TOKEN\n              valueFrom:\n                secretKeyRef:\n                  name: "%s"\n                  key: "%s"\n' "$secret_name" "$secret_key"; fi)
          volumeMounts:
            - name: weights
              mountPath: /weights
          # Ready is "the copy verifies": the marker, a config, and every
          # file it lists at its size, re-checked every 15 s. weights.sh
          # label reads this to set and remove the node label, so a failure
          # must persist (three probes) before it unlabels a node, and a
          # slow disk gets 10 s, not the default 1 s, before a probe fails.
          readinessProbe:
            exec:
              command:
                - python3
                - -c
                - |
$(printf '%s\n' "$VERIFY_PY" | sed 's/^/                  /')
                  why = verify(sys.argv[1], sys.argv[2])
                  if why:
                      sys.exit("weights: " + why)
                - "/weights/models/${MODEL}"
                - "${MARKER}"
            periodSeconds: 15
            timeoutSeconds: 10
            failureThreshold: 3
          resources:
            requests:
              cpu: 100m
              memory: 256Mi
            limits:
              memory: ${MEMORY}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
EOF
}

cmd_apply() {
    [ -n "$NAMESPACE" ] || log_error "apply needs -n NAMESPACE"
    [ -n "$MODEL" ] || log_error "apply needs --model HFID"
    [ -n "$NODE_PATH" ] || log_error "apply needs --path DIR (a directory on the node)"
    [ -n "$IMAGE" ] || log_error "apply needs --image IMG (an image with huggingface_hub; the engine image)"
    check_model "$MODEL"
    check_path "$NODE_PATH"
    check_image "$IMAGE"
    if [ -n "$HF_SECRET" ]; then
        case "$HF_SECRET" in *[!A-Za-z0-9._/-]*|*/*/*) log_error "not a Secret reference: '$HF_SECRET' (NAME or NAME/KEY)" ;; esac
    fi
    [ -n "$NODE_LABEL" ] || NODE_LABEL="$(llmsm_weights_node_label_key "$MODEL")"
    check_node_label "$NODE_LABEL" "--node-label"
    if [ "$DRY_RUN" = true ]; then
        render
        return
    fi
    # Four documents in one apply: without leave for the cluster-scoped
    # volume, kubectl would still create the ServiceAccount, the claim and
    # the DaemonSet -- a Pending claim and pods that never schedule -- and
    # report only the volume's Forbidden. Ask first, land nothing otherwise.
    if ! kubectl auth can-i create persistentvolumes -A -q 2>/dev/null; then
        log_error "apply needs leave to create PersistentVolumes (cluster-scoped), which a namespace tenant does not have -- ask the cluster admin to run apply (storage-admin on OpenShift)"
    fi
    # A claim of this name on another storage class (or on none) is somebody
    # else's: the volume here would never bind to it, and its size is not
    # this script's to keep or change. Refused, and not offered for deletion
    # -- weights.sh delete would take it. A claim that cannot be read is an
    # error, not an absent claim.
    local claim class err
    claim="$(name_for "$MODEL")"
    err="$(mktemp)"
    if ! class="$(kubectl get pvc -n "$NAMESPACE" "$claim" --ignore-not-found -o 'jsonpath={.metadata.name}/{.spec.storageClassName}' 2>"$err")"; then
        local why
        why="$(tr '\n' ' ' < "$err" | sed 's/ *$//')"
        rm -f "$err"
        log_error "cannot read the claim ${claim} in ${NAMESPACE} (kubectl: ${why:-no error text}); apply must know whether a claim of that name is already there -- nothing was applied"
    fi
    rm -f "$err"
    if [ -n "$class" ] && [ "${class#*/}" != "$STORAGE_CLASS" ]; then
        log_error "namespace ${NAMESPACE} already has a claim named ${claim} on storage class '${class#*/}', which weights.sh did not make (its claims are on ${STORAGE_CLASS}); it is not this script's to resize or replace. Use a namespace without a claim of that name. Nothing was applied"
    fi
    # an existing claim keeps its size (deploy/lib/nodedir.sh says why)
    nodedir_claim_capacity "$NAMESPACE" "$claim" "$CAPACITY_GIVEN" "weights.sh delete -n ${NAMESPACE} --model ${MODEL}"
    render | kubectl apply -f - >/dev/null
    log_info "downloading ${MODEL} under ${NODE_PATH} on $(accelerator_selector_text "$NODE_SELECTOR"); claim $(name_for "$MODEL") -- mount pvc://$(name_for "$MODEL")/models/${MODEL}"
    log_info "node label ${NODE_LABEL}=ready on each node whose copy verifies, once weights.sh label (or the labeller) runs"
    # On OpenShift the downloader runs as the project's range UID and the
    # directory is not relabelled for containers: both are the admin's to
    # prepare on each node before this apply, and neither shows until the
    # first pod dies on it. Said once, here, when the cluster is one.
    if kubectl api-resources --api-group=security.openshift.io 2>/dev/null | grep -q securitycontextconstraints; then
        log_warning "OpenShift: the downloader runs as the project UID (GID 0) under restricted-v2 and the kubelet does not relabel a hostPath; on each node, before this takes: mkdir -p ${NODE_PATH} && chgrp 0 ${NODE_PATH} && chmod 2775 ${NODE_PATH} && chcon -t container_file_t ${NODE_PATH} (oc debug node/<n> -- chroot /host ...), or a MachineConfig mount at ${NODE_PATH} with context=system_u:object_r:container_file_t:s0. weights.sh --help has the rest; not yet run on OpenShift"
    fi
    # The report after an apply is not a verdict: the download just started.
    # Its exits (no node matches, a node list the caller may not read) are
    # warnings in report mode, and jq is checked here because log_error
    # inside cmd_status ends the process past any `|| true`.
    if ! command -v jq >/dev/null 2>&1; then
        log_warning "jq is not installed; skipping the per-node report (weights.sh status needs it)"
        return 0
    fi
    cmd_status report || true
}

# cmd_status prints, per model and per node, whether the node holds the
# model. Ready is the download complete (the readinessProbe is the marker),
# a running pod that is not Ready is still downloading, a pod that keeps
# failing shows its reason, and no pod at all is a node the DaemonSet did
# not reach. The nodes are the ones the DaemonSet was applied for (its
# recorded selector) unless --node-selector overrides. Exits non-zero while
# any selected node lacks the model or the claim is not Bound.
cmd_status() {
    local mode="${1:-verdict}"
    command -v jq >/dev/null 2>&1 || log_error "jq is required for status (the node and pod lists are read with it)"
    [ -n "$NAMESPACE" ] || log_error "status needs -n NAMESPACE"
    local models=()
    if [ -n "$MODEL" ]; then
        check_model "$MODEL"
        models=("$MODEL")
    else
        local line
        while IFS= read -r line; do
            [ -n "$line" ] && models+=("$line")
        done < <(kubectl get daemonset -n "$NAMESPACE" -l "$LABEL_COMPONENT" \
            -o jsonpath='{range .items[*]}{.metadata.annotations.scaling-manager\.llmd\.ai/weights-model}{"\n"}{end}')
        [ "${#models[@]}" -gt 0 ] || log_error "no weights DaemonSets in ${NAMESPACE} and no --model given"
    fi
    local pods_file
    pods_file="$(mktemp)"
    kubectl get pods -n "$NAMESPACE" -l "$LABEL_COMPONENT" -o json > "$pods_file"
    local rc=0
    local model
    for model in "${models[@]}"; do
        local name ready holders selector
        name="$(name_for "$model")"
        ready=0
        holders=0
        selector="$NODE_SELECTOR"
        if [ "$NODE_SELECTOR_GIVEN" = false ]; then
            # Off the live DaemonSet's pod template: a nodeSelector is the
            # KEY=VALUE pairs it was applied with, none is the affinity default.
            # An object a namespace tenant can edit, so held to the same rule
            # as the flag before it reaches kubectl.
            selector="$(kubectl get daemonset -n "$NAMESPACE" "$name" \
                -o jsonpath='{.spec.template.spec.nodeSelector}' 2>/dev/null \
                | accelerator_selector_from_json || true)"
            local why
            if ! why="$(accelerator_selector_ok "$selector")"; then
                if [ "$mode" = report ]; then
                    log_warning "DaemonSet ${name} carries a nodeSelector this script did not write (${why}); pass --node-selector to report on it"
                    rc=1
                    continue
                fi
                log_error "DaemonSet ${name} carries a nodeSelector this script did not write (${why}); pass --node-selector to report on it"
            fi
        fi
        local nodes_json err_file
        err_file="$(mktemp)"
        # Caught on the line (report mode runs under `|| true`); the reason
        # is kubectl's, and only a Forbidden is the permission story.
        if ! nodes_json="$(accelerator_nodes_json "$selector" 2>"$err_file")"; then
            local why
            if grep -q Forbidden "$err_file"; then
                why="a namespace tenant may not list nodes; this needs cluster-scoped node read (cluster-reader) -- ask for it or check with the cluster admin"
            else
                why="$(tr '\n' ' ' < "$err_file")"
            fi
            rm -f "$err_file"
            if [ "$mode" = report ]; then
                log_warning "cannot list nodes for ${model}: ${why}"
                rc=1
                continue
            fi
            log_error "cannot list nodes: ${why}"
        fi
        rm -f "$err_file"
        local node_count excluded
        # Usable nodes only: a cordoned or foreign-tainted node can never hold a
        # copy an engine starts on, and counting it failed finished preparations.
        excluded="$(printf '%s' "$nodes_json" | accelerator_excluded_json "$(accelerator_ds_toleration_keys "$NAMESPACE" "$name" "${TOLERATIONS[@]}")")"
        nodes_json="$(printf '%s' "$nodes_json" | accelerator_usable_nodes_json "$excluded")"
        node_count="$(printf '%s' "$nodes_json" | jq '.items | length')"
        [ "$node_count" -gt 0 ] || accelerator_print_excluded "$excluded" >&2
        if [ "$node_count" -eq 0 ]; then
            if [ "$mode" = report ]; then
                log_warning "no node matches $(accelerator_selector_text "$selector"); ${model} will be downloaded nowhere (--node-selector picks the nodes)"
                rc=1
                continue
            fi
            log_error "no node matches $(accelerator_selector_text "$selector") (--node-selector picks the nodes)"
        fi
        local bound
        bound="$(kubectl get pvc -n "$NAMESPACE" "$name" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
        # The node label the gate waits on: the one apply recorded on the
        # DaemonSet, else the default for the model.
        local key
        key="$(kubectl get daemonset -n "$NAMESPACE" "$name" -o json 2>/dev/null \
            | jq -r --arg a "$LABEL_ANNOTATION" '.metadata.annotations[$a] // empty' 2>/dev/null || true)"
        [ -n "$key" ] || key="$(llmsm_weights_node_label_key "$model")"
        echo "${model}  (claim ${name}: ${bound:-missing}; nodes: $(accelerator_selector_text "$selector"); node label ${key})"
        if [ "$bound" != Bound ]; then
            rc=1
            [ -z "$bound" ] || echo "  the claim is not bound: its volume is cluster-scoped and was not created -- apply needs leave to create PersistentVolumes (ask the cluster admin to run apply)"
        fi
        printf '  %-28s %-12s %s\n' NODE WEIGHTS DOWNLOADER
        accelerator_print_excluded "$excluded"
        local eacces=0
        # Not a tab: bash treats a tab in IFS as whitespace, and an empty
        # field between two tabs (the reason, on every healthy row) is
        # swallowed, shifting the pod name into its place. The unit
        # separator is not whitespace and keeps empty fields.
        while IFS=$'\x1f' read -r node phase reason podname message; do
            local has=absent
            [ "$phase" = "no pod" ] || holders=$((holders + 1))
            if [ "$phase" = Ready ]; then
                has=present
                ready=$((ready + 1))
            elif [ "$phase" = Downloading ]; then
                has=downloading
                rc=1
            else
                rc=1
            fi
            printf '  %-28s %-12s %s %s\n' "$node" "$has" "$phase" "$reason"
            # A downloader that keeps dying says why in its log, not in its
            # state: a permission error is the node directory (root-owned,
            # or not labelled for containers), which is the platform's, not
            # the namespace's.
            case "$reason" in
                CrashLoopBackOff|Error)
                    if kubectl logs -n "$NAMESPACE" "$podname" --tail=20 2>/dev/null | grep -q 'Permission denied\|PermissionError\|Errno 13'; then
                        echo "    the downloader cannot write ${NODE_PATH:-the node directory}: Permission denied in its log"
                        eacces=$((eacces + 1))
                    fi ;;
                CreateContainerConfigError) [ -z "$message" ] || echo "    ${message}" ;;
            esac
        done < <(printf '%s' "$nodes_json" | jq -r --arg ds "$name" --slurpfile pods "$pods_file" \
            '.items[] as $node
             | ($pods[0].items | map(select(.metadata.labels["scaling-manager.llmd.ai/weights"] == $ds
                  and (.spec.nodeName == $node.metadata.name
                       or ((.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms // [])[0].matchFields // [])[0].values[0]? == $node.metadata.name))) | first) as $pod
             | [ $node.metadata.name,
                 (if $pod == null then "no pod"
                  elif $pod.status.phase == "Running" and (($pod.status.containerStatuses // []) | map(.ready) | all) then "Ready"
                  elif $pod.status.phase == "Running" and ((($pod.status.containerStatuses // [])[0].state // {}) | has("running")) then "Downloading"
                  else $pod.status.phase + " (not ready)" end),
                 (if $pod == null then ""
                  else ($pod.status.reason // ((($pod.status.containerStatuses // [])[0].state // {}) | to_entries | (.[0].value.reason // "")))
                       + ((($pod.status.containerStatuses // [])[0].lastState.terminated) | if . then " (last exit " + (.exitCode | tostring) + (if .reason then " " + .reason else "" end) + ")" else "" end) end),
                 ($pod.metadata.name // ""),
                 (if $pod == null then "" else (((($pod.status.containerStatuses // [])[0].state // {}) | to_entries | (.[0].value.message // "")) | gsub("[\\t\\n]"; " ")) end)
               ] | join("\u001f")')
        if [ "$eacces" -gt 0 ]; then
            echo "  ${eacces} node(s): the node directory is not writable by the downloader. On Kubernetes the downloader is root and the kubelet creates the directory root-owned, so this is a mount that is read-only or not there; on OpenShift it runs as the project UID (GID 0) under restricted-v2 and the directory is not relabelled -- on each node: mkdir -p DIR && chgrp 0 DIR && chmod 2775 DIR && chcon -t container_file_t DIR, or a MachineConfig mount with context=...:container_file_t:s0 (weights.sh --help)"
        fi
        echo "  ${ready}/${node_count} usable nodes hold it; $((node_count - ready)) do not"
        echo "  $(printf '%s' "$nodes_json" | jq --arg k "$key" '[.items[] | select(.metadata.labels[$k] == "ready")] | length')/${node_count} usable nodes carry ${key}=ready (set by weights.sh label, or the labeller)"
        [ -n "$selector" ] || printf '%s' "$nodes_json" | accelerator_vendor_warning
        if [ "$holders" -eq 0 ]; then
            local why
            why="$(kubectl get events -n "$NAMESPACE" \
                --field-selector "involvedObject.kind=DaemonSet,involvedObject.name=${name},reason=FailedCreate" \
                -o jsonpath='{.items[-1:].message}' 2>/dev/null || true)"
            [ -n "$why" ] && echo "  the DaemonSet cannot create its pods: ${why}"
        fi
    done
    rm -f "$pods_file"
    return $rc
}

# run_delete runs a kubectl delete, or under --dry-run prints it.
run_delete() {
    if [ "$DRY_RUN" = true ]; then
        echo "would run: kubectl $*"
    else
        kubectl "$@"
    fi
}

# cmd_delete removes the downloader, the claim and the volume. The files on
# the nodes stay: they are the point, and the next apply finds the marker
# and skips the download.
#
# The volumes to delete are found by what only this script (with volume
# write rights) can set -- the labels on the PersistentVolume and its
# claimRef into this namespace -- never by a name read off a claim: a claim
# is a tenant's object, and a tenant who writes a foreign volume's name
# into one must not have the admin's delete remove that volume.
cmd_delete() {
    [ -n "$NAMESPACE" ] || log_error "delete needs -n NAMESPACE"
    command -v jq >/dev/null 2>&1 || log_error "jq is required for delete (the volumes are matched on their claimRef with it)"
    local selector=() claim=""
    if [ "$ALL" = true ]; then
        selector=(-l "$LABEL_COMPONENT")
    else
        [ -n "$MODEL" ] || log_error "delete needs --model HFID or --all"
        check_model "$MODEL"
        claim="$(name_for "$MODEL")"
        selector=("$claim")
    fi
    local volumes=()
    local line
    while IFS= read -r line; do
        [ -n "$line" ] && volumes+=("$line")
    done < <(kubectl get pv -l "$LABEL_COMPONENT" -o json 2>/dev/null \
        | jq -r --arg ns "$NAMESPACE" --arg claim "$claim" \
            '.items[] | select(.spec.claimRef.namespace == $ns and ($claim == "" or .spec.claimRef.name == $claim)) | .metadata.name' \
        2>/dev/null || true)
    run_delete delete daemonset -n "$NAMESPACE" "${selector[@]}" --ignore-not-found
    run_delete delete pvc -n "$NAMESPACE" "${selector[@]}" --ignore-not-found
    [ "${#volumes[@]}" -eq 0 ] || run_delete delete pv "${volumes[@]}" --ignore-not-found
    # the ServiceAccount is shared by every model's downloader; it goes with --all
    [ "$ALL" = true ] && run_delete delete serviceaccount -n "$NAMESPACE" "$SA_NAME" --ignore-not-found
    return 0
}

# check_scope refuses a label or labeller scope that is not -n NS[,NS...]
# (each a namespace name, none twice) or -A, alone.
check_scope() {
    if [ "$ALL_NAMESPACES" = true ]; then
        [ -z "$NAMESPACE" ] || log_error "$1: -n and -A together; one or the other"
        return 0
    fi
    [ -n "$NAMESPACE" ] || log_error "$1 needs -n NS[,NS...] (the namespaces whose downloaders may label nodes) or -A (every namespace)"
    local ns seen=","
    case "$NAMESPACE" in ,*|*,|*,,*) log_error "$1: -n '${NAMESPACE}' has an empty namespace" ;; esac
    for ns in ${NAMESPACE//,/ }; do
        [[ "$ns" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]] || log_error "$1: not a namespace name: '${ns}'"
        case "$seen" in *",${ns},"*) log_error "$1: namespace ${ns} given twice" ;; esac
        seen="${seen}${ns},"
    done
}

check_label_prefix() {
    [[ "$LABEL_PREFIX" =~ $ACCELERATOR_LABEL_PREFIX_RE ]] && [ "${#LABEL_PREFIX}" -le 253 ] \
        || log_error "--label-prefix: not a DNS subdomain: '${LABEL_PREFIX}'"
}

# UNREAD is what a pass could not read, one "NS (what: why)" per entry;
# UNREAD_NS the same namespaces, comma-delimited. Set by label_scope,
# label_sources and label_pods; a namespace in it holds its own labels.
UNREAD=()
UNREAD_NS=","
# UNREAD_KNOWN: the unread namespaces whose DaemonSets were read (their
# pods were not), so whose labels are known.
UNREAD_KNOWN=","
# KEYS_SEEN remembers, one "NS KEY" per line, the node labels each
# namespace's DaemonSets asked for on the last pass that read it ("NS -"
# alone: read, and none). A namespace that cannot be read later holds those
# labels as they are, and only those. --watch keeps it from pass to pass;
# a single run starts with nothing remembered.
KEYS_SEEN=""
# LABEL_ERR is the file a pass keeps kubectl's last complaint in.
LABEL_ERR=/dev/null

# unread NS WHY records that namespace NS could not be read, and why.
unread() {
    case "$UNREAD_NS" in *",$1,"*) return 0 ;; esac
    UNREAD_NS="${UNREAD_NS}$1,"
    UNREAD+=("$1 ($2)")
}

# label_err prints the first line of what kubectl last said.
label_err() {
    head -n 1 "$LABEL_ERR" 2>/dev/null | tr -d '\r' || true
}

# label_sources prints, one JSON object per line, every weights DaemonSet in
# scope: its namespace, name and node label, the one apply recorded on it.
# Skipped with a warning: a DaemonSet that records no label, which a
# weights.sh from before the node gate applied -- its readiness probe checks
# that the marker exists, not the files, so its Ready says nothing about the
# copy -- and a DaemonSet whose label is not PREFIX/NAME-cache under
# --label-prefix: a model namespace can edit its DaemonSet, and what it
# writes there must not steer a node label outside the convention. With -n,
# a namespace whose list fails is recorded as unread and the rest go on;
# with -A, a failed list fails.
label_sources() {
    local ns lines=() list
    if [ "$ALL_NAMESPACES" = true ]; then
        list="$(kubectl get daemonset -A -l "$LABEL_COMPONENT" -o json)" || return 1
        lines+=("$list")
    else
        for ns in "${SCOPE_NS[@]}"; do
            if list="$(kubectl get daemonset -n "$ns" -l "$LABEL_COMPONENT" -o json 2>"$LABEL_ERR")"; then
                lines+=("$list")
            else
                unread "$ns" "DaemonSets: $(label_err)"
            fi
        done
    fi
    local dsns dsname key model why
    while IFS=$'\x1f' read -r dsns dsname key model; do
        [ -n "$dsname" ] || continue
        if [ -z "$key" ]; then
            log_warning "label: DaemonSet ${dsns}/${dsname} records no node label: a weights.sh from before the node gate applied it, and its readiness probe checks the marker, not the files. It is skipped. Apply it again with this release (weights.sh apply -n ${dsns} --model ${model:-<its model>} ...)."
            continue
        fi
        why="$(node_label_why "$key")"
        if [ -z "$why" ] && [ "${key%%/*}" != "$LABEL_PREFIX" ]; then
            why="its prefix is not ${LABEL_PREFIX} (--label-prefix)"
        fi
        if [ -n "$why" ]; then
            log_warning "label: DaemonSet ${dsns}/${dsname} asks for node label '${key}', which is skipped: ${why}"
            continue
        fi
        jq -cn --arg ns "$dsns" --arg name "$dsname" --arg key "$key" --arg model "$model" '{ns: $ns, name: $name, key: $key, model: $model}'
    done < <(printf '%s\n' "${lines[@]}" | jq -r --arg ann "$LABEL_ANNOTATION" \
        '.items[]? | [.metadata.namespace, .metadata.name, (.metadata.annotations[$ann] // ""),
                      (.metadata.annotations["scaling-manager.llmd.ai/weights-model"] // "")] | join("\u001f")')
}

# label_pods prints the downloader pods in scope, one JSON list per
# namespace. With -n, a namespace whose list fails is recorded as unread.
label_pods() {
    local ns
    if [ "$ALL_NAMESPACES" = true ]; then
        kubectl get pods -A -l "$LABEL_COMPONENT" -o json
        return
    fi
    for ns in "${SCOPE_NS[@]}"; do
        case "$UNREAD_NS" in *",${ns},"*) continue ;; esac
        kubectl get pods -n "$ns" -l "$LABEL_COMPONENT" -o json 2>"$LABEL_ERR" && continue
        UNREAD_KNOWN="${UNREAD_KNOWN}${ns},"
        unread "$ns" "pods: $(label_err)"
    done
}

# ns_phase prints the phase of namespace $1 (Active, Terminating), or
# nothing when there is no such namespace. Fails when it cannot be read.
ns_phase() {
    local out
    out="$(kubectl get namespace "$1" --ignore-not-found -o json)" || return 1
    [ -n "$out" ] || return 0
    printf '%s' "$out" | jq -r '.status.phase // "Active"'
}

# SCOPE_NS is what label_sources and label_pods read with -n: the namespaces
# of -n that exist now, set by label_scope.
SCOPE_NS=()

# label_scope sets SCOPE_NS to the namespaces of -n that exist and are not
# being deleted, and warns about the rest. A deleted namespace takes the
# labeller's RoleBinding with it, so listing it is Forbidden; failing the
# pass on that would freeze every node label, for every other namespace,
# until someone edits the labeller. Its downloaders are gone with it, so
# leaving it out is the truth: the labels only its downloaders held go. A
# namespace that cannot be read is recorded as unread (Forbidden is not
# absent): it holds its labels, and the others go on.
label_scope() {
    local ns phase
    SCOPE_NS=()
    for ns in ${NAMESPACE//,/ }; do
        if ! phase="$(ns_phase "$ns" 2>"$LABEL_ERR")"; then
            unread "$ns" "the namespace: $(label_err)"
            continue
        fi
        case "$phase" in
            "") log_warning "label: namespace ${ns} does not exist; it is left out (its downloaders are gone with it)" ;;
            Terminating) log_warning "label: namespace ${ns} is being deleted; it is left out" ;;
            *) SCOPE_NS+=("$ns") ;;
        esac
    done
}

# label_health records the labeller's health in ConfigMap
# weights-labeller-status, in its own namespace, for an operator to read with
# kubectl: state (ok, degraded: a namespace could not be read, or a node not
# patched; failed: nothing was changed), detail, and since (when this state
# and detail began). Only in the labeller (WEIGHTS_LABELLER_STATUS_NAMESPACE
# is set there), and only when something changed: no write every pass.
LABEL_HEALTH_LAST=""
label_health() {
    local state="$1" detail="$2"
    [ -n "${WEIGHTS_LABELLER_STATUS_NAMESPACE:-}" ] && [ "$DRY_RUN" = false ] && [ "$CLEAR" = false ] || return 0
    [ "${state}|${detail}" != "$LABEL_HEALTH_LAST" ] || return 0
    if jq -n --arg ns "$WEIGHTS_LABELLER_STATUS_NAMESPACE" --arg s "$state" --arg d "$detail" \
        --arg t "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '
        {apiVersion: "v1", kind: "ConfigMap",
         metadata: {name: "weights-labeller-status", namespace: $ns,
                    labels: {"app.kubernetes.io/name": "weights-labeller", "app.kubernetes.io/managed-by": "scaling-manager-weights"}},
         data: {state: $s, detail: $d, since: $t}}' | kubectl apply -f - >/dev/null; then
        LABEL_HEALTH_LAST="${state}|${detail}"
    else
        log_warning "label: could not write ConfigMap weights-labeller-status in ${WEIGHTS_LABELLER_STATUS_NAMESPACE}; the labels are not affected"
    fi
}

# label_once reconciles the node labels once. For each node and each label
# key, the node should carry KEY=ready when at least one downloader for that
# key has a pod there and every such downloader has a Ready pod there (its
# probe verifies the files). It sets what should be there and is not, and
# removes what it set before and should no longer be there. A label it did
# not set is never touched; one that disagrees with the downloader is
# reported. --clear removes every label it set, whatever the downloaders say.
label_once() {
    local tmp rc=0
    tmp="$(mktemp -d)" || return 1
    LABEL_ERR="$tmp/err"
    label_pass "$tmp/src" "$tmp/pods" "$tmp/nodes" || rc=$?
    LABEL_ERR=/dev/null
    rm -rf "$tmp"
    return $rc
}

# label_pass is one pass of label_once, with its three scratch files.
label_pass() {
    local src="$1" pods="$2" nodes="$3" plan rc=0 frozen="[]" hold_all=false
    : > "$src"; : > "$pods"
    UNREAD=() UNREAD_NS="," UNREAD_KNOWN=","
    if [ "$CLEAR" = false ]; then
        [ "$ALL_NAMESPACES" = true ] || label_scope
        if ! label_sources > "$src"; then
            log_warning "label: cannot list the weights DaemonSets in scope; no node was changed"
            label_health failed "cannot list the weights DaemonSets in scope; no node was changed"
            return 1
        fi
        if ! label_pods > "$pods"; then
            log_warning "label: cannot list the downloader pods in scope; no node was changed"
            label_health failed "cannot list the downloader pods in scope; no node was changed"
            return 1
        fi
        # A namespace that could not be read holds the labels it asked for
        # when it was last read, as they are: never removed, never set on
        # what the other namespaces alone say. Never read, its labels are
        # unknown: no label is removed anywhere this pass (setting one is
        # still safe -- the init container checks the copy).
        local ns k keys held=""
        for ns in ${UNREAD_NS//,/ }; do
            keys="$(jq -r --arg ns "$ns" 'select(.ns == $ns) | .key' "$src")
$(printf '%s\n' "$KEYS_SEEN" | awk -v n="$ns" '$1 == n && $2 != "-" { print $2 }')"
            case "$UNREAD_KNOWN" in
                *",${ns},"*) ;;
                *) [ -n "$(printf '%s\n' "$KEYS_SEEN" | awk -v n="$ns" '$1 == n')" ] || hold_all=true ;;
            esac
            for k in $keys; do held="${held}${k}"$'\n'; done
        done
        if [ "${#UNREAD[@]}" -gt 0 ]; then
            frozen="$(printf '%s' "$held" | jq -R -s -c 'split("\n") | map(select(. != "")) | unique')"
            jq -c --arg u "$UNREAD_NS" 'select(.ns as $n | $u | contains("," + $n + ",") | not)' "$src" > "$src.read" && mv "$src.read" "$src"
            local what
            if [ "$hold_all" = true ]; then
                what="one was never read here, so which labels are its is not known: no label is removed on any node this pass"
            elif [ "$frozen" = "[]" ]; then
                what="they asked for no node label when last read"
            else
                what="the labels they asked for when last read are held as they are: $(jq -r 'join(", ")' <<< "$frozen")"
            fi
            log_warning "label: cannot read $(printf '%s; ' "${UNREAD[@]}" | sed 's/; $//'). Forbidden is not absent: ${what}. The other namespaces are labelled as usual."
        fi
        # Remember what each namespace read in full asked for.
        if [ "$ALL_NAMESPACES" = false ]; then
            for ns in "${SCOPE_NS[@]}"; do
                case "$UNREAD_NS" in *",${ns},"*) continue ;; esac
                KEYS_SEEN="$(printf '%s\n' "$KEYS_SEEN" | awk -v n="$ns" 'NF && $1 != n')
${ns} -
$(jq -r --arg ns "$ns" 'select(.ns == $ns) | $ns + " " + .key' "$src")"
            done
        fi
        # The default key drops the organisation, so Qwen/Qwen3-8B and
        # my-org/Qwen3-8B share one: a node that holds either is labelled
        # for both, and replicas of one may prefer a node holding the other.
        local collide
        while IFS= read -r collide; do
            [ -z "$collide" ] || log_warning "label: one node label for different models: ${collide}. Give each its own with weights.sh apply --node-label."
        done < <(jq -rs 'group_by(.key)[] | select((map(.model) | unique | length) > 1)
                         | "\(.[0].key) (" + (map(.ns + "/" + .name + ": " + .model) | join(", ")) + ")"' "$src")
    fi
    if ! kubectl get nodes -o json > "$nodes"; then
        log_warning "label: cannot list nodes (this needs cluster-scoped node get, list and patch: a cluster-admin piece); no node was changed"
        label_health failed "cannot list nodes; no node was changed"
        return 1
    fi
    # One line per node with something to do or say. A node is judged by
    # its pods per DaemonSet (any Ready pod of that DaemonSet there: an
    # evicted pod beside its replacement does not count against it), then
    # per key (every DaemonSet with a pod there Ready).
    plan="$(jq -c -n --slurpfile src "$src" --slurpfile pods "$pods" --slurpfile nodes "$nodes" \
        --arg owned_ann "$OWNED_ANNOTATION" --arg prefix "$LABEL_PREFIX/" --argjson clear "$CLEAR" \
        --argjson frozen "$frozen" --argjson hold_all "$hold_all" '
        ($src | map({key: (.ns + "/" + .name), value: .key}) | from_entries) as $keyof
        | [ $pods[].items[]?
            | select(.spec.nodeName != null)
            | {node: .spec.nodeName,
               ds: (.metadata.namespace + "/" + (.metadata.labels["scaling-manager.llmd.ai/weights"] // "")),
               ready: (.status.phase == "Running" and .metadata.deletionTimestamp == null
                       and ((.status.conditions // []) | any(.type == "Ready" and .status == "True")))}
            | .key = $keyof[.ds]
            | select(.key != null) ] as $p
        | $nodes[0].items[]
        | .metadata.name as $n
        | (.metadata.labels // {}) as $l
        | ((.metadata.annotations // {})[$owned_ann] // "" | split(",") | map(select(. != ""))) as $owned
        | ([ $p[] | select(.node == $n) ] | group_by(.ds)
           | map({key: .[0].key, ready: any(.ready)}) | group_by(.key)
           | map({key: .[0].key, value: all(.ready)}) | from_entries) as $judged
        | (if $clear then {} else $judged end) as $j
        | ([ $j | to_entries[] | select(.value) | .key ]) as $want
        | ([ $want[] | select(. as $k | ($frozen | index($k)) == null)
             | select(. as $k | ($l[$k] == null) or (($owned | index($k)) != null and $l[$k] != "ready")) ]) as $set
        | (if $hold_all then [] else
             [ $owned[] | select(startswith($prefix))
               | select(. as $k | ($want | index($k)) == null and ($frozen | index($k)) == null) ] end) as $unset
        | ([ $want[] | select(. as $k | $l[$k] != null and ($owned | index($k)) == null) ]) as $foreign
        | ([ $j | to_entries[] | select(.value | not) | .key
             | select(. as $k | $l[$k] != null and ($owned | index($k)) == null) ]) as $conflict
        | ([ $owned[] | select(startswith($prefix) | not) ]) as $orphan
        | ((($owned - $unset) + $set) | unique) as $now
        | select(($set | length) + ($unset | length) + ($foreign | length) + ($conflict | length) + ($orphan | length) > 0)
        | {node: $n, set: $set, unset: $unset, foreign: $foreign, conflict: $conflict, orphan: $orphan,
           change: (($set | length) + ($unset | length) > 0),
           patch: {metadata: {
             labels: (($set | map({key: ., value: "ready"})) + ($unset | map({key: ., value: null})) | from_entries),
             annotations: {($owned_ann): (if ($now | length) == 0 then null else ($now | join(",")) end)}}}}')" || {
        log_warning "label: could not judge the nodes (jq failed); no node was changed"
        label_health failed "could not judge the nodes (jq failed); no node was changed"
        return 1
    }
    local line node k unpatched=""
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        node="$(jq -r '.node' <<< "$line")"
        while IFS= read -r k; do [ -z "$k" ] || echo "  ${node}: ${k}=ready -- its copy verifies"; done < <(jq -r '.set[]' <<< "$line")
        while IFS= read -r k; do [ -z "$k" ] || echo "  ${node}: remove ${k} -- $([ "$CLEAR" = true ] && echo "--clear" || echo "no verified copy there now")"; done < <(jq -r '.unset[]' <<< "$line")
        while IFS= read -r k; do [ -z "$k" ] || echo "  ${node}: ${k} kept as it is -- set by something else, and the copy verifies"; done < <(jq -r '.foreign[]' <<< "$line")
        while IFS= read -r k; do [ -z "$k" ] || echo "  ${node}: ${k} is set by something else, but the downloader there is not Ready -- left alone (not this command's label)"; done < <(jq -r '.conflict[]' <<< "$line")
        while IFS= read -r k; do [ -z "$k" ] || echo "  ${node}: ${k} was set by this command under another --label-prefix; left as it is (label --clear --label-prefix ${k%%/*} removes it)"; done < <(jq -r '.orphan[]' <<< "$line")
        [ "$(jq -r '.change' <<< "$line")" = true ] || continue
        if [ "$DRY_RUN" = true ]; then
            echo "  would run: kubectl patch node ${node} --type=merge -p '$(jq -c '.patch' <<< "$line")'"
        elif ! kubectl patch node "$node" --type=merge -p "$(jq -c '.patch' <<< "$line")" >/dev/null; then
            log_warning "label: could not patch node ${node}"
            unpatched="${unpatched:+${unpatched}, }${node}"
            rc=1
        fi
    done <<< "$plan"
    [ -n "$plan" ] || echo "  every node already carries the labels its copies call for"
    local detail=""
    [ "${#UNREAD[@]}" -eq 0 ] || { detail="cannot read $(printf '%s; ' "${UNREAD[@]}" | sed 's/; $//'); held: $(jq -r 'if length == 0 then "nothing" else join(", ") end' <<< "$frozen")$([ "$hold_all" = false ] || echo ", and no label removed anywhere")"; rc=1; }
    [ -z "$unpatched" ] || detail="${detail:+${detail}. }could not patch ${unpatched}"
    if [ -n "$detail" ]; then
        label_health degraded "$detail"
    else
        label_health ok "every namespace in scope read, every node patched"
    fi
    return $rc
}

cmd_label() {
    command -v jq >/dev/null 2>&1 || log_error "jq is required for label"
    [ "$CLEAR" = true ] || check_scope label
    check_label_prefix
    case "$WATCH" in
        "") label_once; return ;;
        *[!0-9]*|0) log_error "--watch takes a number of seconds, at least 1: '${WATCH}'" ;;
    esac
    [ "$CLEAR" = false ] || log_error "--clear runs once; drop --watch"
    # In the labeller this shell is PID 1, which ignores a TERM it has no
    # trap for: the pod would hold every rollout (Recreate) for its whole
    # grace period. The sleep runs in the background so the trap fires at
    # once rather than after it.
    trap 'exit 0' TERM
    while :; do
        label_once || log_warning "label: this pass failed; the next is in ${WATCH}s"
        sleep "$WATCH" &
        wait "$!"
    done
}

# render_labeller prints the labeller: its namespace (Pod Security
# restricted: it mounts nothing from the node), its ServiceAccount, node
# get/list/patch for it cluster-wide, read of DaemonSets and pods in the
# namespaces in scope, write of its status ConfigMap in its own namespace,
# and one Deployment running `weights label --watch` from the tools image
# (it records its health in ConfigMap weights-labeller-status: see
# label_health). Every object carries scaling-manager.llmd.ai/part:
# admin, as the admin objects of the install do: the namespace owner never
# applies or needs any of it.
LABELLER_LABELS='    app.kubernetes.io/name: weights-labeller
    app.kubernetes.io/component: weights-labeller
    app.kubernetes.io/managed-by: scaling-manager-weights
    scaling-manager.llmd.ai/part: admin'

# render_labeller_rolebinding prints the RoleBinding that lets the labeller
# read the downloaders in namespace $1.
render_labeller_rolebinding() {
    cat <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: scaling-manager-weights-labeller-reader
  namespace: $1
  labels:
${LABELLER_LABELS}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: scaling-manager-weights-labeller-reader
subjects:
  - kind: ServiceAccount
    name: ${LABELLER_NAME}
    namespace: ${LABELLER_NS}
EOF
}

render_labeller() {
    local labels="$LABELLER_LABELS" scope_args ns
    if [ "$ALL_NAMESPACES" = true ]; then
        scope_args='"-A"'
    else
        scope_args="\"-n\", \"${NAMESPACE}\""
    fi
    cat <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${LABELLER_NS}
  labels:
${labels}
    pod-security.kubernetes.io/enforce: restricted
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${LABELLER_NAME}
  namespace: ${LABELLER_NS}
  labels:
${labels}
---
# Nodes: read, and patch for the labels. Cluster-scoped and not narrowable
# to one label by RBAC, so it lives in a namespace of its own, where only
# the cluster admin creates pods.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: scaling-manager-weights-labeller
  labels:
${labels}
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "patch"]
  # Whether a namespace it watches still exists: a deleted one is left out
  # of the pass rather than failing it.
  - apiGroups: [""]
    resources: ["namespaces"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: scaling-manager-weights-labeller
  labels:
${labels}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: scaling-manager-weights-labeller
subjects:
  - kind: ServiceAccount
    name: ${LABELLER_NAME}
    namespace: ${LABELLER_NS}
---
# What it judges by: the downloaders and their pods. Read only.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: scaling-manager-weights-labeller-reader
  labels:
${labels}
rules:
  - apiGroups: ["apps"]
    resources: ["daemonsets"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
---
# Its health, in its own namespace: ConfigMap weights-labeller-status.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: weights-labeller-status
  namespace: ${LABELLER_NS}
  labels:
${labels}
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    resourceNames: ["weights-labeller-status"]
    verbs: ["get", "patch"]
  # create cannot be narrowed to a name; this namespace holds nothing else.
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: weights-labeller-status
  namespace: ${LABELLER_NS}
  labels:
${labels}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: weights-labeller-status
subjects:
  - kind: ServiceAccount
    name: ${LABELLER_NAME}
    namespace: ${LABELLER_NS}
EOF
    if [ "$ALL_NAMESPACES" = true ]; then
        cat <<EOF
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: scaling-manager-weights-labeller-reader
  labels:
${labels}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: scaling-manager-weights-labeller-reader
subjects:
  - kind: ServiceAccount
    name: ${LABELLER_NAME}
    namespace: ${LABELLER_NS}
EOF
    else
        for ns in ${NAMESPACE//,/ }; do
            echo "---"
            render_labeller_rolebinding "$ns"
        done
    fi
    cat <<EOF
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${LABELLER_NAME}
  namespace: ${LABELLER_NS}
  labels:
${labels}
spec:
  replicas: 1
  # One writer: two would only race on the same patches.
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app.kubernetes.io/name: weights-labeller
  template:
    metadata:
      labels:
${labels//    /        }
    spec:
      serviceAccountName: ${LABELLER_NAME}
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: label
          image: "${IMAGE}"
          imagePullPolicy: IfNotPresent
          # The tools image entry point; kubectl finds the in-cluster
          # ServiceAccount (the image's /kube/config is not mounted here).
          args: ["weights", "label", ${scope_args}, "--watch", "${WATCH}", "--label-prefix", "${LABEL_PREFIX}"]
          env:
            # Where it records its health (ConfigMap weights-labeller-status).
            - name: WEIGHTS_LABELLER_STATUS_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
          resources:
            requests:
              cpu: 10m
              memory: 64Mi
            limits:
              memory: 256Mi
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir: {}
EOF
}

# cmd_labeller applies, prints or deletes the in-cluster labeller.
# labeller_live_scope prints what the running labeller watches: "-A", a
# comma-separated namespace list, or nothing when there is none. Returns 1
# when it cannot be read (not absent: Forbidden or unreachable), 2 when its
# Deployment does not carry the arguments this script writes.
labeller_live_scope() {
    local err out
    err="$(mktemp)"
    if ! out="$(kubectl get deployment "$LABELLER_NAME" -n "$LABELLER_NS" --ignore-not-found -o json 2>"$err")"; then
        cat "$err" >&2
        rm -f "$err"
        return 1
    fi
    rm -f "$err"
    [ -n "$out" ] || return 0
    printf '%s' "$out" | jq -er '
        (.spec.template.spec.containers[0].args // []) as $a
        | if ($a | index("-A")) != null then "-A"
          elif ($a | index("-n")) != null then $a[($a | index("-n")) + 1]
          else error("no scope") end' 2>/dev/null || return 2
}

# labeller_with_list prints the live labeller Deployment $1 (JSON) with
# its -n list set to $2, ready to apply: everything else it carries (its
# image, interval, prefix) stays as it is.
labeller_with_list() {
    printf '%s' "$1" | jq --arg list "$2" '
        del(.status, .metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp,
            .metadata.generation, .metadata.managedFields, .metadata.annotations["deployment.kubernetes.io/revision"])
        | .spec.template.spec.containers[0].args |= (index("-n") as $i | .[:$i + 1] + [$list] + .[$i + 2:])' | yq -P
}

# labeller_edit adds NAMESPACE to the running labeller (add) or takes it
# off (remove): the Deployment re-applied with the namespace in or out of
# its -n list, and the RoleBinding in NAMESPACE created or deleted. Both are
# namespaced writes, so a namespace admin of NAMESPACE and of the labeller
# namespace can do it; no cluster-wide right is used. Removing a namespace
# leaves the labeller and every other namespace's labels as they are; its
# next pass removes the labels only NAMESPACE's copies held.
labeller_edit() {
    local op="$1" ns="$NAMESPACE" scope rc=0 list="" n verb
    case "$ns" in *,*) log_error "labeller --${op} takes one namespace" ;; esac
    check_scope labeller
    [ "$ns" != "$LABELLER_NS" ] || log_error "labeller --${op}: ${ns} is the labeller's own namespace, never one it watches"
    [ "$op" = add ] && verb=create || verb=delete

    scope="$(labeller_live_scope)" || rc=$?
    case "$rc" in
        1) echo "Cannot read the weights labeller in ${LABELLER_NS} (kubectl says why, above)."
           echo "  Someone who can patch deployments in ${LABELLER_NS} and ${verb} rolebindings in ${ns} runs:"
           echo "    smt weights labeller --${op} -n ${ns}"
           return 3 ;;
        2) echo "The Deployment ${LABELLER_NAME} in ${LABELLER_NS} does not carry the arguments weights.sh writes; it was not changed."
           return 3 ;;
    esac
    if [ -z "$scope" ]; then
        if [ "$op" = remove ]; then
            echo "No weights labeller runs in ${LABELLER_NS}; nothing watches ${ns}."
            return 0
        fi
        echo "No weights labeller runs in ${LABELLER_NS}: nothing labels the nodes whose weights copy verifies."
        echo "  The cluster admin adds one with the cluster-wide install (smt install --scope cluster ... --model-namespace ${ns}),"
        echo "  or on its own: smt weights labeller -n ${ns} --image ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools:<release>"
        return 4
    fi
    if [ "$scope" = "-A" ]; then
        if [ "$op" = add ]; then
            echo "The weights labeller in ${LABELLER_NS} watches every namespace, ${ns} included."
            return 0
        fi
        echo "The weights labeller in ${LABELLER_NS} watches every namespace (-A); --remove cannot leave one out."
        echo "  The cluster admin applies it again with the namespaces to keep: smt weights labeller -n NS[,NS...] --image <tools image>"
        return 3
    fi
    if [ "$op" = add ]; then
        case ",${scope}," in
            *",${ns},"*) echo "The weights labeller in ${LABELLER_NS} already watches ${ns}."; return 0 ;;
        esac
        list="${scope},${ns}"
    else
        case ",${scope}," in
            *",${ns},"*) ;;
            *) echo "The weights labeller in ${LABELLER_NS} does not watch ${ns}."; return 0 ;;
        esac
        for n in ${scope//,/ }; do
            [ "$n" = "$ns" ] || list="${list:+${list},}${n}"
        done
        if [ -z "$list" ]; then
            echo "${ns} is the only namespace the weights labeller in ${LABELLER_NS} watches; --remove does not leave it watching nothing."
            echo "  To remove it for the whole cluster, the cluster admin runs: smt weights labeller --delete, then smt weights label --clear"
            return 3
        fi
    fi
    if ! kubectl auth can-i "$verb" rolebindings -n "$ns" -q >/dev/null 2>&1 \
       || ! kubectl auth can-i patch deployments -n "$LABELLER_NS" -q >/dev/null 2>&1; then
        echo "The weights labeller in ${LABELLER_NS} $([ "$op" = add ] && echo "does not watch" || echo "watches") ${ns}, and changing that needs ${verb} rolebindings in ${ns} and patch deployments in ${LABELLER_NS}."
        echo "  Someone who has both runs:"
        echo "    smt weights labeller --${op} -n ${ns}"
        return 3
    fi
    local live image doc
    live="$(kubectl get deployment "$LABELLER_NAME" -n "$LABELLER_NS" -o json 2>/dev/null)" || {
        echo "Could not read the weights labeller in ${LABELLER_NS} again; nothing was changed. Run: smt weights labeller --${op} -n ${ns}"
        return 3
    }
    image="$(printf '%s' "$live" | jq -r '.spec.template.spec.containers[0].image')"
    doc="$(mktemp)"
    {
        if [ "$op" = add ]; then
            render_labeller_rolebinding "$ns"
            echo "---"
        fi
        labeller_with_list "$live" "$list"
    } > "$doc"
    if [ "$DRY_RUN" = true ]; then
        cat "$doc"
        rm -f "$doc"
        [ "$op" = add ] || echo "# then: kubectl delete rolebinding -n ${ns} scaling-manager-weights-labeller-reader --ignore-not-found"
        return 0
    fi
    if ! kubectl apply -f "$doc" >/dev/null; then
        rm -f "$doc"
        echo "The API server refused the change; nothing was changed. Run: smt weights labeller --${op} -n ${ns}"
        return 3
    fi
    rm -f "$doc"
    if [ "$op" = add ]; then
        echo "Added ${ns} to the weights labeller in ${LABELLER_NS} (it now watches ${list}; image ${image})."
        return 0
    fi
    # After the Deployment, so the labeller never reads a namespace it has
    # lost the right to.
    if ! kubectl delete rolebinding -n "$ns" scaling-manager-weights-labeller-reader --ignore-not-found >/dev/null; then
        echo "The weights labeller in ${LABELLER_NS} no longer watches ${ns} (it now watches ${list}), but its RoleBinding in ${ns} was not deleted:"
        echo "  kubectl delete rolebinding -n ${ns} scaling-manager-weights-labeller-reader"
        return 3
    fi
    echo "Removed ${ns} from the weights labeller in ${LABELLER_NS} (it now watches ${list}). Its next pass removes the node labels only ${ns}'s copies held."
}

cmd_labeller() {
    if [ "$ADD" = true ] || [ "$REMOVE" = true ]; then
        local op=add
        [ "$ADD" = false ] || [ "$REMOVE" = false ] || log_error "labeller: --add and --remove together; one or the other"
        [ "$REMOVE" = false ] || op=remove
        [ "$DELETE" = false ] && [ -z "$IMAGE" ] && [ "$MERGE" = false ] && [ "$ALL_NAMESPACES" = false ] && [ -z "$WATCH" ] \
            || log_error "labeller --${op} takes -n, --labeller-namespace and --dry-run only"
        local rc=0
        labeller_edit "$op" || rc=$?
        exit "$rc"
    fi
    [ "$MERGE" = false ] || [ "$ALL_NAMESPACES" = true ] || [ -n "$NAMESPACE" ] || log_error "labeller --merge needs -n NS[,NS...] or -A"
    # What the running labeller watches, read before any change: --merge
    # keeps it, --delete finds its RoleBindings by it, and a plain apply
    # must not narrow it unseen. A dry run reads no cluster.
    local live="" rc=0 n phase dropped=""
    if [ "$DRY_RUN" = false ]; then
        live="$(labeller_live_scope)" || rc=$?
        case "$rc" in
            0) ;;
            1) log_error "labeller: cannot read the running labeller in ${LABELLER_NS} (kubectl says why, above); nothing was changed" ;;
            *) [ "$MERGE" = false ] || log_error "labeller --merge: the Deployment in ${LABELLER_NS} does not carry the arguments weights.sh writes; nothing was applied"
               live="" ;;
        esac
    fi
    if [ "$MERGE" = true ] && [ "$ALL_NAMESPACES" = false ] && [ -n "$live" ]; then
        if [ "$live" = "-A" ]; then
            ALL_NAMESPACES=true
            NAMESPACE=""
        else
            # The running list first, in its order, then what is new: a
            # re-run with the same namespaces renders the same arguments,
            # and does not restart the labeller.
            local kept=""
            for n in ${live//,/ }; do
                # A namespace deleted since is dropped: its RoleBinding
                # cannot be applied, and it has no downloaders left.
                phase="$(ns_phase "$n")" \
                    || log_error "labeller --merge: cannot read namespace ${n}, which the running labeller watches; nothing was applied"
                case "$phase" in
                    ""|Terminating) log_info "the running labeller watches ${n}, which is gone or being deleted; it is dropped" ;;
                    *) kept="${kept:+${kept},}${n}" ;;
                esac
            done
            for n in ${NAMESPACE//,/ }; do
                case ",${kept}," in *",${n},"*) ;; *) kept="${kept:+${kept},}${n}" ;; esac
            done
            NAMESPACE="$kept"
        fi
    elif [ "$MERGE" = false ] && [ "$DELETE" = false ] && [ "$ALL_NAMESPACES" = false ] && [ -n "$live" ]; then
        if [ "$live" = "-A" ]; then
            dropped="every namespace (-A)"
        else
            for n in ${live//,/ }; do
                case ",${NAMESPACE}," in *",${n},"*) continue ;; esac
                phase="$(ns_phase "$n")" || log_error "labeller: cannot read namespace ${n}, which the running labeller watches; nothing was applied"
                case "$phase" in ""|Terminating) ;; *) dropped="${dropped:+${dropped},}${n}" ;; esac
            done
        fi
        [ -z "$dropped" ] || log_error "labeller: the running labeller also watches ${dropped}; -n ${NAMESPACE} alone would stop that, and the node labels only those downloaders hold would go. Add --merge to keep them. To stop watching one namespace, run labeller --remove -n NS."
    fi
    if [ "$DELETE" = true ]; then
        [ "$ALL_NAMESPACES" = false ] || [ -z "$NAMESPACE" ] || log_error "labeller: -n and -A together; one or the other"
        local ns
        run_delete delete namespace "$LABELLER_NS" --ignore-not-found
        run_delete delete clusterrolebinding scaling-manager-weights-labeller scaling-manager-weights-labeller-reader --ignore-not-found
        run_delete delete clusterrole scaling-manager-weights-labeller scaling-manager-weights-labeller-reader --ignore-not-found
        # The RoleBindings in every namespace given, and in every one the
        # running labeller watches: none is left behind.
        local rbns=""
        [ "$ALL_NAMESPACES" = true ] || rbns="$NAMESPACE"
        [ "$live" = "-A" ] || rbns="${rbns:+${rbns},}${live}"
        local seen=","
        for ns in ${rbns//,/ }; do
            case "$seen" in *",${ns},"*) continue ;; esac
            seen="${seen}${ns},"
            run_delete delete rolebinding -n "$ns" scaling-manager-weights-labeller-reader --ignore-not-found
        done
        # With no scope given, by their labels in every namespace: one left
        # from an earlier list, or from a labeller already gone.
        if [ "$ALL_NAMESPACES" = false ] && [ -z "$NAMESPACE" ]; then
            run_delete delete rolebinding -A -l app.kubernetes.io/name=weights-labeller,app.kubernetes.io/managed-by=scaling-manager-weights --ignore-not-found
        fi
        log_info "the labels it set stay on the nodes; weights.sh label --clear removes them"
        return 0
    fi
    check_scope labeller
    check_label_prefix
    [ -n "$IMAGE" ] || log_error "labeller needs --image IMG (the release's tools image, which carries kubectl and this script)"
    check_image "$IMAGE"
    [ -n "$WATCH" ] || WATCH=30
    case "$WATCH" in *[!0-9]*|0) log_error "--watch takes a number of seconds, at least 1: '${WATCH}'" ;; esac
    [[ "$LABELLER_NS" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]] || log_error "--labeller-namespace: not a namespace name: '${LABELLER_NS}'"
    case ",${NAMESPACE}," in
        *",${LABELLER_NS},"*) log_error "--labeller-namespace ${LABELLER_NS} is also in scope (-n): the labeller must not share a namespace with downloaders, whose pod creators could run as it" ;;
    esac
    if [ "$DRY_RUN" = true ]; then
        render_labeller
        return
    fi
    render_labeller | kubectl apply -f - >/dev/null
    log_info "labeller running in ${LABELLER_NS}: every ${WATCH}s it labels the nodes whose copies verify ($( [ "$ALL_NAMESPACES" = true ] && echo "every namespace" || echo "namespaces ${NAMESPACE}"))"
}

CMD="${1:-}"
[ -n "$CMD" ] || usage
shift
case "$CMD" in
    -h|--help|help) usage ;;
    apply|status|delete|label|labeller) ;;
    *) log_error "unknown command: ${CMD} (apply | status | delete | label | labeller)" ;;
esac

while [ $# -gt 0 ]; do
    case "$1" in
        -n|--namespace|--model|--path|--image|--hf-token-secret|--capacity|--memory|--node-selector|--toleration|--node-label|--watch|--label-prefix|--labeller-namespace)
            [ $# -ge 2 ] || log_error "$1 needs a value" ;;
    esac
    case "$1" in
        -n|--namespace) NAMESPACE="$2"; shift 2 ;;
        --model) MODEL="$2"; shift 2 ;;
        --path) NODE_PATH="$2"; shift 2 ;;
        --image) IMAGE="$2"; shift 2 ;;
        --hf-token-secret) HF_SECRET="$2"; shift 2 ;;
        --capacity) CAPACITY="$2"; CAPACITY_GIVEN=true; shift 2 ;;
        --memory) MEMORY="$2"; shift 2 ;;
        --node-selector) NODE_SELECTOR="$2"; NODE_SELECTOR_GIVEN=true; shift 2 ;;
        --toleration) accelerator_check_toleration "$2"; TOLERATIONS+=("$2"); shift 2 ;;
        --node-label) [ "$CMD" = apply ] || log_error "--node-label is an apply option"; NODE_LABEL="$2"; shift 2 ;;
        -A|--all-namespaces) case "$CMD" in label|labeller) ;; *) log_error "$1 is a label and labeller option" ;; esac; ALL_NAMESPACES=true; shift ;;
        --watch) case "$CMD" in label|labeller) ;; *) log_error "--watch is a label and labeller option" ;; esac; WATCH="$2"; shift 2 ;;
        --label-prefix) case "$CMD" in label|labeller) ;; *) log_error "--label-prefix is a label and labeller option" ;; esac; LABEL_PREFIX="$2"; shift 2 ;;
        --labeller-namespace) [ "$CMD" = labeller ] || log_error "--labeller-namespace is a labeller option"; LABELLER_NS="$2"; shift 2 ;;
        --clear) [ "$CMD" = label ] || log_error "--clear is a label option"; CLEAR=true; shift ;;
        --delete) [ "$CMD" = labeller ] || log_error "--delete is a labeller option"; DELETE=true; shift ;;
        --add) [ "$CMD" = labeller ] || log_error "--add is a labeller option"; ADD=true; shift ;;
        --remove) [ "$CMD" = labeller ] || log_error "--remove is a labeller option"; REMOVE=true; shift ;;
        --merge) [ "$CMD" = labeller ] || log_error "--merge is a labeller option"; MERGE=true; shift ;;
        --dry-run) [ "$CMD" != status ] || log_error "--dry-run applies to apply, delete, label and labeller, not status"; DRY_RUN=true; shift ;;
        --all) [ "$CMD" = delete ] || log_error "--all is a delete option"; ALL=true; shift ;;
        -h|--help) usage ;;
        *) log_error "unknown option: $1" ;;
    esac
done
accelerator_check_selector "$NODE_SELECTOR"
case "$CAPACITY" in
    ""|*[!0-9A-Za-z.]*) log_error "--capacity must be a Kubernetes quantity such as 1Ti, got '${CAPACITY}'" ;;
esac
case "$MEMORY" in
    ""|*[!0-9A-Za-z.]*|[!0-9]*) log_error "--memory must be a Kubernetes quantity such as 16Gi, got '${MEMORY}'" ;;
esac

"cmd_${CMD}"
