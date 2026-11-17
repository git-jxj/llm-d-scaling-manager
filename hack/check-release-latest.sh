#!/usr/bin/env bash
#
# Runs the release workflow's "Resolve image, version and tags" step against a
# scratch git repository and asserts when it moves `latest`.
#
# Only the highest final vMAJOR.MINOR.PATCH tag may move it. Before this, every
# final tag did, so a backport (v1.2.1, tagged after v1.3.0) moved `latest` back
# to the older line. Nothing else can see that: the step is valid shell, and a
# release run looks the same either way.
#
# The step's own script is read out of .github/workflows/ci-release.yaml, so
# this tests what the workflow runs, not a copy. Needs git and yq.

set -u

ROOT="${CHECK_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
WORKFLOW="$ROOT/.github/workflows/ci-release.yaml"
# One ok or FAIL per case. A run that stops early otherwise passes having
# checked less than it says. Change this deliberately with the cases.
EXPECT_CASES=55
PASS=0
FAIL=0

ok()   { PASS=$((PASS + 1)); echo "ok   $1"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL $1"; [ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/       /'; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

tr -d '\r' < "$WORKFLOW" > "$WORK/release.yaml"
yq '.jobs.release.steps[] | select(.id == "meta") | .run' "$WORK/release.yaml" > "$WORK/meta.sh"
if ! grep -q 'GITHUB_OUTPUT' "$WORK/meta.sh"; then
    echo "FAIL could not read the meta step out of $WORKFLOW"
    exit 1
fi

# The checkout must bring the tags the step compares against.
depth="$(yq '.jobs.release.steps[] | select(.uses == "actions/checkout*") | .with."fetch-depth"' "$WORK/release.yaml")"
if [ "$depth" = 0 ]; then
    ok "the release checkout fetches full history and tags (fetch-depth: 0)"
else
    fail "the release checkout must use fetch-depth: 0, or the step sees no other tag (got '$depth')"
fi

# resolve <ref> <tag>... -- a repository holding those tags, the step run for
# <ref>; leaves its outputs in $out and its log in $log.
resolve() {
    local ref="$1" repo="$WORK/repo" t
    shift
    rm -rf "$repo"
    git init -q "$repo"
    git -C "$repo" -c user.name=check -c user.email=check@example.invalid commit -q --allow-empty -m base
    for t in "$@"; do
        git -C "$repo" tag "$t"
    done
    : > "$WORK/out"
    log="$(cd "$repo" && env REF_NAME="$ref" GITHUB_OUTPUT="$WORK/out" \
        GITHUB_REPOSITORY_OWNER=Owner GITHUB_REPOSITORY=Owner/repo bash "$WORK/meta.sh" 2>&1)"
    rc=$?
    out="$(cat "$WORK/out")"
}
tags() { printf '%s\n' "$out" | sed -n 's/^tags=//p'; }

resolve v1.3.0 v1.2.0 v1.3.0
if [ "$rc" -eq 0 ] && [ "$(tags)" = "ghcr.io/owner/repo:v1.3.0,ghcr.io/owner/repo:latest" ]; then
    ok "the highest final tag moves latest"
else
    fail "v1.3.0, the highest final tag, must move latest (rc=$rc)" "$log"
fi

resolve v1.2.1 v1.2.0 v1.3.0 v1.2.1
if [ "$rc" -eq 0 ] && [ "$(tags)" = "ghcr.io/owner/repo:v1.2.1" ]; then
    ok "a backport (v1.2.1 after v1.3.0) does not move latest"
else
    fail "a backport must not move latest back (rc=$rc, tags=$(tags))" "$log"
fi

resolve v1.4.0-rc1 v1.3.0 v1.4.0-rc1
if [ "$rc" -eq 0 ] && [ "$(tags)" = "ghcr.io/owner/repo:v1.4.0-rc1" ] \
    && printf '%s\n' "$out" | grep -qx 'prerelease=true'; then
    ok "a prerelease does not move latest"
else
    fail "a prerelease must not move latest (rc=$rc, tags=$(tags))" "$log"
fi

resolve v1.3.0 v1.3.0 v1.4.0-rc1
if [ "$rc" -eq 0 ] && [ "$(tags)" = "ghcr.io/owner/repo:v1.3.0,ghcr.io/owner/repo:latest" ]; then
    ok "a newer prerelease does not stop the highest final tag moving latest"
else
    fail "v1.3.0 is the highest FINAL tag beside v1.4.0-rc1, and must move latest (rc=$rc)" "$log"
fi

resolve v1.10.0 v1.9.0 v1.10.0
if [ "$rc" -eq 0 ] && [ "$(tags)" = "ghcr.io/owner/repo:v1.10.0,ghcr.io/owner/repo:latest" ]; then
    ok "versions compare as versions: v1.10.0 is above v1.9.0"
else
    fail "v1.10.0 must sort above v1.9.0 and move latest (rc=$rc)" "$log"
fi

resolve v1.3.0
if [ "$rc" -ne 0 ] && printf '%s\n' "$log" | grep -q 'tags were not fetched'; then
    ok "a checkout with no tags refuses, rather than guessing"
else
    fail "with no tags fetched the step must refuse (rc=$rc, tags=$(tags))" "$log"
fi

# One concurrency group for every release. Per tag, two releases ran side by
# side, and the older one could finish last and move latest back.
group="$(yq '.concurrency.group' "$WORK/release.yaml")"
cancel="$(yq '.concurrency."cancel-in-progress"' "$WORK/release.yaml")"
case "$group" in
    *'${{'*|''|null) fail "the release concurrency group must be one fixed name for every run (got '$group')" ;;
    *) if [ "$cancel" = false ]; then
           ok "releases share one concurrency group ($group) and never cancel a running one"
       else
           fail "cancel-in-progress must be false, or a running release is killed mid-push (got '$cancel')"
       fi ;;
