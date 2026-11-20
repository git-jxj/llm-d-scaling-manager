#!/usr/bin/env bash
# Builds the capacity planner release binaries the way the release does
# (make capacity-planner-dist, every platform of CAPACITY_PLANNER_PLATFORMS,
# darwin and windows cross-compiled, the windows one named .exe) under a test
# version, and asserts each exists, was built with -trimpath, CGO off and the
# learner-commit and version ldflags, and that the one for this host prints
# the version and commit:
#
#   bash hack/check-capacity-planner-dist.sh
#
# So a broken build, platform or ldflags fails a PR rather than a tag.
# ci-pr-checks.yaml (capacity-planner-image) runs it. Needs go and git.
set -euo pipefail

cd "$(dirname "$0")/.."

V=v0.0.0-distcheck
COMMIT="$(git rev-parse HEAD)"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
FAILED=0
PASSED=0
# One ok or FAIL per case: the five platforms' build settings, the host
# binary's version line, and the windows-only files vetted and their tests
# built (no other PR check compiles them). A run that stops early fails on
# the count.
EXPECT_CASES=7
ok()   { printf '  ok   %s\n' "$1"; PASSED=$((PASSED + 1)); }
fail() { printf '  FAIL %s\n' "$1"; FAILED=$((FAILED + 1)); }

make --no-print-directory capacity-planner-dist RELEASE_VERSION="$V" DIST="$T" GIT_COMMIT="$COMMIT"

# A windows binary is named .exe, as the release attaches it.
exe() { case "$1" in windows) echo .exe ;; esac; }

for p in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
    f="$T/capacity-planner-$V-${p%/*}-${p#*/}$(exe "${p%/*}")"
    if [ ! -s "$f" ]; then fail "$p: $f was not built"; continue; fi
    info="$(go version -m "$f")"
    miss=""
    for want in "-trimpath=true" "CGO_ENABLED=0" "GOOS=${p%/*}" "GOARCH=${p#*/}"; do
        printf '%s\n' "$info" | grep -qF -- "$want" || miss="$miss $want"
    done
    # -trimpath drops -ldflags from the build settings, so the two -X values
    # are looked for in the binary itself: a cross-built one cannot run here.
    for want in "$COMMIT" "$V"; do
        grep -qaF -- "$want" "$f" || miss="$miss ldflag-value:$want"
    done
    if [ -z "$miss" ]; then ok "$p: built with -trimpath, CGO off, the commit and the version"; else fail "$p: build settings lack:$miss"; fi
done

host="$T/capacity-planner-$V-$(go env GOOS)-$(go env GOARCH)$(exe "$(go env GOOS)")"
got="$("$host" version 2>&1 || true)"
want="capacity-planner $V (commit $COMMIT)"
if [ "$got" = "$want" ]; then ok "the host binary prints: $got"; else fail "the host binary prints '$got', want '$want'"; fi

if out="$(GOOS=windows go vet ./cmd/capacity-planner 2>&1 && GOOS=windows go test -c -o /dev/null ./cmd/capacity-planner 2>&1)"; then
    ok "windows: the planner and its tests vet and build"
else
    fail "windows: the planner or its tests do not vet or build: $out"
fi

echo ""
echo "capacity planner dist: $PASSED passed, $FAILED failed"
if [ $((PASSED + FAILED)) -ne "$EXPECT_CASES" ]; then
    echo "FAIL: $((PASSED + FAILED)) case(s) ran, $EXPECT_CASES expected"
    exit 1
fi
[ "$FAILED" -eq 0 ]
