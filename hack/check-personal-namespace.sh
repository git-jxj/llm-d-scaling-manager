#!/usr/bin/env bash
# No tracked file may point at a personal GitHub or GHCR namespace
# (ev-shindin), except the references listed below, each for a stated reason.
#
# The warm-pool proxy shipped from ghcr.io/ev-shindin/llm-scaler/warmpool-proxy
# long after the project moved to llm-d-incubation: the manifests, the deploy
# script and the kind e2e all pulled an image this repository did not build and
# could not republish. Nothing noticed, because the reference was valid and the
# image was public. This fails on any new one.
#
# Allowed, and why (each pattern is stripped from a line before it is tested,
# so a line holding an allowed reference AND another one still fails):
#
#   1. OWNERS: the line "  - ev-shindin". A GitHub login of an approver and
#      reviewer, which is what OWNERS is for.
#   2. Cross-references into the repository this one was renamed from:
#      ev-shindin/llm-scaling-manager#<n>, its pull, issues, releases/tag and
#      blob URLs, ev-shindin/llm-scaler/pull/<n>, and the heading anchors
#      markdown derives from them (-ev-shindinllm-scaling-manager<n>). They
#      record where a decision was made; those PRs, issues and v0.10.x release
#      notes exist only there and cannot be renumbered into this repository.
#      message_words.txt holds the bare word because make
#      learner-message-words generates it from the sources that cite them.
#   3. Fork artifacts with no home in this organization:
#      ghcr.io/ev-shindin/fma-launcher and dual-pods-controller (the headless
#      launcher a pool of groups needs, and the benchmark FMA controller), and
#      the forks they are built from, ev-shindin/llm-d-fast-model-actuation and
#      ev-shindin/vllm-conf. This repository does not build them, and the
#      warm-pool and P/D docs must say where they come from.
#   4. This script, which has to spell the patterns it allows.
#
# Everything else fails, notably any other ghcr.io/ev-shindin image. Before
# the scan, the filter is run on fixtures: each allowed form must pass and each
# forbidden one must fail, so a pattern widened by an edit is caught here.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SELF="hack/check-personal-namespace.sh"
NEEDLE="ev-shindin"
FAILED=0

ok()   { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; FAILED=1; }

# residue: reads git grep -n lines (path:line:text) and prints those that
# still name the namespace once every allowed reference is removed.
residue() {
    sed -E \
        -e '/^OWNERS:[0-9]+:  - ev-shindin$/d' \
        -e '/^internal\/engines\/analyzers\/learnreplay\/message_words\.txt:[0-9]+:ev-shindin\/llm-scaling-manager$/d' \
        -e 's#ev-shindin/llm-scaling-manager\#[0-9]+##g' \
        -e 's#github\.com/ev-shindin/llm-scaling-manager/(pull|issues)/[0-9]+##g' \
        -e 's#github\.com/ev-shindin/llm-scaling-manager/releases/tag/v[0-9][0-9A-Za-z.-]*##g' \
        -e 's#github\.com/ev-shindin/llm-scaling-manager/blob/[^)[:space:]]+##g' \
        -e 's#github\.com/ev-shindin/llm-scaler/pull/[0-9]+##g' \
        -e 's#-ev-shindinllm-scaling-manager[0-9]+##g' \
        -e 's#ghcr\.io/ev-shindin/(fma-launcher|dual-pods-controller)([:@][A-Za-z0-9._:-]+|[^A-Za-z0-9_/-]|$)##g' \
        -e 's#(github\.com/|[^./A-Za-z0-9_-])ev-shindin/(llm-d-fast-model-actuation|vllm-conf)([^A-Za-z0-9_-]|$)##g' \
    | grep -F "$NEEDLE"
}

