#!/usr/bin/env bash
#
# Acceptance for the Phase Boundary Hardening checkpoint (2026-09-13).
#
# A checkpoint that only CLAIMS to have closed five structural defects is the
# same failure mode as a gate that reports green without running: the point of
# this file is that each item is checkable, and that the checks fail when the
# property is absent. Where a property lives in Go, the Go test is run; where
# it lives in the tree, the tree is inspected.
#
# Item 5 (the RSG gate) is a real gate, not a formality: while P2's contract
# paths were unserved it was expected RED, and it now passes. So the checkpoint
# accepts either — a pass, or a red that names the unserved contract paths — and
# treats neither as a failure. A vacuously green G3 is the defect this file
# exists to remove; a G3 wired to nothing is the same defect one step earlier,
# and both halves are asked below.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

step() { printf '\n== %s ==\n' "$*"; }

# An item this environment cannot ask. Not a pass and not a failure, and the
# summary below cannot print "all items verified" while one is outstanding —
# the whole point of this file is that a green line means the check ran.
#
# It exists because the two callers have different means. `scripts/ci.sh` runs
# on the Supervisor's machine, where the dev stack is normally up; the CI
# acceptance job runs on a bare runner, which ci.yml says outright ("Only
# migration-integration requires infrastructure; everything else passes on a
# bare runner"). An item needing PostgreSQL or Redis is askable in the first and
# not the second, and the honest report differs — so the item says which it is
# rather than reporting an answer it did not get.
UNCHECKED=()
unchecked() { printf 'N/A  %s\n       not asked here: %s\n' "$1" "$2"; UNCHECKED+=("$1"); }

# Anything this file repeats out of a gate's own output goes through this first.
# The G3 gates put their admin URL inside their FAILED lines — "no PostgreSQL
# accepting connections at $PG_URL" — and a Postgres URL carries a password. The
# one in the tree today is a documented dev default (Makefile:81), so nothing
# secret leaks today; but POSTGRES_TEST_ADMIN_URL is an environment variable,
# and the day it points at an endpoint with a real password is the day this file
# prints it into a CI log. Redact by construction, not by luck. Same rule the
# Supervisor applies when reporting: userinfo in a URL never survives.
mask() { sed -E 's#://[^/[:space:]@]*@#://***@#g'; }

# Every Go-backed item below goes through this, because the obvious spelling is
# fail-open. `go test -run NAME` EXITS 0 when NAME matches nothing at all — it
# prints "ok ... [no tests to run]" and says nothing is wrong. So
#
#     if go test -run "$name" >/dev/null 2>&1; then ok ...; fi
#
# reports green for a test that has been renamed or deleted, which is exactly
# the failure this file opens by promising to prevent ("a gate that reports
# green without running"). It was measured, not reasoned about: a probe with the
# name TestNoSuchTestExistsAtAll reported ok.
#
# The idiom is not invented here — the real-services G3 scripts already guard
# this the same way, and say why (state-commit-real-services-e2e.sh: "a run in
# which either did not execute is a failure ... deleting or renaming the
# criteria-bearing test would otherwise turn this gate green while asserting
# nothing"). This file was the one place that had not been told. The caller
# names every test in full — a prefix such as TestDispatchRefusesAPhase matches
# the test named ...WithNoRealServicesGate for -run, but there is no PASS line
# under the prefix, so the prefix is not a name this accepts.
go_tests_pass() {
  local pkg="$1"; shift
  local out pattern
  pattern="$(IFS='|'; printf '%s' "$*")"
  if ! out="$(go test "$pkg" -count=1 -v -run "$pattern" 2>&1)"; then
    printf '%s\n' "$out" | grep -E '^\s*--- (FAIL|PASS)|^(FAIL|ok)\s' | head -8
    return 1
  fi
  local name
  for name in "$@"; do
    grep -q -- "--- PASS: $name\b" <<<"$out" || {
      printf 'no PASS line for %s — renamed or deleted?\n' "$name"
      return 1
    }
  done
  return 0
}

step "1. G2 verifies current main + the task's change, not the task's own tree"
if go_tests_pass ./internal/devorchestrator TestGateVerifiesMainPlusTheTaskChange; then
  ok "the integration-tree check passes"
else
  fail "TestGateVerifiesMainPlusTheTaskChange fails — G2 may be verifying the wrong tree"
fi
grep -q 'prepareIntegrationTree' internal/devorchestrator/gate_run.go \
  && ok "gates build a scratch tree from main plus the change" \
  || fail "prepareIntegrationTree is gone"

