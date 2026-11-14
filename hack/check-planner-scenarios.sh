#!/usr/bin/env bash
#
# Runs the capacity planner page's scenario logic on fixtures
# (hack/check-planner-scenarios.mjs on internal/whatifapi/page/scenario.js).
#
# The page saves scenarios in the browser and imports them from files, and
# both are untrusted when read back. The Go tests can only read the page's
# text: a check that admits everything, an escape that escapes nothing, or a
# store edit that erases another version's entries all read fine there. This
# runs the functions.
#
# It needs node. CI runners have it; without it the check fails under CI and
# is skipped, loudly, elsewhere.
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

if ! command -v node >/dev/null 2>&1; then
  if [ -n "${CI:-}" ]; then
    echo "FAIL: hack/check-planner-scenarios.sh needs node, and this CI runner has none." >&2
    exit 1
  fi
  echo "SKIPPED: hack/check-planner-scenarios.sh needs node; the page's scenario logic was NOT checked." >&2
  exit 0
fi

node hack/check-planner-scenarios.mjs internal/whatifapi/page/scenario.js
