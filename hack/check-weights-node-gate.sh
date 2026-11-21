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
# A copy that breaks while the downloader runs: the marker goes before it
# exits, so a replica's wait-for-weights (marker and config only) stops
# passing then, not after the restart.
mkmodel "$T/d/breaks"
: > "$T/d/breaks.calls"
( CALLS="$T/d/breaks.calls" PYTHONPATH="$T/py" TARGET_DIR="$T/d/breaks" MARKER=.download-complete MODEL_ID=org/m RECHECK_SECONDS=1 \
    timeout 8 python3 "$T/download.py" > "$T/d/breaks.log" 2>&1; echo $? > "$T/d/breaks.rc" ) &
dl=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do grep -q 'present at' "$T/d/breaks.log" 2>/dev/null && break; sleep 0.2; done
printf 'A' > "$T/d/breaks/model-00001-of-00002.safetensors"
wait "$dl"
[ "$(cat "$T/d/breaks.rc")" = 1 ] && [ ! -e "$T/d/breaks/.download-complete" ] && grep -q 'marker is removed' "$T/d/breaks.log"
expect $? "a copy that breaks under a running downloader: within its re-check it removes the marker and exits 1 (rc $(cat "$T/d/breaks.rc" 2>/dev/null))"
grep -q 'every = int(os.environ.get("RECHECK_SECONDS") or "30")' "$T/download.py"
expect $? "the downloader re-checks every 30 s unless RECHECK_SECONDS says otherwise"

# ---------------------------------------------------------------------------
# 5. label, offline.
# ---------------------------------------------------------------------------
echo "label"
LBL='app.kubernetes.io/component=node-local-weights,app.kubernetes.io/managed-by=scaling-manager-weights'
Q=llm-d.ai/qwen3-32b-cache
G=llm-d.ai/glm-5.3-cache
OWN=scaling-manager.llmd.ai/weights-node-labels
# ns1: Qwen and GLM, and one applied before the node gate (no key recorded:
# its probe checked the marker only); ns2: Qwen again (same key) and one
# asking for a label outside the convention.
cat > "$T/ds-ns1.json" <<EOF
{"items":[
 {"metadata":{"namespace":"ns1","name":"weights-qwen","annotations":{"scaling-manager.llmd.ai/weights-model":"Qwen/Qwen3-32B","scaling-manager.llmd.ai/weights-node-label":"$Q"}}},
 {"metadata":{"namespace":"ns1","name":"weights-glm","annotations":{"scaling-manager.llmd.ai/weights-model":"zai/GLM-5.3","scaling-manager.llmd.ai/weights-node-label":"$G"}}},
 {"metadata":{"namespace":"ns1","name":"weights-legacy","annotations":{"scaling-manager.llmd.ai/weights-model":"zai/Legacy-1"}}}
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
 $(pod ns1 weights-legacy node-a True),
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
gone() { [ -n "\${STUB_NS_GONE:-}" ] && [ "\$1" = "\$STUB_NS_GONE" ]; }
forbid_gone() { gone "\$1" && { echo "Error from server (Forbidden): \$2 is forbidden: cannot list resource in the namespace \$1" >&2; exit 1; }; }
# forbid KIND NS: Forbidden when \$T/forbid-KIND names NS (a file, so a
# running --watch can be switched between passes).
forbid() { [ -f "$T/forbid-\$1" ] && [ "\$(cat "$T/forbid-\$1")" = "\$2" ] && { echo "Error from server (Forbidden): \$1 is forbidden: User cannot list resource \"\$1\" in the namespace \"\$2\"" >&2; exit 1; }; return 0; }
case "\$1 \$2" in
  "get namespace")
      [ -z "\${STUB_NS_FORBIDDEN:-}" ] || { echo 'Error from server (Forbidden): namespaces is forbidden' >&2; exit 1; }
      case " \$* " in *" --ignore-not-found "*) ;; *) echo "stub: namespace read without --ignore-not-found" >&2; exit 2 ;; esac
      gone "\$3" && exit 0
      [ "\$3" = "\${STUB_NS_TERMINATING:-}" ] && { echo '{"status":{"phase":"Terminating"}}'; exit 0; }
      echo '{"status":{"phase":"Active"}}' ;;
  "get daemonset")
      case " \$* " in *" -l $LBL "*) ;; *) echo "stub: no label selector: \$*" >&2; exit 2 ;; esac
      forbid_gone "\$4" daemonsets
      forbid daemonsets "\$4"
      case "\$3 \$4" in
        "-n ns1") cat "$T/ds-ns1.json" ;;
        "-n ns2") cat "\${STUB_DS_NS2:-$T/ds-ns2.json}" ;;
        "-A -l") [ -n "\${STUB_ALLOW_A:-}" ] || { echo "stub: -A not expected" >&2; exit 2; }; jq -s '{items: map(.items[]) }' "$T/ds-ns1.json" "$T/ds-ns2.json" ;;
        *) echo "stub: daemonset scope: \$*" >&2; exit 2 ;;
      esac ;;
  "get pods")
      forbid_gone "\$4" pods
      forbid pods "\$4"
      case "\$3 \$4" in
        "-n ns1") cat "\${STUB_PODS_NS1:-$T/pods-ns1.json}" ;;
        "-n ns2") cat "$T/pods-ns2.json" ;;
        "-A -l") [ -n "\${STUB_ALLOW_A:-}" ] || { echo "stub: -A not expected" >&2; exit 2; }; jq -s '{items: map(.items[]) }' "$T/pods-ns1.json" "$T/pods-ns2.json" ;;
        *) echo "stub: pods scope: \$*" >&2; exit 2 ;;
      esac ;;
  "get nodes")
      [ -z "\${STUB_NODES_FORBIDDEN:-}" ] || { echo 'Error from server (Forbidden): nodes is forbidden' >&2; exit 1; }
      echo PASS >> "$T/patches"
      cat "$T/nodes.json" ;;
  "get deployment")
      [ -z "\${STUB_LABELLER_FORBIDDEN:-}" ] || { echo 'Error from server (Forbidden): deployments.apps is forbidden' >&2; exit 1; }
      [ -z "\${STUB_LABELLER:-}" ] || cat "\$STUB_LABELLER" ;;
  "auth can-i")
      case " \${STUB_CANI_NO:-} " in *" \$3/\$6 "*) echo no; exit 1 ;; esac
      echo yes ;;
  "delete rolebinding") printf '%s\n' "\$*" >> "$T/deleted" ;;
  "patch node")
      [ "\$4 \$5" = "--type=merge -p" ] || { echo "stub: patch form: \$*" >&2; exit 2; }
      [ "\$3" != "\${STUB_PATCH_FAILS:-}" ] || { echo "Error from server (Conflict): node \$3" >&2; exit 1; }
      printf '%s %s\n' "\$3" "\$6" >> "$T/patches" ;;
  "apply -f") [ -z "\${STUB_APPLY_FAILS:-}" ] || { echo 'Error from server (Invalid): refused' >&2; exit 1; }; if [ "\$3" = - ]; then cat > "$T/applied"; else cp "\$3" "$T/applied"; fi; echo applied ;;
  *) echo "stub kubectl: unexpected \$*" >&2; exit 2 ;;
