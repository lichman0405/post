#!/usr/bin/env bash
#
# G3 — T0204 Project State 与 State Commit. Real PostgreSQL.
#
# The two acceptance criteria, verbatim from the task DAG, and the test that
# evidences each (the house convention is one test named after one criterion;
# T0204's own suite states them in those words):
#
#   "每次 semantic write 形成可追溯 state transition"
#       TestStateCommitCreatesTraceableTransition
#   "失败 transaction 不产生半状态"
#       TestStateCommitFailedTransactionLeavesNoHalfState
#
# THIS IS NOT rsg-real-services, AND IT DOES NOT REPLACE IT. That job drives
# the RSG HTTP surface (projects → branches → objects → versions → relations →
# :validate) and needs T0208 plus T0205 and T0207 on main. None of those routes
# exists yet — T0209 RSG Query API builds the HTTP surface — and T0205/T0207
# depend on this task, so an HTTP-driven gate here can never be green. This
# script pins what T0204 itself delivers, on its own dependency closure
# (T0202, T0203), and the whole-chain assertion stays on rsg-real-services for
# the 93 carriers that can satisfy it. The loss is recorded, not implied away:
# T0204 has no check on the end-to-end RSG path, and neither it nor any later
# task can have one until T0208 exists.
#
# What this adds over G2's migration-integration job, which runs the same
# package: the suite is run whole, so the verdict on these two criteria is
# carried by whichever tests happen to be there. Here the two tests are named,
# and a run in which either did not execute is a failure. `go test -run` exits
# 0 with "no tests to run" when nothing matches, so deleting or renaming the
# criteria-bearing test would otherwise turn this gate green while asserting
# nothing — the check below refuses that explicitly.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

PG_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"

if ! python3 scripts/pg-ready.py "$PG_URL" >/dev/null 2>&1; then
  echo "G3 state-commit-real-services: FAILED — no PostgreSQL accepting connections at $PG_URL (make infra-up)" >&2
  exit 1
fi

# Named, one per criterion. The order is the DAG's.
TESTS=(
  TestStateCommitCreatesTraceableTransition
  TestStateCommitFailedTransactionLeavesNoHalfState
)

selector="^($(IFS='|'; echo "${TESTS[*]}"))\$"

OUT="$(mktemp)"
trap 'rm -f "$OUT"' EXIT

echo "G3 state-commit-real-services: T0204 against real PostgreSQL"
echo "  pinned: ${TESTS[*]}"

POSTGRES_TEST_ADMIN_URL="$PG_URL" go test ./tests/integration \
  -count=1 -run "$selector" -v >"$OUT" 2>&1
rc=$?

if (( rc != 0 )); then
  echo
  echo "--- the pinned run failed (go test rc=$rc) ---"
  grep -E '^(=== RUN|--- (PASS|FAIL|SKIP)|FAIL|ok|panic|# )' "$OUT" | head -40
  echo "(full output: re-run the command above)" >&2
  exit 1
fi

# Both criteria must have been evidenced by a test that actually ran. A
# `-run` selector that matches nothing is a green go test with no test in it,
# which is exactly the shape that certifies nothing.
missing=()
for t in "${TESTS[@]}"; do
  grep -q -- "--- PASS: ${t}\b" "$OUT" || missing+=("$t")
done
if (( ${#missing[@]} )); then
  echo "G3 state-commit-real-services: FAILED — go test was green but these criteria-bearing tests did not run:" >&2
  printf '  - %s\n' "${missing[@]}" >&2
  echo "They are the evidence for T0204's acceptance criteria. Deleted, renamed or skipped," >&2
  echo "the task is not verified, however green the rest of the suite is." >&2
  exit 1
fi

for t in "${TESTS[@]}"; do
  printf 'ok   %s\n' "$t"
done
printf '\nG3 state-commit-real-services: all checks passed against real PostgreSQL\n'
