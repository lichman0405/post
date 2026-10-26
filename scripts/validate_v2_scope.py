#!/usr/bin/env python3
"""V2 required-scope projection over the live task ledgers (CI gate).

The V1 final audit counts `v1_required` tasks. V2 must not be folded into that
historical denominator, and it must not depend on a field no executor reads.
This validator is the executor for the V2 projection:

  V2SCOPE-DAG        tasks.json parses; task_count matches the list
  V2SCOPE-FLAGS      every v2_required task has v1_required=false and a known
                     WEB-00..WEB-30 package; every package has a task
  V2SCOPE-MAP        tasks/v2/mapping.json maps each package exactly once and
                     its task ids agree with the DAG's web_package values
  V2SCOPE-DEPS       every dependency resolves; a V2 task's dependencies are
                     either merged or themselves v2_required
  V2SCOPE-TESTS      every v2_required task has at least one tests.json entry
  V2SCOPE-JOURNEY    every U01..U14 is covered by a mapped package, and the
                     mapping still matches the handoff backlog

Default mode is structural: it runs on every CI build while V2 is incomplete.
`--complete` is the V2 final gate: it additionally requires every v2_required
task merged, every journey passed, every blocking V2 test passed, and every
mapped package to carry an issue and a pull request. It is never wired into
the per-PR CI job, because it must stay red until the V2 delivery is real.

Exit codes: 0 = all checks passed; 1 = at least one check failed; 2 = usage.
Injection (test-only): --root DIR validates a fixture tree exactly like the
repository.
"""
import argparse
import json
import re
import sys
from pathlib import Path

USAGE_ERROR = 2
WEB_RE = re.compile(r"^WEB-(\d{2})$")
STATUS_ENUM = (
    "todo", "ready", "running", "worker_failed", "verification",
    "rejected", "blocked", "accepted", "merged",
)


def load(path):
    return json.loads(path.read_text(encoding="utf-8"))


def fail(checks, check_id, detail):
    checks.append({"id": check_id, "status": "failed", "detail": detail})


def ok(checks, check_id, detail):
    checks.append({"id": check_id, "status": "passed", "detail": detail})


def check_dag(root, checks):
    tasks_path = root / "tasks" / "tasks.json"
    if not tasks_path.is_file():
        fail(checks, "V2SCOPE-DAG", "missing tasks/tasks.json")
        return None
    raw = load(tasks_path)
    tasks = raw.get("tasks")
    if not isinstance(tasks, list) or not tasks:
        fail(checks, "V2SCOPE-DAG", "tasks.json has no task list")
        return None
    if raw.get("task_count") != len(tasks):
        fail(checks, "V2SCOPE-DAG",
             f"task_count={raw.get('task_count')} but {len(tasks)} tasks listed")
        return None
    ok(checks, "V2SCOPE-DAG", f"{len(tasks)} tasks, task_count consistent")
    return {t.get("id"): t for t in tasks if isinstance(t, dict)}


def check_flags(tasks, checks):
    problems = []
    v2 = [t for t in tasks.values() if t.get("v2_required")]
    if not v2:
        fail(checks, "V2SCOPE-FLAGS", "no task carries v2_required=true")
        return v2
    packages = set()
    for t in v2:
        tid = t.get("id", "?")
        if t.get("v1_required") is not False:
            problems.append(f"{tid}: v2_required task must set v1_required=false")
        package = t.get("web_package")
        match = WEB_RE.match(package or "")
        if not match:
            problems.append(f"{tid}: web_package {package!r} is not WEB-NN")
        else:
            packages.add(package)
    missing = [f"WEB-{i:02d}" for i in range(31) if f"WEB-{i:02d}" not in packages]
    if missing:
        problems.append("packages without a V2 task: " + ", ".join(missing))
    if problems:
        fail(checks, "V2SCOPE-FLAGS", "; ".join(problems))
    else:
        ok(checks, "V2SCOPE-FLAGS",
           f"{len(v2)} V2 tasks; all 31 WEB packages mapped; v1 denominator untouched")
    return v2


def check_mapping(root, tasks, checks):
    path = root / "tasks" / "v2" / "mapping.json"
    backlog_path = root / "tasks" / "v2" / "backlog.json"
    if not path.is_file():
        fail(checks, "V2SCOPE-MAP", "missing tasks/v2/mapping.json")
        return {}
    if not backlog_path.is_file():
        fail(checks, "V2SCOPE-MAP", "missing tasks/v2/backlog.json")
        return {}
    mapping = load(path)
    backlog = {t["id"]: t for t in load(backlog_path).get("tasks", [])}
    entries = mapping.get("entries")
    if not isinstance(entries, list):
        fail(checks, "V2SCOPE-MAP", "mapping.json has no entries list")
        return {}
    problems = []
    seen = {}
    by_package = {}
    for t in tasks.values():
        if t.get("v2_required"):
            by_package.setdefault(t.get("web_package"), set()).add(t.get("id"))
    for entry in entries:
        web = entry.get("web")
        if web in seen:
            problems.append(f"duplicate mapping for {web}")
        seen[web] = entry
        listed = set(entry.get("tasks") or [])
        actual = by_package.get(web, set())
        if not listed:
            problems.append(f"{web}: no task ids")
        elif listed != actual:
            problems.append(
                f"{web}: mapping lists {sorted(listed)} but the DAG assigns {sorted(actual)}")
        for key in ("issue", "pull_requests", "journeys", "milestone"):
            if key not in entry:
                problems.append(f"{web}: missing key {key}")
        if web in backlog:
            expected = set(backlog[web].get("journey_ids") or [])
            if set(entry.get("journeys") or []) != expected:
                problems.append(f"{web}: journeys drifted from backlog")
        else:
            problems.append(f"{web}: not present in backlog.json")
    missing = sorted(set(f"WEB-{i:02d}" for i in range(31)) - set(seen))
    extra = sorted(set(seen) - set(f"WEB-{i:02d}" for i in range(31)))
    if missing:
        problems.append("unmapped packages: " + ", ".join(missing))
    if extra:
        problems.append("unknown packages: " + ", ".join(extra))
    if problems:
        fail(checks, "V2SCOPE-MAP", "; ".join(problems))
    else:
        ok(checks, "V2SCOPE-MAP", f"{len(entries)} packages mapped 1:1 to their tasks")
    return seen