esac

# Only the built-in token logs in to the registry.
password="$(yq '.jobs.release.steps[] | select(.uses == "docker/login-action*") | .with.password' "$WORK/release.yaml")"
if [ "$password" = '${{ secrets.GITHUB_TOKEN }}' ]; then
    ok "the registry login uses the built-in GITHUB_TOKEN alone"
else
    fail "the registry login must use secrets.GITHUB_TOKEN only (got '$password')"
fi

# Only a commit on main is released. The step runs against a clone whose
# origin has main, and a commit on a side branch.
yq '.jobs.release.steps[] | select(.id == "onmain") | .run' "$WORK/release.yaml" > "$WORK/onmain.sh"
if ! grep -q 'merge-base --is-ancestor' "$WORK/onmain.sh"; then
    fail "could not read the on-main step out of $WORKFLOW"
    fail "(the on-main step was not run)"
else
    G=(git -c user.name=check -c user.email=check@example.invalid)
    rm -rf "$WORK/origin" "$WORK/clone"
    git init -q -b main "$WORK/origin"
    "${G[@]}" -C "$WORK/origin" commit -q --allow-empty -m one
    "${G[@]}" -C "$WORK/origin" commit -q --allow-empty -m two
    git clone -q "$WORK/origin" "$WORK/clone"
    "${G[@]}" -C "$WORK/clone" checkout -q -b side
    "${G[@]}" -C "$WORK/clone" commit -q --allow-empty -m unreviewed
    main_sha="$(git -C "$WORK/origin" rev-parse HEAD~1)"
    side_sha="$(git -C "$WORK/clone" rev-parse HEAD)"
    # Without origin/main in the checkout, the step must fetch it.
    git -C "$WORK/clone" update-ref -d refs/remotes/origin/main
    onmain_log="$(cd "$WORK/clone" && env GITHUB_SHA="$main_sha" GITHUB_REF_NAME=v1.0.0 bash "$WORK/onmain.sh" 2>&1)"
    if [ $? -eq 0 ]; then
        ok "a tag on a commit of main is released (origin/main fetched when missing)"
    else
        fail "a commit on main must pass the on-main step" "$onmain_log"
    fi
    onmain_log="$(cd "$WORK/clone" && env GITHUB_SHA="$side_sha" GITHUB_REF_NAME=v1.0.1 bash "$WORK/onmain.sh" 2>&1)"
    if [ $? -ne 0 ] && printf '%s\n' "$onmain_log" | grep -q 'not on main'; then
        ok "a tag on a commit that is not on main is refused"
    else
        fail "a commit off main must be refused" "$onmain_log"
    fi
    # A patch line: release-1.0 forks from main and carries a commit main has
    # not. Only a tag of that line, on that branch, is released, and only once
    # the line has a tag on main. release-2.0 has none.
    "${G[@]}" -C "$WORK/origin" tag v1.0.0 HEAD~1
    "${G[@]}" -C "$WORK/origin" checkout -q -b release-1.0 HEAD~1
    "${G[@]}" -C "$WORK/origin" commit -q --allow-empty -m patch
    patch_sha="$(git -C "$WORK/origin" rev-parse HEAD)"
    "${G[@]}" -C "$WORK/origin" checkout -q -b release-2.0 main
    "${G[@]}" -C "$WORK/origin" commit -q --allow-empty -m unreleased
    unreleased_sha="$(git -C "$WORK/origin" rev-parse HEAD)"
    # A refused v2.0.0 stays tagged on the branch; it must not count as a release.
    "${G[@]}" -C "$WORK/origin" tag v2.0.0 HEAD
    "${G[@]}" -C "$WORK/origin" checkout -q main
    git -C "$WORK/clone" fetch -q origin
    git -C "$WORK/clone" update-ref -d refs/remotes/origin/release-1.0
    onmain_log="$(cd "$WORK/clone" && env GITHUB_SHA="$patch_sha" GITHUB_REF_NAME=v1.0.2 bash "$WORK/onmain.sh" 2>&1)"
    if [ $? -eq 0 ] && printf '%s\n' "$onmain_log" | grep -q 'is on release-1.0'; then
        ok "a v1.0.x tag on release-1.0 is released (the branch fetched when missing)"
    else
        fail "a tag of the 1.0 line on release-1.0 must pass the on-main step" "$onmain_log"
    fi
    onmain_log="$(cd "$WORK/clone" && env GITHUB_SHA="$patch_sha" GITHUB_REF_NAME=v1.1.0 bash "$WORK/onmain.sh" 2>&1)"
    if [ $? -ne 0 ] && printf '%s\n' "$onmain_log" | grep -q 'not on main or on release-1.1'; then
        ok "a tag of another line on release-1.0 is refused"
    else
        fail "a v1.1.0 tag on a release-1.0 commit must be refused" "$onmain_log"
    fi
    onmain_log="$(cd "$WORK/clone" && env GITHUB_SHA="$side_sha" GITHUB_REF_NAME=v1.0.3 bash "$WORK/onmain.sh" 2>&1)"
    if [ $? -ne 0 ] && printf '%s\n' "$onmain_log" | grep -q 'not on main or on release-1.0'; then
        ok "a v1.0.x tag on a commit on neither branch is refused"
    else
        fail "a v1.0.3 tag off both main and release-1.0 must be refused" "$onmain_log"
    fi
    onmain_log="$(cd "$WORK/clone" && env GITHUB_SHA="$unreleased_sha" GITHUB_REF_NAME=v2.0.1-rc1 bash "$WORK/onmain.sh" 2>&1)"
    if [ $? -ne 0 ] && printf '%s\n' "$onmain_log" | grep -q 'never released from main'; then
        ok "a new release-2.0 branch cannot publish (its v2.0.0 tag is not on main)"
    else
        fail "a tag on a branch whose line has no tag on main must be refused" "$onmain_log"
    fi
