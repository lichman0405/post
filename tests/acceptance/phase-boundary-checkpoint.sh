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
# Item 5 (the RSG gate) is expected to be RED until P2 builds those paths. The
# checkpoint asserts it is red FOR THE RIGHT REASON — naming the unserved
# contract paths — not that it passes, because a vacuously green G3 would be
# the defect this checkpoint exists to remove.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

step() { printf '\n== %s ==\n' "$*"; }

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
OUT="$(bash tests/acceptance/rsg-real-services-e2e.sh 2>&1)"
if (( $? == 0 )); then
  ok "the RSG gate passes (P2 has built the chain)"
elif echo "$OUT" | grep -q 'are not served yet'; then
  ok "the RSG gate is red and names the unserved contract paths (P2's work list)"
else
  fail "the RSG gate fails without naming the contract paths: $(echo "$OUT" | tail -3)"
fi

step "6. the gate machinery is exercised by the gate machinery"
if [[ -n "${POST_INSIDE_ACCEPTANCE_STAGE:-}" ]]; then
  # stage_acceptance runs this file, and this step asks that stage to run
  # itself: without the marker it recurses until the machine gives up. The
  # marker also keeps the step honest rather than skipping it — the four e2e
  # below are running in the enclosing stage at this very moment, so the
  # property is being checked by the thing that would be re-checked. Run this
  # file by hand and the marker is unset, so the step still runs for real.
  ok "the four-gate, rejection-retry and supervisor-git e2e are running in the enclosing stage"
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
printf 'phase-boundary-checkpoint: all checkpoint items verified\n'
