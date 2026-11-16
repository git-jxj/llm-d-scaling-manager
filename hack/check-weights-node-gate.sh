#!/usr/bin/env bash
# Executes the weights node gate against fixtures and asserts what it does:
# the node label key (llmsm_weights_node_label_key), the downloader's
# completion marker and the readiness probe that verifies the files,
# `deploy/weights.sh label` (which nodes it labels and unlabels, and which
# labels it leaves alone), the in-cluster labeller it renders, and the gate
# `make workload-patch` writes into a model server that reads a node-local
# weights claim (docs/proposals/weights-node-gate.md).
#
# Every defect this covers parses fine: a probe that passes on a marker over
# a truncated file, a downloader that blesses a partial copy left by an
# earlier version, a labeller that removes a label another tool set or keeps
# one whose copy is gone, a required affinity that replaces the GPU term it
# should have been ANDed with, and an init container on an image the node
# does not already hold.
#
# kubectl is a stub on PATH (weights.sh) or a shell function (the sourced
# library); the downloader runs against a fake huggingface_hub.
set -uo pipefail   # no -e: every check reports through expect/ok/fail

for tool in jq yq python3; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'FATAL: %s is required to run these checks.\n' "$tool" >&2; exit 1; }
done
cd "$(dirname "$0")/.."
ROOT="$PWD"

T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
FAILED=0
PASSED=0
ok()   { printf '  ok   %s\n' "$1"; PASSED=$((PASSED + 1)); }
fail() { printf '  FAIL %s\n' "$1"; FAILED=1; }
# check COND-RC MESSAGE: ok when the previous command succeeded
expect() { if [ "$1" -eq 0 ]; then ok "$2"; else fail "$2"; fi; }

# The libraries through a CR-stripped copy (a Windows checkout writes them
# CRLF; check-workload-gaps.sh says why).
for lib in common.sh scaledobject.sh accelerator_nodes.sh; do
    tr -d '\r' < "deploy/lib/$lib" > "$T/$lib"
done

# ---------------------------------------------------------------------------
# 1. The label key.
# ---------------------------------------------------------------------------
echo "label key"
(
    BLUE='' GREEN='' YELLOW='' RED='' NC=''
    # shellcheck source=/dev/null
    source "$T/common.sh"
    k() { llmsm_weights_node_label_key "$1"; }
    rc=0
    [ "$(k Qwen/Qwen3-32B)" = "llm-d.ai/qwen3-32b-cache" ] || { echo "  FAIL Qwen/Qwen3-32B -> $(k Qwen/Qwen3-32B)"; rc=1; }
    # the shape the node-preparation convention uses: name without org, lower-cased, dots kept
    [ "$(k meta-llama/Llama-3.1-8B-Instruct)" = "llm-d.ai/llama-3.1-8b-instruct-cache" ] || { echo "  FAIL meta-llama/Llama-3.1-8B-Instruct -> $(k meta-llama/Llama-3.1-8B-Instruct)"; rc=1; }
    [ "$(k gpt2)" = "llm-d.ai/gpt2-cache" ] || { echo "  FAIL gpt2 -> $(k gpt2)"; rc=1; }
    [ "$(k org/Foo_Bar)" = "llm-d.ai/foo_bar-cache" ] || { echo "  FAIL org/Foo_Bar -> $(k org/Foo_Bar)"; rc=1; }
    [ "$(k org/_x.)" = "llm-d.ai/x-cache" ] || { echo "  FAIL org/_x. -> $(k org/_x.)"; rc=1; }
    long1="org/$(printf 'a%.0s' $(seq 1 70))-one"
    long2="org/$(printf 'a%.0s' $(seq 1 70))-two"
    l1="$(k "$long1")" l2="$(k "$long2")"
    n1="${l1#*/}"
    [ "${#n1}" -le 63 ] || { echo "  FAIL a long name is ${#n1} characters"; rc=1; }
    [ "$l1" != "$l2" ] || { echo "  FAIL two long names fold onto one key: $l1"; rc=1; }
    [[ "$n1" =~ ^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$ ]] || { echo "  FAIL a long name is not a label name: $n1"; rc=1; }
    exit $rc
) && ok "the key is llm-d.ai/<name without org, lower-cased>-cache, a label name of at most 63, distinct for long ids" \
  || fail "the label key"

# ---------------------------------------------------------------------------
# 2. Rendered: the key on the claim and DaemonSet, and the probe.
# ---------------------------------------------------------------------------
echo "render"
IMG=quay.io/example/vllm-openai:v0.26.0
bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" --dry-run > "$T/r.yaml"
bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" --node-label example.com/qwen32-cache --dry-run > "$T/r2.yaml"
ann() { yq -r "select(.kind == \"$2\") | .metadata.annotations[\"scaling-manager.llmd.ai/weights-node-label\"]" "$1"; }
[ "$(ann "$T/r.yaml" PersistentVolumeClaim)" = llm-d.ai/qwen3-32b-cache ] && [ "$(ann "$T/r.yaml" DaemonSet)" = llm-d.ai/qwen3-32b-cache ]
expect $? "the claim and the DaemonSet record the default node label"
[ "$(ann "$T/r2.yaml" PersistentVolumeClaim)" = example.com/qwen32-cache ] && [ "$(ann "$T/r2.yaml" DaemonSet)" = example.com/qwen32-cache ]
expect $? "--node-label replaces it on both"
bad=0
for nl in qwen-cache llm-d.ai/qwen llm-d.ai/a/b-cache 'llm-d.ai/-x-cache' 'llm-d.ai/x y-cache' "llm-d.ai/$(printf 'a%.0s' $(seq 1 60))-cache"; do
    if bash deploy/weights.sh apply -n check-ns --model m --path /mnt/local/models --image "$IMG" --node-label "$nl" --dry-run >/dev/null 2>&1; then
        fail "--node-label '$nl' must be refused"; bad=1
    fi