esac
EOF
chmod +x "$T/bin/kubectl"
SP="$T/bin:$PATH"
runlabel() { : > "$T/calls"; : > "$T/patches"; rm -f "$T/applied"; PATH="$SP" bash deploy/weights.sh label "$@" > "$T/label.out" 2>&1; }
# patch_of NODE: its patch in the last pass (after the last PASS marker)
PATCHES="$T/patches"
patch_of() { awk -v n="$1" '$1 == "PASS" { p = "" } $1 == n { $1 = ""; sub(/^ /, ""); p = $0 } END { if (p != "") print p }' "$PATCHES"; }
npatched() { awk '$1 == "PASS" { c = 0; next } { c++ } END { print c + 0 }' "$T/patches"; }
runlabel -n ns1,ns2; rc=$?
[ "$rc" -eq 0 ]; expect $? "label -n ns1,ns2 exits 0"
[ "$(patch_of node-a | jq -c --sort-keys .)" = "$(jq -cn --sort-keys --arg q "$Q" --arg g "$G" --arg o "$OWN" '{metadata: {labels: {($g): "ready", ($q): "ready"}, annotations: {($o): ($g + "," + $q)}}}')" ]
expect $? "node-a: both verified copies labelled, and recorded as set here"
! grep -q 'legacy-1-cache' "$T/patches" && grep -q 'DaemonSet ns1/weights-legacy records no node label: a weights.sh from before the node gate applied it' "$T/label.out"
expect $? "a DaemonSet from before the node gate (no label recorded; its probe checked the marker only) is skipped, said so, and its Ready pod labels nothing"
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
[ "$(npatched)" -eq 5 ]
expect $? "exactly five nodes patched (a, b, c, f, i): $(grep -v PASS "$T/patches" | cut -d' ' -f1 | tr '\n' ' ')"
runlabel -n ns1,ns2 --dry-run; rc=$?
[ "$rc" -eq 0 ] && [ "$(npatched)" -eq 0 ] && grep -q "would run: kubectl patch node node-a --type=merge" "$T/label.out"
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
# A namespace deleted after the labeller was told to watch it: its
# RoleBinding went with it, so listing it is Forbidden. The pass goes on
# for the others (node-g: ns2's download there is gone with ns2).
STUB_NS_GONE=ns2 runlabel -n ns1,ns2; rc=$?
[ "$rc" -eq 0 ] && [ "$(patch_of node-g | jq -c '.metadata.labels')" = "{\"$Q\":\"ready\"}" ] && [ -n "$(patch_of node-a)" ] \
    && grep -q 'namespace ns2 does not exist; it is left out' "$T/label.out" && ! grep -q 'get daemonset -n ns2' "$T/calls"
expect $? "a deleted namespace in scope is left out, and the pass labels the rest (rc $rc)"
STUB_NS_TERMINATING=ns2 runlabel -n ns1,ns2; rc=$?
[ "$rc" -eq 0 ] && grep -q 'namespace ns2 is being deleted' "$T/label.out" && ! grep -q 'get pods -n ns2' "$T/calls"
expect $? "a namespace being deleted is left out too"
STUB_NS_FORBIDDEN=1 runlabel -n ns1,ns2; rc=$?
[ "$rc" -ne 0 ] && [ "$(npatched)" -eq 0 ] && grep -q 'cannot read ns1 (the namespace: Error from server (Forbidden)' "$T/label.out" \
    && grep -q 'no label is removed on any node this pass' "$T/label.out"
expect $? "no namespace can be read: the pass fails, names each, and changes nothing (Forbidden is not absent)"
# One namespace unreadable, never read before (a single run remembers
# nothing): its labels are unknown, so nothing is removed anywhere; the
# other namespace's verified copies are still labelled.
echo ns2 > "$T/forbid-daemonsets"
runlabel -n ns1,ns2; rc=$?
rm -f "$T/forbid-daemonsets"
[ "$rc" -ne 0 ] && [ -n "$(patch_of node-a)" ] && [ "$(patch_of node-a | jq -c '.metadata.labels | keys')" = "[\"$G\",\"$Q\"]" ] \
    && ! grep -q null "$T/patches" && grep -q 'cannot read ns2 (DaemonSets: Error from server (Forbidden)' "$T/label.out" \
    && grep -q 'one was never read here' "$T/label.out"
expect $? "ns2's DaemonSets Forbidden on a first pass: ns1's copies are labelled, no label is removed anywhere, and ns2 is named (rc $rc)"
# Its DaemonSets read but not its pods: its labels are known, and only
# those are held (Q); GLM, ns1's alone, is labelled, and node-i's stale label goes.
echo ns2 > "$T/forbid-pods"
runlabel -n ns1,ns2; rc=$?
rm -f "$T/forbid-pods"
[ "$rc" -ne 0 ] && [ "$(patch_of node-a | jq -c '.metadata.labels')" = "{\"$G\":\"ready\"}" ] \
    && [ -z "$(patch_of node-b)" ] && [ -z "$(patch_of node-c)" ] && [ -z "$(patch_of node-f)" ] \
    && [ "$(patch_of node-i | jq -c '.metadata.labels')" = '{"llm-d.ai/old-model-cache":null}' ] \
    && grep -q "the labels they asked for when last read are held as they are: $Q" "$T/label.out"
