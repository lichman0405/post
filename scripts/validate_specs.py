#!/usr/bin/env python3
"""POST specification bundle + task DAG validator (CI gate).

Validates the repository spec bundle deterministically:

  TASKS-*       tasks/tasks.json integrity: parses, task_count matches the
                listed tasks, ids unique and well-formed (T####), every task
                has non-empty requirements and acceptance_criteria, every
                dependency resolves to a defined task (no orphan dependency),
                no task depends on itself, DAG acyclic.
  SPECS-*       every JSON and YAML file under specs/ parses; every other
                spec file (CSV/SQL/...) is readable, non-empty UTF-8.
  DOCS-*        every Markdown file under docs/ and at the repo root is
                readable, non-empty UTF-8.
  SOURCEREPO-*  specs/orchestrator/source-repository.yaml: canonical identity
                self-consistent, default_branch == main, owner-confirmed
                visibility_expectation recorded and valid.

This extends the T0000 bootstrap validator; the original checks (task count,
dependency resolution, non-empty requirements/acceptance_criteria, DAG
acyclicity, JSON schema parseability) are all retained.

Exit codes: 0 all checks passed; 1 at least one check failed; 2 usage error.
Injection (test-only): --root DIR overrides the repository root; a fixture
tree under a temp root is validated exactly like the real repo, so the
decision logic is exercisable without touching the real tree.
"""
import argparse
import json
import sys
from pathlib import Path

import speclib

USAGE_ERROR = 2


def fail_usage(msg):
    print(f"error: {msg}", file=sys.stderr)
    sys.exit(USAGE_ERROR)


def parse_args(argv):
    p = argparse.ArgumentParser(
        prog="scripts/validate_specs.py",
        description="Validate the POST spec bundle and task DAG (see "
                    "scripts/speclib.py for the shared decision logic).")
    p.add_argument("--root", default=str(speclib.default_root()),
                   help="repository root to validate "
                        "(default: parent of scripts/)")
    p.add_argument("--json", action="store_true",
                   help="machine-readable output on stdout")
    return p.parse_args(argv)


# ---------------------------------------------------------------------------
# Individual checks — each returns (status, detail) via speclib.make_check
# ---------------------------------------------------------------------------

