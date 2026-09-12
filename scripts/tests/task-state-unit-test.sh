#!/usr/bin/env bash
#
# Unit tests for scripts/validate_task_state.py (fixture-driven).
#
# Exercises the DAG/state/test-registry coverage and shape checks against
# injected fixture trees (--root); never touches the real tasks/ files.
# Runnable on a host with bash + coreutils + python3 (stdlib only).
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
VALIDATOR="$ROOT/scripts/validate_task_state.py"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILS=0
RC=0; OUT=""; ERR=""

fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

# build_case DIR TASKS_JSON STATUS_JSON TESTS_JSON — writes a fixture tree.
# TASKS_JSON is the full tasks.json content; STATUS_JSON the tasks object of
# task_status.json (keys only); TESTS_JSON the tests list of tests.json.
build_case() {
  local dir="$1" tjson="$2" sjson="$3" jjson="$4"
  mkdir -p "$dir/tasks"
  python3 - "$dir" "$tjson" "$sjson" "$jjson" <<'PY'
import json, sys
dir_, tj, sj, jj = sys.argv[1:]
t = json.loads(tj)
json.dump({"version": "2026-09", "task_count": len(t["tasks"]), "tasks": t["tasks"]},
          open(f"{dir_}/tasks/tasks.json", "w"), indent=2)
st = json.loads(sj)
json.dump({"version": "2026-09", "overall": "P0_in_progress", "tasks": st},
          open(f"{dir_}/tasks/task_status.json", "w"), indent=2)
tt = json.loads(jj)
json.dump({"version": "2026-09", "tests": tt},
          open(f"{dir_}/tasks/tests.json", "w"), indent=2)
PY
}

# Base fixture: three tasks, coherent status + tests. Derived cases modify
# one file only, so a failing case can only be explained by that edit.
BASE_TASKS='{"tasks":[{"id":"T0001","title":"one","phase":"P0"},{"id":"T0002","title":"two","phase":"P0"},{"id":"T0003","title":"three","phase":"P0"}]}'
BASE_STATUS='{"T0001":{"status":"merged","merged_at":"2026-09-12T12:00:00Z"},"T0002":{"status":"todo"},"T0003":{"status":"running","started_at":"2026-09-12T13:00:00Z"}}'
BASE_TESTS='[{"id":"T0001-TEST-01","task_id":"T0001","name":"a","gate":"G1/G2","blocking":true,"status":"passed","last_run":"2026-09-12T12:00:00Z","evidence":"e"},{"id":"T0002-TEST-01","task_id":"T0002","name":"b","gate":"task-defined","blocking":true,"status":"not_run","last_run":null,"evidence":null},{"id":"T0003-TEST-01","task_id":"T0003","name":"c","gate":"task-defined","blocking":true,"status":"not_run","last_run":null,"evidence":null}]'

run_validator() { # dir [extra args...] -> sets RC/OUT/ERR
  OUT="$(python3 "$VALIDATOR" --root "$1" "${@:2}" 2>"$WORK/err")"
  RC=$?
  ERR="$(cat "$WORK/err")"
}

# --- 1. clean fixture passes ------------------------------------------------
build_case "$WORK/clean" "$BASE_TASKS" "$BASE_STATUS" "$BASE_TESTS"
run_validator "$WORK/clean"
assert_eq() { :; }
if [[ $RC -eq 0 ]]; then ok "clean fixture: rc=0"; else fail "clean fixture: rc=$RC out=$OUT err=$ERR"; fi

# --- 2. task in DAG but missing from task_status ----------------------------
build_case "$WORK/dag-only" "$BASE_TASKS" \
  '{"T0001":{"status":"merged","merged_at":"2026-09-12T12:00:00Z"},"T0002":{"status":"todo"}}' \
  "$BASE_TESTS"
run_validator "$WORK/dag-only"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "TASKSTATE-SET" && echo "$OUT" | grep -q "T0003"; then
  ok "dag-only task: rc=1, names TASKSTATE-SET and the task id"
