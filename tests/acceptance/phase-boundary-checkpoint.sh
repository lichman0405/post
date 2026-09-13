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

step "1. G2 verifies current main + the task's change, not the task's own tree"
if go test ./internal/devorchestrator -run TestGateVerifiesMainPlusTheTaskChange -count=1 >/dev/null 2>&1; then
  ok "the integration-tree check passes"
else
  fail "TestGateVerifiesMainPlusTheTaskChange fails — G2 may be verifying the wrong tree"
fi
grep -q 'prepareIntegrationTree' internal/devorchestrator/gate_run.go \
  && ok "gates build a scratch tree from main plus the change" \
  || fail "prepareIntegrationTree is gone"

step "2. a review verdict is bound to the code it judged"
if go test ./internal/devorchestrator -run TestReviewVerdictIsBoundToTheCodeItJudged -count=1 >/dev/null 2>&1; then
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
if go test ./internal/devorchestrator -run TestMigrationNumbersAreAllocatedNotInferred -count=1 >/dev/null 2>&1; then
  ok "parallel dispatches cannot collide on a number"
else
  fail "the migration allocator test fails"
fi
grep -q 'migration_number' internal/devorchestrator/worker_render.go \
  && ok "the reserved number travels in the task package" \
  || fail "the task package does not carry migration_number"

step "5. G3 is wired for P2 and P3, and the RSG gate fails for the right reason"
if go test ./internal/devorchestrator -run 'TestEveryTaskOfThePhases|TestDispatchRefusesAPhase' -count=1 >/dev/null 2>&1; then
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
bash scripts/ci.sh acceptance >/dev/null 2>&1 \
  && ok "the four-gate, rejection-retry and supervisor-git e2e pass" \
  || fail "the acceptance e2e fail"

step "7. unattended orchestration exists"
[[ -x scripts/supervise.sh ]] && bash -n scripts/supervise.sh 2>/dev/null \
  && ok "scripts/supervise.sh drives the loop and stops only for a decision" \
  || fail "the unattended driver is missing or does not parse"
grep -q 'phase complete' scripts/supervise.sh \
  && ok "it reports completion rather than waiting for a prompt" \
  || fail "the driver has no completion path"

printf '\n'
if (( FAILS )); then
  printf 'phase-boundary-checkpoint: %d failure(s)\n' "$FAILS"
  exit 1
fi
printf 'phase-boundary-checkpoint: all checkpoint items verified\n'
