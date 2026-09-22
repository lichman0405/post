#!/usr/bin/env python3
"""Check ops/security/absent-checks.json — the Master Security/Quality Gate's
inventory of docs/23_SECURITY_PRIVACY.md §11.

A gap that is written down is a decision; a gap that is written down
*loosely* is a sentence nobody can contradict. This checker is what makes the
manifest falsifiable, and it is a row of the gate itself
(tests/security/master-security-gate.sh --list), so it runs with every gate
run:

  1. every capability docs/23 §11 asks a Release to run is in `items` —
     exactly the six of that section, no more and no fewer, so an item
     cannot be dropped quietly;
  2. an item marked `covered` names check ids that EXIST in the live gate
     registry, read out of the gate itself (--list-json). A manifest pointing
     at a check that was renamed, deleted or never written is red here;
  3. an item marked `absent` has a detail entry with the four things a
     reader needs — why it is absent, what stands in for it today, what
     adding it would take, what the gap costs — and it does not say the
     capability is unnecessary;
  4. the absent set is exactly the container scan. SAST and the SBOM are NOT
     absent any more — they are eight rows of the gate (sast-*, sbom-*,
     license-audit, container-scan; see the manifest's items), and the checker
     proves those ids exist rather than letting a capability slide back into
     this list. The dependency audit is likewise a gate row (govulncheck /
     pnpm audit / uv audit); if one of those rows is removed, the
     covered-item check goes red instead of the audit becoming an "absence";
  5. every `absence_witness` command is RUN, and its `expect` has to hold —
     no output for a walk that must find nothing, or a match for something
     that must still be there (a registration, a written acceptance).
     This is the half that keeps the file from rotting: add a Dockerfile and
     the witness prints it, rename the ADR the acceptance lives in and the
     witness stops matching, and this row fails until the manifest is updated
     to say so;
  6. the remaining absence is a GUARDED one, and the guard is checked to
     exist: the entry names `guard_check_id`, a row of the live gate registry
     whose only job is to keep the absence visible and to go red the moment
     it stops being true. A guard that was renamed or deleted guards nothing,
     and that is a failure here rather than a green row pointing at a check
     nobody runs.

Nothing here decides whether a gap is an accepted risk. docs/23 §11 allows a
written ADR/risk acceptance for High findings and does not accept Critical in
V1; that call is the Supervisor's, and the manifest records it as such.

Usage
    python3 tests/security/check-absent-manifest.py [--manifest PATH] [--repo PATH] [--selftest]

Exit codes
    0  the manifest is complete, its covered ids exist, and the witnesses
       still show the gaps
    1  the manifest is wrong, stale, or names a check the gate no longer has
    2  a required file is missing (counted as a failure, never a silent pass)
    3  usage error
    4  --selftest found the checker unable to say no

--selftest runs two mutations on a COPY of the manifest and requires this
checker to reject each of them, naming container-scan: (a) the guard it cites
is pointed at a check id the gate does not have, and (b) the absence entry is
deleted outright. A checker whose failure mode has never been executed is a
checker nobody has evidence for.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
GATE = "tests/security/master-security-gate.sh"
DEFAULT_MANIFEST = "ops/security/absent-checks.json"

# Every capability a Release is asked to run: the six of
# docs/23_SECURITY_PRIVACY.md §11 — dependency audit、SAST、secret scan、
# container scan、OWASP smoke、permission E2E — plus the SBOM, which is not in
# §11 but is asked for by docs/25_CICD_DEVOPS.md items 9-10 ("security/
# dependency/secret scan"、"container build/SBOM"). Hard-coded on purpose: a
# list read out of the manifest could not notice an item leaving it.
REQUIRED_ITEMS = [
    "dependency-audit",
    "sast",
    "secret-scan",
    "container-scan",
    "owasp-smoke",
    "permission-e2e",
    "sbom",
]

# The capability this tree genuinely still cannot perform: scanning container
# images it does not build. SAST and the SBOM were on this list and are not any
# more — they are rows of the gate now, and a capability that came back here
# would mean those rows disappeared. That is what the covered-item check above
# is for.
REQUIRED_ABSENT = ["container-scan"]

ABSENT_FIELDS = ["why_absent", "substitute_today", "to_add", "impact", "suggested_disposition"]
# A guarded absence carries one more thing: the prose that says what keeps it
# honest, and (below, checked against the registry) the row that does it.
GUARDED_FIELDS = ABSENT_FIELDS + ["guard"]

# A gap restated as a non-gap. Every one of these turns "we do not have this"
# into "we do not need this", which docs/23 §11 is explicit about: the
# capability is required for every Release, and a gap is answered with a
# written risk acceptance, never with a sentence in a manifest.
NOT_A_GAP = re.compile(
    r"(?i)\b(not\s+(?:needed|required|necessary|applicable)|no\s+need|unnecessary|"
    r"nothing\s+to\s+worry\s+about|n/?a\b)"
)

FAILS = 0


def fail(msg: str) -> None:
    global FAILS
    FAILS += 1
    print(f"FAIL {msg}")


def ok(msg: str) -> None:
    print(f"ok   {msg}")


def gate_registry(repo: str) -> dict:
    """The live gate registry, straight out of the gate itself."""
    proc = subprocess.run(
        ["bash", GATE, "--list-json"],
        cwd=repo,
        capture_output=True,
        text=True,
        timeout=120,
    )
    if proc.returncode != 0:
        raise RuntimeError(
            f"`bash {GATE} --list-json` exited {proc.returncode}: {proc.stderr.strip()[:400]}"
        )
    return json.loads(proc.stdout)


def check_manifest(path: str, repo: str) -> None:
    if not os.path.isfile(path):
        print(f"check-absent-manifest: no manifest at {path}", file=sys.stderr)
        sys.exit(2)
    with open(path, encoding="utf-8") as fh:
        manifest = json.load(fh)

    try:
        registry = gate_registry(repo)
    except Exception as exc:  # the registry is the other half of this check
        fail(f"the gate registry could not be read ({exc})")
        registry = {"checks": []}
    check_ids = {c["id"] for c in registry.get("checks", [])}

    ok(f"manifest: {path} parses and the gate registry was read ({len(check_ids)} check(s) registered)")

    items = manifest.get("items")
    if not isinstance(items, list) or not items:
        fail("manifest has no `items` list")
        return

    seen = [i.get("id") for i in items]
    if sorted(seen) != sorted(REQUIRED_ITEMS):
        fail(
            "items must be exactly the capabilities docs/23 §11 (+ docs/25 item 10 for the SBOM) ask of a Release: "
            f"expected {sorted(REQUIRED_ITEMS)}, found {sorted(seen)}"
        )
    else:
        ok("manifest: items are exactly the required capabilities of docs/23 §11 (plus the SBOM of docs/25 item 10)")

    absent_entries = {a.get("id"): a for a in manifest.get("absent", [])}
    if sorted(absent_entries) != sorted(REQUIRED_ABSENT):
        fail(
            "the absent set must be exactly the capabilities this tree still cannot perform: "
            f"expected {sorted(REQUIRED_ABSENT)}, found {sorted(absent_entries)}. SAST and the SBOM "
            "left this list when the gate gained rows for them; a capability reappearing here means "
            "those rows went away."
        )
    else:
        ok("manifest: the absent set is exactly the container scan (SAST and the SBOM are gate rows now)")

    for item in items:
        iid = item.get("id")
        status = item.get("status")
        if status == "covered":
            ids = item.get("check_ids") or []
            if not ids:
                fail(f"item {iid}: marked covered with no check_ids")
                continue
            missing = [c for c in ids if c not in check_ids]
            if missing:
                fail(
                    f"item {iid}: names check(s) the gate does not have: {missing} "
                    f"(a manifest pointing at a check that was renamed or deleted is a claim about nothing)"
                )
                continue
            ok(f"item: {iid} — covered by {', '.join(ids)}, present in the gate registry")
        elif status == "absent":
            absent_id = item.get("absent_id")
            entry = absent_entries.get(absent_id)
            if entry is None:
                fail(f"item {iid}: status absent, but there is no `absent` entry with id '{absent_id}'")
                continue
            kind = entry.get("kind")
            if kind not in ("absent", "guarded-absence"):
                fail(f"item {iid}: entry '{absent_id}' has kind '{kind}', want 'absent' or 'guarded-absence'")
                continue
            if entry.get("command") is not None:
                fail(f"item {iid}: an absent capability must not carry a command (found {entry.get('command')!r})")
            guard = ""
            if kind == "guarded-absence":
                guard = entry.get("guard_check_id") or ""
                if not guard:
                    fail(
                        f"item {iid}: entry '{absent_id}' is a guarded absence with no guard_check_id. "
                        "A guard that is not named is a guard nobody can check."
                    )
                    continue
                if guard not in check_ids:
                    fail(
                        f"item {iid}: the guard check '{guard}' is not in the gate registry. The whole "
                        "point of this entry is that a row exists which goes red when the absence stops "
                        "being real; a guard that was renamed or deleted guards nothing."
                    )
                    continue
            fields = GUARDED_FIELDS if kind == "guarded-absence" else ABSENT_FIELDS
            empty = [f for f in fields if not str(entry.get(f) or "").strip()]
            if empty:
                fail(f"item {iid}: entry '{absent_id}' is missing {empty} — a gap has to name its substitute, its cost and its impact")
                continue
            witnesses = entry.get("absence_witness") or []
            if not witnesses:
                fail(f"item {iid}: entry '{absent_id}' carries no absence_witness, so nothing checks that it is still absent")
                continue
            prose = " ".join(str(entry.get(f) or "") for f in ("why_absent", "impact", "substitute_today"))
            if NOT_A_GAP.search(prose):
                fail(
                    f"item {iid}: entry '{absent_id}' reads as 'this is not needed'. "
                    "docs/23 §11 requires the capability for every Release: an absence is answered with a written risk acceptance, not with a sentence."
                )
                continue
            for w in witnesses:
                cmd = w.get("command")
                if not cmd:
                    fail(f"item {iid}: an absence_witness has no command")
                    continue
                proc = subprocess.run(
                    ["bash", "-c", cmd], cwd=repo, capture_output=True, text=True, timeout=120
                )
                produced = [ln for ln in proc.stdout.splitlines() if ln.strip()]
                expect = w.get("expect")
                if isinstance(expect, dict):
                    pat = expect.get("stdout_matches")
                    if pat and not any(re.search(pat, ln) for ln in produced):
                        fail(
                            f"item {iid}: the absence witness `{cmd}` must match {pat!r} and printed "
                            f"{produced[:3] or 'nothing'}"
                        )
                        continue
                    ok(f"item: {iid} — witness `{cmd}` matches {pat!r}")
                elif produced:
                    fail(
                        f"item {iid}: the absence witness now produces output, so this entry is stale — "
                        f"`{cmd}` printed: {produced[:3]}"
                    )
                else:
                    ok(f"item: {iid} — witness still shows the gap (`{cmd}` printed nothing)")
            if guard:
                ok(
                    f"item: {iid} — guarded absence: the '{guard}' row of this gate is registered and goes red "
                    "the moment the absence stops being real"
                )
            else:
                ok(f"item: {iid} — absent, with substitute, cost and impact")
        else:
            fail(f"item {iid}: status '{status}' is not covered|absent")


def selftest(repo: str) -> int:
    """Prove this checker can say no: two mutations of a copy, each of which
    has to come back red and name container-scan.

    The two are the shapes a guarded absence can rot into: its guard is
    renamed out from under it (so the entry cites a row nobody runs), and the
    entry is deleted outright (so the capability is silently covered).
    """
    with open(os.path.join(repo, DEFAULT_MANIFEST), encoding="utf-8") as fh:
        manifest = json.load(fh)

    def guard_points_at_nothing(m: dict) -> None:
        for a in m.get("absent", []):
            if a.get("id") == "container-scan":
                a["guard_check_id"] = "container-scan-that-does-not-exist"

    def absence_deleted(m: dict) -> None:
        m["absent"] = [a for a in m.get("absent", []) if a.get("id") != "container-scan"]
        for item in m.get("items", []):
            if item.get("id") == "container-scan":
                item["absent_id"] = "container-scan-that-does-not-exist"

    mutations = [
        ("the guard it cites is not a check of this gate", guard_points_at_nothing),
        ("the absence entry is deleted outright", absence_deleted),
    ]

    failures = 0
    with tempfile.TemporaryDirectory() as tmp:
        for what, mutate in mutations:
            mutated = json.loads(json.dumps(manifest))
            mutate(mutated)
            path = os.path.join(tmp, "absent-checks.json")
            with open(path, "w", encoding="utf-8") as fh:
                json.dump(mutated, fh)
            proc = subprocess.run(
                [sys.executable, os.path.abspath(__file__), "--manifest", path, "--repo", repo],
                capture_output=True,
                text=True,
                timeout=300,
            )
            if proc.returncode == 0:
                print(
                    f"FAIL selftest: the checker accepted a manifest in which {what} — it cannot say no",
                    file=sys.stderr,
                )
                failures += 1
                continue
            if "container-scan" not in (proc.stdout + proc.stderr):
                print(
                    f"FAIL selftest: the checker rejected the manifest in which {what} but never named "
                    f"container-scan, so the rejection was for another reason:\n{proc.stdout}{proc.stderr}",
                    file=sys.stderr,
                )
                failures += 1
                continue
            print(
                f"ok   selftest: {what} makes this checker exit {proc.returncode} and name container-scan"
            )
    return 4 if failures else 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--manifest", default=DEFAULT_MANIFEST)
    ap.add_argument("--repo", default=REPO)
    ap.add_argument("--selftest", action="store_true")
    args = ap.parse_args()

    repo = os.path.abspath(args.repo)
    if not os.path.isfile(os.path.join(repo, GATE)):
        print(f"check-absent-manifest: the gate is missing at {GATE}", file=sys.stderr)
        return 2

    if args.selftest:
        rc = selftest(repo)
        if rc != 0:
            return rc
        # and then the real manifest still has to pass, or the selftest would
        # be measuring a checker that says no to everything
        check_manifest(os.path.join(repo, DEFAULT_MANIFEST), repo)
        return 1 if FAILS else 0

    suite = args.manifest if os.path.isabs(args.manifest) else os.path.join(repo, args.manifest)
    check_manifest(suite, repo)
    if FAILS:
        print(f"\ncheck-absent-manifest: {FAILS} failure(s)", file=sys.stderr)
        return 1
    print("\ncheck-absent-manifest: OK — every §11 capability is covered or named, and every named gap is still a gap")
    return 0


if __name__ == "__main__":
    sys.exit(main())