# --- the filter, on fixtures ------------------------------------------------
allowed='OWNERS:12:  - ev-shindin
internal/x.go:3:// PR ev-shindin/llm-scaling-manager#64 moved it, see also pre-ev-shindin/llm-scaling-manager#219.
docs/a.md:9:[notes](https://github.com/ev-shindin/llm-scaling-manager/releases/tag/v0.10.2) and https://github.com/ev-shindin/llm-scaling-manager/pull/481
docs/b.md:1:[x](https://github.com/ev-shindin/llm-scaling-manager/blob/bench/keda-recipe-arm/docs/a.md)
hack/c.sh:5:# (https://github.com/ev-shindin/llm-scaler/pull/87)
docs/c.md:2:([as built](#rollouts-as-built-ev-shindinllm-scaling-manager481-ev-shindinllm-scaling-manager495))
docs/d.md:7:`--launcher-image ghcr.io/ev-shindin/fma-launcher:v0.6.4-headless` from [the fork](https://github.com/ev-shindin/llm-d-fast-model-actuation)
Makefile:3:#     FMA_CONTROLLER_IMAGE=ghcr.io/ev-shindin/dual-pods-controller:aa072ef
docs/e.md:4:[pd_role.py](https://github.com/ev-shindin/vllm-conf/blob/feat/pd-role-switch/vllm/v1/engine/pd_role.py)
internal/engines/analyzers/learnreplay/message_words.txt:1084:ev-shindin/llm-scaling-manager'
forbidden='config/warmpool/warmpool-deployment.yaml:394:          image: ghcr.io/ev-shindin/llm-scaler/warmpool-proxy:v11@sha256:9e3e
deploy/kind-emulator/install.sh:42:WARMPOOL_PROXY_IMAGE=${WARMPOOL_PROXY_IMAGE:-"ghcr.io/ev-shindin/warmpool-proxy:v11"}
docs/f.md:1:pull from `ghcr.io/ev-shindin`
docs/g.md:2:see https://github.com/ev-shindin/some-other-repo
internal/y.go:8:// ev-shindin/llm-scaling-manager#12 and ghcr.io/ev-shindin/new-image:v1
docs/h.md:3:  - ev-shindin
OWNERS:14:  - ev-shindin-bot
docs/i.md:5:ghcr.io/ev-shindin/fma-launcher-extra:v1
docs/j.md:6:image: ghcr.io/ev-shindin/llm-d-fast-model-actuation:v1
docs/k.md:7:https://github.com/ev-shindin/vllm-conf-evil/x'

n_allowed="$(printf '%s\n' "$allowed" | grep -c .)"
leaked="$(printf '%s\n' "$allowed" | residue)"
if [ -z "$leaked" ]; then
    ok "every allowed form passes the filter ($n_allowed fixtures)"
else
    fail "allowed references were flagged:"
    printf '%s\n' "$leaked" | sed 's/^/         /'
fi

n_forbidden="$(printf '%s\n' "$forbidden" | grep -c .)"
caught="$(printf '%s\n' "$forbidden" | residue | grep -c .)"
if [ "$caught" = "$n_forbidden" ]; then
    ok "every forbidden form is caught ($caught of $n_forbidden fixtures)"
else
    fail "only $caught of $n_forbidden forbidden fixtures were caught; these passed:"
    printf '%s\n' "$forbidden" | while IFS= read -r line; do
        [ -n "$(printf '%s\n' "$line" | residue)" ] || printf '         %s\n' "$line"
    done
fi

# --- the tracked files -------------------------------------------------------
if ! git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
    fail "$ROOT is not a git checkout; cannot list tracked files"
    exit 1
fi
hits="$(git -C "$ROOT" grep -n -I -F -e "$NEEDLE" -- . ":(exclude)$SELF")"
rc=$?
hits="$(printf '%s' "$hits" | tr -d '\r')"
if [ "$rc" -gt 1 ]; then
    fail "git grep failed (rc=$rc)"
    exit 1
fi
n_hits="$(printf '%s\n' "$hits" | grep -c .)"
n_files="$(printf '%s\n' "$hits" | grep . | cut -d: -f1 | sort -u | grep -c .)"
bad="$(printf '%s\n' "$hits" | grep . | residue)"
if [ -z "$bad" ]; then
    ok "no personal-namespace reference outside the allowed ones ($n_hits allowed lines in $n_files tracked files)"
else
    fail "$(printf '%s\n' "$bad" | grep -c .) line(s) name the personal namespace $NEEDLE (allowed forms: see the header of $SELF):"
    printf '%s\n' "$bad" | sed 's/^/         /'
fi

exit $FAILED
