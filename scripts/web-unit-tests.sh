#!/usr/bin/env bash
#
# Web unit-test gate: run every test file under apps/web/lib, not one
# hardcoded name.
#
# Why a glob (T0101): until now make check, make test and CI all ran exactly
# `--test "apps/web/lib/config.test.mjs"`, by name. Any test file added next
# to it was therefore never executed by any gate — apps/web/lib/
# correlation.test.mjs had been sitting in the tree, green and unrun, since it
# was written, and T0101's auth.test.mjs would have joined it. A gate that
# only runs the tests someone remembered to list is not a gate.
#
# Why the zero-match guard: a glob that matches nothing makes `node --test`
# exit 0 reporting "tests 0". That is a green stage that tested nothing —
# the same fail-open shape as an empty baseline. If the tests are ever moved
# or renamed, this must fail loudly rather than quietly stop testing.
#
# WEB_TEST_GLOB overrides the pattern so the guard itself is fixture-testable
# (scripts/tests/web-tests-unit-test.sh) without touching real test files.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GLOB="${WEB_TEST_GLOB:-apps/web/lib/*.test.mjs}"

# Count matches with the shell, so the guard does not depend on how the test
# runner reports an empty run.
shopt -s nullglob
matches=( $GLOB )
shopt -u nullglob

if (( ${#matches[@]} == 0 )); then
  echo "web-tests: \"$GLOB\" matched no test file." >&2
  echo "web-tests: refusing to report success for a stage that ran nothing." >&2
  echo "web-tests: if the tests moved, point this glob at them; if they were" >&2
  echo "web-tests: deleted, delete this gate deliberately — do not let it" >&2
  echo "web-tests: pass vacuously." >&2
  exit 1
fi

echo "web-tests: ${#matches[@]} file(s) matched by \"$GLOB\""
# The quoted pattern is passed through to node, which expands it itself;
# passing the already-expanded list would lose node's own file discovery.
node --disable-warning=MODULE_TYPELESS_PACKAGE_JSON --test "$GLOB"