fi

# --- The shape of the release job. Each of these was a surviving mutant: the
# workflow still parsed, and nothing else noticed the guard was gone.
R="$WORK/release.yaml"
# expect_yq <name> <want> <yq expression>
expect_yq() {
    local got
    got="$(yq -r "$3" "$R" 2>&1)"
    if [ "$got" = "$2" ]; then ok "$1"; else fail "$1 (want [$2], got [$got])"; fi
}
expect_yq "the release runs the PR checks, unconditionally" \
    './.github/workflows/ci-pr-checks.yaml|null' '.jobs.checks.uses + "|" + (.jobs.checks.if // "null")'
expect_yq "nothing is released before the checks pass (release needs checks)" \
    'checks' '[.jobs.release.needs] | flatten | join(",")'
expect_yq "only a v* tag push triggers a release" \
    'push|v*' '(.on | keys | join(",")) + "|" + (.on.push.tags | join(","))'
expect_yq "workflow permissions are contents: read alone" \
    'contents=read' '.permissions | to_entries | map(.key + "=" + .value) | join(",")'
expect_yq "the release job may sign (id-token) and push (packages), nothing more" \
    'contents=read,id-token=write,packages=write' \
    '.jobs.release.permissions | to_entries | map(.key + "=" + .value) | sort | join(",")'
expect_yq "the release checkout leaves no credentials behind" \
    'false' '.jobs.release.steps[] | select(.uses == "actions/checkout*") | .with."persist-credentials"'
expect_yq "the push publishes exactly the tags the meta step resolved" \
    '${{ steps.meta.outputs.tags }}' '.jobs.release.steps[] | select(.id == "push") | .with.tags'
# The blocking scan: CRITICAL, exit-code 1, and before the registry login.
scan_at="$(yq '[.jobs.release.steps[] | (.with."exit-code" // "")] | to_entries | map(select(.value == "1")) | .[0].key // -1' "$R")"
login_at="$(yq '[.jobs.release.steps[] | (.uses // "")] | to_entries | map(select(.value == "docker/login-action*")) | .[0].key // -1' "$R")"
scan_sev="$(yq -r '.jobs.release.steps[] | select(.with."exit-code" == "1") | .with.severity' "$R")"
if [ "$scan_at" -ge 0 ] && [ "$login_at" -ge 0 ] && [ "$scan_at" -lt "$login_at" ] && [ "$scan_sev" = CRITICAL ]; then
    ok "a blocking CRITICAL scan (exit-code 1) runs before the registry login"
else
    fail "the blocking scan must be severity CRITICAL, exit-code 1, before the login (scan step $scan_at, login step $login_at, severity '$scan_sev')"
fi

# The meta step refuses what is not vMAJOR.MINOR.PATCH.
bad=""
for v in v01.2.3 v1.2 vX; do
    resolve "$v" v1.0.0 "$v" 2>/dev/null
    if [ "$rc" -eq 0 ] || ! printf '%s\n' "$log" | grep -q 'not vMAJOR.MINOR.PATCH'; then bad="$bad $v"; fi
done
if [ -z "$bad" ]; then ok "a malformed tag (v01.2.3, v1.2, vX) is refused"; else fail "malformed tags were accepted:$bad"; fi

# The sign step signs the digest, and refuses without one. A stub cosign
# records what it was asked to sign.
yq '.jobs.release.steps[] | select(.name == "Sign the image") | .run' "$R" > "$WORK/sign.sh"
mkdir -p "$WORK/bin"
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$*" > "%s/cosign.args"\n' "$WORK" > "$WORK/bin/cosign"
chmod +x "$WORK/bin/cosign"
rm -f "$WORK/cosign.args"
PATH="$WORK/bin:$PATH" IMAGE=ghcr.io/o/r DIGEST=sha256:abc VERSION=v1.0.0 bash "$WORK/sign.sh" >/dev/null 2>&1
if [ "$(cat "$WORK/cosign.args" 2>/dev/null)" = "sign --yes ghcr.io/o/r@sha256:abc" ]; then
    ok "the image is signed by digest, never by tag"
else
    fail "cosign must sign IMAGE@DIGEST (got '$(cat "$WORK/cosign.args" 2>/dev/null)')"
fi
rm -f "$WORK/cosign.args"
if ! PATH="$WORK/bin:$PATH" IMAGE=ghcr.io/o/r DIGEST='' bash "$WORK/sign.sh" >/dev/null 2>&1 && [ ! -e "$WORK/cosign.args" ]; then
    ok "no digest from the push: the sign step fails and signs nothing"