else
  fail "dag-only task: rc=$RC out=$OUT"
fi

# --- 3. task in task_status but missing from DAG ----------------------------
build_case "$WORK/status-only" \
  '{"tasks":[{"id":"T0001","title":"one","phase":"P0"},{"id":"T0002","title":"two","phase":"P0"}]}' \
  "$BASE_STATUS" "$BASE_TESTS"
run_validator "$WORK/status-only"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "TASKSTATE-SET" && echo "$OUT" | grep -q "T0003"; then
  ok "status-only task: rc=1, names TASKSTATE-SET and the task id"
else
  fail "status-only task: rc=$RC out=$OUT"
fi

# --- 4. task_count drift ----------------------------------------------------
build_case "$WORK/count" "$BASE_TASKS" "$BASE_STATUS" "$BASE_TESTS"
python3 - "$WORK/count" <<'PY'
import json, sys
p = f"{sys.argv[1]}/tasks/tasks.json"
d = json.load(open(p)); d["task_count"] = 99
json.dump(d, open(p, "w"))
PY
run_validator "$WORK/count"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "TASKSTATE-DAG-COUNT"; then
  ok "task_count drift: rc=1, TASKSTATE-DAG-COUNT"
else
  fail "task_count drift: rc=$RC out=$OUT"
fi

# --- 5. status outside the canonical enum -----------------------------------
build_case "$WORK/bad-status" "$BASE_TASKS" \
  '{"T0001":{"status":"merged","merged_at":"2026-09-12T12:00:00Z"},"T0002":{"status":"done"},"T0003":{"status":"running"}}' \
  "$BASE_TESTS"
run_validator "$WORK/bad-status"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "TASKSTATE-STATUS-ENUM" && echo "$OUT" | grep -q "done"; then
  ok "bad status enum: rc=1, names the offending status"
else
  fail "bad status enum: rc=$RC out=$OUT"
fi

# --- 6. malformed timestamp -------------------------------------------------
build_case "$WORK/bad-ts" "$BASE_TASKS" \
  '{"T0001":{"status":"merged","merged_at":"yesterday"},"T0002":{"status":"todo"},"T0003":{"status":"running"}}' \
  "$BASE_TESTS"
run_validator "$WORK/bad-ts"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "TASKSTATE-TIMESTAMPS"; then
  ok "malformed timestamp: rc=1, TASKSTATE-TIMESTAMPS"
else
  fail "malformed timestamp: rc=$RC out=$OUT"
fi

# --- 7. duplicate DAG id ----------------------------------------------------
build_case "$WORK/dup-id" \
  '{"tasks":[{"id":"T0001","title":"one","phase":"P0"},{"id":"T0001","title":"one-bis","phase":"P0"},{"id":"T0002","title":"two","phase":"P0"},{"id":"T0003","title":"three","phase":"P0"}]}' \
  "$BASE_STATUS" "$BASE_TESTS"
run_validator "$WORK/dup-id"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "TASKSTATE-DAG-IDS"; then
  ok "duplicate DAG id: rc=1, TASKSTATE-DAG-IDS"
else
  fail "duplicate DAG id: rc=$RC out=$OUT"
fi

# --- 8. malformed tasks.json ------------------------------------------------
mkdir -p "$WORK/broken-dag/tasks"
printf '{not json' > "$WORK/broken-dag/tasks/tasks.json"
cp "$WORK/clean/tasks/task_status.json" "$WORK/clean/tasks/tests.json" "$WORK/broken-dag/tasks/"
run_validator "$WORK/broken-dag"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "TASKSTATE-DAG-COUNT"; then
  ok "malformed tasks.json: rc=1, TASKSTATE-DAG-COUNT"
else
  fail "malformed tasks.json: rc=$RC out=$OUT"
fi