def check_tasks(root):
    """TASKS-PARSE/TASKS-COUNT/TASKS-IDS/TASKS-FIELDS/TASKS-DEPS — the
    structural DAG checks over tasks/tasks.json."""
    checks = []
    tasks_path = root / speclib.DEFAULT_TASKS_REL
    if not tasks_path.is_file():
        checks.append(speclib.make_check(
            "TASKS-PARSE", "tasks.json parses and lists tasks", "failed",
            "file", "", "tasks/tasks.json present",
            f"missing: {speclib.DEFAULT_TASKS_REL}"))
        return checks
    try:
        tasks = speclib.load_json(tasks_path)
    except (OSError, json.JSONDecodeError) as exc:
        checks.append(speclib.make_check(
            "TASKS-PARSE", "tasks.json parses and lists tasks", "failed",
            "file", "", "valid JSON",
            f"{speclib.DEFAULT_TASKS_REL} unreadable: {exc}"))
        return checks

    tlist = tasks.get("tasks")
    if not isinstance(tlist, list) or not tlist:
        checks.append(speclib.make_check(
            "TASKS-PARSE", "tasks.json parses and lists tasks", "failed",
            "file", "no tasks list", "non-empty 'tasks' array",
            f"{speclib.DEFAULT_TASKS_REL} has no non-empty 'tasks' array"))
        return checks
    checks.append(speclib.make_check(
        "TASKS-PARSE", "tasks.json parses and lists tasks", "passed",
        "file", f"{len(tlist)} tasks", "non-empty 'tasks' array", ""))

    expected_count = tasks.get("task_count")
    checks.append(speclib.make_check(
        "TASKS-COUNT", "task_count matches listed tasks",
        "passed" if expected_count == len(tlist) else "failed",
        "file", f"task_count={expected_count} listed={len(tlist)}",
        "task_count == len(tasks)",
        "" if expected_count == len(tlist) else
        f"task_count {expected_count!r} != {len(tlist)} listed tasks"))

    ids = [t.get("id") for t in tlist]
    dupes = sorted({i for i in ids if ids.count(i) > 1})
    bad_form = sorted({i for i in ids
                       if not isinstance(i, str) or len(i) != 5
                       or not (i[0] == "T" and i[1:].isdigit())})
    ok_ids = not dupes and not bad_form
    detail = ""
    if dupes:
        detail += f"duplicate ids: {dupes}; "
    if bad_form:
        detail += f"malformed ids (expected T####): {bad_form}; "
    checks.append(speclib.make_check(
        "TASKS-IDS", "task ids unique and well-formed",
        "passed" if ok_ids else "failed",
        "file", f"{len(ids)} ids", "unique ids matching ^T[0-9]{{4}}$",
        detail))

    id_set = set(ids)
    missing_fields = []
    for t in tlist:
        if not isinstance(t.get("requirements"), list) or not t["requirements"]:
            missing_fields.append(f"{t.get('id')}: empty requirements")
        if not isinstance(t.get("acceptance_criteria"), list) \
                or not t["acceptance_criteria"]:
            missing_fields.append(f"{t.get('id')}: empty acceptance_criteria")
    checks.append(speclib.make_check(
        "TASKS-FIELDS", "every task has requirements and acceptance_criteria",
        "passed" if not missing_fields else "failed",
        "file", f"{len(missing_fields)} problem(s)",
        "non-empty lists on every task",
        "; ".join(missing_fields)))

    orphans = []
    self_deps = []
    for t in tlist:
        tid = t.get("id")
        deps = t.get("dependencies")
        if not isinstance(deps, list):
            orphans.append(f"{tid}: dependencies not a list")
            continue
        for d in deps:
            if d == tid:
                self_deps.append(f"{tid} depends on itself")
            elif d not in id_set:
                orphans.append(f"{tid} -> {d} (not a defined task)")
    deps_ok = not orphans and not self_deps
    detail = "; ".join(orphans + self_deps)
    checks.append(speclib.make_check(
        "TASKS-DEPS", "every dependency resolves (no orphan dependency)",
        "passed" if deps_ok else "failed",
        "file", f"{len(orphans)} orphan(s), {len(self_deps)} self-dep(s)",
        "all dependencies defined, none self-referential", detail))

    return checks


def check_acyclic(tlist):
    """TASKS-ACYCLIC — DFS with cycle path reporting."""
    by_id = {t.get("id"): t for t in tlist}
    state = {}

    def visit(tid, path):
        st = state.get(tid)
        if st == 2:
            return None
        if st == 1:
            return path[path.index(tid):] + [tid]
        state[tid] = 1
        deps = by_id[tid].get("dependencies") or []
        for d in deps:
            if d not in by_id:
                continue  # reported by TASKS-DEPS
            cycle = visit(d, path + [tid])
            if cycle:
                return cycle
        state[tid] = 2
        return None

    for tid in by_id:
        cycle = visit(tid, [])
        if cycle:
            return cycle
    return None


