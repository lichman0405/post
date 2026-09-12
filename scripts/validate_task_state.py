#!/usr/bin/env python3
"""POST task DAG / state / test-registry coverage validator (CI gate).

Guards the drift class the Supervisor found after T0013: a task was added to
tasks/tasks.json but never got a task_status.json entry, so task_status
silently held one entry less than the DAG and rddev read the missing task as
todo although it was already merged.

Checks (all hard failures):

  TASKSTATE-DAG-COUNT       tasks.json task_count == number of listed tasks
  TASKSTATE-DAG-IDS         task ids unique and well-formed (T####)
  TASKSTATE-SET             DAG ids == task_status ids; both directions are
                            named explicitly (in-DAG-not-status and
                            in-status-not-DAG)
  TASKSTATE-STATUS-ENUM     every state entry uses the canonical status enum
                            (CLAUDE.md §4: todo, ready, running, worker_failed,
                            verification, rejected, blocked, accepted, merged)
  TASKSTATE-TIMESTAMPS      present timestamps are ISO-8601 dates/datetimes
  TASKSTATE-OVERALL         task_status.json overall is a non-empty string
  TASKSTATE-TESTS-SHAPE     tests.json parses; ids unique; every entry carries
                            the required fields with the right types
  TASKSTATE-TESTS-REF       every test's task_id resolves to a DAG task
  TASKSTATE-TESTS-COVERAGE  every DAG task has at least one registered test

Exit codes: 0 = all passed; 1 = at least one check failed; 2 = usage error.
Injection (test-only): --root DIR overrides the repository root; a fixture
tree under a temp root is validated exactly like the real repo.
"""
import argparse
import json
import re
import sys
from pathlib import Path

import speclib

USAGE_ERROR = 2
# Canonical task lifecycle statuses (CLAUDE.md §4). A status outside this
# enum means rddev's reader and this check disagree about reality.
STATUS_ENUM = (
    "todo", "ready", "running", "worker_failed", "verification",
    "rejected", "blocked", "accepted", "merged",
)
TEST_STATUS_ENUM = ("passed", "failed", "not_run", "skipped")
ISO_TS_RE = re.compile(
    r"^\d{4}-\d{2}-\d{2}(T\d{2}:\d{2}:\d{2}(Z|[+-]\d{2}:\d{2})?)?$")
TASK_ID_RE = re.compile(r"^T[0-9]{4}$")


def fail_usage(msg):
    print(f"error: {msg}", file=sys.stderr)
    sys.exit(USAGE_ERROR)


def parse_args(argv):
    p = argparse.ArgumentParser(
        prog="scripts/validate_task_state.py",
        description="Validate task DAG/state/test coverage and shape "
                    "(see scripts/speclib.py for the shared decision logic).")
    p.add_argument("--root", default=str(speclib.default_root()),
                   help="repository root to validate "
                        "(default: parent of scripts/)")
    p.add_argument("--json", action="store_true",
                   help="machine-readable output on stdout")
    return p.parse_args(argv)


# ---------------------------------------------------------------------------
# Loading helpers — a missing or unreadable state file is itself a failure.

def load_state_file(root, rel, checks, check_id, name):
    path = root / rel
    if not path.is_file():
        checks.append(speclib.make_check(
            check_id, name, "failed", "file", "", f"{rel} present",
            f"missing: {rel}"))
        return None
    try:
        return speclib.load_json(path)
    except (OSError, json.JSONDecodeError) as exc:
        checks.append(speclib.make_check(
            check_id, name, "failed", "file", "", "valid JSON",
            f"{rel} unreadable: {exc}"))
        return None


def add_fail(checks, check_id, name, measured, detail):
    checks.append(speclib.make_check(
        check_id, name, "failed", "file", measured, "coherent",
        detail))


