#!/usr/bin/env python3
"""Turn a SAST tool's JSON report into a red/green verdict against a
per-finding baseline. Used by tests/security/sast.sh for all three languages
(gosec for Go, eslint+eslint-plugin-security for Node/TypeScript); bandit for
Python reports in the same shape through its own reader below.

WHY A BASELINE FILE AND NOT A FLAG
----------------------------------
gosec reports 256 findings on this tree and eslint-plugin-security 88. (Both
re-derived 2026-09-24, from the rows' own output: `bash tests/security/sast.sh
go` prints the first as "256 finding(s) reported" — this docstring said 254, the
count when it was written, and nothing moves a number here except the code — and
`grep -c '^[^#]' ops/ci/eslint-security-baseline.txt` the second. They are what
those two faces report, not numbers this file maintains.) Some are
the tool's own false positives (it flags every `m[k]` as an object-injection
sink, every `exec.Command(a, "git", args...)` as command injection, every
cookie whose `Secure` is a variable as an insecure cookie); some are real-shaped
sinks in dev tooling and tests. A gate that cannot tell "new" from "there
before" is a gate nobody can keep green, and a gate kept green by a flag
(`-exclude-dir`, `-nosec`, a raised threshold) has stopped measuring.

So every finding that is not new has to be written down, one line per finding:

    <path>:<line>:<column>:<RULE><TAB><category><TAB><reason ≥ 20 chars>

  * the key is the finding itself — file, line, column, rule — so the entry
    stops applying the moment the code moves or the rule stops firing. The
    column is what keeps the key one-to-one: a line can carry two findings of
    the same rule (i18n.ts:1265 has two computed accesses; entity-meta.ts:255
    two regex literals), and a key without it would let one reviewed entry
    cover both;
  * <category> is a closed vocabulary (CATEGORIES below). There is no
    "accepted risk" category: docs/23 §11 routes an accepted High to a written
    ADR, which is the Supervisor's document, not a line in this file. A finding
    nobody can judge does not get to be baselined as safe — it stays red;
  * <reason> says why THIS finding is not a security issue. A line whose reason
    is a rule-level platitude is a line that was not reviewed.

Nothing here can exclude a directory, mute a rule or lower a severity: the
parser below rejects a key containing a glob, and a category outside the
vocabulary or a missing reason is a hard failure of the baseline file itself.
That is the difference between a baseline and a silencer.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from dataclasses import dataclass, field

# The closed vocabulary of why a finding is not a security issue. Every entry
# in every baseline file must use one of these; anything else fails the row.
CATEGORIES = {
    "not-a-security-issue":
        "the rule's claim is factually wrong for this line (the value it calls a "
        "credential is an error code, the sink it names is a constant path, ...)",
    "not-attacker-reachable":
        "the sink is real but no untrusted input can reach it here (test fixture, "
        "dev tooling, operator-supplied CLI argument, compile-time constant)",
    "bounded":
        "the flagged conversion/index is bounded by construction, so the rule's "
        "overflow/out-of-range claim cannot hold",
    "escaping-is-elsewhere":
        "the flagged byte write is the transport of an already-encoded document "
        "(the encoder escapes, the media type is declared, nosniff is set)",
}

KEY_RE = re.compile(r"^(?P<path>[^\t:*]+):(?P<line>[0-9]+):(?P<col>[0-9]+):(?P<rule>[A-Za-z0-9_/.-]+)$")


@dataclass
class Finding:
    path: str          # repo-relative
    line: int
    column: int
    rule: str
    severity: str
    detail: str
    code: str = ""

    @property
    def key(self) -> str:
        return f"{self.path}:{self.line}:{self.column}:{self.rule}"


@dataclass
class Baseline:
    entries: dict = field(default_factory=dict)   # key -> (category, reason)
    problems: list = field(default_factory=list)  # malformed lines, verbatim

    def load(self, path: str) -> None:
        if not os.path.isfile(path):
            self.problems.append(f"baseline file is missing: {path}")
            return
        with open(path, encoding="utf-8") as fh:
            for n, raw in enumerate(fh, 1):
                line = raw.rstrip("\n")
                if not line.strip() or line.lstrip().startswith("#"):
                    continue
                parts = line.split("\t")
                if len(parts) != 3:
                    self.problems.append(
                        f"{path}:{n}: an entry must be `path:line:column:RULE<TAB>category<TAB>reason` "
                        f"(found {len(parts)} tab-separated field(s)): {line!r}"
                    )
                    continue
                key, category, reason = (p.strip() for p in parts)
                m = KEY_RE.match(key)
                if not m:
                    self.problems.append(
                        f"{path}:{n}: {key!r} is not `path:line:column:RULE`. A class entry (a glob, a "
                        "directory, or a rule with no file and line) would exempt findings nobody "
                        "reviewed; list the findings themselves."
                    )
                    continue
                if key in self.entries:
                    self.problems.append(f"{path}:{n}: duplicate entry for {key}")
                    continue
                if category not in CATEGORIES:
                    self.problems.append(
                        f"{path}:{n}: category {category!r} is not one of {sorted(CATEGORIES)}. "
                        "A risk acceptance is a written ADR (docs/23 §11), not a baseline line."
                    )
                    continue
                if len(reason) < 20:
                    self.problems.append(
                        f"{path}:{n}: the reason is {len(reason)} character(s). Say why THIS "
                        "finding is not a security issue; a rule name is not a review."
                    )
                    continue
                self.entries[key] = (category, reason)


def line_of(value) -> int:
    """gosec reports an int for a one-line match and "15-30" for a match that
    spans lines. The baseline key has to be the line the finding starts on, so
    both shapes resolve to the same integer (never an exception: a crash here
    would read as "the tool failed", not as "here is a finding")."""
    first = str(value or "0").split("-")[0].strip()
    return int(first) if first.isdigit() else 0


def parse_gosec(report: dict, root: str) -> tuple[list[Finding], dict]:
    findings = []
    for issue in report.get("Issues") or []:
        path = os.path.relpath(issue.get("file") or "", root)
        findings.append(
            Finding(
                path=path,
                line=line_of(issue.get("line")),
                column=int(issue.get("column") or 0),
                rule=issue.get("rule_id") or "?",
                severity=(issue.get("severity") or "").upper(),
                detail=issue.get("details") or "",
                code=(issue.get("code") or "").strip().splitlines()[-1].strip() if issue.get("code") else "",
            )
        )
    # gosec reports a package it could not type-check twice: once under the
    # package key and once per file. Count the distinct FILES, and treat a
    # file mentioned only inside the aggregated error text as one too, so the
    # row's evidence line is a count of things rather than of report entries.
    errs = report.get("Golang errors") or {}
    untyped_files: set[str] = set()
    for key, entries in errs.items():
        if key:
            untyped_files.add(os.path.relpath(key, root))
        for e in entries or []:
            for hit in re.findall(r"[^\s:]+\.go(?::[0-9]+)?", str(e.get("error") or "")):
                untyped_files.add(os.path.relpath(hit.split(":")[0], root))
    stats = {
        "scanned": int((report.get("Stats") or {}).get("files") or 0),
        "found": int((report.get("Stats") or {}).get("found") or len(findings)),
        "untyped_packages": len(untyped_files),
        "untyped_files": sorted(untyped_files),
    }
    return findings, stats


def parse_eslint(report: list, root: str) -> tuple[list[Finding], dict]:
    findings = []
    files = 0
    for entry in report:
        path = os.path.relpath(entry.get("filePath") or "", root)
        files += 1
        for msg in entry.get("messages") or []:
            findings.append(
                Finding(
                    path=path,
                    line=int(msg.get("line") or 0),
                    column=int(msg.get("column") or 0),
                    rule=msg.get("ruleId") or "parse-error",
                    severity="ERROR" if msg.get("severity") == 2 else "WARNING",
                    detail=(msg.get("message") or "").strip(),
                )
            )
    return findings, {"scanned": files, "found": len(findings), "untyped_packages": 0}


def parse_bandit(report: dict, root: str) -> tuple[list[Finding], dict]:
    findings = []
    for issue in report.get("results") or []:
        findings.append(
            Finding(
                path=os.path.relpath(issue.get("filename") or "", root),
                line=int(issue.get("line_number") or 0),
                column=int(issue.get("col_offset") or 0),
                rule=issue.get("test_id") or "?",
                severity=(issue.get("issue_severity") or "").upper(),
                detail=issue.get("issue_text") or "",
                code=(issue.get("code") or "").strip().splitlines()[0].strip() if issue.get("code") else "",
            )
        )
    metrics = report.get("metrics") or {}
    files = max(0, len([k for k in metrics if k != "_totals"]))
    return findings, {"scanned": files, "found": len(findings), "untyped_packages": 0}


PARSERS = {"gosec": parse_gosec, "eslint": parse_eslint, "bandit": parse_bandit}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--tool", required=True, choices=sorted(PARSERS))
    ap.add_argument("--report", required=True, help="the tool's JSON output")
    ap.add_argument("--baseline", required=True, help="the per-finding baseline file")
    ap.add_argument("--repo", default=os.getcwd())
    ap.add_argument("--id", required=True, help="the check id this line belongs to (sast-go, ...)")
    ap.add_argument("--version", required=True, help="the pinned tool version this run asserted")
    ap.add_argument("--rules", type=int, default=0, help="rule count the run enforced (eslint)")
    ap.add_argument("--allow-empty-scan", action="store_true",
                    help="mutation-check hook: a scan of a planted fixture is allowed to be small. "
                         "Never used by a gate row: the row's evidence line requires a positive file count.")
    args = ap.parse_args()

    root = os.path.abspath(args.repo)
    baseline = Baseline()
    baseline.load(args.baseline if os.path.isabs(args.baseline) else os.path.join(root, args.baseline))

    try:
        with open(args.report, encoding="utf-8") as fh:
            report = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        print(f"{args.id}: the {args.tool} report could not be read ({exc})", file=sys.stderr)
        return 1

    findings, stats = PARSERS[args.tool](report, root)

    # A baseline that cannot be parsed is not "no findings to report": it is a
    # gate whose tolerance is unknown. Fail before looking at any finding.
    if baseline.problems:
        print(f"{args.id}: the baseline file is not usable ({len(baseline.problems)} problem(s)):", file=sys.stderr)
        for p in baseline.problems:
            print(f"  {p}", file=sys.stderr)
        return 1

    if stats["scanned"] == 0 and not args.allow_empty_scan:
        print(
            f"{args.id}: the scan covered 0 file(s). An empty scan reports no findings and would "
            "read as a pass; the target or the tool's arguments are wrong.",
            file=sys.stderr,
        )
        return 1

    unbaselined = [f for f in findings if f.key not in baseline.entries]
    baselined = len(findings) - len(unbaselined)
    used = {f.key for f in findings}
    stale = [k for k in baseline.entries if k not in used]

    sev = {}
    for f in findings:
        sev[f.severity] = sev.get(f.severity, 0) + 1

    if unbaselined:
        print(f"{args.id}: {len(unbaselined)} finding(s) are not in {args.baseline}:", file=sys.stderr)
        for f in unbaselined:
            where = f"{f.path}:{f.line}:{f.column}"
            print(f"  {where}: {f.rule} ({f.severity}) {f.detail}", file=sys.stderr)
            if f.code:
                print(f"      {f.code}", file=sys.stderr)
        print("", file=sys.stderr)
        print(
            f"{args.id}: triage each one. If it is a real finding, fix it — the gate is not the "
            "place to answer it. If it is not, add ONE line per finding to "
            f"{args.baseline} with the category and the reason, in your own review, and never a "
            "rule-wide or directory-wide entry.",
            file=sys.stderr,
        )
        print(
            f"FAIL {args.id}: {len(findings)} finding(s), {len(unbaselined)} unbaselined "
            f"({stats['scanned']} file(s) scanned)",
            file=sys.stderr,
        )
        return 1

    rules = f", {args.rules} rule(s)" if args.rules else ""
    extra = ""
    if stats["untyped_packages"]:
        extra = (f", {stats['untyped_packages']} file(s) it could not type-check "
                 "(listed below — a finding inside one of them would not be a finding)")
    stale_note = f", {len(stale)} baseline entr(ies) no longer fire" if stale else ""
    print(
        f"ok   {args.id}: {args.tool} {args.version}: {stats['scanned']} file(s) scanned{rules}, "
        f"{stats['found']} finding(s) reported ({baselined} baselined (reviewed), 0 unbaselined)"
        f"{extra}{stale_note}"
    )
    if sev:
        print(f"     severity mix: {', '.join(f'{k}={v}' for k, v in sorted(sev.items()))}")
    if stats.get("untyped_files"):
        print("     it could not type-check (so a finding there would go unreported):")
        for p in stats["untyped_files"]:
            print(f"       {p}")
    if stale:
        print(f"     {len(stale)} baseline entr(ies) no longer fire (the code moved or the finding was fixed):")
        for key in sorted(stale)[:10]:
            print(f"       {key}")
        if len(stale) > 10:
            print(f"       ... and {len(stale) - 10} more")
    return 0


if __name__ == "__main__":
    sys.exit(main())
