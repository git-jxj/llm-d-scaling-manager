#!/usr/bin/env bash
#
# Compares the upstream components we pin with their latest GitHub releases.
#
# The pins themselves live where they are used (Makefile, deploy scripts,
# workflows, benchmark scenarios). hack/upstream-pins.yaml says where each one
# is and how to read it, so this report reads the real pin and cannot drift
# from it. A component pinned in several places must carry one value in all of
# them; files that disagree are reported as such.
#
# Usage:
#   hack/upstream-versions.sh            print the report, exit 0
#   hack/upstream-versions.sh --check    print the report, then exit
#                                          0  every pin is current
#                                          1  a pin is behind, or pins disagree
#                                          2  a component could not be checked
#                                             (pin not found, API error)
#
# Exit 2 in either mode when the inventory itself cannot be read (there is no
# report to print then), or when a network failure or a rate limit stopped
# the run (the report it prints is mostly "not asked").
#
# "Latest" is the highest-versioned GitHub release that is neither a draft nor
# a prerelease (rc, alpha, beta and dev tags are skipped even when upstream
# does not flag them). An API failure is reported as one, never as current.
# A network failure or a rate limit stops the run from asking again: every
# component not yet checked is reported as not checked, and --check exits 2.
#
# A component held on purpose (`hold` in the inventory) is reported as held,
# not behind, until its review date passes.
#
# Environment:
#   GITHUB_TOKEN          sent as a bearer token when set (60 requests an
#                         hour without one; one run needs about 30)
#   UPSTREAM_PINS_FILE    inventory (default hack/upstream-pins.yaml)
#   UPSTREAM_PINS_ROOT    directory the inventory paths are relative to
#                         (default the repository root)
#   UPSTREAM_API_URL      GitHub API base (default https://api.github.com)
#   UPSTREAM_MAX_PAGES    release pages of 100 read per repository (default 5)
#   UPSTREAM_FETCH        command run as `$UPSTREAM_FETCH <url>` instead of
#                         curl; the tests answer from fixtures with it. It
#                         prints the body and exits 0, exits 75 to stop the
#                         run asking again, or exits non-zero otherwise
#   UPSTREAM_CURL_RETRY   curl retries per request (default 2; each attempt
#                         times out after 30 s)
#   UPSTREAM_TODAY        date the report is stamped with, and holds are
#                         checked against (default today, UTC)
#
# Needs bash, curl, jq, yq (v4) and GNU sort/date.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PINS_FILE="${UPSTREAM_PINS_FILE:-$ROOT/hack/upstream-pins.yaml}"
PINS_ROOT="${UPSTREAM_PINS_ROOT:-$ROOT}"
API_URL="${UPSTREAM_API_URL:-https://api.github.com}"
MAX_PAGES="${UPSTREAM_MAX_PAGES:-5}"
CURL_RETRY="${UPSTREAM_CURL_RETRY:-2}"
TODAY="${UPSTREAM_TODAY:-$(date -u +%Y-%m-%d)}"
# fetch exits with this when asking again is pointless: the network is down,
# or GitHub refused us (rate limit).
STOP_RC=75

MODE=report
case "${1:-}" in
    "") ;;
    --check) MODE=check ;;
    -h|--help) sed -n '2,/^$/s/^# \{0,1\}//p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown argument: $1 (want --check or nothing)" >&2; exit 2 ;;
esac

for tool in jq yq sort date; do
    command -v "$tool" >/dev/null 2>&1 || { echo "upstream-versions: $tool not found" >&2; exit 2; }