def check_task_state(root):
    """All TASKSTATE-* checks over tasks/tasks.json, tasks/task_status.json
    and tasks/tests.json."""
    checks = []
    tlist, dag_ids = [], []
    tasks_path = root / speclib.DEFAULT_TASKS_REL
    if tasks_path.is_file():
        try:
            tlist = speclib.load_json(tasks_path).get("tasks", [])
        except (OSError, json.JSONDecodeError) as exc:
            add_fail(checks, "TASKSTATE-DAG-COUNT",
                     "tasks.json parses and lists tasks",
                     "unreadable", f"{speclib.DEFAULT_TASKS_REL}: {exc}")

    if tlist:
        dag_ids = [t.get("id") for t in tlist if isinstance(t, dict)]
        raw_count = None
        try:
            raw_count = speclib.load_json(tasks_path).get("task_count")
        except (OSError, json.JSONDecodeError):
            pass
        if raw_count == len(tlist):
            checks.append(speclib.make_check(
                "TASKSTATE-DAG-COUNT",
                "task_count matches the listed tasks", "passed", "file",
                f"{raw_count}", f"{len(tlist)}", ""))
        else:
            add_fail(checks, "TASKSTATE-DAG-COUNT",
                     "task_count matches the listed tasks",
                     f"task_count={raw_count} listed={len(tlist)}",
                     "the DAG counts drifted; a stale task_count makes "
                     "readers misreport the pipeline size")

        bad_ids = sorted({i for i in dag_ids
                          if not (isinstance(i, str) and TASK_ID_RE.match(i))})
        dups = sorted({i for i in dag_ids if dag_ids.count(i) > 1})
        if not bad_ids and not dups:
            checks.append(speclib.make_check(
                "TASKSTATE-DAG-IDS", "task ids unique and well-formed",
                "passed", "file", f"{len(dag_ids)} ids", "T####", ""))
        else:
            add_fail(checks, "TASKSTATE-DAG-IDS",
                     "task ids unique and well-formed",
                     f"bad={bad_ids[:5]} dup={dups[:5]}",
                     "every task needs exactly one well-formed id (T####)")

    status = load_state_file(
        root, "tasks/task_status.json", checks,
        "TASKSTATE-SET", "task_status.json parses and covers the DAG")
    if status is not None:
        status_tasks = status.get("tasks") or {}
        if not isinstance(status_tasks, dict):
            add_fail(checks, "TASKSTATE-SET",
                     "task_status.json parses and covers the DAG",
                     f"tasks={type(status_tasks).__name__}",
                     "tasks/task_status.json tasks must be an object "
                     "keyed by task id")
            status_tasks = {}

        only_dag = sorted(set(dag_ids) - set(status_tasks))
        only_status = sorted(set(status_tasks) - set(dag_ids))
        if not only_dag and not only_status and dag_ids:
            checks.append(speclib.make_check(
                "TASKSTATE-SET",
                "DAG and task_status cover the same task set",
                "passed", "file",
                f"{len(dag_ids)} tasks both sides",
                "identical sets", ""))
        else:
            add_fail(checks, "TASKSTATE-SET",
                     "DAG and task_status cover the same task set",
                     f"dag_only={only_dag[:10]} status_only={only_status[:10]}",
                     "every task needs a state entry and vice versa — "
                     "a task without one is silently read as todo")

        bad_status = sorted(
            f"{tid}:{v.get('status')}"
            for tid, v in status_tasks.items()
            if not isinstance(v, dict) or v.get("status") not in STATUS_ENUM)
        if not bad_status:
            checks.append(speclib.make_check(
                "TASKSTATE-STATUS-ENUM",
                "state entries use the canonical status enum",
                "passed", "file",
                f"{len(status_tasks)} entries",
                f"one of {', '.join(STATUS_ENUM)}", ""))
        else:
            add_fail(checks, "TASKSTATE-STATUS-ENUM",
                     "state entries use the canonical status enum",
                     f"bad={bad_status[:5]}",
                     "statuses outside the canonical enum (CLAUDE.md §4) "
                     "are invisible to the orchestrator reader")

        bad_ts = sorted(
            f"{tid}.{key}"
            for tid, v in status_tasks.items()
            if isinstance(v, dict)
            for key in ("started_at", "completed_at",
                        "accepted_by_supervisor_at", "merged_at")
            if v.get(key) is not None
            and not (isinstance(v.get(key), str)
                     and ISO_TS_RE.match(v.get(key))))
        if not bad_ts:
            checks.append(speclib.make_check(
                "TASKSTATE-TIMESTAMPS",
                "present timestamps are ISO-8601 dates/datetimes",
                "passed", "file", "all present timestamps parse",
                "ISO-8601", ""))
        else:
            add_fail(checks, "TASKSTATE-TIMESTAMPS",
                     "present timestamps are ISO-8601 dates/datetimes",
                     f"bad={bad_ts[:5]}",
                     "malformed timestamps break timeline rendering")

        overall = status.get("overall")
        if isinstance(overall, str) and overall.strip():
            checks.append(speclib.make_check(
                "TASKSTATE-OVERALL",
                "task_status.json overall is a non-empty string",
                "passed", "file", overall, "non-empty string", ""))
        else:
            add_fail(checks, "TASKSTATE-OVERALL",
                     "task_status.json overall is a non-empty string",
                     repr(overall),
                     "the overall phase marker must stay a readable string")

    tests = load_state_file(
        root, "tasks/tests.json", checks,
        "TASKSTATE-TESTS-SHAPE", "tests.json parses with valid entries")
    if tests is not None:
        t_entries = tests.get("tests")
        if not isinstance(t_entries, list):
            add_fail(checks, "TASKSTATE-TESTS-SHAPE",
                     "tests.json parses with valid entries",
                     f"tests={type(t_entries).__name__}",
                     "tests.json tests must be a list")
            t_entries = []
        t_ids = [e.get("id") for e in t_entries if isinstance(e, dict)]
        dup_t = sorted({i for i in t_ids if t_ids.count(i) > 1})
        missing_fields = []
        for e in t_entries:
            if not isinstance(e, dict):
                missing_fields.append(str(e))
                continue
            for key in ("id", "task_id", "name", "gate"):
                if not isinstance(e.get(key), str) or not e.get(key):
                    missing_fields.append(f"{e.get('id')}:{key}")
                    break
            else:
                if not isinstance(e.get("blocking"), bool):
                    missing_fields.append(f"{e.get('id')}:blocking")
                if e.get("status") not in TEST_STATUS_ENUM:
                    missing_fields.append(f"{e.get('id')}:status")
                for key in ("last_run",):
                    v = e.get(key)
                    if v is not None and not (
                            isinstance(v, str) and ISO_TS_RE.match(v)):
                        missing_fields.append(f"{e.get('id')}:{key}")
                for key in ("evidence",):
                    v = e.get(key)
                    if v is not None and not isinstance(v, str):
                        missing_fields.append(f"{e.get('id')}:{key}")
        if not missing_fields and not dup_t:
            checks.append(speclib.make_check(
                "TASKSTATE-TESTS-SHAPE",
                "tests.json entries carry the required fields",
                "passed", "file",
                f"{len(t_entries)} entries",
                "id/task_id/name/gate/blocking/status typed correctly", ""))
        else:
            add_fail(checks, "TASKSTATE-TESTS-SHAPE",
                     "tests.json entries carry the required fields",
                     f"bad={missing_fields[:5]} dup={dup_t[:5]}",
                     "every test entry needs id, task_id, name, gate, "
                     "blocking, status, last_run, evidence in shape")

        unknown_refs = sorted({e.get("task_id") for e in t_entries
                               if isinstance(e, dict)}
                              - set(dag_ids))
        if not unknown_refs:
            checks.append(speclib.make_check(
                "TASKSTATE-TESTS-REF",
                "every registered test points at a DAG task",
                "passed", "file",
                f"{len(t_entries)} tests", "task_id ∈ DAG", ""))
        else:
            add_fail(checks, "TASKSTATE-TESTS-REF",
                     "every registered test points at a DAG task",
                     f"unknown={unknown_refs[:5]}",
                     "a test for a task outside the DAG records noise")

        covered = {e.get("task_id") for e in t_entries if isinstance(e, dict)}
        uncovered = sorted(set(dag_ids) - covered)
        if not uncovered:
            checks.append(speclib.make_check(
                "TASKSTATE-TESTS-COVERAGE",
                "every DAG task has at least one registered test",
                "passed", "file",
                f"{len(dag_ids)} tasks covered",
                "≥ 1 test per task", ""))
        else:
            add_fail(checks, "TASKSTATE-TESTS-COVERAGE",
                     "every DAG task has at least one registered test",
                     f"uncovered={uncovered[:10]}",
                     "a task without a registered test has no gate "
                     "evidence trail")

    return checks


def print_human(checks):
    failed = 0
    for c in checks:
        mark = {"passed": "ok   ", "failed": "FAIL ", "unknown": "?    ",
                "error": "ERR  "}.get(c["status"], "?    ")
        line = f"{mark}{c['id']}: {c['name']}"
        if c["status"] == "failed":
            failed += 1
            line += f"\n      measured: {c['measured']}\n      detail: {c['detail']}"
        print(line)
    total = len(checks)
    print(f"{failed}/{total} task-state checks failed"
          if failed else f"all {total} task-state checks passed")


def main(argv):
    args = parse_args(argv)
    checks = check_task_state(Path(args.root))
    if args.json:
        print(json.dumps({"checks": checks}, indent=2))
    else:
        print_human(checks)
    return 1 if any(c["status"] == "failed" for c in checks) else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