done
[ "$bad" -eq 0 ] && ok "a --node-label that is not PREFIX/NAME-cache of label syntax is refused"
if bash deploy/weights.sh status -n check-ns --node-label llm-d.ai/x-cache >/dev/null 2>&1; then fail "--node-label must be an apply option only"; else ok "--node-label is refused outside apply"; fi
probe="$(yq -o json -I0 'select(.kind == "DaemonSet") | .spec.template.spec.containers[0].readinessProbe' "$T/r.yaml")"
jq -e '.exec.command[0:2] == ["python3", "-c"] and .exec.command[3:] == ["/weights/models/Qwen/Qwen3-32B", ".download-complete"]
       and .periodSeconds == 15 and .timeoutSeconds == 10 and .failureThreshold == 3' <<< "$probe" >/dev/null
expect $? "the probe runs the verifier on the model directory, every 15 s, 10 s to answer, 3 failures before NotReady"
yq -r 'select(.kind == "DaemonSet") | .spec.template.spec.containers[0].readinessProbe.exec.command[2]' "$T/r.yaml" > "$T/probe.py"
yq -r 'select(.kind == "DaemonSet") | .spec.template.spec.containers[0].args[0]' "$T/r.yaml" > "$T/download.py"
yq -r 'select(.kind == "DaemonSet") | .spec.template.spec.containers[0].command | join(" ")' "$T/r.yaml" > "$T/cmd"
[ "$(cat "$T/cmd")" = "python3 -c" ] && ! grep -q '\$(\|\$\$' "$T/download.py" "$T/probe.py"
expect $? "the downloader is python3 -c, and neither program carries \$( or \$\$ (Kubernetes rewrites both)"

# ---------------------------------------------------------------------------
# 3. The verifier, on real directories.
# ---------------------------------------------------------------------------
echo "verifier"
mkmodel() {   # mkmodel DIR: a complete two-shard model and a manifest marker
    local d="$1"
    mkdir -p "$d"
    printf '{"architectures":["X"]}' > "$d/config.json"
    printf 'AAAA' > "$d/model-00001-of-00002.safetensors"
    printf 'BBBBBB' > "$d/model-00002-of-00002.safetensors"
    printf '{"weight_map":{"a":"model-00001-of-00002.safetensors","b":"model-00002-of-00002.safetensors"}}' > "$d/model.safetensors.index.json"
    ( cd "$d" && for f in config.json model-00001-of-00002.safetensors model-00002-of-00002.safetensors model.safetensors.index.json; do
          printf '%s\t%s\n' "$(wc -c < "$f" | tr -d ' ')" "$f"; done ) > "$d/.download-complete"
}
probe_rc() { python3 "$T/probe.py" "$1" .download-complete >/dev/null 2>&1; }
mkmodel "$T/m/ok"
probe_rc "$T/m/ok"; expect $? "a complete copy verifies"
mkmodel "$T/m/nomarker"; rm "$T/m/nomarker/.download-complete"
! probe_rc "$T/m/nomarker"; expect $? "no marker: not verified (a copy still being written)"
mkmodel "$T/m/empty"; : > "$T/m/empty/.download-complete"
! probe_rc "$T/m/empty"; expect $? "an empty marker (an earlier downloader) is not verified by the probe"
mkmodel "$T/m/short"; printf 'B' > "$T/m/short/model-00002-of-00002.safetensors"
! probe_rc "$T/m/short"; expect $? "a file shorter than the marker lists: not verified"
mkmodel "$T/m/gone"; rm "$T/m/gone/model-00001-of-00002.safetensors"
! probe_rc "$T/m/gone"; expect $? "a file the marker lists is gone: not verified"
mkmodel "$T/m/noconfig"; rm "$T/m/noconfig/config.json"; sed -i '/config.json/d' "$T/m/noconfig/.download-complete"
! probe_rc "$T/m/noconfig"; expect $? "no config.json: not verified"
mkmodel "$T/m/mistral"; mv "$T/m/mistral/config.json" "$T/m/mistral/params.json"; sed -i 's/config.json/params.json/' "$T/m/mistral/.download-complete"
probe_rc "$T/m/mistral"; expect $? "a Mistral-format copy (params.json, no config.json) verifies"
! probe_rc "$T/m/absent"; expect $? "a directory that is not there (a disk that failed or was remounted empty): not verified"

# ---------------------------------------------------------------------------
# 4. The downloader, with a fake huggingface_hub.
# ---------------------------------------------------------------------------
echo "downloader"
mkdir -p "$T/py/huggingface_hub"
cat > "$T/py/huggingface_hub/__init__.py" <<'PYEOF'
import os
def snapshot_download(repo_id, local_dir, **kw):
    os.makedirs(local_dir, exist_ok=True)
    open(os.path.join(os.environ["CALLS"]), "a").write(repo_id + "\n")
    for name, body in (("config.json", "{}"), ("model.safetensors", "W" * 32)):
        with open(os.path.join(local_dir, name), "w") as fh:
            fh.write(body)
    os.makedirs(os.path.join(local_dir, ".cache", "huggingface"), exist_ok=True)
    open(os.path.join(local_dir, ".cache", "huggingface", "meta"), "w").write("x")