done
if [ -z "${UPSTREAM_FETCH:-}" ]; then
    command -v curl >/dev/null 2>&1 || { echo "upstream-versions: curl not found" >&2; exit 2; }
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# fetch prints the body of a GitHub API GET, and fails on any HTTP error, so
# an error can never read as an empty release list. It exits $STOP_RC on a
# network failure, a 403 or a 429 (GitHub's rate limits): every later request
# would fail the same way, slowly.
fetch() {
    if [ -n "${UPSTREAM_FETCH:-}" ]; then
        "$UPSTREAM_FETCH" "$1"
        return
    fi
    local code rc=0 msg
    local args=(-sSL --connect-timeout 10 --max-time 30
        --retry "$CURL_RETRY" --retry-delay 2 --retry-all-errors
        -o "$WORK/body" -w '%{http_code}'
        -H "Accept: application/vnd.github+json"
        -H "X-GitHub-Api-Version: 2022-11-28")
    if [ -n "${GITHUB_TOKEN:-}" ]; then
        args+=(-H "Authorization: Bearer $GITHUB_TOKEN")
    fi
    : > "$WORK/body"
    code="$(curl "${args[@]}" "$1")" || rc=$?
    if [ "$rc" -ne 0 ]; then
        echo "network failure (curl exit $rc)" >&2
        return "$STOP_RC"
    fi
    case "$code" in
        2??) cat "$WORK/body"; return 0 ;;
    esac
    msg="$({ jq -r '.message? // empty' "$WORK/body" 2>/dev/null || head -c 80 "$WORK/body"; } | tr '\n' ' ' | cut -c1-120)"
    echo "HTTP $code${msg:+: $msg}" >&2
    case "$code" in
        403|429) return "$STOP_RC" ;;
    esac
    return 22
}

# STOPPED holds why the run stopped asking GitHub; empty while it still asks.
STOPPED=""

# ask URL OUT: fetch URL into OUT, or explain in ASK_ERROR why not. Returns 0
# when OUT holds an answer.
ask() {
    local rc=0
    ASK_ERROR=""
    if [ -n "$STOPPED" ]; then
        ASK_ERROR="not asked: $STOPPED"
        return 1
    fi
    fetch "$1" > "$2" 2>"$WORK/fetch.err" || rc=$?
    [ "$rc" -eq 0 ] && return 0
    # curl repeats its error once per retry; say it once.
    why="$(awk '!seen[$0]++' "$WORK/fetch.err" | tr '\n' ' ' | cut -c1-200)"
    ASK_ERROR="request failed (exit $rc) $why"
    if [ "$rc" -eq "$STOP_RC" ]; then
        STOPPED="an earlier request failed: $why"
    fi
    return 1
}

# vgt A B: true when version A sorts strictly after version B.
vgt() {
    [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n 1)" = "$1" ]
}

# norm strips a tag prefix and a leading v, so 0.8.0 and v0.8.0 compare equal.
norm() {
    local v="${1#"$2"}"
    printf '%s' "${v#v}"
}

days_between() {
    local a b
    a="$(date -u -d "$1" +%s)" || return 1
    b="$(date -u -d "$2" +%s)" || return 1
    echo $(((b - a) / 86400))
}

# read_pin PATH KIND EXPR prints every value the pin location yields, one per
# line. Nothing printed means the pin was not found.
read_pin() {
    local path="$1" kind="$2" expr="$3"
    [ -f "$path" ] || return 0
    case "$kind" in
        regex)
            # The first capture group of every matching line. sed reads the
            # same ERE grep does; \001 as the delimiter, so a pattern may hold
            # any printable character.
            tr -d '\r' < "$path" | grep -E -- "$expr" | sed -E "s"$'\001'".*${expr}.*"$'\001''\1'$'\001' || true
            ;;
        yq)
            tr -d '\r' < "$path" | yq -r "$expr" - 2>/dev/null | grep -v -x -e null -e '' || true
            ;;
    esac
}

# The inventory, as JSON. A broken inventory is a broken tool, not a finding.
if ! tr -d '\r' < "$PINS_FILE" | yq -o=json '.' - > "$WORK/pins.json" 2>"$WORK/yq.err"; then
    echo "upstream-versions: cannot parse $PINS_FILE:" >&2
    cat "$WORK/yq.err" >&2
    exit 2
