#!/usr/bin/env bash
#
# Runs deploy/ci-pr-checks/tools-pin.sh on fixture trees, then its `pr` rule on
# this tree. The fixture that matters is the v0.12.2 tag: its release commit
# pinned v0.12.1's tools digest (30e265eb...) in every install guide, so a user
# following the docs at the tag ran the release before. That tree must fail.
# Also: an empty extraction (no pin found) is a failure, never a pass, and
# `pin` rewrites a pin but never a placeholder or a variable.

set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PIN="$ROOT/deploy/ci-pr-checks/tools-pin.sh"
W="$(mktemp -d)"
trap 'rm -rf "$W"' EXIT
PASS=0
FAIL=0
ok()  { printf '  ok   %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf '  FAIL %s\n' "$1"; FAIL=$((FAIL + 1)); }

IMG=ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools
D121=sha256:30e265ebcdd7ff110faa6adb3aac92afad0a314cc28a9df12cc35e6f73d50855
D122=sha256:fc34a5e01283b5167e3e1accc0e7421ae6dccc42652d50e493a5f2a12ee59695

# tree <dir> <newTag> <ref>: a docs tree pinning tools<ref> in a guide.yaml,
# its README (CRLF, as a Windows checkout writes it) and README.md, with a
# placeholder and a variable that are no pins.
tree() {
    local d="$W/$1"
    rm -rf "$d"
    mkdir -p "$d/config/base/manager" "$d/docs/guides/g" "$d/docs/proposals"
    printf 'images:\n- name: controller\n  newTag: %s\n' "$2" > "$d/config/base/manager/kustomization.yaml"
    printf 'tools:\n  setup:\n    run: |\n      TOOLS=%s%s\n      smt version\n' "$IMG" "$3" > "$d/docs/guides/g/guide.yaml"
    printf '```bash\r\nTOOLS=%s%s\r\nsmt version\r\n```\r\n' "$IMG" "$3" > "$d/docs/guides/g/README.md"
    printf 'TOOLS=%s%s\n' "$IMG" "$3" > "$d/README.md"
    printf 'TOOLS=%s@sha256:<the release tools digest>\nTOOLS=%s:$V\n' "$IMG" "$IMG" > "$d/docs/proposals/p.md"
    printf '%s\n' "$d"
}
run() { ROOT="$1" bash "$PIN" "${@:2}" > "$W/out" 2>&1; }

echo "tools-pin.sh on fixture trees"
t="$(tree v0122 v0.12.2 "@$D121")"
run "$t" pr; rc=$?
if [ "$rc" = 1 ] && grep -q "not v0.12.2's" "$W/out" && [ "$(grep -c "@$D121" "$W/out")" = 3 ]; then
    ok "the v0.12.2 release commit, pinning v0.12.1's digest, fails pr naming all 3 pins"
else
    bad "v0.12.2 fixture: exit $rc: $(head -3 "$W/out" | tr '\n' ' ')"
fi
run "$t" check v0.12.2 "$D122"; rc=$?
if [ "$rc" = 1 ]; then ok "and fails check v0.12.2 with v0.12.2's own digest"; else bad "check with v0.12.2's digest: exit $rc"; fi
t="$(tree rel v0.12.3 :v0.12.3)"
run "$t" pr; rc=$?
if [ "$rc" = 0 ] && grep -q '3 pins name v0.12.3' "$W/out"; then ok "a release commit pinning its own tag passes"; else bad "own tag: exit $rc: $(cat "$W/out")"; fi
run "$t" check v0.12.4; rc=$?
if [ "$rc" = 1 ]; then ok "check of another version fails"; else bad "other version: exit $rc"; fi
t="$(tree main122 main "@$D122")"
run "$t" pr; rc=$?
if [ "$rc" = 0 ]; then ok "main pinning a digest everywhere passes"; else bad "main digest: exit $rc: $(cat "$W/out")"; fi
run "$t" check v0.12.2 "$D122"; rc=$?
if [ "$rc" = 0 ]; then ok "check v0.12.2 with its digest passes after the docs PR"; else bad "check with digest: exit $rc"; fi
t="$(tree mainmain main :main)"
run "$t" pr; rc=$?
if [ "$rc" = 1 ] && grep -q 'not a release' "$W/out"; then ok "main pinning tools:main fails"; else bad "tools:main: exit $rc"; fi
t="$(tree mixed main "@$D122")"
printf 'TOOLS=%s@%s\n' "$IMG" "$D121" > "$t/README.md"
run "$t" pr; rc=$?
if [ "$rc" = 1 ] && grep -q '2 different tools images' "$W/out"; then ok "two releases' images in one tree fail"; else bad "mixed: exit $rc: $(head -2 "$W/out")"; fi
t="$(tree none main "@$D122")"
rm -f "$t/README.md" "$t/docs/guides/g/README.md" "$t/docs/guides/g/guide.yaml"
run "$t" pr; rc=$?
if [ "$rc" = 1 ] && grep -q 'nothing to check is not a pass' "$W/out"; then ok "no pin found is a failure, not a pass"; else bad "empty extraction: exit $rc"; fi

echo "tools-pin.sh pin"
t="$(tree pin v0.12.3 "@$D122")"
cp "$t/docs/proposals/p.md" "$W/p.before"
run "$t" pin :v0.12.3; rc=$?
if [ "$rc" = 0 ] && ROOT="$t" bash "$PIN" check v0.12.3 > /dev/null 2>&1 && cmp -s "$W/p.before" "$t/docs/proposals/p.md" \
    && [ "$(tr -cd '\r' < "$t/docs/guides/g/README.md" | wc -c)" = 4 ]; then
    ok "pin :v0.12.3 moves every pin, keeps the CRLF line ends, and leaves the placeholder and the variable"
else
    bad "pin: exit $rc: $(cat "$W/out")"
fi
run "$t" pin "@$D122"; rc=$?
if [ "$rc" = 0 ] && ROOT="$t" bash "$PIN" check v0.12.3 "$D122" > /dev/null 2>&1; then ok "pin @<digest> moves them to the digest"; else bad "pin digest: exit $rc"; fi
for r in :main @sha256:abc v0.12.3; do
    run "$t" pin "$r"; rc=$?
    if [ "$rc" = 2 ]; then ok "pin $r is usage"; else bad "pin $r: exit $rc"; fi
done

echo "this tree"
if ROOT="$ROOT" bash "$PIN" pr; then ok "this tree's pins follow its newTag"; else bad "this tree: see above"; fi

EXPECT_CASES=15
echo "tools pin: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
[ "$PASS" -eq "$EXPECT_CASES" ] || { echo "FAIL ran $PASS cases, expected $EXPECT_CASES"; exit 1; }