step "2. a review verdict is bound to the code it judged"
if go_tests_pass ./internal/devorchestrator TestReviewVerdictIsBoundToTheCodeItJudged; then
  ok "the verdict binding holds (mismatch refused, commit stable, drift invalidates)"
else
  fail "the verdict binding test fails"
fi

step "3. the canonical schema snapshot is generated, and current"
if python3 scripts/gen_schema_snapshot.py --check >/dev/null 2>&1; then
  ok "specs/database/postgres.sql is the ordered migrations"
else
  fail "the schema snapshot is stale"
fi
if bash scripts/tests/schema-snapshot-test.sh >/dev/null 2>&1; then
  ok "the drift check itself reports drift"
else
  fail "the schema-snapshot drift check is broken"
fi
# Structurally, not by grep: the note text also mentions the path, so a grep
# kept passing after the RULE was removed. Found by deliberately breaking it -
# which is why a checkpoint item is not considered done until it can fail.
rule_ok=1
python3 - <<'PYEOF' || rule_ok=0
import json, sys
rules = json.load(open("specs/orchestrator/derived-artifacts.json")).get("rules", [])
ok = any(r.get("marker") == "infra/migrations/**"
         and r.get("derived") == "specs/database/postgres.sql" for r in rules)
sys.exit(0 if ok else 1)
PYEOF
if (( rule_ok )); then
  ok "the snapshot is a declared derived artifact (the Worker's one way into specs/)"
else
  fail "the derived-artifact RULE for the snapshot is missing (a mention in the notes is not a rule)"
fi
grep -q '8.1 Schema' CLAUDE.md && ok "the rule is in the governance doc (CLAUDE.md 8.1)" || fail "CLAUDE.md 8.1 is missing"

step "4. migration numbers are allocated, not inferred"
if go_tests_pass ./internal/devorchestrator TestMigrationNumbersAreAllocatedNotInferred; then
  ok "parallel dispatches cannot collide on a number"
else
  fail "the migration allocator test fails"
fi
grep -q 'migration_number' internal/devorchestrator/worker_render.go \
  && ok "the reserved number travels in the task package" \
  || fail "the task package does not carry migration_number"

step "5. G3 is wired for P2 and P3, and the RSG gate fails for the right reason"
if go_tests_pass ./internal/devorchestrator \
     TestEveryTaskOfThePhasesUnderDevelopmentHasG3 \
     TestDispatchRefusesAPhaseWithNoRealServicesGate; then
  ok "every P1-P3 task carries a G3, and a phase without one refuses dispatch"
else
  fail "the G3 coverage guards fail"
fi
# The RSG gate is a G3 JOB — declared in the gate spec and wired to the tasks
# that must run it — and rddev is what executes it, with the dev stack in the
# environment. So "is it wired" is checkable anywhere, and it is the half of
# this item that a phase boundary actually turns on: a gate wired to nothing
# never runs, whatever its script does when a person calls it by hand. Nothing
# asserted this until now; the checkpoint went straight to running the script,
# which asks a narrower question in a place that usually cannot answer it.
wiring_ok=1
python3 - <<'PYEOF' || wiring_ok=0
import json, sys
spec = json.load(open("specs/orchestrator/gates.json"))
if "rsg-real-services" not in (spec.get("jobs") or {}):
    print("rsg-real-services is not declared as a job in specs/orchestrator/gates.json")
    sys.exit(1)
wired = sorted(t for t, o in (spec.get("task_overrides") or {}).items()
               if "rsg-real-services" in ((o or {}).get("g3_jobs") or []))
if not wired:
    print("rsg-real-services is declared but wired to no task — nothing runs it")
    sys.exit(1)
print("wired to %d task(s), first: %s" % (len(wired), ", ".join(wired[:4])))
PYEOF
if (( wiring_ok )); then
  ok "the RSG gate is declared in the gate spec and wired to the tasks that run it"
else
  fail "the RSG gate is not wired as a G3 job (a gate nothing runs is not a gate)"
fi
# And the gate's own behaviour. Until P2 served the contract paths it had to be
# red WITH A REASON — a vacuously green G3 certifies nothing — and P2 has now
# built enough of the chain that it passes. Both outcomes are accepted. What is
# not accepted is a red that names nothing, or an inability to ask that gets
# reported as an answer.
OUT="$(bash tests/acceptance/rsg-real-services-e2e.sh 2>&1)"
RC=$?
if (( RC == 0 )); then
  ok "the RSG gate passes (P2 has built the chain)"
