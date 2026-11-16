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
# Exit 2 in either mode when the inventory itself cannot be read: there is no
# report to print then.
#
# "Latest" is the highest-versioned GitHub release that is neither a draft nor
# a prerelease (rc, alpha, beta and dev tags are skipped even when upstream
# does not flag them). An API failure is reported as one, never as current.
#
# Environment:
#   GITHUB_TOKEN          sent as a bearer token when set (60 requests an
#                         hour without one, which one run comes close to)
#   UPSTREAM_PINS_FILE    inventory (default hack/upstream-pins.yaml)
#   UPSTREAM_PINS_ROOT    directory the inventory paths are relative to
#                         (default the repository root)
#   UPSTREAM_API_URL      GitHub API base (default https://api.github.com)
#   UPSTREAM_MAX_PAGES    release pages of 100 read per repository (default 5)
#   UPSTREAM_FETCH        command run as `$UPSTREAM_FETCH <url>` instead of
#                         curl; the tests answer from fixtures with it
#   UPSTREAM_TODAY        date the report is stamped with (default today, UTC)
#
# Needs bash, curl, jq, yq (v4) and GNU sort/date.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PINS_FILE="${UPSTREAM_PINS_FILE:-$ROOT/hack/upstream-pins.yaml}"
PINS_ROOT="${UPSTREAM_PINS_ROOT:-$ROOT}"
API_URL="${UPSTREAM_API_URL:-https://api.github.com}"
MAX_PAGES="${UPSTREAM_MAX_PAGES:-5}"
TODAY="${UPSTREAM_TODAY:-$(date -u +%Y-%m-%d)}"

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

# fetch prints the body of a GitHub API GET, and fails on any HTTP error --
# a rate-limit 403 included, so it can never read as an empty release list.
fetch() {
    if [ -n "${UPSTREAM_FETCH:-}" ]; then
        "$UPSTREAM_FETCH" "$1"
        return
    fi
    local args=(-fsSL --retry 3 --retry-delay 5 --retry-all-errors
        -H "Accept: application/vnd.github+json"
        -H "X-GitHub-Api-Version: 2022-11-28")
    if [ -n "${GITHUB_TOKEN:-}" ]; then
        args+=(-H "Authorization: Bearer $GITHUB_TOKEN")
    fi
    curl "${args[@]}" "$1"
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

# read_pin FILE KIND EXPR prints every value the pin location yields, one per
# line. Nothing printed means the pin was not found.
read_pin() {
    local file="$1" kind="$2" expr="$3" path="$PINS_ROOT/$1"
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
            and (((.pins // []) | length > 0) or .tracked == true))' "$WORK/pins.json" >/dev/null; then
    echo "upstream-versions: $PINS_FILE needs components[], each with name, repo, and pins[] (or tracked: true)" >&2
    exit 2
fi

n_current=0 n_behind=0 n_disagree=0 n_error=0 n_tracked=0
: > "$WORK/rows.md"
: > "$WORK/locations.md"

count="$(jq '.components | length' "$WORK/pins.json")"
for ((i = 0; i < count; i++)); do
    jq -c ".components[$i]" "$WORK/pins.json" > "$WORK/c.json"
    name="$(jq -r '.name' "$WORK/c.json")"
    repo="$(jq -r '.repo' "$WORK/c.json")"
    prefix="$(jq -r '.tagPrefix // ""' "$WORK/c.json")"
    tracked="$(jq -r '.tracked // false' "$WORK/c.json")"
    npins="$(jq '(.pins // []) | length' "$WORK/c.json")"

    # --- our pins -----------------------------------------------------------
    missing="" pinned=""
    : > "$WORK/values"
    {
        printf -- '- **%s** ([%s](https://github.com/%s))\n' "$name" "$repo" "$repo"
        [ "$npins" -gt 0 ] || printf '  - not pinned in this repository\n'
    } >> "$WORK/locations.md"
    for ((j = 0; j < npins; j++)); do
        file="$(jq -r ".pins[$j].file" "$WORK/c.json")"
        if jq -e ".pins[$j] | has(\"yq\")" "$WORK/c.json" >/dev/null; then
            kind=yq
        else
            kind=regex
        fi
        expr="$(jq -r ".pins[$j].$kind // empty" "$WORK/c.json")"
        found="$(read_pin "$file" "$kind" "$expr")"
        if [ -z "$expr" ] || [ -z "$found" ]; then
            missing="${missing:+$missing, }$file"
            printf '  - `%s`: **pin not found**\n' "$file" >> "$WORK/locations.md"
            continue
        fi
        printf '%s\n' "$found" >> "$WORK/values"
        printf '  - `%s`: %s\n' "$file" "$(printf '%s' "$found" | sort -u | paste -sd ' ' - | sed 's/ /, /g')" >> "$WORK/locations.md"
    done
    # Compared as versions, so 0.8.0 in one file and v0.8.0 in another agree.
    distinct="$(sed '/^$/d' "$WORK/values" | while IFS= read -r v; do norm "$v" "$prefix"; echo; done | sort -u -V)"
    ndistinct="$(printf '%s' "$distinct" | grep -c . || true)"
    pinned="$(sort -u "$WORK/values" | sed '/^$/d' | paste -sd ' ' - | sed 's/ /, /g')"
    # The newest of our values, for the comparison when they disagree.
    ours=""
    if [ "$ndistinct" -gt 0 ]; then
        ours="$(printf '%s\n' "$distinct" | tail -n 1)"
    fi

    # --- upstream releases --------------------------------------------------
    api_error=""
    : > "$WORK/releases.json"
    echo '[]' > "$WORK/releases.json"
    for ((page = 1; page <= MAX_PAGES; page++)); do
        rc=0
        fetch "$API_URL/repos/$repo/releases?per_page=100&page=$page" > "$WORK/page.json" 2>"$WORK/fetch.err" || rc=$?
        if [ "$rc" -ne 0 ]; then
            api_error="release list request failed (exit $rc) $(tr '\n' ' ' < "$WORK/fetch.err" | cut -c1-160)"
            break
        fi
        if ! jq -e 'type == "array"' "$WORK/page.json" >/dev/null 2>&1; then
            api_error="unexpected answer: $(jq -r '.message? // empty' "$WORK/page.json" 2>/dev/null | cut -c1-160)"
            break
        fi
        jq -s '.[0] + .[1]' "$WORK/releases.json" "$WORK/page.json" > "$WORK/merged.json"
        mv "$WORK/merged.json" "$WORK/releases.json"
        [ "$(jq 'length' "$WORK/page.json")" -ge 100 ] || break
        # Releases come newest first: once a page reaches our pin, the
        # releases after it are not needed to count how far behind it is.
        if [ -n "$ours" ]; then
            oldest="$(jq -r --arg p "$prefix" '[.[] | select(.tag_name | startswith($p)) | .tag_name | ltrimstr($p) | ltrimstr("v")] | last // empty' "$WORK/page.json")"
            if [ -n "$oldest" ] && ! vgt "$oldest" "$ours"; then
                break
            fi
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

**$total components:** $n_current current, $n_behind behind, $n_disagree with pins that disagree, $n_error could not be checked, $n_tracked tracked but not pinned.

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

if [ "$MODE" = check ]; then
    if [ "$n_error" -gt 0 ]; then exit 2; fi
    if [ $((n_behind + n_disagree)) -gt 0 ]; then exit 1; fi
fi
exit 0