expect $? "ns2's pods Forbidden: only ns2's label ($Q) is held, on every node; the rest is reconciled (rc $rc)"
# A watch remembers: a namespace read once and unreadable later holds only
# its own labels. And the labeller's health is a ConfigMap, written when it
# changes, not on every pass.
: > "$T/calls"; : > "$T/patches"; rm -f "$T/applied" "$T/forbid-daemonsets"
( PATH="$SP" WEIGHTS_LABELLER_STATUS_NAMESPACE=lns exec bash deploy/weights.sh label -n ns1,ns2 --watch 1 > "$T/watch2.out" 2>&1 ) &
wpid=$!
# Three passes begun: the first two are done, so a second write would show.
for _ in $(seq 1 80); do [ "$(grep -c PASS "$T/patches")" -ge 3 ] && break; sleep 0.1; done
applies_ok="$(grep -c '^apply -f' "$T/calls")"
state_ok="$(yq -r '.data.state' "$T/applied" 2>/dev/null)"
echo ns2 > "$T/forbid-daemonsets"
# The health write ends a pass; the next starts a second later. The
# snapshot is that pass alone, whole.
for _ in $(seq 1 80); do [ "$(yq -r '.data.state' "$T/applied" 2>/dev/null)" = degraded ] && break; sleep 0.1; done
cp "$T/patches" "$T/patches.snap"
kill -TERM "$wpid"; wait "$wpid"
rm -f "$T/forbid-daemonsets"
PATCHES="$T/patches.snap"
[ "$(patch_of node-a | jq -c '.metadata.labels')" = "{\"$G\":\"ready\"}" ] && [ -z "$(patch_of node-b)" ] \
    && [ "$(patch_of node-i | jq -c '.metadata.labels')" = '{"llm-d.ai/old-model-cache":null}' ] \
    && grep -q "held as they are: $Q" "$T/watch2.out"
expect $? "--watch: ns2 read once, then Forbidden: only the label it asked for ($Q) is held; the rest is reconciled"
PATCHES="$T/patches"
[ "$state_ok" = ok ] && [ "$applies_ok" -eq 1 ] && [ "$(yq -r '.data.state' "$T/applied")" = degraded ] \
    && [ "$(yq -r '.metadata.name + " " + .metadata.namespace' "$T/applied")" = "weights-labeller-status lns" ] \
    && yq -r '.data.detail' "$T/applied" | grep -q "cannot read ns2 (DaemonSets: Error from server (Forbidden).*held: $Q" \
    && [ "$(grep -c '^apply -f' "$T/calls")" -eq 2 ]
expect $? "health: ConfigMap weights-labeller-status says ok after $applies_ok write over two passes, then degraded naming ns2 and the held label, one write more ($(grep -c '^apply -f' "$T/calls") writes)"
runlabel -n ns1,ns2
[ ! -e "$T/applied" ]
expect $? "label outside the labeller (no WEIGHTS_LABELLER_STATUS_NAMESPACE) writes no health ConfigMap"
refused=0
for args in "" "-n ns1 -A" "-n ns1,,ns2" "-n ns1,ns1" "-n NS" "-n ns1 --watch 0" "-n ns1 --watch x" "--clear --watch 5" "-n ns1 --label-prefix bad_prefix"; do
    # shellcheck disable=SC2086
    if PATH="$SP" bash deploy/weights.sh label $args >/dev/null 2>&1; then fail "label $args must be refused"; refused=1; fi
done
[ "$refused" -eq 0 ] && ok "label refuses no scope, -n with -A, an empty or repeated or invalid namespace, a bad --watch, --clear with --watch, and a bad prefix"
# --watch stops on TERM at once, with 0: in the labeller pod this shell is
# PID 1, which ignores a TERM it does not trap. (Here it is not PID 1, so an
# untrapped TERM kills it with 143: the exit code is what tells them apart.)
( PATH="$SP" exec bash deploy/weights.sh label -n ns1 --watch 60 > "$T/watch.out" 2>&1 ) &
wpid=$!
for _ in $(seq 1 50); do grep -q 'node-a' "$T/watch.out" 2>/dev/null && break; sleep 0.1; done
sleep 0.3
kill -TERM "$wpid"
start=$SECONDS
wait "$wpid"; rc=$?
took=$((SECONDS - start))
[ "$rc" -eq 0 ] && [ "$took" -le 2 ]
expect $? "label --watch exits 0 on TERM while it sleeps, at once (rc $rc after ${took}s)"
# Two models whose default keys are one: the org is dropped from the key.
jq '.items[0].metadata.annotations["scaling-manager.llmd.ai/weights-model"] = "my-org/Qwen3-32B"' "$T/ds-ns2.json" > "$T/ds-ns2-fork.json"
STUB_DS_NS2="$T/ds-ns2-fork.json" runlabel -n ns1,ns2; rc=$?
[ "$rc" -eq 0 ] && grep -q "one node label for different models: $Q (ns1/weights-qwen: Qwen/Qwen3-32B, ns2/weights-qwen: my-org/Qwen3-32B)" "$T/label.out"
expect $? "two models on one node label are named in a warning"
runlabel -n ns1,ns2
! grep -q 'one node label for different models' "$T/label.out"
expect $? "one model in two namespaces is not warned about"
runlabel -n ns1 --label-prefix example.com; rc=$?
[ "$rc" -eq 0 ] && ! grep -q 'llm-d.ai/' "$T/patches" && grep -q "its prefix is not example.com" "$T/label.out" \
    && grep -q "node-i: llm-d.ai/old-model-cache was set by this command under another --label-prefix; left as it is (label --clear --label-prefix llm-d.ai removes it)" "$T/label.out"
expect $? "--label-prefix: a DaemonSet asking for another prefix is skipped, and a label set under the old prefix is reported, not orphaned unseen"
# One node refuses its patch: the others are still patched, the pass fails.
STUB_PATCH_FAILS=node-a runlabel -n ns1,ns2; rc=$?
[ "$rc" -ne 0 ] && [ -z "$(patch_of node-a)" ] && [ -n "$(patch_of node-b)" ] && [ "$(npatched)" -eq 4 ] \
    && grep -q 'could not patch node node-a' "$T/label.out"
expect $? "a node patch that fails is reported and fails the pass; the other four nodes are still patched (rc $rc)"
# -A: a list that fails fails the whole pass (there is no namespace to hold).
printf '%s\n' -l > "$T/forbid-pods"   # the stub sees -A -l: "-l" stands for every namespace
STUB_ALLOW_A=1 runlabel -A; rc=$?
rm -f "$T/forbid-pods"
[ "$rc" -ne 0 ] && [ "$(npatched)" -eq 0 ] && grep -q 'cannot list the downloader pods in scope; no node was changed' "$T/label.out"
expect $? "-A: a pod list that fails changes no node (rc $rc)"
# Pods that are not Running and Ready do not hold a label: Pending (even
# with a Ready condition), Succeeded, and one being deleted.
jq '{items: [
      (.items[] | select(.spec.nodeName == "node-a" and .metadata.labels["scaling-manager.llmd.ai/weights"] == "weights-qwen") | .status.phase = "Pending"),
      (.items[] | select(.spec.nodeName == "node-d") | .status.phase = "Succeeded"),
      (.items[] | select(.spec.nodeName == "node-h") | .metadata.deletionTimestamp = "2026-01-01T00:00:00Z")]}' "$T/pods-ns1.json" > "$T/pods-ns1-odd.json"
