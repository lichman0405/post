#!/usr/bin/env python3
"""Record the result of a test run you actually performed into tasks/tests.json.

The other half of scripts/reconcile_tests_ledger.py. That one backfills entries
from a Worker's own record, and deliberately refuses to write `passed` for
anything it cannot point at. This one is for the case where the Supervisor (or
T1206's master gate) runs the suite by hand: it writes down the command, the
exit code and a slice of the real output, so a later reader can tell a measured
pass from a copied one.

Refuses to record `passed` for a non-zero exit code -- a run that failed is
written as `failed`, which is the truthful value and is what T1206 must see.

Usage
-----
    scripts/record_test_run.py --id T0012-TEST-01 --command 'bash tests/acceptance/four-gate-e2e.sh' \
        --exit-code 0 --output-file /tmp/four-gate.log [--apply]

    # several ids covered by one command (the same run proves each):
    scripts/record_test_run.py --id T0012-TEST-01 --id T0012-TEST-02 --command ... --exit-code 0 ...

Dry run unless --apply is given. `--output-file` is reduced to its last few
lines, which is what a reader needs to see that the command really ran; the
full log is not copied into a state file.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from datetime import datetime, timezone

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TESTS_REL = "tasks/tests.json"
TAIL_LINES = 4


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--id", action="append", required=True, dest="ids",
                    help="tests.json entry id (repeatable)")
    ap.add_argument("--command", required=True, help="the command that was run, verbatim")
    ap.add_argument("--exit-code", type=int, required=True)
    ap.add_argument("--output-file", help="file holding the command's output")
    ap.add_argument("--note", default="", help="anything a reader needs (host, tree, why)")
    ap.add_argument("--apply", action="store_true")
    args = ap.parse_args()

    path = os.path.join(REPO, TESTS_REL)
    with open(path, encoding="utf-8") as fh:
        ledger = json.load(fh)
    by_id = {e.get("id"): e for e in ledger.get("tests") or []}

    unknown = [i for i in args.ids if i not in by_id]
    if unknown:
        print("no such entry: %s" % ", ".join(unknown), file=sys.stderr)
        return 2

    tail = ""
    if args.output_file:
        try:
            with open(args.output_file, encoding="utf-8", errors="replace") as fh:
                lines = [ln.rstrip() for ln in fh.readlines() if ln.strip()]
            tail = " ; ".join(lines[-TAIL_LINES:])[-600:]
        except OSError as exc:
            print("cannot read --output-file: %s" % exc, file=sys.stderr)
            return 2

    now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    status = "passed" if args.exit_code == 0 else "failed"
    evidence = "Supervisor ran `%s` -> exit %d on %s.%s%s" % (
        args.command, args.exit_code, now,
        (" Last output: %s" % tail) if tail else "",
        (" " + args.note) if args.note else "",
    )

    for entry_id in args.ids:
        entry = by_id[entry_id]
        print("%-22s %-40s %s -> %s" % (
            entry_id, (entry.get("name") or "")[:40], entry.get("status"), status))
        if args.apply:
            entry["status"] = status
            entry["last_run"] = now
            entry["evidence"] = evidence

    if not args.apply:
        print("\nnothing written; re-run with --apply")
        return 0

    with open(path, "w", encoding="utf-8") as fh:
        json.dump(ledger, fh, ensure_ascii=False, indent=2)
        fh.write("\n")
    print("\nwrote %s" % TESTS_REL)
    return 0


if __name__ == "__main__":
    sys.exit(main())
