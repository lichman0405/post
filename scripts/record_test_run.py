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
import re
import sys
from datetime import datetime, timezone

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TESTS_REL = "tasks/tests.json"
TAIL_LINES = 4

# How much of --output-file to pull in: the tail, read by seeking, so a huge or
# endless log cannot be pulled into memory just to quote four lines of it.
READ_BYTES = 256 * 1024

# The credential shapes worker_collect.go refuses to let a Worker introduce
# (secretPatterns, internal/devorchestrator/worker_collect.go:81). Mirrored
# deliberately: this tool copies text out of a log file and into a TRACKED state
# file, which is the same "no secret material introduced" boundary the diff scan
# guards. Keep the two lists in step.
SECRET_PATTERNS = [
    ("GitHub classic PAT (ghp_)", re.compile(r"ghp_[A-Za-z0-9]{20,}")),
    ("GitHub fine-grained PAT (github_pat_)", re.compile(r"github_pat_[A-Za-z0-9_]{20,}")),
    ("AWS access key id (AKIA…)", re.compile(r"\bAKIA[0-9A-Z]{16}\b")),
    ("private key block", re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----")),
    ("Anthropic API key (sk-ant-)", re.compile(r"sk-ant-[A-Za-z0-9_-]{20,}")),
]


def clean(text: str, limit: int) -> str:
    """One-line, control-character-free, length-capped text for a state file."""
    flat = " ".join((text or "").split())
    flat = "".join(ch for ch in flat if ch >= " ")
    return flat[:limit]


def _tail_of(path: str) -> str:
    """The last few non-blank lines of a file, cleaned for a state file.

    Control characters are stripped because the destination is JSON that a human
    reads and diffs; a log line carrying a raw escape sequence would otherwise
    land in the ledger verbatim.
    """
    size = os.path.getsize(path)
    with open(path, "rb") as fh:
        if size > READ_BYTES:
            fh.seek(size - READ_BYTES)
        data = fh.read(READ_BYTES)
    text = data.decode("utf-8", errors="replace")
    lines = [ln.rstrip() for ln in text.splitlines() if ln.strip()]
    tail = " ; ".join(lines[-TAIL_LINES:])[-600:]
    return "".join(ch for ch in tail if ch == "\t" or ch >= " ")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--id", action="append", required=True, dest="ids",
                    help="tests.json entry id (repeatable)")
    ap.add_argument("--command", required=True, help="the command that was run, verbatim")
    ap.add_argument("--exit-code", type=int, required=True)
    ap.add_argument("--output-file", help="file holding the command's output")
    ap.add_argument("--note", default="", help="anything a reader needs (host, tree, why)")
    ap.add_argument("--apply", action="store_true")
    ap.add_argument("--repo", help="repository root (default: this script's repo)")
    args = ap.parse_args()

    repo = os.path.abspath(args.repo) if args.repo else REPO
    path = os.path.join(repo, TESTS_REL)
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
            tail = _tail_of(args.output_file)
        except OSError as exc:
            print("cannot read --output-file: %s" % exc, file=sys.stderr)
            return 2
        # Name the class, never the matched text: the refusal must not copy the
        # credential it found into a terminal, a shell history or this file.
        for label, pattern in SECRET_PATTERNS:
            if pattern.search(tail):
                print("refusing to copy this output into %s: it matches %s "
                      "(worker_collect.go's secret pattern set). Quote it by "
                      "hand only if you have established it is not a credential."
                      % (TESTS_REL, label), file=sys.stderr)
                return 3

    now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    status = "passed" if args.exit_code == 0 else "failed"
    evidence = "Supervisor ran `%s` -> exit %d on %s.%s%s" % (
        clean(args.command, 400), args.exit_code, now,
        (" Last output: %s" % tail) if tail else "",
        (" " + clean(args.note, 400)) if args.note else "",
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
