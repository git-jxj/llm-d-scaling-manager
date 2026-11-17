#!/usr/bin/env bash
#
# Runs hack/upstream-versions.sh against fixture pins and stubbed GitHub API
# answers (no network), and runs the issue step of
# .github/workflows/upstream-versions.yaml against a stubbed gh.
#
# What it guards is the difference between "current" and "we could not tell":
# a failed or rate-limited request, an empty release list, or a pin the
# inventory no longer finds all print a perfectly tidy report, and the old
# failure mode of a check like this is to call every one of them current.
#
# It also reads every location in the real hack/upstream-pins.yaml, so a pin
# that moves without its inventory entry fails here, not on the 1st of the
# month.
#
# Needs bash, jq, yq (v4) and GNU sort/date.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${CHECK_UPSTREAM_SCRIPT:-$ROOT/hack/upstream-versions.sh}"
WORKFLOW="${CHECK_UPSTREAM_WORKFLOW:-$ROOT/.github/workflows/upstream-versions.yaml}"
# One ok or FAIL per case. A run that stops early otherwise passes having
# checked less than it says. Change this deliberately with the cases.
EXPECT_CASES=119
PASS=0
FAIL=0

ok()   { PASS=$((PASS + 1)); echo "ok   $1"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL $1"; [ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/       /'; }

# expect_exit DESC WANT GOT
expect_exit() {
    if [ "$3" = "$2" ]; then ok "$1 (exit $3)"; else fail "$1: want exit $2, got $3" "$OUT"; fi
}
# expect_has DESC NEEDLE: the last report contains NEEDLE.
expect_has() {
    if printf '%s' "$OUT" | grep -qF -- "$2"; then ok "$1"; else fail "$1: missing [$2]" "$OUT"; fi
}
expect_not() {
    if printf '%s' "$OUT" | grep -qF -- "$2"; then fail "$1: unexpected [$2]" "$OUT"; else ok "$1"; fi
}

for tool in jq yq sort date; do
    command -v "$tool" >/dev/null 2>&1 || { echo "FAIL $tool not found; this check cannot run without it"; exit 1; }
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- the stubbed GitHub API -------------------------------------------------
# Answers .../repos/OWNER/REPO/releases?...&page=N from $FIX/OWNER_REPO.N.json.
# OWNER_REPO.fail makes every request for that repository fail like an HTTP
# error; a page with no file answers [] like the API past the last page.
# .../repos/OWNER/REPO/contents/PATH?ref=REF answers the file
# $FIX/OWNER_REPO.contents.REF, base64-encoded like the API.
FIX="$WORK/api"
mkdir -p "$FIX"
cat > "$WORK/fetch" <<'STUB'
#!/usr/bin/env bash
url="$1"
path="${url#*/repos/}"
echo "$url" >> "$FIX/requests.log"
case "$url" in
    */contents/*)
        repo="${path%%/contents/*}"
        f="$FIX/${repo//\//_}.contents.${url##*ref=}"
        if [ -f "$f" ]; then
            jq -n --arg c "$(base64 -w 76 < "$f")" '{content: $c, encoding: "base64"}'
            exit 0
        fi
        echo "HTTP 404: Not Found" >&2
        exit 22
        ;;
esac
repo="${path%%/releases*}"
key="${repo//\//_}"
page="${url##*page=}"
if [ -e "$FIX/$key.fail" ]; then
    echo "curl: (22) The requested URL returned error: 403" >&2
    exit 22
fi
if [ -f "$FIX/$key.$page.json" ]; then
    cat "$FIX/$key.$page.json"
else
    echo '[]'
fi
STUB
chmod +x "$WORK/fetch"
export FIX

# rel TAG DATE [prerelease] [draft]: one release object.
rel() {
    jq -nc --arg t "$1" --arg d "${2}T12:00:00Z" --argjson p "${3:-false}" --argjson dr "${4:-false}" \
        '{tag_name: $t, published_at: $d, prerelease: $p, draft: $dr}'
}
# releases KEY PAGE OBJECT...: write one page of the stubbed answer.
releases() {
    local key="$1" page="$2"
    shift 2
    printf '%s\n' "$@" | jq -s '.' > "$FIX/$key.$page.json"
}

# --- fixture pins -----------------------------------------------------------
PINS="$WORK/root"
mkdir -p "$PINS/deploy" "$PINS/bench"

# run [--check]: run the script on $PINS/inventory.yaml; sets OUT and RC.
run() {
    OUT="$(UPSTREAM_FETCH="$WORK/fetch" UPSTREAM_PINS_FILE="$PINS/inventory.yaml" \
        UPSTREAM_PINS_ROOT="$PINS" UPSTREAM_TODAY=2026-03-01 \
        bash "$SCRIPT" "$@" 2>&1)"
    RC=$?
}

inventory() { printf '%s\n' "components:" "$@" > "$PINS/inventory.yaml"; }

FOO_A='  - name: foo
    repo: example/foo
    pins:
      - file: deploy/a.sh
        regex: '"'"'FOO_VERSION:-(v[0-9][0-9a-z.+-]*)\}'"'"'
      - file: bench/b.yaml
        yq: '"'"'.images.foo.tag'"'"''

set_pins() {
    printf 'FOO_VERSION=${FOO_VERSION:-%s}\n' "$1" > "$PINS/deploy/a.sh"
    printf 'images:\n  foo:\n    tag: %s\n' "$2" > "$PINS/bench/b.yaml"
}

# 1. Current. Newer prereleases, drafts and rc tags upstream did not flag are
#    not "latest" -- vLLM's own shapes included (v0.33.0rc3, .devN), which it
#    publishes with prerelease=false.
rm -f "$FIX"/*
inventory "$FOO_A"
set_pins v1.2.0 v1.2.0
releases example_foo 1 "$(rel v1.5.0.dev0 2026-02-25)" "$(rel v1.4.0rc3 2026-02-22)" \
    "$(rel v1.4.0-rc.1 2026-02-20)" "$(rel v1.3.0 2026-02-10 true)" \
    "$(rel v1.3.5 2026-02-15 false true)" "$(rel v1.2.0 2026-01-01)"
run
expect_exit "current: report mode" 0 "$RC"
expect_has "current: status" "| foo | v1.2.0 | v1.2.0 | 2026-01-01 | - | current |"
expect_not "current: no prerelease, draft or rc is latest" "v1.3"
expect_not "current: an unflagged rc3 tag is not latest" "v1.4.0rc3"
expect_not "current: an unflagged .dev0 tag is not latest" "v1.5.0.dev0"
run --check
expect_exit "current: --check" 0 "$RC"
expect_has "current: summary" "1 current, 0 behind, 0 with pins that disagree, 0 could not be checked"

# 2. Behind: how far, since when, and where to read about it.
rm -f "$FIX"/*
releases example_foo 1 "$(rel v1.3.0 2026-02-01)" "$(rel v1.2.1 2026-01-15)" "$(rel v1.2.0 2026-01-01)" "$(rel v1.1.0 2025-12-01)"
run
expect_exit "behind: report mode still exits 0" 0 "$RC"
expect_has "behind: row" "| foo | v1.2.0 | v1.3.0 | 2026-02-01 | 2 releases, 31 days | behind |"
expect_has "behind: compare link" "https://github.com/example/foo/compare/v1.2.0...v1.3.0"
run --check
expect_exit "behind: --check" 1 "$RC"

# 3. Behind by one: singular.
releases example_foo 1 "$(rel v1.2.1 2026-01-15)" "$(rel v1.2.0 2026-01-01)"
run
expect_has "behind by one" "| 1 release, 14 days | behind |"

# 4. Pins disagree, even when one of them is current.
set_pins v1.2.0 v1.1.0
releases example_foo 1 "$(rel v1.2.0 2026-01-01)" "$(rel v1.1.0 2025-12-01)"
run
expect_exit "disagree: report mode" 0 "$RC"
expect_has "disagree: status" "**pins disagree**"
expect_has "disagree: both values" "| foo | v1.1.0, v1.2.0 |"
expect_has "disagree: each file named" '`bench/b.yaml`: v1.1.0'
run --check
expect_exit "disagree: --check" 1 "$RC"

# 5. 1.2.0 in one file and v1.2.0 in another are one version.
set_pins v1.2.0 1.2.0
run --check
expect_exit "v prefix: 1.2.0 and v1.2.0 agree" 0 "$RC"
expect_has "v prefix: current" "| current |"

# 5b. A pin on a prerelease sorts below its own release: v1.3.0-rc.1 or
#     1.3.0rc1 with v1.3.0 out is behind, never "ahead of the latest".
releases example_foo 1 "$(rel v1.3.0 2026-02-01)" "$(rel v1.2.0 2026-01-01)"
set_pins v1.3.0-rc.1 v1.3.0-rc.1
run --check
expect_exit "rc pin: --check" 1 "$RC"
expect_has "rc pin: behind its release" "| foo | v1.3.0-rc.1 | v1.3.0 | 2026-02-01 | 1 release | behind |"
expect_not "rc pin: not ahead" "ahead of the latest release"
set_pins v1.3.0rc1 v1.3.0rc1
run --check
expect_has "PEP 440 rc pin: behind its release" "| foo | v1.3.0rc1 | v1.3.0 | 2026-02-01 | 1 release | behind |"
set_pins v1.3.0 v1.3.0
releases example_foo 1 "$(rel v1.3.0.post1 2026-02-10)" "$(rel v1.3.0 2026-02-01)"
run --check
expect_has "a .post release is above its base" "| foo | v1.3.0 | v1.3.0.post1 | 2026-02-10 | 1 release, 9 days | behind |"
releases example_foo 1 "$(rel v1.2.0 2026-01-01)" "$(rel v1.1.0 2025-12-01)"

# 6. Two values in one file disagree too (every matching line is a pin).
printf 'FOO_VERSION=${FOO_VERSION:-v1.2.0}\nFOO_VERSION=${FOO_VERSION:-v1.1.0}\n' > "$PINS/deploy/a.sh"
printf 'images:\n  foo:\n    tag: v1.2.0\n' > "$PINS/bench/b.yaml"
run --check
expect_exit "one file, two values: --check" 1 "$RC"
expect_has "one file, two values: status" "**pins disagree**"

# 7. A pin the inventory no longer finds is an error, never current.
set_pins v1.2.0 v1.2.0
printf 'FOO_VERSION=v1.2.0\n' > "$PINS/deploy/a.sh"
run
expect_exit "pin not found: report mode" 0 "$RC"
expect_has "pin not found: status names the file" "**pin not found**: deploy/a.sh"
expect_not "pin not found: not current" "| current |"
run --check
expect_exit "pin not found: --check" 2 "$RC"

# 8. A missing file likewise.
set_pins v1.2.0 v1.2.0
rm -f "$PINS/bench/b.yaml"
run --check
expect_exit "missing file: --check" 2 "$RC"
expect_has "missing file: status" "**pin not found**: bench/b.yaml"

# 9. A failed request is an error, never current, never behind.
set_pins v1.2.0 v1.2.0
touch "$FIX/example_foo.fail"
run
expect_exit "API error: report mode" 0 "$RC"
expect_has "API error: status" "**could not check**: release list request failed (exit 22)"
expect_has "API error: summary" "0 current, 0 behind, 0 with pins that disagree, 1 could not be checked"
run --check
expect_exit "API error: --check" 2 "$RC"
rm -f "$FIX/example_foo.fail"

# 10. A rate-limit message is JSON too, but not a release list.
printf '%s\n' '{"message":"API rate limit exceeded"}' > "$FIX/example_foo.1.json"
run --check
expect_exit "rate-limit body: --check" 2 "$RC"
expect_has "rate-limit body: says so" "unexpected answer: API rate limit exceeded"
printf '%s\n' '<html><body>502 Bad Gateway</body></html>' > "$FIX/example_foo.1.json"
run --check
expect_exit "non-JSON body: --check" 2 "$RC"
expect_has "non-JSON body: says so" "unexpected answer: <html><body>502 Bad Gateway"

# 11. No release at all, or only prereleases: nothing to compare with.
releases example_foo 1 "$(rel v1.3.0-rc.1 2026-02-01 true)"
run --check
expect_exit "no stable release: --check" 2 "$RC"
expect_has "no stable release: status" "no stable release found"

# 12. Errors win over findings: one behind and one unanswered exits 2.
inventory "$FOO_A" '  - name: bar
    repo: example/bar
    pins:
      - file: deploy/a.sh
        regex: '"'"'FOO_VERSION:-(v[0-9][0-9a-z.+-]*)\}'"'"''
releases example_foo 1 "$(rel v1.3.0 2026-02-01)" "$(rel v1.2.0 2026-01-01)"
touch "$FIX/example_bar.fail"
run --check
expect_exit "behind and unanswered: --check" 2 "$RC"
expect_has "behind and unanswered: both shown" "1 behind, 0 with pins that disagree, 1 could not be checked"
rm -f "$FIX/example_bar.fail"

# 13. A repository releasing several things: only the prefixed tags count.
rm -f "$FIX"/*
inventory '  - name: chart
    repo: example/charts
    tagPrefix: mychart-
    pins:
      - file: deploy/c.sh
        regex: '"'"'^CHART_VERSION=([0-9][0-9.]*)'"'"''
printf 'CHART_VERSION=1.2.0\n' > "$PINS/deploy/c.sh"
releases example_charts 1 "$(rel otherchart-9.9.9 2026-02-20)" "$(rel mychart-1.3.0 2026-02-01)" \
    "$(rel mychart-1.2.0 2026-01-01)"
run --check
expect_exit "tag prefix: --check" 1 "$RC"
expect_has "tag prefix: row" "| chart | 1.2.0 | mychart-1.3.0 | 2026-02-01 | 1 release, 31 days | behind |"
expect_has "tag prefix: compare link" "compare/mychart-1.2.0...mychart-1.3.0"

# 14. Tracked components show upstream and never fail --check.
inventory '  - name: engine
    repo: example/engine
    tracked: true'
releases example_engine 1 "$(rel v0.5.6.post2 2026-02-01)" "$(rel v0.5.6 2026-01-01)"
run --check
expect_exit "tracked: --check" 0 "$RC"
expect_has "tracked: row (post release is latest)" "| engine | - | v0.5.6.post2 | 2026-02-01 | - | tracked, not pinned |"

# 15. Pagination: read on until the pin, and say when it stopped short.
rm -f "$FIX"/*
inventory "$FOO_A"
set_pins v1.0.0 v1.0.0
page1=()
for ((n = 100; n >= 1; n--)); do page1+=("$(rel "v1.$n.0" 2026-02-01)"); done
releases example_foo 1 "${page1[@]}"
releases example_foo 2 "$(rel v1.0.0 2026-01-01)"
run
expect_has "pagination: counts across pages" "| 100 releases, 31 days | behind |"
expect_has "pagination: latest is the newest on page 1" "| v1.100.0 |"
OUT="$(UPSTREAM_MAX_PAGES=1 UPSTREAM_FETCH="$WORK/fetch" UPSTREAM_PINS_FILE="$PINS/inventory.yaml" \
    UPSTREAM_PINS_ROOT="$PINS" bash "$SCRIPT" 2>&1)"
expect_has "pagination: a cut-off count says so" "| 100+ releases | behind |"
: > "$FIX/requests.log"
releases example_foo 1 "$(rel v1.1.0 2026-02-01)" "$(rel v1.0.0 2026-01-01)"
run
if [ "$(grep -c . "$FIX/requests.log")" = 1 ]; then
    ok "pagination: a short page is the last one"
else
    fail "pagination: a short page is the last one" "$(cat "$FIX/requests.log")"
fi
# An old-branch patch at the end of a full page (Kubernetes patches several
# branches) is not our pin: read on until the pin itself.
set_pins v1.1.0 v1.1.0
page1=()
for ((n = 100; n >= 2; n--)); do page1+=("$(rel "v1.1.$n" 2026-02-01)"); done
page1+=("$(rel v1.0.9 2026-01-20)")
releases example_foo 1 "${page1[@]}"
releases example_foo 2 "$(rel v1.1.1 2026-01-10)" "$(rel v1.1.0 2026-01-01)"
run
expect_has "pagination: an old-branch patch does not end the count" "| 100 releases, 31 days | behind |"

# 16. A broken inventory is a broken tool: exit 2 in both modes.
inventory '  - name: nothing
    repo: example/nothing'
run
expect_exit "invalid inventory: report mode" 2 "$RC"
run --check
expect_exit "invalid inventory: --check" 2 "$RC"

# 17. The real fetch, against a fake curl: a network failure or a rate limit
#     stops the run at the first request, exits 2 in both modes, and says why.
#     Never "current". An ordinary HTTP error fails that component only.
rm -f "$FIX"/*
inventory "$FOO_A" '  - name: bar
    repo: example/bar
    pins:
      - file: deploy/a.sh
        regex: '"'"'FOO_VERSION:-(v[0-9][0-9a-z.+-]*)\}'"'"''
set_pins v1.2.0 v1.2.0
mkdir -p "$WORK/fakecurl"
cat > "$WORK/fakecurl/curl" <<'STUB'
#!/usr/bin/env bash
out="" hdr="/dev/null" prev=""
for a in "$@"; do
    [ "$prev" = "-o" ] && out="$a"
    [ "$prev" = "-D" ] && hdr="$a"
    prev="$a"
done
echo "curl $*" >> "$FAKE_CURL_LOG"
case "$FAKE_CURL_MODE" in
    dns) echo "curl: (6) Could not resolve host: api.github.com" >&2; printf '000'; exit 6 ;;
    403) printf '%s' '{"message":"API rate limit exceeded for 192.0.2.1."}' > "$out"; printf '403' ;;
    403hdr) printf 'HTTP/2 403\r\nx-ratelimit-remaining: 0\r\n\r\n' > "$hdr"
        printf '%s' '{"message":"Forbidden"}' > "$out"; printf '403' ;;
    403access) printf 'HTTP/2 403\r\nx-ratelimit-remaining: 4000\r\n\r\n' > "$hdr"
        printf '%s' '{"message":"Repository access blocked"}' > "$out"; printf '403' ;;
    429) printf '%s' '{"message":"Too many requests"}' > "$out"; printf '429' ;;
    404) printf '%s' '{"message":"Not Found"}' > "$out"; printf '404' ;;
    ok) printf '%s' '[{"tag_name":"v1.2.0","published_at":"2026-01-01T12:00:00Z","prerelease":false,"draft":false}]' > "$out"; printf '200' ;;
esac
exit 0
STUB
chmod +x "$WORK/fakecurl/curl"
# real MODE [--check]: run the script with the fake curl; sets OUT, RC, CALLS.
real() {
    local mode="$1"
    shift
    : > "$WORK/curl.log"
    OUT="$(env -u GITHUB_TOKEN -u UPSTREAM_FETCH PATH="$WORK/fakecurl:$PATH" FAKE_CURL_MODE="$mode" \
        FAKE_CURL_LOG="$WORK/curl.log" UPSTREAM_PINS_FILE="$PINS/inventory.yaml" UPSTREAM_PINS_ROOT="$PINS" \
        UPSTREAM_TODAY=2026-03-01 bash "$SCRIPT" "$@" 2>&1)"
    RC=$?
    CALLS="$(grep -c . "$WORK/curl.log")"
}
real ok --check
expect_exit "real fetch: an answer reads as one" 0 "$RC"
expect_has "real fetch: both components current" "2 current, 0 behind"
if grep -q -- '--max-time 30' "$WORK/curl.log" && grep -q -- '--connect-timeout 10' "$WORK/curl.log"; then
    ok "real fetch: every request has a hard timeout"
else
    fail "real fetch: no hard timeout on curl" "$(head -n 1 "$WORK/curl.log")"
fi
real dns
expect_exit "network down: report mode exits 2" 2 "$RC"
expect_has "network down: says why" "network failure (curl exit 6)"
expect_has "network down: says it stopped" "**Stopped asking GitHub**"
expect_has "network down: the rest were not asked" "| bar | v1.2.0 | ? | ? | - | **could not check**: not asked"
expect_not "network down: never current" "| current |"
if [ "$CALLS" = 1 ]; then ok "network down: one request, not one per component"; else fail "network down: $CALLS requests"; fi
real dns --check
expect_exit "network down: --check" 2 "$RC"
real 403 --check
expect_exit "rate limit 403: --check" 2 "$RC"
expect_has "rate limit 403: says so" "HTTP 403 (rate limit): API rate limit exceeded"
expect_has "rate limit 403: says it stopped" "**Stopped asking GitHub**"
if [ "$CALLS" = 1 ]; then ok "rate limit 403: one request"; else fail "rate limit 403: $CALLS requests"; fi
real 403hdr --check
if [ "$RC" = 2 ] && [ "$CALLS" = 1 ] && printf '%s' "$OUT" | grep -qF "HTTP 403 (rate limit): Forbidden"; then
    ok "rate limit 403 by header (no requests left): stops the run"
else
    fail "rate limit 403 by header (exit $RC, $CALLS requests)" "$OUT"
fi
real 403access --check
expect_exit "403, not a rate limit: --check" 2 "$RC"
expect_has "403, not a rate limit: says so" "HTTP 403 (access): Repository access blocked"
if [ "$CALLS" = 2 ] && ! printf '%s' "$OUT" | grep -qF "Stopped asking"; then
    ok "403, not a rate limit: the next component is still asked"
else
    fail "403, not a rate limit: $CALLS requests" "$OUT"
fi
real 429 --check
expect_exit "rate limit 429: --check" 2 "$RC"
expect_has "rate limit 429: says so" "HTTP 429 (rate limit): Too many requests"
real 404 --check
expect_exit "HTTP 404: --check" 2 "$RC"
expect_has "HTTP 404: says so" "HTTP 404: Not Found"
expect_not "HTTP 404: does not stop the run" "Stopped asking"
if [ "$CALLS" = 2 ]; then ok "HTTP 404: the next component is still asked"; else fail "HTTP 404: $CALLS requests"; fi
# The real curl, against a port nothing listens on: its exit code reaches
# the report.
if command -v curl >/dev/null 2>&1; then
    OUT="$(env -u GITHUB_TOKEN -u UPSTREAM_FETCH UPSTREAM_API_URL=http://127.0.0.1:9 UPSTREAM_CURL_RETRY=0 \
        UPSTREAM_PINS_FILE="$PINS/inventory.yaml" UPSTREAM_PINS_ROOT="$PINS" bash "$SCRIPT" --check 2>&1)"
    RC=$?
    if [ "$RC" = 2 ] && printf '%s' "$OUT" | grep -qF "network failure (curl exit 7)"; then
        ok "real curl, connection refused: network failure, exit 2"
    else
        fail "real curl, connection refused (exit $RC)" "$OUT"
    fi
else
    fail "real curl, connection refused: curl not found"
fi

# 18. Holds: a pin behind on purpose reads "held" until its review date, and
#     does not keep the issue open; after the date it is behind again; a hold
#     the pin has moved past is an error.
rm -f "$FIX"/*
set_pins v1.2.0 v1.2.0
releases example_foo 1 "$(rel v1.3.0 2026-02-01)" "$(rel v1.2.0 2026-01-01)"
hold() {  # hold VERSION UNTIL
    inventory "$FOO_A" '    hold:
      version: '"$1"'
      reason: matched with another pin
      until: "'"$2"'"'
}
hold v1.2.0 2026-06-01
run --check
expect_exit "held: --check" 0 "$RC"
expect_has "held: row" "| 1 release, 31 days | held until 2026-06-01: matched with another pin |"
expect_has "held: summary" "0 behind, 0 with pins that disagree, 0 could not be checked, 0 tracked but not pinned, 1 held on purpose"
hold v1.2.0 2026-03-01
run --check
expect_exit "held: the review date itself is still held" 0 "$RC"
hold v1.2.0 2026-02-28
run --check
expect_exit "hold ended: --check" 1 "$RC"
expect_has "hold ended: says so" "| behind (hold ended 2026-02-28: matched with another pin) |"
hold v1.1.0 2026-06-01
run --check
expect_exit "stale hold: --check" 2 "$RC"
expect_has "stale hold: says so" "**hold is stale**: held at v1.1.0, pinned at v1.2.0"
inventory "$FOO_A" '    hold:
      version: v1.2.0
      reason: no date'
run
expect_exit "hold without a review date: invalid inventory" 2 "$RC"

# 19. A pin read upstream, at the ref another pin names (the harness default
#     engine, read from llm-d-benchmark at our llm-d-benchmark pin).
rm -f "$FIX"/*
printf 'BENCH_REF=v0.7.8\n' > "$PINS/deploy/bench.sh"
inventory '  - name: bench
    repo: example/bench
    pins:
      - file: deploy/bench.sh
        regex: '"'"'^BENCH_REF=(v[0-9][0-9a-z.+-]*)'"'"'' '  - name: engine
    repo: example/engine
    pins:
      - from: example/bench
        at: bench
        file: config/defaults.yaml
        yq: '"'"'explode(.) | .images.vllm.tag'"'"''
printf 'v:\n  vllm_version: &vllm_version v0.26.0\nimages:\n  vllm:\n    tag: *vllm_version\n' > "$FIX/example_bench.contents.v0.7.8"
releases example_bench 1 "$(rel v0.7.8 2026-01-01)"
releases example_engine 1 "$(rel v0.27.0 2026-02-01)" "$(rel v0.26.0 2026-01-01)"
: > "$FIX/requests.log"
run --check
expect_exit "upstream pin: --check" 1 "$RC"
expect_has "upstream pin: value read through the anchor" "| engine | v0.26.0 | v0.27.0 | 2026-02-01 | 1 release, 31 days | behind |"
expect_has "upstream pin: location names repository, ref and file" '`example/bench@v0.7.8:config/defaults.yaml`: v0.26.0'
if grep -qF 'example/bench/contents/config/defaults.yaml?ref=v0.7.8' "$FIX/requests.log"; then
    ok "upstream pin: read at the ref our pin names"
else
    fail "upstream pin: not read at our ref" "$(cat "$FIX/requests.log")"
fi
rm -f "$FIX/example_bench.contents.v0.7.8"
run --check
expect_exit "upstream pin unreadable: --check" 2 "$RC"
expect_has "upstream pin unreadable: could not check, not pin not found" "| engine | - | ? | ? | - | **could not check**: request failed (exit 22) HTTP 404: Not Found"
printf 'BENCH_REF=v0.7.8\nBENCH_REF=v0.7.9\n' > "$PINS/deploy/bench.sh"
run --check
expect_has "upstream pin, ref ambiguous: pin not found" "(no single pin for bench above it)"

# 20. The real inventory: every location yields a pin. Offline, so every
#     component is "could not check" -- and none is "pin not found", and no
#     hold is stale.
OUT="$(UPSTREAM_FETCH=false UPSTREAM_PINS_FILE="$ROOT/hack/upstream-pins.yaml" UPSTREAM_PINS_ROOT="$ROOT" \
    bash "$SCRIPT" 2>&1)"
n_components="$(tr -d '\r' < "$ROOT/hack/upstream-pins.yaml" | yq '.components | length' -)"
expect_not "real inventory: every pin location is found" "pin not found"
expect_not "real inventory: every hold matches its pin" "hold is stale"
expect_has "real inventory: every component read, none answered" "**$n_components components:** 0 current, 0 behind, 0 with pins that disagree, $n_components could not be checked"

# 21. The real inventory's upstream pin (vLLM, harness default) against a copy
#     of the real llm-d-benchmark defaults.yaml: its yq expression reads the
#     engine tag through the file's anchors. The copy records the ref it was
#     taken at; once the llm-d-benchmark pin moves, the request names the new
#     ref, this copy no longer answers, and the case fails until it is
#     refreshed.
DEFAULTS="$ROOT/hack/testdata/upstream-versions/llm-d-benchmark-defaults.yaml"
rm -f "$FIX"/*
fix_ref="$(tr -d '\r' < "$DEFAULTS" | sed -n 's/^# ref: //p')"
fix_engine="$(tr -d '\r' < "$DEFAULTS" | sed -n 's/^# engine: //p')"
tr -d '\r' < "$DEFAULTS" > "$FIX/llm-d_llm-d-benchmark.contents.$fix_ref"
OUT="$(UPSTREAM_FETCH="$WORK/fetch" UPSTREAM_PINS_FILE="$ROOT/hack/upstream-pins.yaml" UPSTREAM_PINS_ROOT="$ROOT" \
    bash "$SCRIPT" 2>&1)"
if [ -n "$fix_ref" ] && [ -n "$fix_engine" ] \
    && printf '%s' "$OUT" | grep -qF "| vLLM (harness default) | $fix_engine |" \
    && printf '%s' "$OUT" | grep -qF "\`llm-d/llm-d-benchmark@$fix_ref:config/templates/values/defaults.yaml\`: $fix_engine"; then
    ok "real inventory: the harness default engine reads $fix_engine from llm-d-benchmark $fix_ref"
else
    fail "real inventory: harness default engine (copy ref '$fix_ref', engine '$fix_engine'); refresh $DEFAULTS if the llm-d-benchmark pin moved" \
        "$(printf '%s' "$OUT" | grep -F -e 'vLLM (harness default)' -e 'llm-d/llm-d-benchmark@')"
fi

# --- the workflow -----------------------------------------------------------
tr -d '\r' < "$WORKFLOW" > "$WORK/wf.yaml"

perms="$(yq -o=json -I=0 '.permissions' "$WORK/wf.yaml")"
if [ "$perms" = '{"contents":"read","issues":"write"}' ]; then
    ok "workflow: permissions are contents: read, issues: write"
else
    fail "workflow: permissions" "$perms"
fi
jobperms="$(yq '[.jobs[] | select(has("permissions"))] | length' "$WORK/wf.yaml")"
if [ "$jobperms" = 0 ]; then ok "workflow: no job widens the permissions"; else fail "workflow: a job sets its own permissions"; fi
cron="$(yq '.on.schedule[0].cron' "$WORK/wf.yaml")"
if [ "$(echo "$cron" | awk '{print $3 " " $4}')" = "1 *" ]; then
    ok "workflow: scheduled on the 1st of every month ($cron)"
else
    fail "workflow: schedule" "$cron"
fi
if [ "$(yq '.on | has("workflow_dispatch")' "$WORK/wf.yaml")" = true ]; then
    ok "workflow: can be run by hand"
else
    fail "workflow: no workflow_dispatch"
fi
unpinned="$(yq '.jobs[].steps[] | select(has("uses")) | .uses' "$WORK/wf.yaml" | grep -vE '@[0-9a-f]{40}$' || true)"
if [ -z "$unpinned" ] && [ "$(yq '[.jobs[].steps[] | select(has("uses"))] | length' "$WORK/wf.yaml")" -gt 0 ]; then
    ok "workflow: every action is pinned by commit"
else
    fail "workflow: actions not pinned by commit" "$unpinned"
fi
if yq '.jobs[].steps[] | select(.id == "report") | .run' "$WORK/wf.yaml" | grep -q 'hack/upstream-versions.sh --check'; then
    ok "workflow: the report step runs the check mode"
else
    fail "workflow: the report step does not run hack/upstream-versions.sh --check"
fi

# The issue step, against a stub gh that records what it was asked.
yq '.jobs[].steps[] | select(.id == "issue") | .run' "$WORK/wf.yaml" > "$WORK/issue.sh"
mkdir -p "$WORK/bin"
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
echo "gh $*" >> "$GH_LOG"
case "$1 $2" in
    "issue list")
        [ "${GH_LIST_FAIL:-}" = 1 ] && { echo "HTTP 502" >&2; exit 1; }
        [ -n "${GH_OPEN:-}" ] && echo "$GH_OPEN"
        ;;
    "label create")
        [ "${GH_LABEL_EXISTS:-}" = 1 ] && { echo "label with name \"$3\" already exists; use \`--force\` to update" >&2; exit 1; }
        [ "${GH_LABEL_FAIL:-}" = 1 ] && { echo "HTTP 403: Resource not accessible by integration" >&2; exit 1; }
        ;;
esac
exit 0
STUB
chmod +x "$WORK/bin/gh"
echo "report body" > "$WORK/report.md"

# issue STATUS OPEN [extra env...]: run the step; sets GH (the calls) and RC.
issue() {
    local status="$1" open="$2"
    shift 2
    : > "$WORK/gh.log"
    OUT="$(env PATH="$WORK/bin:$PATH" GH_LOG="$WORK/gh.log" GH_OPEN="$open" STATUS="$status" \
        REPORT_FILE="$WORK/report.md" ISSUE_LABEL=upstream-versions \
        ISSUE_TITLE="Upstream components behind our pins" GITHUB_REPOSITORY=o/r \
        "$@" bash "$WORK/issue.sh" 2>&1)"
    RC=$?
    GH="$(cat "$WORK/gh.log")"
}
gh_has() { if printf '%s' "$GH" | grep -qF -- "$2"; then ok "$1"; else fail "$1: no call [$2]" "$GH"; fi; }
gh_not() { if printf '%s' "$GH" | grep -qF -- "$2"; then fail "$1: unexpected call [$2]" "$GH"; else ok "$1"; fi; }

issue 0 ""
gh_not "issue: current and none open: nothing created or closed" "issue c"
issue 0 7
gh_has "issue: current closes the open issue" "gh issue close 7 --repo o/r"
issue 1 ""
gh_has "issue: behind and none open creates one, labelled" \
    "gh issue create --repo o/r --title Upstream components behind our pins --label upstream-versions --body-file $WORK/report.md"
issue 1 7
gh_has "issue: behind and one open updates its body" "gh issue edit 7 --repo o/r --body-file $WORK/report.md"
gh_not "issue: behind and one open creates no second issue" "gh issue create"
issue 2 7
gh_not "issue: unchecked components never close it" "gh issue close"
gh_has "issue: unchecked components update it" "gh issue edit 7"
issue 1 "" GH_LABEL_EXISTS=1
gh_has "issue: an existing label does not stop the create" "gh issue create"
issue 1 "" GH_LABEL_FAIL=1
if [ "$RC" -ne 0 ] && ! printf '%s' "$GH" | grep -qF "gh issue create" && printf '%s' "$OUT" | grep -qF "cannot create label"; then
    ok "issue: any other label failure fails the step and says so"
else
    fail "issue: a label failure (exit $RC)" "$OUT"
fi
issue 1 "" GH_LIST_FAIL=1
if [ "$RC" -ne 0 ] && ! printf '%s' "$GH" | grep -qF "gh issue create"; then
    ok "issue: a failed list fails the step and creates nothing"
else
    fail "issue: a failed list (exit $RC)" "$GH"
fi
: > "$WORK/report.md"
issue 1 7
if [ "$RC" -ne 0 ] && ! printf '%s' "$GH" | grep -qF "gh issue edit"; then
    ok "issue: an empty report is refused"
else
    fail "issue: an empty report (exit $RC)" "$GH"
fi
echo "report body" > "$WORK/report.md"
issue 5 7
if [ "$RC" -ne 0 ] && [ -z "$GH" ]; then
    ok "issue: an unknown status touches nothing"
else
    fail "issue: unknown status (exit $RC)" "$GH"
fi

total=$((PASS + FAIL))
echo "upstream-versions: $PASS passed, $FAIL failed, $total cases (expected $EXPECT_CASES)"
if [ "$FAIL" -ne 0 ] || [ "$total" -ne "$EXPECT_CASES" ]; then
    exit 1
fi