elif grep -q 'are not served yet' <<<"$OUT"; then
  ok "the RSG gate is red and names the unserved contract paths (P2's work list)"
elif grep -qE 'no PostgreSQL accepting connections at |no Redis at |psql is required to give this run|could not create this run.s own database' <<<"$OUT"; then
  # The gate's own statement that the dev stack is not here, which is a fact
  # about this machine and not a verdict about the tree. The four forms are the
  # whole "cannot be asked here" family in that script — its other two FAILED
  # messages are verdicts and are left to the `fail` below: a malformed admin
  # URL is a broken setup, and "could not migrate the database with this tree's
  # migrations" is the tree answering badly, which is exactly what a gate is for.
  #
  # Matched by exact wording on purpose. If that wording changes, this falls
  # through to the `fail` below — the failure mode is a red someone looks at,
  # never a quiet pass.
  unchecked "the RSG gate is red for the right reason" \
    "$(grep -m1 -oE '(no PostgreSQL accepting connections at|no Redis at|psql is required|could not create this run).*' <<<"$OUT" | mask)"
else
  fail "the RSG gate fails without naming the contract paths: $(tail -3 <<<"$OUT" | mask)"
fi

step "6. the gate machinery is exercised by the gate machinery"
if [[ -n "${POST_INSIDE_ACCEPTANCE_STAGE:-}" ]]; then
  # Both callers set this — the acceptance stage of scripts/ci.sh, and the
  # acceptance job of ci.yml — and both run those e2e immediately before this
  # file, which is why asking for them again would recurse rather than verify
  # anything. The marker is not a skip: the property is being checked by the
  # runs that just happened in the enclosing stage, and a failure in any of them
  # would have stopped the stage before this line was reached. Run this file by
  # hand and the marker is unset, so the step runs them for real.
  ok "the four-gate, rejection-retry and supervisor-git e2e ran in the enclosing stage"
else
  bash scripts/ci.sh acceptance >/dev/null 2>&1 \
    && ok "the four-gate, rejection-retry and supervisor-git e2e pass" \
    || fail "the acceptance e2e fail"
fi

step "7. unattended orchestration exists"
# This step named scripts/supervise.sh, which 72583ee replaced with the
# persistent driver. Nothing runs this file, so nothing noticed — an acceptance
# script no stage calls does not fail, it just quietly stops describing the tree
# it was written to guard. That is the rot the comment in scripts/ci.sh warns
# about, and the reason this file is now wired into the acceptance stage.
if [[ -f cmd/rddev/drive.go && -f internal/devorchestrator/driver_run.go ]]; then
  ok "rddev drive is the unattended driver"
else
  fail "the unattended driver is missing (cmd/rddev/drive.go, internal/devorchestrator/driver_run.go)"
fi
# Stop conditions are Go, so the Go tests are run rather than grepped for. A
# grep for the sentence proves the sentence exists; it cannot prove the driver
# reaches it.
if go_tests_pass ./internal/devorchestrator \
     TestARedMergeRefusalBecomesADecisionAndAWaitDoesNot \
     TestACapacityRefusalIsAWaitNotADecision \
     TestDecisionsSurviveAndClearThemselves; then
  ok "it drives the loop and stops only for a decision"
else
  fail "the driver's stop conditions are not the tested ones"
fi
# Completion is Go too, and had no test until this step went looking for one —
# which is how the missing test was found. See the note on the test itself.
if go_tests_pass ./internal/devorchestrator TestAnExhaustedDriverReportsCompletionInsteadOfWaiting; then
  ok "it reports completion rather than waiting for a prompt"
else
  fail "the driver has no completion path"
fi

printf '\n'
if (( FAILS )); then
  printf 'phase-boundary-checkpoint: %d failure(s)\n' "$FAILS"
  exit 1
fi
# Exit 0 with an N/A is the deliberate call: the item is unaskable HERE by
# construction, not unverified, and turning the bare-runner acceptance job red
# would be a false alarm about the tree. What must not happen is a green line
# that reads as "everything was checked", so the count is printed and the
# sentence "all items verified" is withheld.
if (( ${#UNCHECKED[@]} )); then
  printf 'phase-boundary-checkpoint: %d item(s) NOT ASKED in this environment: %s\n' \
    "${#UNCHECKED[@]}" "$(IFS='; '; printf '%s' "${UNCHECKED[*]}")"
  printf '  not a pass for those items — the rest were verified. Ask them with the dev stack up (make infra-up).\n'
  exit 0
fi
printf 'phase-boundary-checkpoint: all checkpoint items verified\n'