fi
if ! jq -e '(.components | type == "array" and length > 0)
        and all(.components[]; (.name | type == "string") and (.repo | type == "string")
            and ((.pins // []) | type == "array")
            and (((.pins // []) | length > 0) or .tracked == true)
            and all((.pins // [])[]; (.file | type == "string")
                and ((has("from") | not) or ((.from | type == "string") and (.at | type == "string")))))' \
        "$WORK/pins.json" >/dev/null; then
    echo "upstream-versions: $PINS_FILE needs components[], each with name, repo, and pins[] (or tracked: true); a pin with \`from\` needs \`at\`" >&2
    exit 2
fi
# A hold needs all three fields, and a review date that is a date: a hold
# without one never ends, which is the nagging issue's opposite failure.
if ! jq -e 'all(.components[] | select(has("hold")) | .hold;
            (.version | type == "string") and (.reason | type == "string") and (.reason | length > 0)
            and (.until | type == "string") and (.until | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$")))' \
        "$WORK/pins.json" >/dev/null; then
    echo "upstream-versions: $PINS_FILE: every hold needs version, reason, and until (YYYY-MM-DD)" >&2
    exit 2
fi

n_current=0 n_behind=0 n_disagree=0 n_error=0 n_tracked=0 n_held=0
: > "$WORK/rows.md"
: > "$WORK/locations.md"
# name<TAB>pinned value of every component read so far, for `at`.
: > "$WORK/ours.tsv"

count="$(jq '.components | length' "$WORK/pins.json")"
for ((i = 0; i < count; i++)); do
    jq -c ".components[$i]" "$WORK/pins.json" > "$WORK/c.json"
    name="$(jq -r '.name' "$WORK/c.json")"
    repo="$(jq -r '.repo' "$WORK/c.json")"
    prefix="$(jq -r '.tagPrefix // ""' "$WORK/c.json")"
    tracked="$(jq -r '.tracked // false' "$WORK/c.json")"
    npins="$(jq '(.pins // []) | length' "$WORK/c.json")"

    # --- our pins -----------------------------------------------------------
    missing="" pinned="" remote_error=""
    : > "$WORK/values"
    {
        printf -- '- **%s** ([%s](https://github.com/%s))\n' "$name" "$repo" "$repo"
        [ "$npins" -gt 0 ] || printf '  - not pinned in this repository\n'
    } >> "$WORK/locations.md"
    for ((j = 0; j < npins; j++)); do
        file="$(jq -r ".pins[$j].file" "$WORK/c.json")"
        from="$(jq -r ".pins[$j].from // empty" "$WORK/c.json")"
        if jq -e ".pins[$j] | has(\"yq\")" "$WORK/c.json" >/dev/null; then
            kind=yq
        else
            kind=regex
        fi
        expr="$(jq -r ".pins[$j].$kind // empty" "$WORK/c.json")"
        where="$file" path="$PINS_ROOT/$file"
        if [ -n "$from" ]; then
            # A pin that lives upstream: FILE in repository FROM, at the
            # version this repository pins for component AT.
            at="$(jq -r ".pins[$j].at" "$WORK/c.json")"
            ref="$(awk -F '\t' -v n="$at" '$1 == n { print $2; exit }' "$WORK/ours.tsv")"
            if [ -z "$ref" ]; then
                missing="${missing:+$missing, }$from:$file (no single pin for $at above it)"
                printf '  - `%s:%s`: **pin not found** (no single pin for %s above it)\n' "$from" "$file" "$at" >> "$WORK/locations.md"
                continue
            fi
            where="$from@$ref:$file" path="$WORK/remote"
            if ask "$API_URL/repos/$from/contents/$file?ref=$ref" "$WORK/remote.json" \
                && jq -r '.content' "$WORK/remote.json" 2>/dev/null | tr -d '\n' | base64 -d > "$WORK/remote" 2>/dev/null \
                && [ -s "$WORK/remote" ]; then
                :
            else
                # Not knowing the upstream file is not knowing the pin: an
                # API error, not a pin that moved.
                remote_error="${ASK_ERROR:-unexpected answer for $where}"
                printf '  - `%s`: **could not read**\n' "$where" >> "$WORK/locations.md"
                continue
            fi
        fi
        found="$(read_pin "$path" "$kind" "$expr")"
        if [ -z "$expr" ] || [ -z "$found" ]; then
            missing="${missing:+$missing, }$where"
            printf '  - `%s`: **pin not found**\n' "$where" >> "$WORK/locations.md"
            continue
        fi
        printf '%s\n' "$found" >> "$WORK/values"
        printf '  - `%s`: %s\n' "$where" "$(printf '%s' "$found" | sort -u | paste -sd ' ' - | sed 's/ /, /g')" >> "$WORK/locations.md"
    done
    if [ "$(sort -u "$WORK/values" | sed '/^$/d' | wc -l)" -eq 1 ]; then
        printf '%s\t%s\n' "$name" "$(sed '/^$/d' "$WORK/values" | head -n 1)" >> "$WORK/ours.tsv"
    fi
    # Compared as versions, so 0.8.0 in one file and v0.8.0 in another agree.
    distinct="$(sed '/^$/d' "$WORK/values" | while IFS= read -r v; do norm "$v" "$prefix"; echo; done | sort -u -V)"
    ndistinct="$(printf '%s' "$distinct" | grep -c . || true)"
    pinned="$(sort -u "$WORK/values" | sed '/^$/d' | paste -sd ' ' - | sed 's/ /, /g')"
    # The newest of our values, for the comparison when they disagree.
    ours=""
    if [ "$ndistinct" -gt 0 ]; then
        ours="$(printf '%s\n' "$distinct" | tail -n 1)"
    fi

    # --- hold -------------------------------------------------------------
    hold_version="$(jq -r '.hold.version // empty' "$WORK/c.json")"
    hold_reason="$(jq -r '.hold.reason // empty' "$WORK/c.json")"
    hold_until="$(jq -r '.hold.until // empty' "$WORK/c.json")"
    stale_hold=""
    if [ -n "$hold_version" ] && [ "$ndistinct" -eq 1 ] && [ "$(norm "$hold_version" "$prefix")" != "$ours" ]; then
        # The pin moved and the hold did not: it no longer says anything.
        stale_hold="held at $hold_version, pinned at $pinned; update or remove the hold"
    fi

    # --- upstream releases --------------------------------------------------
    api_error="$remote_error"
    : > "$WORK/releases.json"
    echo '[]' > "$WORK/releases.json"
    for ((page = 1; page <= MAX_PAGES; page++)); do
        [ -z "$api_error" ] || break
        if ! ask "$API_URL/repos/$repo/releases?per_page=100&page=$page" "$WORK/page.json"; then
            case "$ASK_ERROR" in
                "not asked"*) api_error="$ASK_ERROR" ;;
                *) api_error="release list $ASK_ERROR" ;;
            esac
            break
        fi
        if ! jq -e 'type == "array"' "$WORK/page.json" >/dev/null 2>&1; then
            # The body may not be JSON at all (an HTML error page).
            api_error="unexpected answer: $({ jq -r '.message? // empty' "$WORK/page.json" 2>/dev/null || head -c 80 "$WORK/page.json"; } | tr '\n' ' ' | cut -c1-160)"
            break
        fi
        jq -s '.[0] + .[1]' "$WORK/releases.json" "$WORK/page.json" > "$WORK/merged.json"
        mv "$WORK/merged.json" "$WORK/releases.json"
        [ "$(jq 'length' "$WORK/page.json")" -ge 100 ] || break
        # Releases come newest first by date: once a page holds our pin, the
        # pages after it were published before it. Not "a page reaches a
        # version below ours": a project that patches several branches
        # (Kubernetes) puts old-branch patches among new releases.
        if [ -n "$ours" ] && jq -e --arg p "$prefix" --arg v "$ours" \
                'any(.[]; .tag_name | startswith($p) and (ltrimstr($p) | ltrimstr("v")) == $v)' \
                "$WORK/page.json" >/dev/null; then
            break
        fi
    done
    complete=true
    [ "$page" -le "$MAX_PAGES" ] || complete=false

    if [ -z "$api_error" ]; then
        # Stable releases only: not a draft, not flagged prerelease, and a
        # plain dotted version (a .postN suffix allowed) after the prefix.
        jq -r --arg p "$prefix" '
            [ .[]
              | select((.draft | not) and (.prerelease | not))
              | select(.tag_name | startswith($p))
              | { tag: .tag_name,
                  v: (.tag_name | ltrimstr($p) | ltrimstr("v")),
                  date: ((.published_at // .created_at // "") | .[0:10]) }
              | select(.v | test("^[0-9]+(\\.[0-9]+)*(\\.post[0-9]+)?$")) ]
            | .[] | [.v, .tag, .date] | @tsv' "$WORK/releases.json" | sort -t $'\t' -k1,1 -V -r > "$WORK/stable.tsv"
        if [ ! -s "$WORK/stable.tsv" ]; then
            api_error="no stable release found${prefix:+ with tag prefix $prefix}"
        fi
    fi

    latest_v="" latest_tag="" latest_date=""
    if [ -z "$api_error" ]; then
        IFS=$'\t' read -r latest_v latest_tag latest_date < "$WORK/stable.tsv"
    fi

    # --- verdict ------------------------------------------------------------
    behind="" link="https://github.com/$repo/releases"
    if [ -n "$missing" ]; then
        status="**pin not found**: $missing"
        n_error=$((n_error + 1))
    elif [ -n "$stale_hold" ]; then
        status="**hold is stale**: $stale_hold"
        n_error=$((n_error + 1))
    elif [ -n "$api_error" ]; then
        status="**could not check**: $api_error"
        n_error=$((n_error + 1))
    elif [ "$npins" -eq 0 ] && [ "$tracked" = true ]; then
        status="tracked, not pinned"
        pinned="-"
        n_tracked=$((n_tracked + 1))
    else
        nnewer=0
        while IFS=$'\t' read -r v _ _; do
            if vgt "$v" "$ours"; then nnewer=$((nnewer + 1)); fi
        done < "$WORK/stable.tsv"
        pin_date="$(awk -F '\t' -v v="$ours" '$1 == v { print $3; exit }' "$WORK/stable.tsv")"
        pin_tag="$(awk -F '\t' -v v="$ours" '$1 == v { print $2; exit }' "$WORK/stable.tsv")"
        if [ -z "$pin_tag" ]; then
            case "$latest_tag" in
                "${prefix}v"*) pin_tag="${prefix}v$ours" ;;
                *) pin_tag="${prefix}$ours" ;;
            esac
        fi
        if [ "$nnewer" -gt 0 ]; then
            more=""
            [ "$complete" = true ] || [ -n "$pin_date" ] || more="+"
            behind="${nnewer}${more} release$([ "$nnewer" -eq 1 ] && [ -z "$more" ] || echo s)"
            if [ -n "$pin_date" ] && [ -n "$latest_date" ]; then
                behind="$behind, $(days_between "$pin_date" "$latest_date") days"
            fi
            link="https://github.com/$repo/compare/$pin_tag...$latest_tag"
        fi
        if [ "$ndistinct" -gt 1 ]; then
            status="**pins disagree**"
            n_disagree=$((n_disagree + 1))
        elif [ "$nnewer" -gt 0 ] && [ -n "$hold_version" ] && [[ ! "$TODAY" > "$hold_until" ]]; then
            status="held until $hold_until: $hold_reason"
            n_held=$((n_held + 1))
        elif [ "$nnewer" -gt 0 ] && [ -n "$hold_version" ]; then
            status="behind (hold ended $hold_until: $hold_reason)"
            n_behind=$((n_behind + 1))
        elif [ "$nnewer" -gt 0 ]; then
            status="behind"
            n_behind=$((n_behind + 1))
        elif vgt "$ours" "$latest_v"; then
            status="ahead of the latest release"
            n_current=$((n_current + 1))
        else
            status="current"
            n_current=$((n_current + 1))
        fi
    fi

    [ -n "$pinned" ] || pinned="-"
    if [ "$latest_tag" ]; then
        latest_cell="$latest_tag" date_cell="$latest_date"
    else
        latest_cell="?" date_cell="?"
    fi
    if [ "$link" = "https://github.com/$repo/releases" ]; then
        link_cell="[releases]($link)"
    else
        link_cell="[compare]($link)"
    fi
    printf '| %s | %s | %s | %s | %s | %s | %s |\n' \
        "$name" "$pinned" "$latest_cell" "$date_cell" "${behind:--}" "$status" "$link_cell" >> "$WORK/rows.md"
done

total="$count"
cat <<EOF
Pinned upstream components compared with their latest stable GitHub release on $TODAY.
Generated by \`hack/upstream-versions.sh\` from \`hack/upstream-pins.yaml\`.

**$total components:** $n_current current, $n_behind behind, $n_disagree with pins that disagree, $n_error could not be checked, $n_tracked tracked but not pinned, $n_held held on purpose.
${STOPPED:+
**Stopped asking GitHub**: $STOPPED. A row marked *not asked* is neither current nor behind: it was not checked.
}
| Component | Pinned | Latest | Released | Behind by | Status | Changes |
|---|---|---|---|---|---|---|
EOF
cat "$WORK/rows.md"
cat <<EOF

### Where each pin lives

EOF
cat "$WORK/locations.md"
cat <<'EOF'

To act on this report: read the upstream release notes for breaking changes,
move every location of a pin in one pull request, and rerun the e2e or benchmark
that exercises the component. See docs/developer-guide/upstream-dependencies.md.
EOF

if [ -n "$STOPPED" ]; then
    echo "upstream-versions: stopped asking GitHub: $STOPPED" >&2
    exit 2
fi
if [ "$MODE" = check ]; then
    if [ "$n_error" -gt 0 ]; then exit 2; fi
    if [ $((n_behind + n_disagree)) -gt 0 ]; then exit 1; fi
fi
exit 0
