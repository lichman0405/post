#!/usr/bin/env python3
"""Reconcile tasks/tests.json against the tasks that have actually been merged.

Why this exists
---------------
docs/24_TEST_STRATEGY.md 7 requires every blocking test suite to be tracked in
tasks/tests.json "so a long-horizon Claude Code can follow it". T1206's
acceptance criterion is literally "tests.json blocking all passed", and T1207's
is "all V1 gates checked". On 2026-09-21 the ledger held 173 blocking entries
with 22 passed and 151 not_run -- because nothing in the orchestrator writes
this file (no Go code under internal/ or cmd/ names it; validate_task_state.py
checks its SHAPE only). A ledger that no one maintains cannot be the instrument
for the master gate: it stays red after the work is green, and the temptation is
to flip entries by hand.

So this reconciles it from evidence that already exists. A task that reached
`merged` went through G1 (Worker ran the task's required tests), G2 (Supervisor
checked the diff, scope and acceptance criteria) and G4 (merge gate). Its
Worker's RESULT.json records each required test with a label, a command, a
status and its output. An entry whose task is merged, and whose name matches a
passed test in that task's RESULT.json, is a fact already proven; recording it
is bookkeeping, not a gate being talked into green.

The trust boundary, stated plainly
----------------------------------
The key to this file is the task being `merged` in tasks/task_status.json, and
Workers cannot write that file: it is forbidden scope for them
(specs/orchestrator/worker-permissions.yaml) and the Supervisor owns every
transition into it. So a flipped entry means "the Supervisor accepted this task,
and the Worker's record for the tree that merged says this test passed". It does
NOT mean the Supervisor re-ran this test -- the evidence string says so.

What that leaves open: a Worker could record `passed` for a command it did not
really run, and no amount of reading files here would tell. This tool raises the
bar (the task must be merged; the entry must name a command that exists; the
label must match exactly) but it cannot close that, because the only thing that
closes it is running the test again -- which is what scripts/record_test_run.py
is for, and a real re-run writes its own evidence string.

What it deliberately does NOT do
--------------------------------
* It never touches an entry whose task is not `merged`. Those tests have not
  run because the work is not done, and `not_run` is the truthful value.
* It never writes `passed` for a test it cannot point at. No label match, no
  flip -- that entry stays in the residue report.
* It never writes `passed` from an entry that records no command.
  specs/orchestrator/worker-result.schema.json requires `command` on every test
  entry, so an entry without one is not a record of a run and is not evidence of
  anything; it goes to the residue report. (Checked on 2026-09-21 across every
  RESULT.json in .rddev/workers: no entry anywhere claimed `passed` without a
  command, so this rule refuses nothing that was ever true.)
* It does not claim the Supervisor re-ran anything. The evidence string says
  where the pass came from (the Worker's own record, on the tree that merged)
  and leaves "verified by re-run" to a real re-run, whose result is written by
  scripts/record_test_run.py or by hand with the command output quoted.
* Dry run by default. `--apply` writes.

Usage
-----
    scripts/reconcile_tests_ledger.py                # show what would change
    scripts/reconcile_tests_ledger.py --apply        # write
    scripts/reconcile_tests_ledger.py --residue      # only the leftover report
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from datetime import datetime, timezone

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TESTS_REL = "tasks/tests.json"
STATUS_REL = "tasks/task_status.json"
WORKERS_REL = ".rddev/workers"


def load(rel: str):
    with open(os.path.join(REPO, rel), encoding="utf-8") as fh:
        return json.load(fh)


def worker_results() -> dict:
    """task_id -> RESULT.json contents, for every Worker that left one."""
    out = {}
    root = os.path.join(REPO, WORKERS_REL)
    if not os.path.isdir(root):
        return out
    for name in os.listdir(root):
        path = os.path.join(root, name, "RESULT.json")
        if not os.path.exists(path):
            continue
        try:
            with open(path, encoding="utf-8") as fh:
                out[name] = json.load(fh)
        except (OSError, ValueError):
            continue  # an unreadable record is not evidence of anything
    return out


def clean(text, limit: int = 300) -> str:
    """Flatten Worker-authored text before it lands in a TRACKED state file.

    RESULT.json is written by a Worker and never tracked; tasks/tests.json is
    tracked and committed. Copying one into the other makes the Worker's strings
    part of the repository's record, so they are flattened to one line, stripped
    of control characters and capped: a label or command is a fact to quote, not
    a place to put a paragraph.
    """
    flat = " ".join(str(text or "").split())
    flat = "".join(ch for ch in flat if ch >= " ")
    return flat[:limit]


def matching_pass(result: dict, name: str):
    """The RESULT.json test entry this ledger entry refers to, if any.

    Matched on the label, because that is the field a Worker is required to set
    to the task's required-test name. Exact match only: a near miss is a
    question for a human, and a fuzzy matcher here would be a machine for
    turning "roughly the same test" into "passed".
    """
    for entry in result.get("tests") or []:
        if not isinstance(entry, dict):
            continue
        if (entry.get("label") or "").strip() == name.strip():
            return entry
    return None


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--apply", action="store_true",
                    help="write the changes (default: report only)")
    ap.add_argument("--residue", action="store_true",
                    help="print only the entries that stay unpassed")
    ap.add_argument("--repo", help="repository root (default: this script's repo)")
    args = ap.parse_args()
    if args.repo:
        global REPO
        REPO = os.path.abspath(args.repo)

    ledger = load(TESTS_REL)
    entries = ledger.get("tests") or []
    status = load(STATUS_REL).get("tasks") or {}
    results = worker_results()
    today = datetime.now(timezone.utc).strftime("%Y-%m-%d")

    flipped, residue_merged, residue_open = [], [], []
    for entry in entries:
        if entry.get("status") == "passed":
            continue
        tid = entry.get("task_id") or entry["id"].split("-")[0]
        task = status.get(tid) or {}
        name = entry.get("name", "")

        if task.get("status") != "merged":
            residue_open.append((entry["id"], tid, task.get("status", "(no record)"), name))
            continue

        result = results.get(tid)
        hit = matching_pass(result, name) if result else None
        if hit is None or hit.get("status") != "passed":
            why = ("no RESULT.json for this task" if result is None
                   else "no passed test labelled " + repr(name) + " in its RESULT.json")
            residue_merged.append((entry["id"], tid, name, why))
            continue

        # The schema requires `command` on every test entry, so an entry without
        # one is a claim about nothing that was run. Refused, not backfilled.
        command = clean(hit.get("command"), 300)
        if not command:
            residue_merged.append((entry["id"], tid, name,
                                   "the matching entry records no command, and "
                                   "worker-result.schema.json requires one -- it is not a run"))
            continue

        merged_at = clean(task.get("merged_at") or task.get("accepted_by_supervisor_at") or "", 40)
        evidence = (
            "Backfilled from the task's own Worker record on %s (not a fresh re-run). "
            "The Worker ran `%s` on the tree that was merged -- RESULT.json test label "
            "%r, status passed -- and the task then cleared G2 (Supervisor diff/scope/"
            "acceptance review) and G4, merging at %s. The pass is the Worker's recorded "
            "run as validated by that acceptance, not a Supervisor re-run; a re-run "
            "writes its own evidence string."
            % (today, command, name, merged_at or "an unrecorded time")
        )
        flipped.append((entry, merged_at, evidence))

    if args.residue:
        print_residue(residue_merged, residue_open)
        return 0

    print("tests.json reconciliation -- %s" % ("APPLYING" if args.apply else "dry run"))
    print("  entries total          : %d" % len(entries))
    print("  already passed         : %d" % sum(1 for e in entries if e.get("status") == "passed"))
    print("  would flip to passed   : %d" % len(flipped))
    print("  stay not_run (task open): %d" % len(residue_open))
    print("  stay not_run (no evidence): %d" % len(residue_merged))
    print()
    if flipped:
        print("flipping (first 10):")
        for entry, merged_at, _ in flipped[:10]:
            print("  %-22s %-46s merged %s" % (entry["id"], entry["name"][:46], merged_at or "-"))
    print_residue(residue_merged, residue_open)

    if not args.apply:
        print("\nnothing written; re-run with --apply")
        return 0

    for entry, merged_at, evidence in flipped:
        entry["status"] = "passed"
        entry["last_run"] = merged_at or (today + "T00:00:00Z")
        entry["evidence"] = evidence

    with open(os.path.join(REPO, TESTS_REL), "w", encoding="utf-8") as fh:
        json.dump(ledger, fh, ensure_ascii=False, indent=2)
        fh.write("\n")
    print("\nwrote %s (%d entries flipped)" % (TESTS_REL, len(flipped)))
    return 0


def print_residue(residue_merged, residue_open):
    if residue_merged:
        print("\nMERGED but no evidence found -- these need a real run before T1206:")
        for entry_id, _, name, why in residue_merged:
            print("  %-22s %-46s %s" % (entry_id, name[:46], why))
    if residue_open:
        print("\nstill open (not_run is the truthful value; %d entries):" % len(residue_open))
        for entry_id, _, st, name in residue_open:
            print("  %-22s %-12s %-44s" % (entry_id, st, name[:44]))


if __name__ == "__main__":
    sys.exit(main())