else
    fail "an empty digest must fail the sign step before cosign runs"
fi

# --- ci-main-image.yaml: an on-demand build may not overwrite a published tag,
# and logs in with the built-in token alone.
M="$WORK/main-image.yaml"
tr -d '\r' < "$ROOT/.github/workflows/ci-main-image.yaml" > "$M"
yq '.jobs.docker-build-and-push.steps[] | select(.id == "img") | .run' "$M" \
    | sed 's/\${{ github.event_name }}/workflow_dispatch/' > "$WORK/img.sh"
dispatch() { # dispatch <tag input> <branch>: prints rc|tag
    : > "$WORK/img.out"
    env INPUT_TAG="$1" INPUT_PLATFORMS='' GITHUB_REF_NAME="$2" GITHUB_OUTPUT="$WORK/img.out" \
        GITHUB_REPOSITORY_OWNER=Owner GITHUB_REPOSITORY=Owner/repo bash "$WORK/img.sh" >/dev/null 2>&1
    printf '%s|%s' "$?" "$(sed -n 's/^tag=//p' "$WORK/img.out")"
}
refused=""
for t in latest main v1.2.3 v2; do
    case "$(dispatch "$t" feat/x)" in 0\|*) refused="$refused $t" ;; esac
done
case "$(dispatch '' main)" in 0\|*) refused="$refused main(branch)" ;; esac
if [ -z "$refused" ]; then
    ok "a dispatch build refuses latest, main and v<digit> tags"
else
    fail "a dispatch build must not publish these tags:$refused"
fi
if [ "$(dispatch '' feat/my-branch)" = "0|feat-my-branch" ] && [ "$(dispatch pr-158 feat/x)" = "0|pr-158" ]; then
    ok "a dispatch build of a branch, or with an ordinary tag, still publishes"
else
    fail "branch-name and ordinary dispatch tags must still work (got $(dispatch '' feat/my-branch), $(dispatch pr-158 feat/x))"
fi
# A tag input is one line. Checked per line by grep, "foo", a newline,
# "tag=latest" passed, and the second line landed in GITHUB_OUTPUT as tag=latest.
NL_TAG="$(printf 'foo\ntag=latest')"
nl_rc=0
: > "$WORK/img.out"
env INPUT_TAG="$NL_TAG" INPUT_PLATFORMS='' GITHUB_REF_NAME=feat/x GITHUB_OUTPUT="$WORK/img.out" \
    GITHUB_REPOSITORY_OWNER=Owner GITHUB_REPOSITORY=Owner/repo bash "$WORK/img.sh" >/dev/null 2>&1 || nl_rc=$?
nl_plat_rc=0
env INPUT_TAG=pr-1 INPUT_PLATFORMS="$(printf 'linux/amd64\nplatforms=linux/evil')" GITHUB_REF_NAME=feat/x \
    GITHUB_OUTPUT="$WORK/img2.out" GITHUB_REPOSITORY_OWNER=Owner GITHUB_REPOSITORY=Owner/repo \
    bash "$WORK/img.sh" >/dev/null 2>&1 || nl_plat_rc=$?
if [ "$nl_rc" -ne 0 ] && ! grep -q '^tag=' "$WORK/img.out" && [ "$nl_plat_rc" -ne 0 ]; then
    ok "a dispatch tag or platform list holding a newline is refused, and writes no output"
else
    fail "a multi-line tag or platform input must be refused (tag rc=$nl_rc, platforms rc=$nl_plat_rc)" "$(cat "$WORK/img.out")"
fi

# The image source label names the repository that built the image: the
# workflows pass it, and the Dockerfile reads it.
mbuild="$(yq -r '.jobs.docker-build-and-push.steps[] | select(.name == "Build and push image") | .run' "$M")"
if printf '%s\n' "$mbuild" | grep -qF -- '--build-arg "IMAGE_SOURCE=${{ github.server_url }}/${{ github.repository }}"'; then
    ok "the main-image build passes IMAGE_SOURCE from this repository"
else
    fail "ci-main-image.yaml must pass --build-arg IMAGE_SOURCE=\${{ github.server_url }}/\${{ github.repository }}"
fi
if tr -d '\r' < "$ROOT/Dockerfile" | grep -qxF 'LABEL org.opencontainers.image.source="${IMAGE_SOURCE}"'; then
    ok "the Dockerfile source label reads IMAGE_SOURCE"
else
    fail "the Dockerfile must set LABEL org.opencontainers.image.source=\"\${IMAGE_SOURCE}\""
fi
rbuilds="$(yq -r '[.jobs.release.steps[] | select(.uses == "docker/build-push-action*") | .with."build-args" // ""] | map(select(contains("IMAGE_SOURCE=${{ github.server_url }}/${{ github.repository }}"))) | length' "$R")"
rtotal="$(yq -r '[.jobs.release.steps[] | select(.uses == "docker/build-push-action*")] | length' "$R")"
if [ "$rtotal" = 2 ] && [ "$rbuilds" = 2 ]; then
    ok "both release builds (scan and push) pass IMAGE_SOURCE from this repository"
else
    fail "both ci-release.yaml build steps must pass IMAGE_SOURCE in build-args (builds $rtotal, with it $rbuilds)"