PYEOF
# run_dl DIR: the downloader on DIR for 3 s (it then sleeps between checks);
# the log in DIR.log, the download calls in DIR.calls
run_dl() {
    local rc=0
    : > "$1.calls"
    CALLS="$1.calls" PYTHONPATH="$T/py" TARGET_DIR="$1" MARKER=.download-complete MODEL_ID=org/m \
        timeout 3 python3 "$T/download.py" > "$1.log" 2>&1 || rc=$?
    echo "$rc"
}
mkdir -p "$T/d"
rc="$(run_dl "$T/d/fresh")"
[ "$rc" = 124 ] && [ "$(wc -l < "$T/d/fresh.calls")" -eq 1 ] && probe_rc "$T/d/fresh" \
    && grep -q "$(printf '32\tmodel.safetensors')" "$T/d/fresh/.download-complete" && ! grep -q '\.cache' "$T/d/fresh/.download-complete"
expect $? "a fresh directory: one download, then a marker listing each file and its size (not .cache), which the probe verifies"
rc="$(run_dl "$T/d/fresh")"
[ "$rc" = 124 ] && [ "$(wc -l < "$T/d/fresh.calls")" -eq 0 ] && grep -q 'present at' "$T/d/fresh.log"
expect $? "a verified copy: no download on restart"
mkmodel "$T/d/legacy"; : > "$T/d/legacy/.download-complete"
rc="$(run_dl "$T/d/legacy")"
[ "$rc" = 124 ] && [ "$(wc -l < "$T/d/legacy.calls")" -eq 0 ] && probe_rc "$T/d/legacy"
expect $? "an empty marker over a whole copy (every shard its index names) is upgraded to a list, with no download"
mkmodel "$T/d/legacy-broken"; : > "$T/d/legacy-broken/.download-complete"; rm "$T/d/legacy-broken/model-00002-of-00002.safetensors"
rc="$(run_dl "$T/d/legacy-broken")"
[ "$(wc -l < "$T/d/legacy-broken.calls")" -eq 1 ]
expect $? "an empty marker over a copy missing a shard is not blessed: it is downloaded again"
mkmodel "$T/d/truncated"; printf 'A' > "$T/d/truncated/model-00001-of-00002.safetensors"
rc="$(run_dl "$T/d/truncated")"
[ "$(wc -l < "$T/d/truncated.calls")" -eq 1 ] && grep -q 'is broken (wrong size: model-00001-of-00002.safetensors)' "$T/d/truncated.log"
expect $? "a marker over a truncated file: the copy is called broken, by file, and downloaded again"