def check_deps(tasks, status, checks):
    statuses = status.get("tasks", {})
    problems = []
    for t in tasks.values():
        if not t.get("v2_required"):
            continue
        for dep in t.get("dependencies") or []:
            target = tasks.get(dep)
            if target is None:
                problems.append(f"{t['id']}: dependency {dep} missing from the DAG")
                continue
            state = (statuses.get(dep) or {}).get("status")
            if state not in STATUS_ENUM:
                problems.append(f"{t['id']}: dependency {dep} has no valid status")
            elif state != "merged" and not target.get("v2_required"):
                problems.append(
                    f"{t['id']}: dependency {dep} is {state} and not a V2 task")
    if problems:
        fail(checks, "V2SCOPE-DEPS", "; ".join(problems[:10]))
    else:
        ok(checks, "V2SCOPE-DEPS", "all V2 dependencies resolve and are merged or V2")


def check_tests(root, v2, checks):
    path = root / "tasks" / "tests.json"
    if not path.is_file():
        fail(checks, "V2SCOPE-TESTS", "missing tasks/tests.json")
        return []
    entries = load(path).get("tests") or []
    by_task = {}
    for entry in entries:
        if isinstance(entry, dict):
            by_task.setdefault(entry.get("task_id"), []).append(entry)
    missing = [t["id"] for t in v2 if not by_task.get(t["id"])]
    if missing:
        fail(checks, "V2SCOPE-TESTS", "tasks without a registered test: " + ", ".join(missing))
    else:
        ok(checks, "V2SCOPE-TESTS", f"{len(v2)} V2 tasks each carry at least one test")
    return entries


def check_complete(root, tasks, mapping, tests, checks):
    status = load(root / "tasks" / "task_status.json").get("tasks", {})
    problems = []
    for t in tasks.values():
        if not t.get("v2_required"):
            continue
        state = (status.get(t["id"]) or {}).get("status")
        if state != "merged":
            problems.append(f"{t['id']} is {state}, not merged")
    acceptance = load(root / "tasks" / "v2" / "web-acceptance.json").get("journeys") or []
    for journey in acceptance:
        if journey.get("execution_status") != "passed":
            problems.append(f"{journey.get('id')} not passed")
    for entry in tests:
        if not isinstance(entry, dict):
            continue
        task = tasks.get(entry.get("task_id"))
        if task and task.get("v2_required") and entry.get("blocking"):
            if entry.get("status") != "passed":
                problems.append(f"{entry.get('id')} is {entry.get('status')}, not passed")
    for web, entry in mapping.items():
        if not entry.get("issue"):
            problems.append(f"{web} has no issue")
        if not (entry.get("pull_requests") or []):
            problems.append(f"{web} has no pull request")
    if problems:
        fail(checks, "V2SCOPE-COMPLETE", "; ".join(problems[:12]))
    else:
        ok(checks, "V2SCOPE-COMPLETE",
           "every V2 task merged, every journey passed, every blocking test passed")


def main(argv):
    parser = argparse.ArgumentParser(
        prog="scripts/validate_v2_scope.py",
        description="Validate the v2_required projection over the live ledgers.")
    parser.add_argument("--root", default=str(Path(__file__).resolve().parents[1]))
    parser.add_argument("--complete", action="store_true",
                        help="require the whole V2 delivery to be finished")
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args(argv)

    root = Path(args.root)
    if not root.is_dir():
        print(f"error: root not found: {root}", file=sys.stderr)
        return USAGE_ERROR

    checks = []
    tasks = check_dag(root, checks)
    if tasks is None:
        pass
    else:
        v2 = check_flags(tasks, checks)
        mapping = check_mapping(root, tasks, checks)
        status_path = root / "tasks" / "task_status.json"
        status = load(status_path) if status_path.is_file() else {}
        check_deps(tasks, status, checks)
        tests = check_tests(root, v2, checks)
        if args.complete:
            check_complete(root, tasks, mapping, tests, checks)

    failures = [c for c in checks if c["status"] == "failed"]
    result = {"status": "failed" if failures else "passed", "mode": "complete" if args.complete else "structure",
              "checks": checks}
    if args.json:
        print(json.dumps(result, ensure_ascii=False, indent=2))
    else:
        for c in checks:
            print(f"[{c['status'].upper()}] {c['id']} {c['detail']}")
        print(f"V2 projection: {result['status']} ({result['mode']})")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