fi

mlogin="$(yq -r '.jobs.docker-build-and-push.steps[] | select(.name == "Log in to GitHub Container Registry") | .env.REGISTRY_TOKEN' "$M")"
if [ "$mlogin" = '${{ secrets.GITHUB_TOKEN }}' ]; then
    ok "the main-image login uses the built-in GITHUB_TOKEN alone"
else
    fail "the main-image login must use secrets.GITHUB_TOKEN only (got '$mlogin')"
fi

# --- The tools image (release-tools): the same guards as the controller image.
# The meta step's latest= output is what release-tools reads; it must say what
# the tags say, or the tools image moves `latest` when the controller did not.
bad=""
for spec in "v1.3.0|v1.2.0 v1.3.0|true" "v1.2.1|v1.2.0 v1.3.0 v1.2.1|false" "v1.4.0-rc1|v1.3.0 v1.4.0-rc1|false"; do
    ref="${spec%%|*}"; rest="${spec#*|}"; want="${rest##*|}"
    # shellcheck disable=SC2086
    resolve "$ref" ${rest%|*}
    got="$(printf '%s\n' "$out" | sed -n 's/^latest=//p')"
    case "$(tags)" in *:latest*) moved=true ;; *) moved=false ;; esac
    if [ "$rc" -ne 0 ] || [ "$got" != "$want" ] || [ "$moved" != "$want" ]; then bad="$bad $ref(latest=$got,tags-move=$moved)"; fi
done
if [ -z "$bad" ]; then ok "the meta step's latest= output agrees with the tags it resolved"; else fail "latest= disagrees with the tags:$bad"; fi

expect_yq "the tools image is released only after the release job (its tag guard) and the signed release files it bakes in" \
    'release,release-sign' '[.jobs.release-tools.needs] | flatten | join(",")'
expect_yq "the tools job may sign and push, reads contents, and keeps no credentials" \
    'contents=read,id-token=write,packages=write|false' \
    '(.jobs.release-tools.permissions | to_entries | map(.key + "=" + .value) | sort | join(",")) + "|" + (.jobs.release-tools.steps[] | select(.uses == "actions/checkout*") | .with."persist-credentials" | tostring)'
expect_yq "every tools build (amd64 scan, arm64 scan, push) uses Dockerfile.tools, and the push publishes the tags resolved" \
    'Dockerfile.tools,Dockerfile.tools,Dockerfile.tools|${{ steps.tools-meta.outputs.tags }}' \
    '([.jobs.release-tools.steps[] | select(.uses == "docker/build-push-action*") | .with.file] | join(",")) + "|" + (.jobs.release-tools.steps[] | select(.id == "tools-push") | .with.tags)'

# Two blocking scans (amd64 and arm64), both before the login: the last one's
# index is what must precede it.
tscan_at="$(yq '[.jobs.release-tools.steps[] | (.with."exit-code" // "")] | to_entries | map(select(.value == "1")) | .[-1].key // -1' "$R")"
tscan_n="$(yq '[.jobs.release-tools.steps[] | select(.with."exit-code" == "1")] | length' "$R")"
tlogin_at="$(yq '[.jobs.release-tools.steps[] | (.uses // "")] | to_entries | map(select(.value == "docker/login-action*")) | .[0].key // -1' "$R")"
tscan_sev="$(yq -r '[.jobs.release-tools.steps[] | select(.with."exit-code" == "1") | .with.severity] | unique | join(",")' "$R")"
if [ "$tscan_n" = 2 ] && [ "$tscan_at" -ge 0 ] && [ "$tlogin_at" -ge 0 ] && [ "$tscan_at" -lt "$tlogin_at" ] && [ "$tscan_sev" = CRITICAL ]; then
    ok "the tools image: blocking CRITICAL scans of amd64 and arm64 run before the registry login"
else
    fail "the tools image needs two blocking scans, CRITICAL, exit-code 1, both before the login ($tscan_n scans, last at step $tscan_at, login step $tlogin_at, severity '$tscan_sev')"
fi

# The tools meta step, run: <image>/tools, :latest only when the release moved it.
yq '.jobs.release-tools.steps[] | select(.id == "tools-meta") | .run' "$R" > "$WORK/tools-meta.sh"
tools_tags() {
    : > "$WORK/tools.out"
    env IMAGE=ghcr.io/owner/repo VERSION="$1" LATEST="$2" GITHUB_OUTPUT="$WORK/tools.out" bash "$WORK/tools-meta.sh" >/dev/null 2>&1 || echo "rc=$?"
    sed -n 's/^tags=//p' "$WORK/tools.out"
}
t1="$(tools_tags v1.3.0 true)"
t2="$(tools_tags v1.2.1 false)"
if [ "$t1" = "ghcr.io/owner/repo/tools:v1.3.0,ghcr.io/owner/repo/tools:latest" ] && [ "$t2" = "ghcr.io/owner/repo/tools:v1.2.1" ]; then
    ok "the tools image takes the version, and latest only when the controller moved it"
else
    fail "tools tags: latest=true gave [$t1], latest=false gave [$t2]"
fi