# --- 9. missing task_status.json --------------------------------------------
build_case "$WORK/no-status" "$BASE_TASKS" "$BASE_STATUS" "$BASE_TESTS"
rm "$WORK/no-status/tasks/task_status.json"
run_validator "$WORK/no-status"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "TASKSTATE-SET"; then
  ok "missing task_status.json: rc=1, TASKSTATE-SET"
else
  fail "missing task_status.json: rc=$RC out=$OUT"
fi

# --- 10. test referencing a task outside the DAG ----------------------------
build_case "$WORK/unknown-test-ref" "$BASE_TASKS" "$BASE_STATUS" \
  '[{"id":"T0001-TEST-01","task_id":"T0001","name":"a","gate":"G1/G2","blocking":true,"status":"passed","last_run":null,"evidence":null},{"id":"T0002-TEST-01","task_id":"T0002","name":"b","gate":"task-defined","blocking":true,"status":"not_run","last_run":null,"evidence":null},{"id":"T0003-TEST-01","task_id":"T0003","name":"c","gate":"task-defined","blocking":true,"status":"not_run","last_run":null,"evidence":null},{"id":"T9999-TEST-01","task_id":"T9999","name":"ghost","gate":"task-defined","blocking":true,"status":"not_run","last_run":null,"evidence":null}]'
run_validator "$WORK/unknown-test-ref"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "TASKSTATE-TESTS-REF" && echo "$OUT" | grep -q "T9999"; then
  ok "test for unknown task: rc=1, names TASKSTATE-TESTS-REF and the id"
else
  fail "test for unknown task: rc=$RC out=$OUT"
fi

# --- 11. DAG task without any registered test -------------------------------
build_case "$WORK/uncovered" "$BASE_TASKS" "$BASE_STATUS" \
  '[{"id":"T0001-TEST-01","task_id":"T0001","name":"a","gate":"G1/G2","blocking":true,"status":"passed","last_run":null,"evidence":null},{"id":"T0002-TEST-01","task_id":"T0002","name":"b","gate":"task-defined","blocking":true,"status":"not_run","last_run":null,"evidence":null}]'
run_validator "$WORK/uncovered"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "TASKSTATE-TESTS-COVERAGE" && echo "$OUT" | grep -q "T0003"; then
  ok "uncovered task: rc=1, TASKSTATE-TESTS-COVERAGE names the task"
else
  fail "uncovered task: rc=$RC out=$OUT"
fi

# --- 12. tests.json shape violations (dup id, non-bool blocking) ------------
build_case "$WORK/bad-test-shape" "$BASE_TASKS" "$BASE_STATUS" \
  '[{"id":"T0001-TEST-01","task_id":"T0001","name":"a","gate":"G1/G2","blocking":"yes","status":"passed","last_run":null,"evidence":null},{"id":"T0002-TEST-01","task_id":"T0002","name":"b","gate":"task-defined","blocking":true,"status":"not_run","last_run":null,"evidence":null},{"id":"T0002-TEST-01","task_id":"T0003","name":"c","gate":"task-defined","blocking":true,"status":"not_run","last_run":null,"evidence":null}]'
run_validator "$WORK/bad-test-shape"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "TASKSTATE-TESTS-SHAPE"; then
  ok "test shape violations: rc=1, TASKSTATE-TESTS-SHAPE"
else
  fail "test shape violations: rc=$RC out=$OUT"
fi

# --- 13. --json output shape on the clean fixture ---------------------------
run_validator "$WORK/clean" --json
python3 - "$OUT" <<'PY' && ok "--json: valid JSON with non-empty checks list" || fail "--json: invalid output"
import json, sys
d = json.loads(sys.argv[1])
assert isinstance(d["checks"], list) and d["checks"], "checks list empty"
assert all(c["id"] and c["status"] in ("passed", "failed", "unknown", "error") for c in d["checks"])
PY

# --- summary ----------------------------------------------------------------
echo ""
if [[ $FAILS -eq 0 ]]; then
  echo "task-state-unit-test: all cases passed"
  exit 0
fi
echo "task-state-unit-test: $FAILS case(s) failed"
exit 1
