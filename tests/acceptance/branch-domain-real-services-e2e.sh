#!/usr/bin/env bash
#
# G3 — T0205 Research Branch Domain. Real PostgreSQL.
#
# The two acceptance criteria, verbatim from the task DAG:
#
#   "branch head 可独立演化"
#   "main 与 branch state 不混淆"
#
# THE TESTS THIS PINS DO NOT EXIST YET. T0205 is not started, so the two names
# below are the contract the task package carries, in the convention T0204's
# suite already established (one test named after one criterion, in
# tests/integration, against real PostgreSQL). Until the task delivers them
# this gate is RED — loudly, naming what is missing — and that is the intended
# reading: a G3 that is green before the work exists certifies nothing. It is
# not a knot: these tests are satisfiable from T0205's own dependency closure
# (T0204), which is the whole point of splitting the job off rsg-real-services.
#
# THIS IS NOT rsg-real-services, AND IT DOES NOT REPLACE IT. That job drives
# the RSG HTTP surface (projects → branches → objects → versions → relations →
# :validate) and needs T0208 on main. T0208 depends on this task, so an
# HTTP-driven gate here can never be green. The whole-chain assertion stays on
# rsg-real-services for the 93 carriers that can satisfy it; T0205 has no check
# on the end-to-end RSG path, and that loss is recorded rather than implied
# away.
#
# What this adds over G2's migration-integration job, which runs the same
# package: the two tests are named, and a run in which either did not execute
# is a failure. `go test -run` exits 0 with "no tests to run" when nothing
# matches, so a branch implementation that simply never wrote them would
# otherwise turn this gate green while asserting nothing.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

PG_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"

if ! python3 scripts/pg-ready.py "$PG_URL" >/dev/null 2>&1; then
  echo "G3 branch-domain-real-services: FAILED — no PostgreSQL accepting connections at $PG_URL (make infra-up)" >&2
  exit 1
fi

SUITE="./tests/integration"
TESTS=(
  TestBranchHeadEvolvesIndependently
  TestMainAndBranchStateDoNotMix
)

selector="^($(IFS='|'; echo "${TESTS[*]}"))\$"

OUT="$(mktemp)"
trap 'rm -f "$OUT"' EXIT

echo "G3 branch-domain-real-services: T0205 against real PostgreSQL"
echo "  suite:  $SUITE"
echo "  pinned: ${TESTS[*]}"

POSTGRES_TEST_ADMIN_URL="$PG_URL" go test "$SUITE" \
  -count=1 -run "$selector" -v >"$OUT" 2>&1
rc=$?

if (( rc != 0 )); then
  echo
  echo "--- the pinned run failed (go test rc=$rc) ---"
  grep -E '^(=== RUN|--- (PASS|FAIL|SKIP)|FAIL|ok|panic|# )' "$OUT" | head -40
  echo "(full output: re-run the command above)" >&2
  exit 1
fi

missing=()
for t in "${TESTS[@]}"; do
  grep -q -- "--- PASS: ${t}\b" "$OUT" || missing+=("$t")
done
if (( ${#missing[@]} )); then
  echo "G3 branch-domain-real-services: FAILED — go test was green but these criteria-bearing tests did not run:" >&2
  printf '  - %s\n' "${missing[@]}" >&2
  echo >&2
  echo "They are the evidence for T0205's acceptance criteria:" >&2
  echo "  branch head 可独立演化            -> TestBranchHeadEvolvesIndependently" >&2
  echo "  main 与 branch state 不混淆      -> TestMainAndBranchStateDoNotMix" >&2
  echo "Deliver them in $SUITE; deleted, renamed or skipped, the task is not" >&2
  echo "verified however green the rest of the suite is." >&2
  exit 1
fi

for t in "${TESTS[@]}"; do
  printf 'ok   %s\n' "$t"
done
printf '\nG3 branch-domain-real-services: all checks passed against real PostgreSQL\n'
