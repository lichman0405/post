#!/usr/bin/env python3
"""Validate one face's CycloneDX SBOM against the key-component manifest.

    python3 tests/security/check-sbom.py --face go --report .sbom/go.cdx.json \
        --manifest tests/security/key-components.json --id sbom-go \
        --generator "cyclonedx-gomod v1.9.0"

An SBOM that a gate trusts has to answer three questions, and this script is
where each of them becomes a red:

  1. IS IT A DOCUMENT AT ALL? The file must parse as JSON, say
     bomFormat=CycloneDX, carry a specVersion, and list components that each
     have a name and a version. A truncated or empty artifact fails here —
     which is the state a half-written generation leaves behind, and the one
     that would otherwise read as "no vulnerable dependencies found".
  2. DOES IT DESCRIBE THIS TREE? A component count below the manifest's floor
     for the face means the generator matched nothing (an empty lockfile, a
     walk over the wrong directory). "0 components, 0 findings" is the exact
     shape of a green light that measured nothing.
  3. ARE THE NAMED COMPONENTS IN IT? Every manifest entry for this face is
     resolved: an `in-sbom` entry must be present in the document, a
     `present-elsewhere` entry's witness command must print something (and
     what it prints goes into the row's evidence), and an `absent-from-tree`
     entry's witness must print NOTHING — the documented absence is as much a
     claim as the presence, and it has to be able to fail.

Exit codes: 0 the document answers all three; 1 any of them failed; 2 usage.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys

# How a licence may appear in a CycloneDX component. Collected here so the
# count the row prints and the count the audit judges are the same set.
def licences(component: dict) -> tuple[list[str], list[str]]:
    """(spdx ids/names, raw expressions) for one component."""
    ids, exprs = [], []
    for entry in (component.get("licenses") or []):
        if not isinstance(entry, dict):
            continue
        if entry.get("expression"):
            exprs.append(str(entry["expression"]).strip())
        lic = entry.get("license") or {}
        if lic.get("id"):
            ids.append(str(lic["id"]).strip())
        elif lic.get("name"):
            ids.append(str(lic["name"]).strip())
    for entry in ((component.get("evidence") or {}).get("licenses") or []):
        lic = (entry or {}).get("license") or {}
        if lic.get("id"):
            ids.append(str(lic["id"]).strip())
        elif lic.get("name"):
            ids.append(str(lic["name"]).strip())
    return ids, exprs


def component_id(c: dict) -> str:
    """The name a manifest matcher compares against, per ecosystem."""
    purl = c.get("purl") or ""
    group, name = c.get("group") or "", c.get("name") or ""
    if purl.startswith("pkg:npm/"):
        return f"{group}/{name}" if group else name
    return name


def matches(entry: dict, c: dict) -> bool:
    kind = (entry.get("match") or {}).get("kind")
    want = (entry.get("match") or {}).get("id") or ""
    if kind == "pypi":
        return (c.get("name") or "").lower().replace("_", "-") == want.lower().replace("_", "-")
    return component_id(c) == want


def run_witness(cmd: str, root: str) -> tuple[int, str, str]:
    p = subprocess.run(["bash", "-c", cmd], cwd=root, capture_output=True, text=True)
    return p.returncode, p.stdout, p.stderr


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--face", required=True, choices=["go", "node", "python"])
    ap.add_argument("--report", required=True)
    ap.add_argument("--manifest", default="tests/security/key-components.json")
    ap.add_argument("--id", required=True)
    ap.add_argument("--generator", required=True, help="what produced the document, for the evidence line")
    ap.add_argument("--repo", default=os.getcwd())
    args = ap.parse_args()

    root = os.path.abspath(args.repo)
    report = args.report if os.path.isabs(args.report) else os.path.join(root, args.report)
    manifest_path = args.manifest if os.path.isabs(args.manifest) else os.path.join(root, args.manifest)

    def bad(msg: str) -> int:
        print(f"FAIL {args.id}: {msg}", file=sys.stderr)
        return 1

    try:
        with open(manifest_path, encoding="utf-8") as fh:
            manifest = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        return bad(f"the key-component manifest {args.manifest} could not be read ({exc})")

    # 1. a document at all
    try:
        with open(report, encoding="utf-8") as fh:
            doc = json.load(fh)
    except OSError as exc:
        return bad(f"the SBOM {args.report} does not exist ({exc}) — nothing was generated")
    except json.JSONDecodeError as exc:
        return bad(
            f"the SBOM {args.report} is not valid JSON ({exc}). An unparseable document is not an "
            "empty inventory: it is an inventory nobody can read, and every check below would pass "
            "vacuously against it."
        )
    if not isinstance(doc, dict):
        return bad(f"the SBOM {args.report} is a {type(doc).__name__}, not a CycloneDX object")
    if (doc.get("bomFormat") or "") != "CycloneDX":
        return bad(f"the SBOM {args.report} does not say bomFormat=CycloneDX (got {doc.get('bomFormat')!r})")
    if not doc.get("specVersion"):
        return bad(f"the SBOM {args.report} carries no specVersion")

    components = doc.get("components")
    if not isinstance(components, list):
        return bad(f"the SBOM {args.report} has no components list")
    if not components:
        return bad(
            f"the SBOM {args.report} lists 0 components. A manifest with no components cannot be "
            "checked against anything, and 'no components' is what a generation that walked the "
            "wrong directory produces."
        )

    nameless = [i for i, c in enumerate(components) if not (c.get("name") and c.get("version"))]
    if nameless:
        return bad(
            f"the SBOM {args.report} has {len(nameless)} component(s) without a name and version "
            f"(index {nameless[0]}). An entry that names no version cannot be licensed or audited."
        )

    # 2. does it describe this tree?
    floor = (manifest.get("floors") or {}).get(args.face)
    if not isinstance(floor, int):
        return bad(f"the manifest names no component floor for face {args.face!r}")
    if len(components) < floor:
        return bad(
            f"the SBOM {args.report} lists {len(components)} component(s), and this face requires at "
            f"least {floor}. A security manifest that lost its components reports exactly as clean "
            "as one that has none to report."
        )

    # 3. the named components
    entries = [e for e in (manifest.get("components") or []) if e.get("layer") == args.face]
    in_sbom = [e for e in entries if e.get("sbom") == args.face]
    other = [e for e in entries if e.get("sbom") != args.face]

    problems, present, witness_lines = [], [], []
    for entry in in_sbom:
        subject = (entry.get("match") or {}).get("id") or entry.get("name")
        hit = next((c for c in components if matches(entry, c)), None)
        if hit is None:
            problems.append(
                f"the key component {entry['name']!r} ({subject}) is not in this SBOM. docs/40 names "
                f"it as a component whose version and licence the release record has to carry. If it "
                f"genuinely left the tree, the manifest entry is what changes — never the check."
            )
            continue
        ids, exprs = licences(hit)
        seen = ", ".join(ids + exprs) or "no licence in the document"
        present.append(f"{subject} {hit.get('version')} ({seen})")

    for entry in other:
        cmd = entry.get("witness")
        if not cmd:
            problems.append(f"the manifest entry {entry['name']!r} has no `witness` command to check it with")
            continue
        rc, out, err = run_witness(cmd, root)
        expect = entry.get("expect") or {}
        where = " ".join((out or "").split()) or "(no output)"
        if rc > 1:
            problems.append(
                f"the witness for {entry['name']!r} exited {rc} ({' '.join((err or '').split())[:120]}). "
                f"A witness that cannot run is not evidence about the tree: {cmd}"
            )
            continue
        if expect.get("stdout_nonempty") and not out.strip():
            problems.append(
                f"the witness for {entry['name']!r} printed nothing, so nothing in this tree says it is "
                f"here: {cmd}. Either the component moved (update the manifest) or the thing this entry "
                "watched was removed — and the entry exists precisely so that neither happens quietly."
            )
            continue
        if expect.get("stdout_empty") and out.strip():
            problems.append(
                f"the witness for {entry['name']!r} printed: {where}. The manifest records this component "
                f"as ABSENT from the tree, and {cmd} says otherwise — so either the manifest is stale or "
                "the dependency arrived without anyone deciding its licence. Both are red."
            )
            continue
        pat = expect.get("stdout_matches")
        if pat and not re.search(pat, out):
            problems.append(f"the witness for {entry['name']!r} printed {where}, which does not match {pat!r}")
            continue
        kind = "absent, as documented" if expect.get("stdout_empty") else "witnessed"
        witness_lines.append(f"{entry['name']}: {kind} — {where}")

    if problems:
        print(f"{args.id}: {len(problems)} key-component problem(s):", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        print("", file=sys.stderr)
        return bad(
            f"{len(components)} component(s) in {args.report}, and the named components above could "
            "not be resolved against tests/security/key-components.json"
        )

    with_id = with_expr = without = 0
    for c in components:
        ids, exprs = licences(c)
        if ids:
            with_id += 1
        elif exprs:
            with_expr += 1
        else:
            without += 1

    meta = doc.get("metadata") or {}
    subject = (meta.get("component") or {}).get("name") or "(unnamed)"
    print(
        f"ok   {args.id}: {args.generator}: {len(components)} component(s), spec {doc['specVersion']}, "
        f"licence: {with_id} spdx id / {with_expr} expression / {without} none"
    )
    print(f"     subject: {subject} (the document's own metadata.component; not a dependency, not audited as one)")
    for line in witness_lines:
        print(f"     {line}")
    if present:
        print(f"     in this SBOM: {'; '.join(present)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
