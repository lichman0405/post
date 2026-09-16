#!/usr/bin/env bash
#
# ci.sh — local replication of .github/workflows/ci.yml, same stages and
# same order (keep in sync with the workflow when either changes).
#
# Each stage is isolated: a failing stage stops the run and is named in a
# final "STAGE FAILED: <name>" banner, so local runs identify the failing
# stage exactly like the PR check summary does.
#
# Usage:
#   bash scripts/ci.sh                # every stage (integration included)
#   bash scripts/ci.sh spec go        # only the named stages, in this order
#
# The integration stage needs a reachable PostgreSQL admin endpoint:
# POSTGRES_TEST_ADMIN_URL (the DSN default lives in the test-integration target
# in the Makefile). All other stages are infrastructure-free.
#
# Implementation note: each stage runs as a re-exec'ed child process
# (CI_STAGE_SINGLE) because bash suppresses errexit inside every
# status-capturing context — if/|| conditions, subshells, even function
# bodies called from them. A stage executed directly under an `if` would
# silently continue after a failing command and could pass by accident;
# the child process runs the stage as a top-level command with errexit
# genuinely active, so the first failure really does fail the stage.
set -euo pipefail
export LC_ALL=C

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

ALL_STAGES=(workflows spec task-state go web python acceptance integration)

STAGES=("$@")
if [[ ${#STAGES[@]} -eq 0 ]]; then
  STAGES=("${ALL_STAGES[@]}")
fi

banner() {
  echo ""
  echo "=============================================================="
  echo "== stage: $*"
  echo "=============================================================="
}

run_stage() {
  local name="$1"
  banner "$name"
  if CI_STAGE_SINGLE="$name" bash "${BASH_SOURCE[0]}"; then
    echo "stage PASS: $name"
  else
    local rc=$?
    echo ""
    echo "STAGE FAILED: $name" >&2
    echo "exit code: $rc" >&2
    echo ""
    exit 1
  fi
}

stage_workflows() {
  # A workflow that does not parse never runs at all — GitHub fails the run in
  # 0 seconds and the repository silently has no CI. The local replica tests
  # the commands the workflow runs, never the document itself, so this is the
  # only place that can catch it.
  python3 scripts/validate_workflows.py
}

stage_spec() {
  python3 scripts/validate_specs.py
  python3 scripts/gen_schema_snapshot.py --check
  bash scripts/tests/schema-snapshot-test.sh
  python3 scripts/spec_version.py --check
  bash scripts/tests/spec-validation-unit-test.sh
  bash scripts/tests/spec-validation-smoke-test.sh
}

stage_task_state() {
  python3 scripts/validate_task_state.py
  bash scripts/tests/task-state-unit-test.sh
  bash scripts/tests/progress-update-test.sh
}

stage_go() {
  make fmt-check
  go vet ./...
  make staticcheck
  bash scripts/tests/staticcheck-unit-test.sh
  go test $(go list ./... | grep -v '/tests/integration')
}

stage_web() {
  pnpm --filter @post/ui typecheck
  pnpm --filter @post/web typecheck
  pnpm --filter @post/web lint
  # Every apps/web test file, with a guard against the glob matching none
  # (a "tests 0" run is a green stage that tested nothing).
  bash scripts/web-unit-tests.sh
  bash scripts/tests/web-tests-unit-test.sh
  POST_ENV=prod API_BASE_URL=http://127.0.0.1:18080 \
    SCIENTIFIC_ADAPTER_URL=http://127.0.0.1:19100 \
    pnpm --filter @post/web build
}

stage_python() {
  cd services/scientific-adapter
  uvx ruff check . --config ../../ops/ci/ruff.toml
  uvx mypy --config-file ../../ops/ci/mypy.ini src/post_scientific_adapter
  uv run pytest -q
}

stage_acceptance() {
  # The gate machinery's own e2e: the four-gate loop, rework/respawn, and the
  # Supervisor git control plane. These were written for T0012 and then not run
  # by anything, and rejection-retry rotted the moment a bug it had encoded was
  # fixed — it asserted the invalid --session-id/--resume combination that real
  # claude refuses. Self-contained: scratch repos, a fake claude, a fake gh.
  bash tests/acceptance/four-gate-e2e.sh
  bash tests/acceptance/rejection-retry-e2e.sh
  bash tests/acceptance/supervisor-git-e2e.sh
  bash tests/acceptance/driver-persistence-e2e.sh
  # The G3 gate script is graded against a fake instance, offline: it was once
  # run in a tree it had already committed to (L1-20260913-16), and a gate that
  # mutates what it grades cannot be believed about anything else it reports.
  bash scripts/tests/gitea-e2e-guard-unit-test.sh
  # The phase-boundary checkpoint (2026-09-13) closes the loop on the five
  # structural defects that hardening was about. It was written for that work
  # and then invoked by nothing, which is exactly how its step 7 came to name a
  # script that had since been deleted — the rot the comment above warns about.
  # The marker tells the checkpoint's step 6 that this stage is already running
  # the e2e it would otherwise ask for again, which without it recurses.
  POST_INSIDE_ACCEPTANCE_STAGE=1 bash tests/acceptance/phase-boundary-checkpoint.sh
}

stage_integration() {
  # The probe is a gate input: `make test-integration` decides whether to run
  # at all from its verdict. A probe that says "ready" for a socket that never
  # speaks PostgreSQL turns a missing database into a confusing postgres reset
  # error instead of the loud, actionable failure this stage promises.
  bash scripts/tests/pg-ready-unit-test.sh
  make test-integration
}

if [[ -n "${CI_STAGE_SINGLE:-}" ]]; then
  # Child mode: run exactly one stage; errexit is active here. The stage
  # name -> function mapping must stay in sync with the case below
  # (function names cannot contain the hyphen in "task-state").
  case "$CI_STAGE_SINGLE" in
    workflows)    stage_workflows ;;
    spec)         stage_spec ;;
    task-state)   stage_task_state ;;
    acceptance)   stage_acceptance ;;
    go)           stage_go ;;
    web)          stage_web ;;
    python)       stage_python ;;
    integration)  stage_integration ;;
    *)            echo "unknown stage: $CI_STAGE_SINGLE" >&2; exit 2 ;;
  esac
  exit 0
fi

for name in "${STAGES[@]}"; do
  case "$name" in
    workflows)    run_stage workflows ;;
    spec)         run_stage spec ;;
    task-state)   run_stage task-state ;;
    acceptance)   run_stage acceptance ;;
    go)           run_stage go ;;
    web)          run_stage web ;;
    python)       run_stage python ;;
    integration)  run_stage integration ;;
    *)
      echo "unknown stage: $name (valid: ${ALL_STAGES[*]})" >&2
      exit 2
      ;;
  esac
done

echo ""
echo "ALL STAGES PASSED: ${STAGES[*]}"