# ---------------------------------------------------------------------------
# 5. label, offline.
# ---------------------------------------------------------------------------
echo "label"
LBL='app.kubernetes.io/component=node-local-weights,app.kubernetes.io/managed-by=scaling-manager-weights'
Q=llm-d.ai/qwen3-32b-cache
G=llm-d.ai/glm-5.3-cache
OWN=scaling-manager.llmd.ai/weights-node-labels
# ns1: Qwen (key recorded), GLM (no key recorded: an older apply); ns2: Qwen
# again (same key) and one asking for a label outside the convention.
cat > "$T/ds-ns1.json" <<EOF
{"items":[
 {"metadata":{"namespace":"ns1","name":"weights-qwen","annotations":{"scaling-manager.llmd.ai/weights-model":"Qwen/Qwen3-32B","scaling-manager.llmd.ai/weights-node-label":"$Q"}}},
 {"metadata":{"namespace":"ns1","name":"weights-glm","annotations":{"scaling-manager.llmd.ai/weights-model":"zai/GLM-5.3"}}}
]}
EOF
cat > "$T/ds-ns2.json" <<EOF
{"items":[
 {"metadata":{"namespace":"ns2","name":"weights-qwen","annotations":{"scaling-manager.llmd.ai/weights-model":"Qwen/Qwen3-32B","scaling-manager.llmd.ai/weights-node-label":"$Q"}}},
 {"metadata":{"namespace":"ns2","name":"weights-evil","annotations":{"scaling-manager.llmd.ai/weights-model":"x/y","scaling-manager.llmd.ai/weights-node-label":"node-role.kubernetes.io/control-plane"}}}
]}
EOF
pod() {   # pod NS DS NODE READY [PHASE]
    printf '{"metadata":{"namespace":"%s","name":"%s-%s","labels":{"scaling-manager.llmd.ai/weights":"%s"}},"spec":{"nodeName":"%s"},"status":{"phase":"%s","conditions":[{"type":"Ready","status":"%s"}]}}' \
        "$1" "$2" "$3" "$2" "$3" "${5:-Running}" "$4"
}
cat > "$T/pods-ns1.json" <<EOF
{"items":[
 $(pod ns1 weights-qwen node-a True),
 $(pod ns1 weights-qwen node-b False),
 $(pod ns1 weights-qwen node-d True),
 $(pod ns1 weights-qwen node-e False),
 $(pod ns1 weights-qwen node-f False Failed),
 $(pod ns1 weights-qwen node-f True),
 $(pod ns1 weights-qwen node-g True),
 $(pod ns1 weights-qwen node-h True),
 $(pod ns1 weights-glm node-a True),
 $(pod ns1 weights-evil-not-in-scope node-a True)
]}
EOF
cat > "$T/pods-ns2.json" <<EOF
{"items":[
 $(pod ns2 weights-qwen node-g False),
 $(pod ns2 weights-evil node-a True)
]}
EOF
cat > "$T/nodes.json" <<EOF
{"items":[
 {"metadata":{"name":"node-a","labels":{}}},
 {"metadata":{"name":"node-b","labels":{"$Q":"ready"},"annotations":{"$OWN":"$Q"}}},
 {"metadata":{"name":"node-c","labels":{"$Q":"ready"},"annotations":{"$OWN":"$Q"}}},
 {"metadata":{"name":"node-d","labels":{"$Q":"ready"}}},
 {"metadata":{"name":"node-e","labels":{"$Q":"ready"}}},
 {"metadata":{"name":"node-f","labels":{}}},
 {"metadata":{"name":"node-g","labels":{}}},
 {"metadata":{"name":"node-h","labels":{"$Q":"ready"},"annotations":{"$OWN":"$Q"}}},
 {"metadata":{"name":"node-i","labels":{"$Q":"ready","llm-d.ai/old-model-cache":"ready"},"annotations":{"$OWN":"llm-d.ai/old-model-cache,$Q"}}}
]}
EOF
mkdir -p "$T/bin"
cat > "$T/bin/kubectl" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$T/calls"
case "\$1 \$2" in
  "get daemonset")
      case " \$* " in *" -l $LBL "*) ;; *) echo "stub: no label selector: \$*" >&2; exit 2 ;; esac
      case "\$3 \$4" in
        "-n ns1") cat "$T/ds-ns1.json" ;;
        "-n ns2") cat "$T/ds-ns2.json" ;;
        "-A -l") [ -n "\${STUB_ALLOW_A:-}" ] || { echo "stub: -A not expected" >&2; exit 2; }; jq -s '{items: map(.items[]) }' "$T/ds-ns1.json" "$T/ds-ns2.json" ;;
        *) echo "stub: daemonset scope: \$*" >&2; exit 2 ;;
      esac ;;
  "get pods")
      case "\$3 \$4" in
        "-n ns1") cat "$T/pods-ns1.json" ;;
        "-n ns2") cat "$T/pods-ns2.json" ;;
        "-A -l") [ -n "\${STUB_ALLOW_A:-}" ] || { echo "stub: -A not expected" >&2; exit 2; }; jq -s '{items: map(.items[]) }' "$T/pods-ns1.json" "$T/pods-ns2.json" ;;
        *) echo "stub: pods scope: \$*" >&2; exit 2 ;;
      esac ;;
  "get nodes")
      [ -z "\${STUB_NODES_FORBIDDEN:-}" ] || { echo 'Error from server (Forbidden): nodes is forbidden' >&2; exit 1; }
      cat "$T/nodes.json" ;;
  "patch node")
      [ "\$4 \$5" = "--type=merge -p" ] || { echo "stub: patch form: \$*" >&2; exit 2; }
      printf '%s %s\n' "\$3" "\$6" >> "$T/patches" ;;
  "apply -f") cat > "$T/applied"; echo applied ;;
  *) echo "stub kubectl: unexpected \$*" >&2; exit 2 ;;
esac
EOF
chmod +x "$T/bin/kubectl"
SP="$T/bin:$PATH"
runlabel() { : > "$T/calls"; : > "$T/patches"; PATH="$SP" bash deploy/weights.sh label "$@" > "$T/label.out" 2>&1; }
patch_of() { awk -v n="$1" '$1 == n { $1 = ""; sub(/^ /, ""); print }' "$T/patches"; }
runlabel -n ns1,ns2; rc=$?
[ "$rc" -eq 0 ]; expect $? "label -n ns1,ns2 exits 0"
[ "$(patch_of node-a | jq -c --sort-keys .)" = "$(jq -cn --sort-keys --arg q "$Q" --arg g "$G" --arg o "$OWN" '{metadata: {labels: {($g): "ready", ($q): "ready"}, annotations: {($o): ($g + "," + $q)}}}')" ]
expect $? "node-a: both verified copies labelled (the GLM key derived for a DaemonSet that recorded none), and recorded as set here"
grep -q 'node-role.kubernetes.io/control-plane' "$T/patches"; [ $? -ne 0 ] && grep -q "weights-evil asks for node label 'node-role.kubernetes.io/control-plane', which is skipped" "$T/label.out"
expect $? "a DaemonSet asking for a label outside PREFIX/NAME-cache is skipped, said so, and never written"
[ "$(patch_of node-b | jq -c --sort-keys .)" = "$(jq -cn --sort-keys --arg q "$Q" --arg o "$OWN" '{metadata: {labels: {($q): null}, annotations: {($o): null}}}')" ]
expect $? "node-b: the downloader is not Ready -- the label this command set is removed"
[ "$(patch_of node-c | jq -c --sort-keys .)" = "$(jq -cn --sort-keys --arg q "$Q" --arg o "$OWN" '{metadata: {labels: {($q): null}, annotations: {($o): null}}}')" ]
expect $? "node-c: no downloader pod at all (the copy or the pod is gone) -- unlabelled"
[ -z "$(patch_of node-d)" ] && grep -q "node-d: $Q kept as it is -- set by something else" "$T/label.out"
expect $? "node-d: a label another tool set, over a verified copy, is left as it is"
[ -z "$(patch_of node-e)" ] && grep -q "node-e: $Q is set by something else, but the downloader there is not Ready" "$T/label.out"
expect $? "node-e: a label another tool set over a copy that does not verify is reported, never removed"
[ "$(patch_of node-f | jq -c '.metadata.labels')" = "{\"$Q\":\"ready\"}" ]
expect $? "node-f: an evicted pod beside its Ready replacement does not hold the label back"
[ -z "$(patch_of node-g)" ]
expect $? "node-g: two namespaces share the key and one is still downloading there -- no label"
[ -z "$(patch_of node-h)" ] && ! grep -q '^  node-h' "$T/label.out"
expect $? "node-h: already labelled by this command and still verified -- nothing to do, nothing said"
[ "$(patch_of node-i | jq -c --sort-keys .)" = "$(jq -cn --sort-keys --arg o "$OWN" '{metadata: {labels: {"llm-d.ai/old-model-cache": null, "llm-d.ai/qwen3-32b-cache": null}, annotations: {($o): null}}}')" ]
expect $? "node-i: labels it set whose downloaders are gone (a deleted model) are removed"
[ "$(wc -l < "$T/patches")" -eq 5 ]
expect $? "exactly five nodes patched (a, b, c, f, i): $(cut -d' ' -f1 "$T/patches" | tr '\n' ' ')"
runlabel -n ns1,ns2 --dry-run; rc=$?
[ "$rc" -eq 0 ] && [ ! -s "$T/patches" ] && grep -q "would run: kubectl patch node node-a --type=merge" "$T/label.out"
expect $? "--dry-run says each patch and makes none"
runlabel --clear; rc=$?
[ "$rc" -eq 0 ] && ! grep -q '^get daemonset\|^get pods' "$T/calls" \
    && [ "$(patch_of node-h | jq -c .metadata.labels)" = "{\"$Q\":null}" ] && [ -z "$(patch_of node-d)" ] && [ -z "$(patch_of node-a)" ]
