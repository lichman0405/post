#!/usr/bin/env bash
#
# G3 — T0207 Progressive Validation Gates. Real PostgreSQL.
#
# The two acceptance criteria, verbatim from the task DAG:
#
#   "不同 gate 严格度不同且可解释"
#   "command 再次 server validate"
#
# THE TESTS THIS PINS DO NOT EXIST YET. T0207 is not started, so the two names
# below are the contract the task package carries, in the convention T0204's
# suite already established (one test named after one criterion, in
# tests/integration, against real PostgreSQL — here against
# validation_results). Until the task delivers them this gate is RED, loudly,
# naming what is missing; it is satisfiable from T0207's own dependency
# closure (T0201, T0204), which is the point of splitting the job off
# rsg-real-services.
#
# The second criterion is the one worth stating plainly, because it is the one
# a gate can only check from outside: "command 再次 server validate" means a
# caller having validated is not a reason for the server to skip validating.
# The pinned test must therefore show the server refusing what the caller
# claims to have checked, not merely that a second check exists.
#
# THIS IS NOT rsg-real-services, AND IT DOES NOT REPLACE IT. That job drives
# the RSG HTTP surface, `:validate` among it, and needs T0208 on main; T0208
# depends on this task, so an HTTP-driven gate here can never be green. The
# whole-chain assertion stays on rsg-real-services for the 93 carriers that can
# satisfy it; T0207 has no check on the end-to-end RSG path, and that loss is
# recorded rather than implied away.
#
# What this adds over G2's migration-integration job, which runs the same
# package: the two tests are named, and a run in which either did not execute
# is a failure. `go test -run` exits 0 with "no tests to run" when nothing
# matches, so a validation layer that simply never wrote them would otherwise
# turn this gate green while asserting nothing.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

PG_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"

if ! python3 scripts/pg-ready.py "$PG_URL" >/dev/null 2>&1; then
  echo "G3 validation-gates-real-services: FAILED — no PostgreSQL accepting connections at $PG_URL (make infra-up)" >&2
  exit 1
fi

SUITE="./tests/integration"
TESTS=(
  TestGatesDifferInStrictnessAndSayWhy
  TestServerRevalidatesWhatTheCommandValidated
)

selector="^($(IFS='|'; echo "${TESTS[*]}"))\$"

OUT="$(mktemp)"
trap 'rm -f "$OUT"' EXIT

echo "G3 validation-gates-real-services: T0207 against real PostgreSQL"
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
  echo "G3 validation-gates-real-services: FAILED — go test was green but these criteria-bearing tests did not run:" >&2
  printf '  - %s\n' "${missing[@]}" >&2
  echo >&2
  echo "They are the evidence for T0207's acceptance criteria:" >&2
  echo "  不同 gate 严格度不同且可解释    -> TestGatesDifferInStrictnessAndSayWhy" >&2
  echo "  command 再次 server validate    -> TestServerRevalidatesWhatTheCommandValidated" >&2
  echo "Deliver them in $SUITE; deleted, renamed or skipped, the task is not" >&2
  echo "verified however green the rest of the suite is." >&2
  exit 1
fi

for t in "${TESTS[@]}"; do
  printf 'ok   %s\n' "$t"
done
printf '\nG3 validation-gates-real-services: all checks passed against real PostgreSQL\n'
