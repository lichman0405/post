#!/usr/bin/env python3
"""Validate the repository's GitHub Actions workflow files.

Why this exists: a workflow file that does not parse is not a *failing* check —
GitHub refuses to run it at all, and the run fails in 0 seconds. That is how
T0008 shipped a replacement `ci.yml` while deleting the previous
`spec-validation.yml`: the repository ended up with **no working CI**, and
nothing in the local test suite could see it, because the local replica
(`scripts/ci.sh`) tests the *commands* the workflow runs, never the workflow
document itself.

An unquoted colon-space inside a plain scalar is the easy way to hit this:
`- name: go: fmt check` makes YAML parse `go:` as a nested mapping.

Exit codes: 0 = all workflows valid, 1 = at least one problem.
"""
from pathlib import Path
import sys

try:
    import yaml
except ImportError:
    print("error: PyYAML is required (pip install pyyaml)", file=sys.stderr)
    sys.exit(2)

ROOT = Path(__file__).resolve().parents[1]
WORKFLOW_DIR = ROOT / ".github" / "workflows"

VALID_TRIGGERS = {"on"}  # documented for readers; validated structurally below


def check_workflow(path: Path) -> list[str]:
    problems: list[str] = []
    try:
        doc = yaml.safe_load(path.read_text(encoding="utf-8"))
    except yaml.YAMLError as exc:
        # A parse error is the fatal one: GitHub will not run the workflow.
        mark = getattr(exc, "problem_mark", None)
        where = f" at line {mark.line + 1} column {mark.column + 1}" if mark else ""
        return [f"{path.name}: does not parse{where}: {getattr(exc, 'problem', exc)}"]

    if not isinstance(doc, dict):
        return [f"{path.name}: top level must be a mapping"]

    # PyYAML implements YAML 1.1, where a bare `on:` key is parsed as the
    # boolean True rather than the string "on". GitHub accepts both spellings,
    # so accept either here instead of reporting a false failure on a valid file.
    if "on" not in doc and True not in doc:
        problems.append(f"{path.name}: missing an 'on' trigger block")

    jobs = doc.get("jobs")
    if not isinstance(jobs, dict) or not jobs:
        return problems + [f"{path.name}: missing a non-empty 'jobs' mapping"]

    for name, job in jobs.items():
        if not isinstance(job, dict):
            problems.append(f"{path.name}: job {name} is not a mapping")
            continue
        if not job.get("runs-on"):
            problems.append(f"{path.name}: job {name} has no runs-on")
        steps = job.get("steps")
        if not isinstance(steps, list) or not steps:
            problems.append(f"{path.name}: job {name} has no steps")
            continue
        for i, step in enumerate(steps):
            if not isinstance(step, dict):
                problems.append(f"{path.name}: job {name} step {i} is not a mapping")
                continue
            if "uses" not in step and "run" not in step:
                problems.append(
                    f"{path.name}: job {name} step {i} "
                    f"({step.get('name', '<unnamed>')}) has neither 'uses' nor 'run'")
        # needs must reference jobs that exist in the same file
        needs = job.get("needs")
        if needs:
            for dep in ([needs] if isinstance(needs, str) else needs):
                if dep not in jobs:
                    problems.append(f"{path.name}: job {name} needs unknown job {dep}")
    return problems


def main() -> int:
    if not WORKFLOW_DIR.is_dir():
        print(f"error: {WORKFLOW_DIR} not found", file=sys.stderr)
        return 2

    files = sorted(WORKFLOW_DIR.glob("*.yml")) + sorted(WORKFLOW_DIR.glob("*.yaml"))
    if not files:
        print("error: no workflow files found", file=sys.stderr)
        return 2

    all_problems: list[str] = []
    for path in files:
        problems = check_workflow(path)
        if problems:
            all_problems.extend(problems)
        else:
            print(f"ok   {path.name}")

    if all_problems:
        for p in all_problems:
            print(f"FAIL {p}", file=sys.stderr)
        print(f"\n{len(all_problems)} workflow problem(s). A workflow that does not parse "
              f"never runs at all — GitHub fails the run in 0 seconds.", file=sys.stderr)
        return 1
    print(f"OK: {len(files)} workflow file(s) valid")
    return 0


if __name__ == "__main__":
    sys.exit(main())