yq '.jobs.release-tools.steps[] | select(.name == "Sign the tools image") | .run' "$R" > "$WORK/tsign.sh"
rm -f "$WORK/cosign.args"
PATH="$WORK/bin:$PATH" IMAGE=ghcr.io/o/r/tools DIGEST=sha256:abc bash "$WORK/tsign.sh" >/dev/null 2>&1
signed="$(cat "$WORK/cosign.args" 2>/dev/null)"
rm -f "$WORK/cosign.args"
if [ "$signed" = "sign --yes ghcr.io/o/r/tools@sha256:abc" ] \
    && ! PATH="$WORK/bin:$PATH" IMAGE=ghcr.io/o/r/tools DIGEST='' bash "$WORK/tsign.sh" >/dev/null 2>&1 \
    && [ ! -e "$WORK/cosign.args" ]; then
    ok "the tools image is signed by digest, and not at all without one"
else
    fail "the tools sign step must sign IMAGE@DIGEST and refuse an empty digest (signed '$signed')"
fi

# --- The capacity planner image (release-capacity-planner): the tools image's
# guards. One blocking scan (the binary is pure Go, the same on both
# platforms), and before the login the image must pass
# hack/check-capacity-planner-image.sh at this release.
P=release-capacity-planner
expect_yq "the capacity planner image is released only after the release job (its tag guard)" \
    'release' "[.jobs.$P.needs] | flatten | join(\",\")"
expect_yq "the capacity planner job may sign and push, reads contents, and keeps no credentials" \
    'contents=read,id-token=write,packages=write|false' \
    "(.jobs.$P.permissions | to_entries | map(.key + \"=\" + .value) | sort | join(\",\")) + \"|\" + (.jobs.$P.steps[] | select(.uses == \"actions/checkout*\") | .with.\"persist-credentials\" | tostring)"
expect_yq "every capacity planner build (amd64 check and scan, push) uses Dockerfile.capacity-planner for both platforms, and the push publishes the tags resolved" \
    'Dockerfile.capacity-planner,Dockerfile.capacity-planner|linux/amd64,linux/arm64|${{ steps.planner-meta.outputs.tags }}' \
    "([.jobs.$P.steps[] | select(.uses == \"docker/build-push-action*\") | .with.file] | join(\",\")) + \"|\" + (.jobs.$P.steps[] | select(.id == \"planner-push\") | .with.platforms + \"|\" + .with.tags)"