STUB_PODS_NS1="$T/pods-ns1-odd.json" runlabel -n ns1; rc=$?
[ "$rc" -eq 0 ] && [ "$(jq -r '.items | length' "$T/pods-ns1-odd.json")" -eq 3 ] \
    && [ -z "$(patch_of node-a | jq -r --arg q "$Q" '.metadata.labels[$q] // empty')" ] \
    && [ "$(patch_of node-h | jq -c '.metadata.labels')" = "{\"$Q\":null}" ] \
    && grep -q "node-d: $Q is set by something else, but the downloader there is not Ready" "$T/label.out"
expect $? "a Pending pod, a Succeeded one and one being deleted are not Ready: no label set, this command's removed (node-h), another tool's reported (node-d)"

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
L = "scaling-manager-weights-labeller"
check(kinds == [("Namespace", None), ("ServiceAccount", L), ("ClusterRole", None), ("ClusterRoleBinding", None),
                ("ClusterRole", None), ("Role", L), ("RoleBinding", L), ("RoleBinding", "ns1"), ("RoleBinding", "ns2"), ("Deployment", L)], "kinds: %s" % kinds)
srole = [d for d in docs if d["kind"] == "Role"][0]
check(srole["rules"] == [{"apiGroups": [""], "resources": ["configmaps"], "resourceNames": ["weights-labeller-status"], "verbs": ["get", "patch"]},
                         {"apiGroups": [""], "resources": ["configmaps"], "verbs": ["create"]}],
      "its own namespace: get and patch its status ConfigMap by name, create (not narrowable): %s" % srole["rules"])
senv = docs[-1]["spec"]["template"]["spec"]["containers"][0].get("env", [])
check(senv == [{"name": "WEIGHTS_LABELLER_STATUS_NAMESPACE", "valueFrom": {"fieldRef": {"fieldPath": "metadata.namespace"}}}], "status namespace from the downward API: %s" % senv)
for d in docs:
    check(d["metadata"]["labels"].get("scaling-manager.llmd.ai/part") == "admin", "part=admin on %s" % d["kind"])
ns = docs[0]
check(ns["metadata"]["labels"].get("pod-security.kubernetes.io/enforce") == "restricted", "its namespace enforces Pod Security restricted")
roles = {d["metadata"]["name"]: d for d in docs if d["kind"] == "ClusterRole"}
check(roles["scaling-manager-weights-labeller"]["rules"] == [{"apiGroups": [""], "resources": ["nodes"], "verbs": ["get", "list", "patch"]},
                                                         {"apiGroups": [""], "resources": ["namespaces"], "verbs": ["get"]}],
      "cluster rights: node get, list, patch and namespace get, nothing else: %s" % roles["scaling-manager-weights-labeller"]["rules"])
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
check(("ClusterRoleBinding", "scaling-manager-weights-labeller-reader") in akinds and not any(k == "RoleBinding" and n != "weights-labeller-status" for k, n in akinds), "-A: the reader is bound cluster-wide, no RoleBindings: %s" % akinds)
adep = alld[-1]
check(adep["metadata"]["namespace"] == "ops-weights" and adep["spec"]["template"]["spec"]["containers"][0]["args"] == ["weights", "label", "-A", "--watch", "60", "--label-prefix", "llm-d.ai"], "-A, --watch and --labeller-namespace reach the Deployment")
if bad:
    for b in bad:
        print("  FAIL " + b)
    sys.exit(1)
print("  ok   labeller: own restricted namespace, node get/list/patch and namespace get only, read-only DaemonSets and pods per namespace in scope (or cluster-wide with -A), its status ConfigMap, part=admin, tools image")
PYEOF
PASSED=$((PASSED + 1))
if bash deploy/weights.sh labeller -n ns1,scaling-manager-weights-labeller --image "$TOOLS" --dry-run >/dev/null 2>&1; then fail "the labeller must refuse a namespace in scope as its own"; else ok "the labeller refuses to run in a namespace whose downloaders it trusts"; fi
if bash deploy/weights.sh labeller -n ns1 --dry-run >/dev/null 2>&1; then fail "the labeller must refuse no --image"; else ok "the labeller refuses no --image"; fi
bash deploy/weights.sh labeller -n ns1,ns2 --delete --dry-run > "$T/del.out" 2>&1
grep -q 'would run: kubectl delete namespace scaling-manager-weights-labeller' "$T/del.out" && grep -q 'would run: kubectl delete rolebinding -n ns2 scaling-manager-weights-labeller-reader' "$T/del.out" \
    && grep -q 'would run: kubectl delete clusterrole scaling-manager-weights-labeller scaling-manager-weights-labeller-reader' "$T/del.out"
expect $? "labeller --delete removes its namespace, cluster roles and bindings, and the RoleBinding in each namespace"
bash deploy/weights.sh labeller --delete --dry-run > "$T/del0.out" 2>&1; rc=$?
[ "$rc" -eq 0 ] && grep -q 'would run: kubectl delete rolebinding -A -l app.kubernetes.io/name=weights-labeller,app.kubernetes.io/managed-by=scaling-manager-weights --ignore-not-found' "$T/del0.out" \
    && grep -q 'would run: kubectl delete namespace scaling-manager-weights-labeller' "$T/del0.out"
expect $? "labeller --delete with no scope (the whole-cluster teardown) finds its RoleBindings by their labels in every namespace"

# --add and --remove: one namespace in or out of the running labeller, with
# namespaced rights. The live Deployment is the one rendered above.
yq -o json 'select(.kind == "Deployment")' "$T/lab.yaml" > "$T/live.json"
yq -o json 'select(.kind == "Deployment") | .spec.template.spec.containers[0].args = ["weights", "label", "-A", "--watch", "30"]' "$T/lab.yaml" > "$T/live-all.json"
yq -o json 'select(.kind == "Deployment") | .spec.template.spec.containers[0].args[3] = "ns1"' "$T/lab.yaml" > "$T/live-one.json"
# runlab ARGS...: weights.sh labeller ARGS against the stub; rc in LRC
runlab() { : > "$T/calls"; rm -f "$T/applied" "$T/deleted"; LRC=0; PATH="$SP" bash deploy/weights.sh labeller "$@" > "$T/lab.out" 2>&1 || LRC=$?; }
largs() { yq -o json -I0 'select(.kind == "Deployment") | .spec.template.spec.containers[0].args' "$T/applied"; }
STUB_LABELLER="$T/live.json" runlab --remove -n ns2
[ "$LRC" -eq 0 ] && [ "$(largs)" = '["weights","label","-n","ns1","--watch","30","--label-prefix","llm-d.ai"]' ] \
    && [ "$(yq -r 'select(.kind == "Deployment") | .spec.template.spec.containers[0].image' "$T/applied")" = "$TOOLS" ] \
    && ! grep -q RoleBinding "$T/applied" && grep -q '^delete rolebinding -n ns2 scaling-manager-weights-labeller-reader' "$T/deleted" \
    && [ "$(grep -n '^apply' "$T/calls" | cut -d: -f1)" -lt "$(grep -n '^delete rolebinding' "$T/calls" | cut -d: -f1)" ] \
    && grep -q "Removed ns2 from the weights labeller" "$T/lab.out"