def check_specs(root):
    """SPECS-JSON / SPECS-YAML / SPECS-READABLE — parseability and
    readability of the machine-readable spec bundle."""
    checks = []
    json_files = speclib.list_files(root, (speclib.SPECS_DIR_REL,), (".json",))
    yaml_files = speclib.list_files(root, (speclib.SPECS_DIR_REL,),
                                    (".yaml", ".yml"))
    other_files = [p for p in speclib.list_files(root, (speclib.SPECS_DIR_REL,))
                   if not p.endswith((".json", ".yaml", ".yml"))]

    failures = []
    for rel in json_files:
        try:
            speclib.load_json(root / rel)
        except (OSError, json.JSONDecodeError) as exc:
            failures.append(f"{rel}: {exc}")
    checks.append(speclib.make_check(
        "SPECS-JSON", "every JSON spec parses",
        "passed" if not failures else "failed",
        "file", f"{len(json_files)} files, {len(failures)} failures",
        "all JSON valid", "; ".join(failures)))

    yaml_failures = []
    if not speclib.yaml_available():
        yaml_failures.append("PyYAML not importable — install it "
                             "(pip install pyyaml) so YAML specs are "
                             "actually parsed, not just read")
    else:
        try:
            import yaml
        except ImportError:
            yaml = None
        for rel in yaml_files:
            try:
                speclib.load_yaml(root / rel)
            except (OSError, yaml.YAMLError) as exc:
                yaml_failures.append(f"{rel}: {exc}")
    checks.append(speclib.make_check(
        "SPECS-YAML", "every YAML spec parses",
        "passed" if not yaml_failures else "failed",
        "file", f"{len(yaml_files)} files, {len(yaml_failures)} failures",
        "all YAML valid (PyYAML)", "; ".join(yaml_failures)))

    read_failures = []
    for rel in other_files:
        try:
            text = (root / rel).read_text(encoding="utf-8")
            if not text.strip():
                read_failures.append(f"{rel}: empty file")
        except (OSError, UnicodeDecodeError) as exc:
            read_failures.append(f"{rel}: {exc}")
    checks.append(speclib.make_check(
        "SPECS-READABLE", "every other spec file is readable non-empty UTF-8",
        "passed" if not read_failures else "failed",
        "file", f"{len(other_files)} files, {len(read_failures)} failures",
        "readable, non-empty, UTF-8", "; ".join(read_failures)))
    return checks


def check_docs(root):
    """DOCS-READABLE — Markdown specs at root and under docs/."""
    md_files = sorted(p.as_posix() for p in root.glob("*.md")
                      if p.is_file()) + \
        speclib.list_files(root, (speclib.DOCS_DIR_REL,), (".md",))
    failures = []
    for rel in md_files:
        try:
            text = (root / rel).read_text(encoding="utf-8")
            if not text.strip():
                failures.append(f"{rel}: empty file")
        except (OSError, UnicodeDecodeError) as exc:
            failures.append(f"{rel}: {exc}")
    return [speclib.make_check(
        "DOCS-READABLE", "Markdown specs readable (root + docs/)",
        "passed" if not failures else "failed",
        "file", f"{len(md_files)} files, {len(failures)} failures",
        "readable, non-empty, UTF-8", "; ".join(failures))]