pscan_at="$(yq "[.jobs.$P.steps[] | (.with.\"exit-code\" // \"\")] | to_entries | map(select(.value == \"1\")) | .[-1].key // -1" "$R")"
pscan_n="$(yq "[.jobs.$P.steps[] | select(.with.\"exit-code\" == \"1\")] | length" "$R")"
pcheck_at="$(yq "[.jobs.$P.steps[] | (.run // \"\")] | to_entries | map(select(.value | test(\"hack/check-capacity-planner-image.sh\"))) | .[0].key // -1" "$R")"
pcheck_ver="$(yq -r ".jobs.$P.steps[] | select((.run // \"\") | test(\"hack/check-capacity-planner-image.sh\")) | .env.PLANNER_VERSION" "$R")"
plogin_at="$(yq "[.jobs.$P.steps[] | (.uses // \"\")] | to_entries | map(select(.value == \"docker/login-action*\")) | .[0].key // -1" "$R")"
if [ "$pscan_n" = 1 ] && [ "$pscan_at" -ge 0 ] && [ "$pcheck_at" -ge 0 ] && [ "$plogin_at" -ge 0 ] \
    && [ "$pscan_at" -lt "$plogin_at" ] && [ "$pcheck_at" -lt "$plogin_at" ] \
    && [ "$pcheck_ver" = '${{ needs.release.outputs.version }}' ]; then
    ok "the capacity planner image: the serve check at this release and a blocking scan run before the registry login"
else
    fail "the capacity planner image needs the serve check (at the release version) and one blocking scan before the login ($pscan_n scans at $pscan_at, check at $pcheck_at version '$pcheck_ver', login at $plogin_at)"
fi

yq ".jobs.$P.steps[] | select(.id == \"planner-meta\") | .run" "$R" > "$WORK/planner-meta.sh"
planner_tags() {
    : > "$WORK/planner.out"
    env IMAGE=ghcr.io/owner/repo VERSION="$1" LATEST="$2" GITHUB_OUTPUT="$WORK/planner.out" bash "$WORK/planner-meta.sh" >/dev/null 2>&1 || echo "rc=$?"
    sed -n 's/^tags=//p' "$WORK/planner.out"
}
p1="$(planner_tags v1.3.0 true)"
p2="$(planner_tags v1.2.1 false)"
if [ "$p1" = "ghcr.io/owner/repo/capacity-planner:v1.3.0,ghcr.io/owner/repo/capacity-planner:latest" ] && [ "$p2" = "ghcr.io/owner/repo/capacity-planner:v1.2.1" ]; then
    ok "the capacity planner image takes the version, and latest only when the controller moved it"
else
    fail "capacity planner tags: latest=true gave [$p1], latest=false gave [$p2]"
fi

yq ".jobs.$P.steps[] | select(.name == \"Sign the capacity planner image\") | .run" "$R" > "$WORK/psign.sh"
rm -f "$WORK/cosign.args"
PATH="$WORK/bin:$PATH" IMAGE=ghcr.io/o/r/capacity-planner DIGEST=sha256:abc bash "$WORK/psign.sh" >/dev/null 2>&1
signed="$(cat "$WORK/cosign.args" 2>/dev/null)"
rm -f "$WORK/cosign.args"
if [ "$signed" = "sign --yes ghcr.io/o/r/capacity-planner@sha256:abc" ] \
    && ! PATH="$WORK/bin:$PATH" IMAGE=ghcr.io/o/r/capacity-planner DIGEST='' bash "$WORK/psign.sh" >/dev/null 2>&1 \
    && [ ! -e "$WORK/cosign.args" ]; then
    ok "the capacity planner image is signed by digest, and not at all without one"
else
    fail "the capacity planner sign step must sign IMAGE@DIGEST and refuse an empty digest (signed '$signed')"
fi

# The platform check, run with a stub docker printing the pushed index: an
# index without arm64 fails the release.
yq ".jobs.$P.steps[] | select(.name == \"Verify the pushed image holds linux/amd64 and linux/arm64\") | .run" "$R" > "$WORK/pverify.sh"
mkdir -p "$WORK/pbin"
printf '%s\n' '#!/usr/bin/env bash' 'printf "%s" "$STUB_INDEX"' > "$WORK/pbin/docker"
chmod +x "$WORK/pbin/docker"
both='{"manifests":[{"platform":{"os":"linux","architecture":"amd64"}},{"platform":{"os":"linux","architecture":"arm64"}},{"platform":{"os":"unknown","architecture":"unknown"}}]}'
one='{"manifests":[{"platform":{"os":"linux","architecture":"amd64"}},{"platform":{"os":"unknown","architecture":"unknown"}}]}'
pv() { PATH="$WORK/pbin:$PATH" STUB_INDEX="$1" IMAGE=ghcr.io/o/r/capacity-planner DIGEST="$2" bash "$WORK/pverify.sh" >/dev/null 2>&1; }
if pv "$both" sha256:abc && ! pv "$one" sha256:abc && ! pv "$both" ''; then
    ok "the pushed capacity planner index must hold linux/amd64 and linux/arm64, by digest"
else
    fail "the platform check must pass both platforms and refuse an index without arm64 or with no digest"
fi

# --- The warm-pool proxy image (release-warmpool-proxy): the capacity planner
# image's guards, without its serve check. config/warmpool names the proxy by
# tag, so the release job refuses a commit that does not pin this release.
W=release-warmpool-proxy
expect_yq "the warm-pool proxy image is released only after the release job (its tag and pin guards)" \
    'release' "[.jobs.$W.needs] | flatten | join(\",\")"
expect_yq "the warm-pool proxy job may sign and push, reads contents, and keeps no credentials" \
    'contents=read,id-token=write,packages=write|false' \
    "(.jobs.$W.permissions | to_entries | map(.key + \"=\" + .value) | sort | join(\",\")) + \"|\" + (.jobs.$W.steps[] | select(.uses == \"actions/checkout*\") | .with.\"persist-credentials\" | tostring)"
expect_yq "every warm-pool proxy build (amd64 scan, push) uses Dockerfile.warmpool-proxy, the push is multi-arch and publishes the tags resolved" \
    'Dockerfile.warmpool-proxy,Dockerfile.warmpool-proxy|linux/amd64,linux/arm64|${{ steps.proxy-meta.outputs.tags }}' \
    "([.jobs.$W.steps[] | select(.uses == \"docker/build-push-action*\") | .with.file] | join(\",\")) + \"|\" + (.jobs.$W.steps[] | select(.id == \"proxy-push\") | .with.platforms + \"|\" + .with.tags)"
wscan_at="$(yq "[.jobs.$W.steps[] | (.with.\"exit-code\" // \"\")] | to_entries | map(select(.value == \"1\")) | .[-1].key // -1" "$R")"
wscan_n="$(yq "[.jobs.$W.steps[] | select(.with.\"exit-code\" == \"1\")] | length" "$R")"
wscan_sev="$(yq -r "[.jobs.$W.steps[] | select(.with.\"exit-code\" == \"1\") | .with.severity] | unique | join(\",\")" "$R")"
wlogin_at="$(yq "[.jobs.$W.steps[] | (.uses // \"\")] | to_entries | map(select(.value == \"docker/login-action*\")) | .[0].key // -1" "$R")"
if [ "$wscan_n" = 1 ] && [ "$wscan_at" -ge 0 ] && [ "$wlogin_at" -ge 0 ] && [ "$wscan_at" -lt "$wlogin_at" ] && [ "$wscan_sev" = CRITICAL ]; then
    ok "the warm-pool proxy image: a blocking CRITICAL scan runs before the registry login"
else
    fail "the warm-pool proxy image needs one blocking CRITICAL scan before the login ($wscan_n scans at $wscan_at, login at $wlogin_at, severity '$wscan_sev')"
fi

yq ".jobs.$W.steps[] | select(.id == \"proxy-meta\") | .run" "$R" > "$WORK/proxy-meta.sh"
proxy_tags() {
    : > "$WORK/proxy.out"
    env IMAGE=ghcr.io/owner/repo VERSION="$1" LATEST="$2" GITHUB_OUTPUT="$WORK/proxy.out" bash "$WORK/proxy-meta.sh" >/dev/null 2>&1 || echo "rc=$?"
    sed -n 's/^tags=//p' "$WORK/proxy.out"
}
w1="$(proxy_tags v1.3.0 true)"
w2="$(proxy_tags v1.2.1 false)"
if [ "$w1" = "ghcr.io/owner/repo/warmpool-proxy:v1.3.0,ghcr.io/owner/repo/warmpool-proxy:latest" ] && [ "$w2" = "ghcr.io/owner/repo/warmpool-proxy:v1.2.1" ]; then
    ok "the warm-pool proxy image takes the version, and latest only when the controller moved it"
else
    fail "warm-pool proxy tags: latest=true gave [$w1], latest=false gave [$w2]"
fi

yq ".jobs.$W.steps[] | select(.name == \"Sign the warm-pool proxy image\") | .run" "$R" > "$WORK/wsign.sh"
rm -f "$WORK/cosign.args"
PATH="$WORK/bin:$PATH" IMAGE=ghcr.io/o/r/warmpool-proxy DIGEST=sha256:abc bash "$WORK/wsign.sh" >/dev/null 2>&1
signed="$(cat "$WORK/cosign.args" 2>/dev/null)"
rm -f "$WORK/cosign.args"
if [ "$signed" = "sign --yes ghcr.io/o/r/warmpool-proxy@sha256:abc" ] \
    && ! PATH="$WORK/bin:$PATH" IMAGE=ghcr.io/o/r/warmpool-proxy DIGEST='' bash "$WORK/wsign.sh" >/dev/null 2>&1 \
    && [ ! -e "$WORK/cosign.args" ]; then
    ok "the warm-pool proxy image is signed by digest, and not at all without one"
else
    fail "the warm-pool proxy sign step must sign IMAGE@DIGEST and refuse an empty digest (signed '$signed')"
fi

yq ".jobs.$W.steps[] | select(.name == \"Verify the pushed image holds linux/amd64 and linux/arm64\") | .run" "$R" > "$WORK/wverify.sh"
wv() { PATH="$WORK/pbin:$PATH" STUB_INDEX="$1" IMAGE=ghcr.io/o/r/warmpool-proxy DIGEST="$2" bash "$WORK/wverify.sh" >/dev/null 2>&1; }
if [ -s "$WORK/wverify.sh" ] && wv "$both" sha256:abc && ! wv "$one" sha256:abc && ! wv "$both" ''; then
    ok "the pushed warm-pool proxy index must hold linux/amd64 and linux/arm64, by digest"
else
    fail "the warm-pool proxy platform check must pass both platforms and refuse an index without arm64 or with no digest"
fi

# The pin step, run in a scratch tree holding only the manifest: it passes the
# release's own tag and refuses :main, another tag, a digest, a missing line
# and a second proxy line. Before any push of the release job, so a forgotten
# pin publishes nothing.
yq '.jobs.release.steps[] | select(.name == "Verify the warm-pool proxy image pins this release") | .run' "$R" > "$WORK/wpin.sh"
wpin() { # wpin <image line(s)> -- rc of the step for VERSION=v1.3.0
    rm -rf "$WORK/pin"; mkdir -p "$WORK/pin/config/warmpool"
    printf 'spec:\n  containers:\n  - name: proxy\n%s\n' "$1" > "$WORK/pin/config/warmpool/warmpool-deployment.yaml"
    (cd "$WORK/pin" && VERSION=v1.3.0 bash "$WORK/wpin.sh" >/dev/null 2>&1)
}
pin_bad=""
wpin '    image: ghcr.io/o/r/warmpool-proxy:v1.3.0' || pin_bad="$pin_bad release-tag-refused"
wpin '    image: "ghcr.io/o/r/warmpool-proxy:v1.3.0"' || pin_bad="$pin_bad quoted-refused"
wpin '    image: ghcr.io/o/r/warmpool-proxy:main' && pin_bad="$pin_bad main-accepted"
wpin '    image: ghcr.io/o/r/warmpool-proxy:v1.3.0-rc1' && pin_bad="$pin_bad other-tag-accepted"
wpin '    image: ghcr.io/o/r/warmpool-proxy@sha256:abc' && pin_bad="$pin_bad digest-accepted"
wpin '    image: ghcr.io/o/r/other:v1.3.0' && pin_bad="$pin_bad no-proxy-line-accepted"
wpin "$(printf '    image: ghcr.io/o/r/warmpool-proxy:v1.3.0\n    image: ghcr.io/o/r/warmpool-proxy:main')" && pin_bad="$pin_bad two-lines-accepted"
pin_at="$(yq '[.jobs.release.steps[] | (.name // "")] | to_entries | map(select(.value == "Verify the warm-pool proxy image pins this release")) | .[0].key // -1' "$R")"
push_at="$(yq '[.jobs.release.steps[] | (.id // "")] | to_entries | map(select(.value == "push")) | .[0].key // -1' "$R")"
if [ -s "$WORK/wpin.sh" ] && [ -z "$pin_bad" ] && [ "$pin_at" -ge 0 ] && [ "$pin_at" -lt "$push_at" ]; then
    ok "the release refuses, before any push, a commit whose proxy image is not tagged with the release"
else
    fail "the proxy pin step must accept only <...>/warmpool-proxy:<version>, once, before the push (failures:${pin_bad:- none}; step $pin_at, push $push_at)"
fi

echo ""
echo "check-release-latest: $PASS passed, $FAIL failed"
if [ $((PASS + FAIL)) -ne "$EXPECT_CASES" ]; then
    echo "check-release-latest: $((PASS + FAIL)) case(s) ran, $EXPECT_CASES expected"
    exit 1
fi
[ "$FAIL" -eq 0 ]