expect $? "--clear removes every label this command set (node-h), reads no downloader, and leaves another tool's (node-d)"
STUB_ALLOW_A=1 runlabel -A; rc=$?
[ "$rc" -eq 0 ] && grep -q '^get daemonset -A' "$T/calls" && [ -n "$(patch_of node-a)" ]
expect $? "-A reads every namespace"
STUB_NODES_FORBIDDEN=1 runlabel -n ns1; rc=$?
[ "$rc" -ne 0 ] && [ ! -s "$T/patches" ] && grep -q 'cannot list nodes' "$T/label.out"
expect $? "a node list that is Forbidden fails the pass and changes nothing"
refused=0
for args in "" "-n ns1 -A" "-n ns1,,ns2" "-n ns1,ns1" "-n NS" "-n ns1 --watch 0" "-n ns1 --watch x" "--clear --watch 5" "-n ns1 --label-prefix bad_prefix"; do
    # shellcheck disable=SC2086
    if PATH="$SP" bash deploy/weights.sh label $args >/dev/null 2>&1; then fail "label $args must be refused"; refused=1; fi
done
[ "$refused" -eq 0 ] && ok "label refuses no scope, -n with -A, an empty or repeated or invalid namespace, a bad --watch, --clear with --watch, and a bad prefix"
runlabel -n ns1 --label-prefix example.com; rc=$?
[ "$rc" -eq 0 ] && ! grep -q 'llm-d.ai/' "$T/patches" && grep -q "its prefix is not example.com" "$T/label.out"
expect $? "--label-prefix: a DaemonSet asking for another prefix is skipped"

# ---------------------------------------------------------------------------
# 6. The labeller.
# ---------------------------------------------------------------------------
echo "labeller"
TOOLS=ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools:v0.0.0
bash deploy/weights.sh labeller -n ns1,ns2 --image "$TOOLS" --dry-run > "$T/lab.yaml"
bash deploy/weights.sh labeller -A --image "$TOOLS" --watch 60 --labeller-namespace ops-weights --dry-run > "$T/lab-all.yaml"
python3 - "$T/lab.yaml" "$T/lab-all.yaml" "$TOOLS" <<'PYEOF' || FAILED=1
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
alld = [d for d in yaml.safe_load_all(open(sys.argv[2])) if d]
tools = sys.argv[3]
bad = []
def check(c, m):
    if not c:
        bad.append(m)
kinds = [(d["kind"], d["metadata"].get("namespace")) for d in docs]
check(kinds == [("Namespace", None), ("ServiceAccount", "scaling-manager-weights-labeller"), ("ClusterRole", None), ("ClusterRoleBinding", None),
                ("ClusterRole", None), ("RoleBinding", "ns1"), ("RoleBinding", "ns2"), ("Deployment", "scaling-manager-weights-labeller")], "kinds: %s" % kinds)
for d in docs:
    check(d["metadata"]["labels"].get("scaling-manager.llmd.ai/part") == "admin", "part=admin on %s" % d["kind"])
ns = docs[0]
check(ns["metadata"]["labels"].get("pod-security.kubernetes.io/enforce") == "restricted", "its namespace enforces Pod Security restricted")
roles = {d["metadata"]["name"]: d for d in docs if d["kind"] == "ClusterRole"}
check(roles["scaling-manager-weights-labeller"]["rules"] == [{"apiGroups": [""], "resources": ["nodes"], "verbs": ["get", "list", "patch"]}],
      "node rights: get, list, patch and nothing else: %s" % roles["scaling-manager-weights-labeller"]["rules"])