def check_source_repo_spec(root):
    """SOURCEREPO-IDENTITY / SOURCEREPO-EXPECTATION — integrity of
    specs/orchestrator/source-repository.yaml."""
    checks = []
    path = root / speclib.SOURCE_REPO_SPEC_REL
    if not path.is_file() or not speclib.yaml_available():
        checks.append(speclib.make_check(
            "SOURCEREPO-IDENTITY", "source-repository.yaml canonical identity",
            "failed", "file", "", "parseable canonical identity spec",
            f"{speclib.SOURCE_REPO_SPEC_REL} missing or unreadable"))
        return checks
    try:
        spec = speclib.load_yaml(path)
    except Exception as exc:  # yaml.YAMLError / OSError
        checks.append(speclib.make_check(
            "SOURCEREPO-IDENTITY", "source-repository.yaml canonical identity",
            "failed", "file", "", "parseable canonical identity spec",
            f"{speclib.SOURCE_REPO_SPEC_REL} unreadable: {exc}"))
        return checks

    src = spec.get("source_repository") if isinstance(spec, dict) else None
    if not isinstance(src, dict):
        checks.append(speclib.make_check(
            "SOURCEREPO-IDENTITY", "source-repository.yaml canonical identity",
            "failed", "file", "no source_repository mapping",
            "owner/name/full_name/default_branch present",
            "source_repository mapping missing"))
        return checks

    problems = []
    owner, name = src.get("owner"), src.get("name")
    if not isinstance(owner, str) or not owner:
        problems.append("missing owner")
    if not isinstance(name, str) or not name:
        problems.append("missing name")
    if src.get("full_name") != f"{owner}/{name}":
        problems.append(f"full_name {src.get('full_name')!r} != "
                        f"{owner}/{name}")
    if src.get("provider") != "github":
        problems.append(f"provider {src.get('provider')!r} != 'github'")
    if src.get("default_branch") != "main":
        problems.append(f"default_branch {src.get('default_branch')!r} "
                        "!= 'main'")
    if isinstance(owner, str) and isinstance(name, str):
        expected_https = f"https://github.com/{owner}/{name}.git"
        if src.get("https_clone_url") != expected_https:
            problems.append(f"https_clone_url {src.get('https_clone_url')!r}"
                            f" != {expected_https!r}")
        expected_ssh = f"git@github.com:{owner}/{name}.git"
        if src.get("ssh_clone_url") != expected_ssh:
            problems.append(f"ssh_clone_url {src.get('ssh_clone_url')!r} "
                            f"!= {expected_ssh!r}")
    checks.append(speclib.make_check(
        "SOURCEREPO-IDENTITY", "source-repository.yaml canonical identity",
        "passed" if not problems else "failed",
        "file", src.get("full_name", "<none>"),
        "provider=github, full_name=owner/name, default_branch=main, "
        "clone URLs consistent", "; ".join(problems)))

    expectation = src.get("visibility_expectation")
    exp_value = speclib.visibility_expectation_value(expectation)
    exp_ok = (exp_value is not None
              and isinstance(expectation.get("confirmed_by"), str)
              and expectation["confirmed_by"].strip()
              and "confirmed_at" in expectation)
    detail = ""
    if exp_ok:
        detail = (f"expectation {exp_value} confirmed by "
                  f"{expectation['confirmed_by']} "
                  f"({expectation.get('confirmed_at')})")
    elif exp_value is None:
        detail = ("visibility_expectation not recorded: record the "
                  "owner-confirmed expectation (value + confirmed_by + "
                  "confirmed_at) — docs/69 §5, "
                  "tasks/decisions.md L3-20260912-1")
    else:
        detail = ("visibility_expectation incomplete: need value + "
                  "confirmed_by + confirmed_at")
    checks.append(speclib.make_check(
        "SOURCEREPO-EXPECTATION", "owner-confirmed visibility expectation "
        "recorded",
        "passed" if exp_ok else "failed",
        "file", exp_value or "<unrecorded>",
        "visibility_expectation with value/confirmed_by/confirmed_at",
        detail))
    return checks


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def run(root):
    checks = []
    checks += check_tasks(root)
    tlist = None
    try:
        tasks = speclib.load_json(root / speclib.DEFAULT_TASKS_REL)
        tlist = tasks.get("tasks") if isinstance(tasks, dict) else None
    except (OSError, json.JSONDecodeError):
        tlist = None
    if isinstance(tlist, list) and tlist:
        cycle = check_acyclic(tlist)
        checks.append(speclib.make_check(
            "TASKS-ACYCLIC", "task dependency graph is acyclic",
            "passed" if cycle is None else "failed",
            "file", "no cycle" if cycle is None else " -> ".join(cycle),
            "no dependency cycle",
            "" if cycle is None else f"cycle: {' -> '.join(cycle)}"))
    else:
        checks.append(speclib.make_check(
            "TASKS-ACYCLIC", "task dependency graph is acyclic", "unknown",
            "file", "tasks list unavailable",
            "no dependency cycle", "skipped: see TASKS-PARSE"))
    checks += check_specs(root)
    checks += check_docs(root)
    checks += check_source_repo_spec(root)
    return checks


def main(argv):
    args = parse_args(argv)
    root = Path(args.root)
    if not root.is_dir():
        fail_usage(f"root directory not found: {root}")
    checks = run(root)
    failures = [c for c in checks if c["status"] == "failed"]
    exit_code = 1 if failures else 0

    if args.json:
        print(json.dumps({"exit_code": exit_code,
                          "ok": exit_code == 0,
                          "checks": checks},
                         sort_keys=True, indent=2))
    else:
        for c in checks:
            mark = {"passed": "ok  ", "failed": "FAIL",
                    "unknown": "----", "error": "ERR "}[c["status"]]
            line = f"{mark} {c['id']:<20} {c['name']}"
            if c["detail"]:
                line += f" — {c['detail']}"
            print(line)
        total = len(checks)
        ok_n = sum(1 for c in checks if c["status"] == "passed")
        print(f"{'OK' if exit_code == 0 else 'FAILED'}: "
              f"{ok_n}/{total} checks passed (exit {exit_code})")
    return exit_code


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