expect $? "--remove -n ns2: the labeller is re-applied watching ns1 alone (image kept), then ns2's RoleBinding goes, in that order (rc $LRC)"
STUB_LABELLER="$T/live.json" runlab --remove -n ns3
[ "$LRC" -eq 0 ] && [ ! -e "$T/applied" ] && [ ! -e "$T/deleted" ] && grep -q 'does not watch ns3' "$T/lab.out"
expect $? "--remove of a namespace it does not watch: 0, nothing changed (rc $LRC)"
STUB_LABELLER="$T/live-one.json" runlab --remove -n ns1
[ "$LRC" -eq 3 ] && [ ! -e "$T/applied" ] && grep -q 'ns1 is the only namespace' "$T/lab.out" && grep -q 'smt weights labeller --delete' "$T/lab.out"
expect $? "--remove of its only namespace: 3, nothing changed, and the whole-cluster teardown is named (rc $LRC)"
STUB_LABELLER="$T/live-all.json" runlab --remove -n ns1
[ "$LRC" -eq 3 ] && [ ! -e "$T/applied" ] && grep -q 'watches every namespace (-A); --remove cannot leave one out' "$T/lab.out"
expect $? "--remove from a labeller with -A: 3, nothing changed (rc $LRC)"
STUB_LABELLER="$T/live.json" STUB_CANI_NO="delete/ns2" runlab --remove -n ns2
[ "$LRC" -eq 3 ] && [ ! -e "$T/applied" ] && [ ! -e "$T/deleted" ] && grep -q 'needs delete rolebindings in ns2 and patch deployments' "$T/lab.out"
expect $? "--remove without delete rolebindings in the namespace: 3, nothing changed, what it needs said (rc $LRC)"
runlab --remove -n ns2
[ "$LRC" -eq 0 ] && [ ! -e "$T/applied" ] && grep -q 'No weights labeller runs' "$T/lab.out"
expect $? "--remove with no labeller: 0, nothing to do (rc $LRC)"
STUB_LABELLER="$T/live.json" runlab --remove -n ns2 --dry-run
[ "$LRC" -eq 0 ] && [ ! -e "$T/applied" ] && [ ! -e "$T/deleted" ] && grep -q '"ns1"\|- ns1' "$T/lab.out" && grep -q '# then: kubectl delete rolebinding -n ns2' "$T/lab.out"
expect $? "--remove --dry-run prints the Deployment and the delete, and changes nothing (rc $LRC)"
for args in "--remove --add -n ns1" "--remove -A" "--remove -n ns1,ns2" "--remove -n ns1 --image x" "--remove -n scaling-manager-weights-labeller"; do
    # shellcheck disable=SC2086
    runlab $args
    [ "$LRC" -ne 0 ] && [ ! -e "$T/applied" ] || { fail "labeller $args must be refused (rc $LRC)"; LRC=x; break; }
done
[ "$LRC" != x ] && ok "--remove refuses --add with it, -A, two namespaces, --image, and the labeller's own namespace"
# --add, the cases the namespaced admin-setup meets.
STUB_LABELLER="$T/live-one.json" runlab --add -n ns2
[ "$LRC" -eq 0 ] && [ "$(largs)" = '["weights","label","-n","ns1,ns2","--watch","30","--label-prefix","llm-d.ai"]' ] \
    && [ "$(yq -r 'select(.kind == "RoleBinding") | .metadata.namespace' "$T/applied")" = ns2 ]
