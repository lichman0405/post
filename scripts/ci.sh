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
# POSTGRES_TEST_ADMIN_URL (default postgres://postgres:postgres_dev_pw@
# 127.0.0.1:15432/post). All other stages are infrastructure-free.
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

ALL_STAGES=(workflows spec task-state go web python integration)

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

stage_integration() {
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