reader = roles["scaling-manager-weights-labeller-reader"]["rules"]
check(all(set(r["verbs"]) <= {"get", "list"} for r in reader) and {x for r in reader for x in r["resources"]} == {"daemonsets", "pods"},
      "the reader reads DaemonSets and pods only: %s" % reader)
for d in docs:
    if d["kind"] in ("ClusterRoleBinding", "RoleBinding"):
        check(d["subjects"] == [{"kind": "ServiceAccount", "name": "weights-labeller", "namespace": "scaling-manager-weights-labeller"}], "binding subject: %s" % d["subjects"])
dep = docs[-1]
ps = dep["spec"]["template"]["spec"]
c = ps["containers"][0]
check(c["image"] == tools, "the labeller runs the tools image")
check(c["args"] == ["weights", "label", "-n", "ns1,ns2", "--watch", "30", "--label-prefix", "llm-d.ai"], "args: %s" % c["args"])
check(ps["securityContext"].get("runAsNonRoot") is True and ps["securityContext"]["seccompProfile"]["type"] == "RuntimeDefault", "restricted pod securityContext")
check(c["securityContext"].get("allowPrivilegeEscalation") is False and c["securityContext"]["capabilities"]["drop"] == ["ALL"] and c["securityContext"].get("readOnlyRootFilesystem") is True, "restricted container securityContext")
check(all("hostPath" not in v for v in ps.get("volumes", [])), "the labeller mounts nothing from the node")
check(dep["spec"]["replicas"] == 1 and dep["spec"]["strategy"]["type"] == "Recreate", "one writer")
check(all(dep["spec"]["selector"]["matchLabels"][k] == v for k, v in [("app.kubernetes.io/name", "weights-labeller")]) and dep["spec"]["template"]["metadata"]["labels"]["app.kubernetes.io/name"] == "weights-labeller", "selector matches the template")
akinds = [(d["kind"], d["metadata"]["name"]) for d in alld]
check(("ClusterRoleBinding", "scaling-manager-weights-labeller-reader") in akinds and not any(k == "RoleBinding" for k, _ in akinds), "-A: the reader is bound cluster-wide, no RoleBindings: %s" % akinds)
adep = alld[-1]
check(adep["metadata"]["namespace"] == "ops-weights" and adep["spec"]["template"]["spec"]["containers"][0]["args"] == ["weights", "label", "-A", "--watch", "60", "--label-prefix", "llm-d.ai"], "-A, --watch and --labeller-namespace reach the Deployment")
if bad:
    for b in bad:
        print("  FAIL " + b)
    sys.exit(1)
print("  ok   labeller: own restricted namespace, node get/list/patch only, read-only DaemonSets and pods per namespace in scope (or cluster-wide with -A), part=admin, tools image")
PYEOF
PASSED=$((PASSED + 1))
if bash deploy/weights.sh labeller -n ns1,scaling-manager-weights-labeller --image "$TOOLS" --dry-run >/dev/null 2>&1; then fail "the labeller must refuse a namespace in scope as its own"; else ok "the labeller refuses to run in a namespace whose downloaders it trusts"; fi
if bash deploy/weights.sh labeller -n ns1 --dry-run >/dev/null 2>&1; then fail "the labeller must refuse no --image"; else ok "the labeller refuses no --image"; fi
bash deploy/weights.sh labeller -n ns1,ns2 --delete --dry-run > "$T/del.out" 2>&1
grep -q 'would run: kubectl delete namespace scaling-manager-weights-labeller' "$T/del.out" && grep -q 'would run: kubectl delete rolebinding -n ns2 scaling-manager-weights-labeller-reader' "$T/del.out" \
    && grep -q 'would run: kubectl delete clusterrole scaling-manager-weights-labeller scaling-manager-weights-labeller-reader' "$T/del.out"
expect $? "labeller --delete removes its namespace, cluster roles and bindings, and the RoleBinding in each namespace"

