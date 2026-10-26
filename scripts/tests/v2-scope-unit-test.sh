#!/usr/bin/env bash
#
# Unit tests for scripts/validate_v2_scope.py (fixture-driven).
#
# Each case builds an isolated fixture tree under a temp root and runs the
# validator against it, so a failing case can only be explained by that edit.
# Stays stdlib-only: bash + coreutils + python3.
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
VALIDATOR="$ROOT/scripts/validate_v2_scope.py"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILS=0
RC=0
OUT=""
ERR=""

fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

# build_case DIR CASE — writes a coherent fixture tree, then applies CASE.
build_case() {
  python3 - "$1" "$2" <<'PY'
import json, sys
from pathlib import Path

out, case = Path(sys.argv[1]), sys.argv[2]
(out / "tasks" / "v2").mkdir(parents=True, exist_ok=True)


def tid(n):
    return f"T{1301 + n:04d}"


tasks = [{
    "id": "T1299", "phase": "P13", "title": "v1 base", "v1_required": True,
    "dependencies": [], "allowed_scope": ["README.md"],
}]
status = {"T1299": {"status": "merged"}}
tests = []
backlog = []
mapping = []
journeys = [{"id": f"U{i:02d}", "execution_status": "not_run", "steps": [{"step": 1}],
             "evidence_required": ["trace"]} for i in range(1, 15)]
for n in range(31):
    web = f"WEB-{n:02d}"
    task_id = tid(n)
    journey = f"U{n % 14 + 1:02d}"
    tasks.append({
        "id": task_id, "phase": "P14", "title": web, "v1_required": False,
        "v2_required": True, "web_package": web, "dependencies": [],
    })
    status[task_id] = {"status": "todo"}
    tests.append({"id": f"{task_id}-TEST-01", "task_id": task_id, "name": "t",
                  "gate": "task-defined", "blocking": True, "status": "not_run",
                  "last_run": None, "evidence": ""})
    backlog.append({"id": web, "journey_ids": [journey], "required_for_v2": True})
    mapping.append({"web": web, "tasks": [task_id], "milestone": "M0",
                    "journeys": [journey], "issue": None, "pull_requests": []})

if case == "missing_package":
    dropped = tasks.pop()          # remove WEB-30
    status.pop(dropped["id"])
elif case == "v1_flag":
    next(t for t in tasks if t.get("v2_required"))["v1_required"] = True
elif case == "map_wrong":
    mapping[0]["tasks"] = ["T9999"]
elif case == "dep_todo":
    next(t for t in tasks if t.get("v2_required"))["dependencies"] = ["T1299"]
    status["T1299"]["status"] = "todo"
elif case == "no_test":
    tests.pop()
elif case == "complete":
    for task in tasks:
        if task.get("v2_required"):
            status[task["id"]]["status"] = "merged"
    for entry in tests:
        entry["status"] = "passed"
        entry["last_run"] = "2026-10-26T00:00:00Z"
        entry["evidence"] = "evidence"
    for journey in journeys:
        journey["execution_status"] = "passed"
    for entry in mapping:
        entry["issue"] = 1
        entry["pull_requests"] = [1]

(out / "tasks" / "tasks.json").write_text(json.dumps(
    {"version": "2026-09", "task_count": len(tasks), "tasks": tasks}), encoding="utf-8")
(out / "tasks" / "task_status.json").write_text(json.dumps(
    {"version": "2026-09", "overall": "P14", "tasks": status}), encoding="utf-8")
(out / "tasks" / "tests.json").write_text(json.dumps(
    {"version": "2026-09", "tests": tests}), encoding="utf-8")
(out / "tasks" / "v2" / "backlog.json").write_text(json.dumps(
    {"version": "1.0", "tasks": backlog}), encoding="utf-8")
(out / "tasks" / "v2" / "mapping.json").write_text(json.dumps(
    {"version": "1.0", "entries": mapping}), encoding="utf-8")
(out / "tasks" / "v2" / "web-acceptance.json").write_text(json.dumps(
    {"journeys": journeys}), encoding="utf-8")
PY
}

run_validator() { # dir [extra args...] -> RC/OUT/ERR
  OUT="$(python3 "$VALIDATOR" --root "$1" "${@:2}" 2>"$WORK/err")"
  RC=$?
  ERR="$(cat "$WORK/err")"
}

# --- 1. clean structural fixture passes ------------------------------------
build_case "$WORK/clean" clean
run_validator "$WORK/clean"
if [[ $RC -eq 0 ]]; then ok "clean fixture: rc=0"; else fail "clean fixture: rc=$RC out=$OUT err=$ERR"; fi

# --- 2. a package without a V2 task fails --------------------------------
build_case "$WORK/missing" missing_package
run_validator "$WORK/missing"
if [[ $RC -eq 1 ]] && grep -q "V2SCOPE-FLAGS" <<<"$OUT"; then
  ok "missing package: rc=1, names V2SCOPE-FLAGS"
else fail "missing package: rc=$RC out=$OUT"; fi

# --- 3. a V2 task marked v1_required fails ------------------------------
build_case "$WORK/v1flag" v1_flag
run_validator "$WORK/v1flag"
if [[ $RC -eq 1 ]] && grep -q "v1_required" <<<"$OUT"; then
  ok "v2 task marked v1_required: rc=1, names the flag"
else fail "v2 task marked v1_required: rc=$RC out=$OUT"; fi

# --- 4. mapping that disagrees with the DAG fails -------------------------
build_case "$WORK/mapwrong" map_wrong
run_validator "$WORK/mapwrong"
if [[ $RC -eq 1 ]] && grep -q "V2SCOPE-MAP" <<<"$OUT"; then
  ok "mapping drift: rc=1, names V2SCOPE-MAP"
else fail "mapping drift: rc=$RC out=$OUT"; fi

# --- 5. dependency on a non-V2 unmerged task fails -----------------------
build_case "$WORK/deptodo" dep_todo
run_validator "$WORK/deptodo"
if [[ $RC -eq 1 ]] && grep -q "V2SCOPE-DEPS" <<<"$OUT"; then
  ok "dependency on unmerged V1 task: rc=1, names V2SCOPE-DEPS"
else fail "dependency on unmerged V1 task: rc=$RC out=$OUT"; fi

# --- 6. a V2 task without a registered test fails ------------------------
build_case "$WORK/notest" no_test
run_validator "$WORK/notest"
if [[ $RC -eq 1 ]] && grep -q "V2SCOPE-TESTS" <<<"$OUT"; then
  ok "missing test: rc=1, names V2SCOPE-TESTS"
else fail "missing test: rc=$RC out=$OUT"; fi

# --- 7. --complete stays red while the delivery is incomplete ------------
build_case "$WORK/incomplete" clean
run_validator "$WORK/incomplete" --complete
if [[ $RC -eq 1 ]] && grep -q "V2SCOPE-COMPLETE" <<<"$OUT"; then
  ok "--complete on unfinished delivery: rc=1"
else fail "--complete on unfinished delivery: rc=$RC out=$OUT"; fi

# --- 8. --complete passes only when tasks, journeys, tests and PRs exist --
build_case "$WORK/complete" complete
run_validator "$WORK/complete" --complete
if [[ $RC -eq 0 ]]; then
  ok "--complete on finished fixture: rc=0"
else fail "--complete on finished fixture: rc=$RC out=$OUT err=$ERR"; fi

if [[ $FAILS -ne 0 ]]; then
  printf '%d failure(s)\n' "$FAILS"
  exit 1
fi
printf 'all v2-scope cases passed\n'