expect $? "--add -n ns2: the RoleBinding in ns2 and the list ns1,ns2 (rc $LRC)"
STUB_LABELLER="$T/live.json" runlab --add -n ns2
[ "$LRC" -eq 0 ] && [ ! -e "$T/applied" ] && grep -q 'already watches ns2' "$T/lab.out"
expect $? "--add of a namespace already watched: 0, no apply (rc $LRC)"
runlab --add -n ns2
[ "$LRC" -eq 4 ] && grep -q 'No weights labeller runs' "$T/lab.out"
expect $? "--add with no labeller: 4, and how to add one (rc $LRC)"
STUB_LABELLER_FORBIDDEN=1 runlab --add -n ns2
[ "$LRC" -eq 3 ] && [ ! -e "$T/applied" ] && grep -q 'Cannot read the weights labeller' "$T/lab.out" && grep -q 'Forbidden' "$T/lab.out"
expect $? "--add when the labeller cannot be read: 3, kubectl's reason shown, nothing applied (rc $LRC)"
STUB_LABELLER="$T/live-one.json" STUB_APPLY_FAILS=1 runlab --add -n ns2
[ "$LRC" -eq 3 ] && grep -q 'The API server refused the change' "$T/lab.out"
expect $? "--add the API server refuses: 3 (rc $LRC)"
STUB_LABELLER="$T/live-one.json" STUB_CANI_NO="patch/scaling-manager-weights-labeller" runlab --add -n ns2
[ "$LRC" -eq 3 ] && [ ! -e "$T/applied" ] && grep -q 'smt weights labeller --add -n ns2' "$T/lab.out"
expect $? "--add without patch deployments in the labeller namespace: 3, the command printed (rc $LRC)"
STUB_LABELLER_FORBIDDEN=1 runlab -n ns1 --image "$TOOLS"
[ "$LRC" -ne 0 ] && [ ! -e "$T/applied" ] && grep -q 'cannot read the running labeller' "$T/lab.out"
expect $? "a plain apply when the running labeller cannot be read changes nothing (rc $LRC)"

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
    OKS=0
    say_ok()   { printf '  ok   %s\n' "$1"; OKS=$((OKS + 1)); }
    say_fail() { printf '  FAIL %s\n' "$1"; FAILS=1; }
    PVC_FORBIDDEN=""
    LWS=""
    PVCS='{"items":[
      {"metadata":{"name":"weights-qwen3-32b-1a2b3c4d","annotations":{"scaling-manager.llmd.ai/weights-model":"Qwen/Qwen3-32B","scaling-manager.llmd.ai/weights-node-label":"llm-d.ai/qwen3-32b-cache"}},
       "spec":{"storageClassName":"node-local-weights","accessModes":["ReadWriteMany"]},"status":{"phase":"Bound"}},
      {"metadata":{"name":"shared-models"},"spec":{"storageClassName":"cephfs","accessModes":["ReadWriteMany"]},"status":{"phase":"Bound"}}]}'
    OBJ=""
    kubectl() {
        case "$1 $2" in
            "get deployments") printf '%s' "$OBJ" ;;
            "get leaderworkersets") printf '%s' "$LWS" ;;
            "get pvc")
                [ -z "$PVC_FORBIDDEN" ] || { echo 'Error from server (Forbidden): persistentvolumeclaims is forbidden: User "owner" cannot list resource "persistentvolumeclaims" in the namespace "ns1"' >&2; return 1; }
                case "${3:-}" in -n) printf '%s' "$PVCS" ;; *) return 1 ;; esac ;;
            "get pods"|"get replicasets") printf '{"items":[]}' ;;
            *) return 0 ;;
        esac
    }
    # dep: a model server with every other half already in place (drain,
    # engine caches, bytecode, the JIT caches beyond vLLM's own), so the
    # weights gate is the only thing a document can carry and an empty one
    # means no gate.
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
                          env: [{name: "VLLM_CACHE_ROOT", value: "/ec/vllm"}, {name: "PYTHONPYCACHEPREFIX", value: "/ec/pyc"},
                                {name: "DG_JIT_CACHE_DIR", value: "/ec/deepgemm"}, {name: "TRTLLM_DG_CACHE_DIR", value: "/ec/trtllm-deepgemm"},
                                {name: "TORCHINDUCTOR_CACHE_DIR", value: "/ec/inductor"}, {name: "CUDA_CACHE_PATH", value: "/ec/cuda"}],
                          volumeMounts: ([{name: "model-storage", mountPath: $mount} + (if $sub == "" then {} else {subPath: $sub} end)]
                                         + [{name: "ec", mountPath: "/ec"}])}]} + $extra)}}}'
    }
    # doc_of: the document; what it said on the console is in $T/doc.err.
    doc_of() { so_workload_patch_doc ns1 deployments w '["spec","template"]' 2>"$T/doc.err"; }
    # read_ok: the last doc_of read its object (an empty document is then a
    # verdict, not a stub that answered nothing).
    read_ok() { ! grep -qi 'could not read\|could not be parsed' "$T/doc.err"; }
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
    [ -z "$d" ] && read_ok && grep -q 'with subPath elsewhere, which does not lead to models/Qwen/Qwen3-32B; the weights gate is left out' "$T/doc.err" \
        && say_ok "a subPath that does not lead to the model: no gate, said so (and nothing else to do: no document)" || say_fail "unrelated subPath: doc '$d', said '$(cat "$T/doc.err")'"

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
    # a preference that names the key beside a zone keeps the zone, at its weight
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "" '{"affinity":{"nodeAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[
        {"weight":10,"preference":{"matchExpressions":[{"key":"zone","operator":"In","values":["a"]},{"key":"llm-d.ai/qwen3-32b-cache","operator":"Exists"}]}}]}}}')"
    s="$(spec_of "$(doc_of)")"
    jq -e '.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution
           == [{weight: 10, preference: {matchExpressions: [{key: "zone", operator: "In", values: ["a"]}]}},
               {weight: 100, preference: {matchExpressions: [{key: "llm-d.ai/qwen3-32b-cache", operator: "In", values: ["ready"]}]}}]' <<< "$s" >/dev/null \
        && say_ok "preferred: a term naming the key loses that expression only; its zone stays at weight 10" || say_fail "preferred, shared term: $(jq -c .affinity <<< "$s")"
    # a mount path YAML would misread unquoted reaches the init container as it is
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d '/model: #x' "")"
    s="$(spec_of "$(doc_of)")"
    [ "$(jq -r '.initContainers[0].volumeMounts[0].mountPath' <<< "$s")" = '/model: #x' ] \
        && [ "$(jq -r '.initContainers[0].env[] | select(.name == "WEIGHTS_DIR") | .value' <<< "$s")" = '/model: #x/models/Qwen/Qwen3-32B' ] \
        && say_ok "the mount path and the directory are quoted: ': #' survives" || say_fail "quoting: $(jq -c .initContainers <<< "$s")"
    # a claim whose model annotation is not a Hugging Face id: no gate (it
    # would be written into the init container's shell script)
    all_pvcs="$PVCS"
    PVCS="$(jq -c '.items[0].metadata.annotations["scaling-manager.llmd.ai/weights-model"] = "Qwen/x\"; touch /tmp/pwned; \""' <<< "$all_pvcs")"
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "")"
    d="$(doc_of)"
    w="$(so_workload_patch_doc ns1 deployments w '["spec","template"]' 2>&1 >/dev/null)"
    PVCS="$all_pvcs"
    [ -z "$d" ] && case "$w" in *"which is not a Hugging Face id; the weights gate is left out"*) true ;; *) false ;; esac \
        && say_ok "a model annotation that is not a Hugging Face id gets no gate, and says so" || say_fail "bad model annotation: doc '$d' warning '$w'"
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "")"
    s="$(LLMSM_WEIGHTS_NODE_AFFINITY=none so_workload_patch_doc ns1 deployments w '["spec","template"]' 2>/dev/null | yq -o json -I0 'select(.kind == "Deployment") | .spec.template.spec')"
    jq -e '.affinity == null and .initContainers[0].name == "wait-for-weights"' <<< "$s" >/dev/null \
        && say_ok "none: no affinity, the init container still" || say_fail "none: $s"
    # already gated: nothing
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "" '{"initContainers":[{"name":"wait-for-weights","image":"x"}],"affinity":{"nodeAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[
        {"weight":100,"preference":{"matchExpressions":[{"key":"llm-d.ai/qwen3-32b-cache","operator":"In","values":["ready"]}]}}]}}}')"
    d="$(doc_of)"
    [ -z "$d" ] && read_ok && ! grep -q 'with no weights gate' "$T/doc.err" \
        && say_ok "a template already gated gets no document" || say_fail "already gated: doc '$d', said '$(cat "$T/doc.err")'"
    # Half gated: only the missing half is written, never a second init container.
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "" '{"initContainers":[{"name":"wait-for-weights","image":"mine"}]}')"
    s="$(spec_of "$(doc_of)")"
    jq -e '.initContainers == null and (.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution | length) == 1' <<< "$s" >/dev/null \
        && say_ok "an init container but no affinity: the affinity alone is written (the owner's init container is not overwritten)" || say_fail "init, no affinity: $s"
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "" '{"affinity":{"nodeAffinity":{"preferredDuringSchedulingIgnoredDuringExecution":[
        {"weight":100,"preference":{"matchExpressions":[{"key":"__Q__","operator":"In","values":["ready"]}]}}]}}}')"; OBJ="${OBJ//__Q__/llm-d.ai/qwen3-32b-cache}"
    s="$(spec_of "$(doc_of)")"
    jq -e '.affinity == null and (.initContainers | length) == 1 and .initContainers[0].name == "wait-for-weights"' <<< "$s" >/dev/null \
        && say_ok "an affinity but no init container: the init container alone is written" || say_fail "affinity, no init: $s"
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "" '{"nodeSelector":{"__Q__":"ready"}}')"; OBJ="${OBJ//__Q__/llm-d.ai/qwen3-32b-cache}"
    s="$(spec_of "$(doc_of)")"
    jq -e '.affinity == null and (.initContainers | length) == 1' <<< "$s" >/dev/null \
        && say_ok "a nodeSelector on the label counts as the affinity: the init container alone is written" || say_fail "nodeSelector: $s"
    # required: a term naming the key with another operator has it replaced;
    # a term of matchFields keeps them and gains the label.
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "" '{"affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[
        {"matchExpressions":[{"key":"__Q__","operator":"Exists"},{"key":"zone","operator":"In","values":["a"]}]},
        {"matchFields":[{"key":"metadata.name","operator":"In","values":["n1"]}]}]}}}}')"; OBJ="${OBJ//__Q__/llm-d.ai/qwen3-32b-cache}"
    s="$(LLMSM_WEIGHTS_NODE_AFFINITY=required so_workload_patch_doc ns1 deployments w '["spec","template"]' 2>/dev/null | yq -o json -I0 'select(.kind == "Deployment") | .spec.template.spec')"
    jq -e --arg q 'llm-d.ai/qwen3-32b-cache' '.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms
           == [{matchExpressions: [{key: "zone", operator: "In", values: ["a"]}, {key: $q, operator: "In", values: ["ready"]}]},
               {matchFields: [{key: "metadata.name", operator: "In", values: ["n1"]}], matchExpressions: [{key: $q, operator: "In", values: ["ready"]}]}]' <<< "$s" >/dev/null \
        && say_ok "required: a term with the key under another operator gets In ready instead; a matchFields term keeps its fields and gains the label" || say_fail "required, odd terms: $(jq -c .affinity <<< "$s")"
    # a shared claim (not node-local): no gate
    OBJ="$(dep shared-models /model-cache "")"
    d="$(doc_of)"
    [ -z "$d" ] && read_ok && ! grep -q 'weights gate\|node-local' "$T/doc.err" \
        && say_ok "a shared claim needs no gate: every node reads the same volume" || say_fail "shared claim: doc '$d', said '$(cat "$T/doc.err")'"
    # The claims cannot be read: no gate, and the reason, never silence.
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "")"
    PVC_FORBIDDEN=1
    d="$(doc_of)"
    PVC_FORBIDDEN=""
    ! grep -q 'wait-for-weights' <<< "$d" \
        && grep -q 'ns1/w: could not read the PersistentVolumeClaims in ns1 (Error from server (Forbidden): persistentvolumeclaims is forbidden.*no weights gate is written. Forbidden is not absent' "$T/doc.err" \
        && say_ok "claims that cannot be read (Forbidden): no gate, and a warning naming what was not read and why" || say_fail "Forbidden claims: doc '$d', said '$(cat "$T/doc.err")'"
    # Settings and annotations that are not what weights.sh writes: no gate, said so.
    LLMSM_WEIGHTS_NODE_AFFINITY=sometimes doc_of > "$T/doc.out"
    [ ! -s "$T/doc.out" ] && grep -q "LLMSM_WEIGHTS_NODE_AFFINITY='sometimes' is not preferred, required or none; the weights gate is left out" "$T/doc.err" \
        && say_ok "LLMSM_WEIGHTS_NODE_AFFINITY that is not preferred, required or none: no gate, said so" || say_fail "bad affinity mode: $(cat "$T/doc.err")"
    bad=""
    for key in a/b/c llm-d.ai/ 'llm-d.ai/x y' 'llm-d.ai/x$y'; do
        PVCS="$(jq -c --arg k "$key" '.items[0].metadata.annotations["scaling-manager.llmd.ai/weights-node-label"] = $k' <<< "$all_pvcs")"
        d="$(doc_of)"
        { [ -z "$d" ] && grep -qF "names node label '$key', which is not a label key; the weights gate is left out" "$T/doc.err"; } || bad="$bad [$key]"
    done
    for model in Qwen/../etc /Qwen/Qwen3-32B Qwen/Qwen3-32B/ a/b/c; do
        PVCS="$(jq -c --arg m "$model" '.items[0].metadata.annotations["scaling-manager.llmd.ai/weights-model"] = $m' <<< "$all_pvcs")"
        d="$(doc_of)"
        { [ -z "$d" ] && grep -qF "names model '$model', which is not a Hugging Face id" "$T/doc.err"; } || bad="$bad [$model]"
    done
    PVCS="$all_pvcs"
    [ -z "$bad" ] && say_ok "node-label annotations (a/b/c, a trailing slash, a space, a dollar) and model ids (.., a leading or trailing slash, two slashes) are refused, each said" \
        || say_fail "annotations let through or not said:$bad"
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache/ "models/Qwen/Qwen3-32B/")"
    s="$(spec_of "$(doc_of)")"
    [ "$(jq -r '.initContainers[0].env[] | select(.name == "WEIGHTS_DIR") | .value' <<< "$s")" = /model-cache ] \
        && say_ok "trailing slashes on the mount and the subPath are trimmed: the directory is the mount" || say_fail "trailing slashes: $(jq -c .initContainers <<< "$s")"
    # A LeaderWorkerSet gets no gate, and is told so per template.
    LWS="$(jq -cn --argjson t "$(dep weights-qwen3-32b-1a2b3c4d /model-cache "" | jq -c '.spec.template')" \
        '{spec: {leaderWorkerTemplate: {leaderTemplate: $t, workerTemplate: $t}}}')"
    so_workload_patch_doc ns1 leaderworkersets lws '["spec","leaderWorkerTemplate","leaderTemplate"]' > "$T/doc.out" 2>"$T/doc.err"
    LWS=""
    ! grep -q 'wait-for-weights' "$T/doc.out" \
        && grep -q 'LeaderWorkerSet ns1/lws (leaderTemplate) reads Qwen/Qwen3-32B from the node-local claim weights-qwen3-32b-1a2b3c4d, and no weights gate is written for a LeaderWorkerSet' "$T/doc.err" \
        && grep -q 'LeaderWorkerSet ns1/lws (workerTemplate) reads Qwen/Qwen3-32B' "$T/doc.err" \
        && say_ok "a LeaderWorkerSet on a node-local claim: no gate, and each template is told it has none" || say_fail "LWS: said '$(cat "$T/doc.err")'"
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

    # The init container's wait, run: it passes on a complete copy, and on
    # none it gives up after WEIGHTS_WAIT_SECONDS, exits 1 and says why.
    OBJ="$(dep weights-qwen3-32b-1a2b3c4d /model-cache "")"
    s="$(spec_of "$(doc_of)")"
    jq -r '.initContainers[0].args[0]' <<< "$s" > "$T/wait.sh"
    [ "$(jq -r '.initContainers[0].env[] | select(.name == "WEIGHTS_WAIT_SECONDS") | .value' <<< "$s")" = 3600 ] \
        && say_ok "the wait defaults to 3600 s (60 min)" || say_fail "default wait: $(jq -c '.initContainers[0].env' <<< "$s")"
    s2="$(LLMSM_WEIGHTS_WAIT_TIMEOUT=120 so_workload_patch_doc ns1 deployments w '["spec","template"]' 2>/dev/null | yq -o json -I0 'select(.kind == "Deployment") | .spec.template.spec')"
    [ "$(jq -r '.initContainers[0].env[] | select(.name == "WEIGHTS_WAIT_SECONDS") | .value' <<< "$s2")" = 120 ] \
        && say_ok "LLMSM_WEIGHTS_WAIT_TIMEOUT sets it" || say_fail "LLMSM_WEIGHTS_WAIT_TIMEOUT=120: $(jq -c '.initContainers[0].env' <<< "$s2")"
    w="$(LLMSM_WEIGHTS_WAIT_TIMEOUT=1h so_workload_patch_doc ns1 deployments w '["spec","template"]' 2>&1 >/dev/null)"
    case "$w" in *"LLMSM_WEIGHTS_WAIT_TIMEOUT='1h' is not a number of seconds"*) say_ok "a timeout that is not seconds is refused with a warning, and the default kept" ;; *) say_fail "bad timeout: $w" ;; esac
    bad=""
    for tv in 0 00 -5 "" 010; do
        s2="$(LLMSM_WEIGHTS_WAIT_TIMEOUT="$tv" so_workload_patch_doc ns1 deployments w '["spec","template"]' 2>"$T/doc.err" | yq -o json -I0 'select(.kind == "Deployment") | .spec.template.spec')"
        got="$(jq -r '.initContainers[0].env[] | select(.name == "WEIGHTS_WAIT_SECONDS") | .value' <<< "$s2")"
        case "$tv" in
            "") want=3600; grep -q 'is not a number' "$T/doc.err" && bad="$bad [empty warned]" ;;
            010) want=10 ;;
            *) want=3600; grep -q "LLMSM_WEIGHTS_WAIT_TIMEOUT='$tv' is not a number of seconds above 0" "$T/doc.err" || bad="$bad [$tv not warned]" ;;
        esac
        [ "$got" = "$want" ] || bad="$bad [$tv -> $got, want $want]"
    done
    [ -z "$bad" ] && say_ok "timeouts 0, 00, -5: warned, 3600 kept; empty: 3600 unwarned (the default); 010 is ten seconds, not octal" || say_fail "timeouts:$bad"
    grep -q '\$(' "$T/wait.sh" && say_fail "the wait script carries \$( (Kubernetes rewrites it)" || say_ok "the wait script carries no \$( for Kubernetes to rewrite"
    WEIGHTS_DIR="$T/m/ok" WEIGHTS_WAIT_SECONDS=0 timeout 5 sh "$T/wait.sh" >/dev/null 2>&1 \
        && say_ok "a complete copy passes at once, even with no time left" || say_fail "the wait failed on a complete copy"
    WEIGHTS_DIR="$T/m/nomarker" WEIGHTS_WAIT_SECONDS=0 timeout 5 sh "$T/wait.sh" > "$T/wait.out" 2>&1; rc=$?
    [ "$rc" -eq 1 ] && grep -q "wait-for-weights: gave up after 0s: this node has no complete copy of Qwen/Qwen3-32B at $T/m/nomarker" "$T/wait.out" \
        && grep -q 'weights status -n ns1' "$T/wait.out" \
        && say_ok "a copy still being written (no marker): after the timeout it exits 1, naming the model, the directory and the status command" \
        || say_fail "timeout: rc $rc: $(cat "$T/wait.out")"
    # The wait checks the marker and a config, not the sizes: a shard
    # truncated under its marker passes here. What catches it is the
    # downloader's probe (the node label goes) and its re-check (the marker
    # goes, so the next replica waits). Pinned, so a change is a decision.
    WEIGHTS_DIR="$T/m/short" WEIGHTS_WAIT_SECONDS=0 timeout 5 sh "$T/wait.sh" >/dev/null 2>&1 && ! probe_rc "$T/m/short" \
        && say_ok "a shard truncated under its marker passes the wait (marker and config only) and fails the probe, which unlabels the node" \
        || say_fail "the truncated copy: the wait and the probe no longer split as documented"
    start=$SECONDS
    WEIGHTS_DIR="$T/m/absent" WEIGHTS_WAIT_SECONDS=10 timeout 30 sh "$T/wait.sh" > "$T/wait.out" 2>&1; rc=$?
    took=$((SECONDS - start))
    [ "$rc" -eq 1 ] && [ "$took" -ge 9 ] && [ "$took" -le 20 ] && grep -q '(0s of 10s' "$T/wait.out" && grep -q 'gave up after 10s' "$T/wait.out" \
        && say_ok "with 10 s it waits one round ($took s), counts it, and then gives up" || say_fail "10 s wait: rc $rc after $took s: $(cat "$T/wait.out")"

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
    # Every case above, counted: a case that stopped running is a failure.
    EXPECT_SUB=37
    [ "$OKS" -eq "$EXPECT_SUB" ] || say_fail "the workload-patch gate ran $OKS cases; expected $EXPECT_SUB"
    echo "$OKS" > "$T/sub.count"
    exit $FAILS
) || FAILED=1
PASSED=$((PASSED + $(cat "$T/sub.count" 2>/dev/null || echo 0)))

echo
if [ "$FAILED" -ne 0 ]; then
    echo "check-weights-node-gate: FAILED"
    exit 1
fi
# A run that checked less than all of it is not a pass.
EXPECT_CASES=112
[ "$PASSED" -eq "$EXPECT_CASES" ] || { echo "check-weights-node-gate: $PASSED checks passed; expected $EXPECT_CASES"; exit 1; }
echo "check-weights-node-gate: all $PASSED checks passed"