# ---------------------------------------------------------------------------
# 7. The gate in the workload patch.
# ---------------------------------------------------------------------------
echo "workload-patch gate"
(
    set +e
    BLUE='' GREEN='' YELLOW='' RED='' NC=''
    # shellcheck source=/dev/null
    source "$T/common.sh"
    # shellcheck source=/dev/null
    source "$T/scaledobject.sh"
    FAILS=0
    say_ok()   { printf '  ok   %s\n' "$1"; }
    say_fail() { printf '  FAIL %s\n' "$1"; FAILS=1; }
    PVCS='{"items":[
      {"metadata":{"name":"weights-qwen3-32b-1a2b3c4d","annotations":{"scaling-manager.llmd.ai/weights-model":"Qwen/Qwen3-32B","scaling-manager.llmd.ai/weights-node-label":"llm-d.ai/qwen3-32b-cache"}},
       "spec":{"storageClassName":"node-local-weights","accessModes":["ReadWriteMany"]},"status":{"phase":"Bound"}},
      {"metadata":{"name":"shared-models"},"spec":{"storageClassName":"cephfs","accessModes":["ReadWriteMany"]},"status":{"phase":"Bound"}}]}'
    OBJ=""
    kubectl() {
        case "$1 $2" in
            "get deployments") printf '%s' "$OBJ" ;;
            "get pvc") case "${3:-}" in -n) printf '%s' "$PVCS" ;; *) return 1 ;; esac ;;
            "get pods"|"get replicasets") printf '{"items":[]}' ;;
            *) return 0 ;;
        esac
    }
    dep() {   # dep CLAIM MOUNT SUBPATH EXTRA_SPEC_JSON
        local extra="${4:-}"
        [ -n "$extra" ] || extra='{}'
        jq -cn --arg claim "$1" --arg mount "$2" --arg sub "$3" --argjson extra "$extra" '
          {spec: {template: {spec: ({
            terminationGracePeriodSeconds: 180,
            volumes: [{name: "model-storage", persistentVolumeClaim: {claimName: $claim}},
                      {name: "ec", persistentVolumeClaim: {claimName: "engine-cache"}}],
            containers: [{name: "vllm", image: "quay.io/example/vllm-openai:v0.26.0",
                          args: ["vllm", "serve", ($mount + "/models/Qwen/Qwen3-32B")],
                          lifecycle: {preStop: {exec: {command: ["sleep", "30"]}}},
                          env: [{name: "VLLM_CACHE_ROOT", value: "/ec/vllm"}, {name: "PYTHONPYCACHEPREFIX", value: "/ec/pyc"}],
                          volumeMounts: ([{name: "model-storage", mountPath: $mount} + (if $sub == "" then {} else {subPath: $sub} end)]
                                         + [{name: "ec", mountPath: "/ec"}])}]} + $extra)}}}'
    }
    doc_of() { so_workload_patch_doc ns1 deployments w '["spec","template"]' 2>/dev/null; }
    spec_of() { printf '%s\n' "$1" | yq -o json -I0 'select(.kind == "Deployment") | .spec.template.spec'; }

    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "")"
    d="$(doc_of)"; s="$(spec_of "$d")"
    jq -e '.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution
           == [{weight: 100, preference: {matchExpressions: [{key: "llm-d.ai/qwen3-32b-cache", operator: "In", values: ["ready"]}]}}]
           and (.affinity.nodeAffinity | has("requiredDuringSchedulingIgnoredDuringExecution") | not)' <<< "$s" >/dev/null \
        && say_ok "preferred by default: the node label at weight 100, no required term" || say_fail "preferred affinity: $(jq -c .affinity <<< "$s")"
    jq -e '.initContainers as $i
           | ([$i[0].env[] | {key: .name, value: .value}] | from_entries) as $e
           | ($i | length) == 1 and $i[0].name == "wait-for-weights" and $i[0].image == "quay.io/example/vllm-openai:v0.26.0"
             and $e.WEIGHTS_DIR == "/model-cache/models/Qwen/Qwen3-32B" and $e.NVIDIA_VISIBLE_DEVICES == "void"
             and $i[0].volumeMounts == [{name: "model-storage", mountPath: "/model-cache", readOnly: true}]
             and ($i[0].args[0] | test("download-complete") and test("config.json"))' <<< "$s" >/dev/null \
        && say_ok "the init container runs the engine image, waits for the marker and a config under the mount's models/<id>, read-only, no GPU" \
        || say_fail "init container: $(jq -c .initContainers <<< "$s")"
    case "$d" in *"weights-gate: vllm reads Qwen/Qwen3-32B from the node-local claim weights-qwen3-32b-1a2b3c4d"*"kubectl get nodes -L llm-d.ai/qwen3-32b-cache"*) say_ok "the document says why, and how to see the label" ;; *) say_fail "the why and the note: $d" ;; esac
    grep -q 'docker.io' <<< "$d" && say_fail "the gate names a docker.io image" || say_ok "no docker.io image in the gate"
    w="$(so_workload_patch_doc ns1 deployments w '["spec","template"]' 2>&1 >/dev/null)"
    case "$w" in *"ns1/w reads Qwen/Qwen3-32B from the node-local claim weights-qwen3-32b-1a2b3c4d with no weights gate"*) say_ok "the console report names the ungated model server" ;; *) say_fail "console report: $w" ;; esac

    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /weights "models/Qwen/Qwen3-32B")"
    s="$(spec_of "$(doc_of)")"
    [ "$(jq -r '.initContainers[0].env[] | select(.name == "WEIGHTS_DIR") | .value' <<< "$s")" = /weights ] \
        && [ "$(jq -r '.initContainers[0].volumeMounts[0].subPath' <<< "$s")" = models/Qwen/Qwen3-32B ] \
        && say_ok "a mount with subPath models/<id>: the directory is the mount, the subPath carried over" || say_fail "subPath models/<id>: $(jq -c .initContainers <<< "$s")"
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /weights "models")"
    s="$(spec_of "$(doc_of)")"
    [ "$(jq -r '.initContainers[0].env[] | select(.name == "WEIGHTS_DIR") | .value' <<< "$s")" = /weights/Qwen/Qwen3-32B ] \
        && say_ok "a mount with subPath models: the directory is <mount>/<id>" || say_fail "subPath models: $(jq -c .initContainers <<< "$s")"
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /weights "elsewhere")"
    d="$(doc_of)"
    [ -z "$d" ] && say_ok "a subPath that does not lead to the model: no gate (and nothing else to do: no document)" || say_fail "unrelated subPath emitted: $d"

    # required: ANDed into every existing term, which survives
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "" '{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[
        {"matchExpressions":[{"key":"nvidia.com/gpu.product","operator":"In","values":["NVIDIA-H200"]}]},
        {"matchExpressions":[{"key":"nvidia.com/gpu.product","operator":"In","values":["NVIDIA-H100-80GB-HBM3"]}]}]}}}}')"
    s="$(LLMSM_WEIGHTS_NODE_AFFINITY=required so_workload_patch_doc ns1 deployments w '["spec","template"]' 2>/dev/null | yq -o json -I0 'select(.kind == "Deployment") | .spec.template.spec')"
    jq -e '.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms
           == [{matchExpressions: [{key: "nvidia.com/gpu.product", operator: "In", values: ["NVIDIA-H200"]}, {key: "llm-d.ai/qwen3-32b-cache", operator: "In", values: ["ready"]}]},
               {matchExpressions: [{key: "nvidia.com/gpu.product", operator: "In", values: ["NVIDIA-H100-80GB-HBM3"]}, {key: "llm-d.ai/qwen3-32b-cache", operator: "In", values: ["ready"]}]}]' <<< "$s" >/dev/null \
        && say_ok "required: the label is ANDed into each existing term, which keeps its GPU product" || say_fail "required merge: $(jq -c .affinity <<< "$s")"
    # preferred: existing preferences survive (the list is replaced whole)
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "" '{"affinity":{"nodeAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[
        {"weight":10,"preference":{"matchExpressions":[{"key":"zone","operator":"In","values":["a"]}]}}]}}}')"
    s="$(spec_of "$(doc_of)")"
    [ "$(jq -c '[.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[].weight]' <<< "$s")" = "[10,100]" ] \
        && say_ok "preferred: an existing preference is carried in the emitted list" || say_fail "preferred merge: $(jq -c .affinity <<< "$s")"
    s="$(LLMSM_WEIGHTS_NODE_AFFINITY=none so_workload_patch_doc ns1 deployments w '["spec","template"]' 2>/dev/null | yq -o json -I0 'select(.kind == "Deployment") | .spec.template.spec')"
    jq -e '.affinity == null and .initContainers[0].name == "wait-for-weights"' <<< "$s" >/dev/null \
        && say_ok "none: no affinity, the init container still" || say_fail "none: $s"
    # already gated: nothing
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "" '{"initContainers":[{"name":"wait-for-weights","image":"x"}],"affinity":{"nodeAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[
        {"weight":100,"preference":{"matchExpressions":[{"key":"llm-d.ai/qwen3-32b-cache","operator":"In","values":["ready"]}]}}]}}}')"
    d="$(doc_of)"
    [ -z "$d" ] && say_ok "a template already gated gets no document" || say_fail "already gated emitted: $d"
    # a shared claim (not node-local): no gate
    OBJ="$(dep shared-models /model-cache "")"
    d="$(doc_of)"
    [ -z "$d" ] && say_ok "a shared claim needs no gate: every node reads the same volume" || say_fail "shared claim emitted: $d"
    # the trim the apply path runs drops the gate, so --apply never sends it
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "")"
    f="$(mktemp)"; printf '%s\n' "$(doc_of)" | yq 'select(.kind == "Deployment")' > "$f"
    yq -i '.spec.template.spec.terminationGracePeriodSeconds = 200' "$f"
    if so_workload_patch_trim "$f" 1 1; then
        [ "$(yq -o json -I0 '.spec.template.spec | [has("affinity"), has("initContainers"), has("terminationGracePeriodSeconds")]' "$f")" = "[false,false,true]" ] \
            && say_ok "the apply trim drops the gate (affinity and init container) and keeps the rest" || say_fail "trim kept: $(yq -o json -I0 '.spec.template.spec' "$f")"
    else
        say_fail "trim reported nothing left although a grace period was there"
    fi
    f2="$(mktemp)"; printf '%s\n' "$(doc_of)" | yq 'select(.kind == "Deployment")' > "$f2"
    ! so_workload_patch_trim "$f2" 1 1 && say_ok "a document carrying only the gate is not applied at all" || say_fail "a gate-only document survived the trim"
    rm -f "$f" "$f2"

    # The HF_HOME cache the weights half proposes is never a node-local
    # weights claim: each node has its own copy there, under models/<id>.
    all_pvcs="$PVCS"
    PVCS="$(jq -c '{items: [.items[] | select(.spec.storageClassName == "node-local-weights")]}' <<< "$all_pvcs")"
    c1="$(so_model_claim ns1)"
    PVCS="$(jq -c '{items: [.items[] | select(.spec.storageClassName != "node-local-weights")]}' <<< "$all_pvcs")"
    c2="$(so_model_claim ns1)"
    PVCS="$all_pvcs"
    c3="$(so_model_claim ns1)"
    [ -z "$c1" ] && [ "$c2" = shared-models ] && [ "$c3" = shared-models ] \
        && say_ok "the weights half never picks a node-local weights claim as its HF_HOME cache; a shared one beside it is still found" \
        || say_fail "so_model_claim: alone '$c1' (want none), shared '$c2', both '$c3' (want shared-models)"
    exit $FAILS
) || FAILED=1

echo
if [ "$FAILED" -ne 0 ]; then
    echo "check-weights-node-gate: FAILED"
    exit 1
fi
# A run that checked nothing is not a pass.
[ "$PASSED" -ge 40 ] || { echo "check-weights-node-gate: only $PASSED checks ran; expected at least 40"; exit 1; }
echo "check-weights-node-gate: all checks passed ($PASSED counted here, plus the workload-patch cases above)"
