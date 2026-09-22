#!/usr/bin/env python3
"""Add licence evidence to the Python face's CycloneDX document.

    uv export --format cyclonedx1.5 --frozen --no-emit-project -o .sbom/python.raw.json
    python3 tests/security/sbom_python_licenses.py \
        --raw .sbom/python.raw.json --venv services/scientific-adapter/.venv \
        --out .sbom/python.cdx.json

WHY THIS STEP EXISTS
--------------------
`uv export --format cyclonedx1.5` emits purl, version and hashes for every
locked distribution, and NO licence at all — so a licence audit run against it
alone would either pass vacuously or have to guess for all six components.
The licence data exists in the tree's own environment: every installed wheel
carries `<name>-<version>.dist-info/METADATA`, and PEP 639 put an SPDX
`License-Expression` in it. This script copies that field across, per
component, and records where each licence came from in the component's own
`properties` — so the SBOM says which licences are asserted by package
metadata and which are not known at all.

WHAT IT DELIBERATELY DOES NOT DO
--------------------------------
It does not look a licence up, infer one from a package's name, or write a
placeholder for a component it could not resolve. A component with no
installed distribution gets no `licenses` field and a `post:licence-source`
property saying why. The audit (tests/security/license_audit.py) is required
to name such a component individually rather than pass over it.

Exit codes: 0 the document was written; 1 the inputs could not be read;
2 usage.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys

# PEP 503 normalisation: compare names the way the packaging tools do.
def normalize(name: str) -> str:
    return re.sub(r"[-_.]+", "-", name or "").lower()


# `Classifier: License :: OSI Approved :: MIT License` -> an SPDX id. Only the
# classifications that appear in this environment are mapped; anything else
# stays unresolved on purpose rather than being approximated.
CLASSIFIER_TO_SPDX = {
    "mit license": "MIT",
    "apache software license": "Apache-2.0",
    "bsd license": "BSD-3-Clause",
    "the unlicense (unlicense)": "Unlicense",
    "isc license": "ISC",
}

SPDX_SIMPLE = re.compile(r"^[A-Za-z0-9.+-]+$")
SPDX_EXPRESSION = re.compile(r"\b(AND|OR|WITH)\b")


def read_metadata(path: str) -> dict[str, str]:
    """The handful of METADATA fields that carry a licence."""
    fields: dict[str, str] = {}
    classifiers: list[str] = []
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            if line.startswith("License-Expression:"):
                fields["expression"] = line.split(":", 1)[1].strip()
            elif line.startswith("License:"):
                value = line.split(":", 1)[1].strip()
                if value:
                    fields["license"] = value
            elif line.startswith("Classifier: License ::"):
                classifiers.append(line.split("::")[-1].strip().lower())
    if classifiers:
        fields["classifier"] = classifiers[-1]
    return fields


def licence_of(meta: dict[str, str]) -> tuple[dict | None, str]:
    """(the CycloneDX `licenses` entry, where the value came from)."""
    expr = meta.get("expression")
    if expr:
        if SPDX_SIMPLE.match(expr) and not SPDX_EXPRESSION.search(expr):
            return {"license": {"id": expr}}, "METADATA License-Expression"
        if SPDX_EXPRESSION.search(expr):
            return {"expression": expr}, "METADATA License-Expression (compound)"
    raw = meta.get("license")
    if raw:
        if SPDX_SIMPLE.match(raw) and not SPDX_EXPRESSION.search(raw):
            return {"license": {"id": raw}}, "METADATA License"
        return {"license": {"name": raw}}, "METADATA License (free text)"
    cls = meta.get("classifier")
    if cls:
        spdx = CLASSIFIER_TO_SPDX.get(cls)
        if spdx:
            return {"license": {"id": spdx}}, f"METADATA Classifier (License :: {cls})"
        return {"license": {"name": cls}}, "METADATA Classifier (free text)"
    return None, ""


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--raw", required=True, help="the uv export --format cyclonedx1.5 document")
    ap.add_argument("--venv", required=True, help="the adapter's virtualenv to read dist-info from")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    try:
        with open(args.raw, encoding="utf-8") as fh:
            doc = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        sys.exit(f"sbom-python: cannot read {args.raw} ({exc})")

    sitepackages = None
    for root, dirs, _ in os.walk(args.venv):
        if os.path.basename(root) == "site-packages":
            sitepackages = root
            break
    if sitepackages is None:
        sys.exit(
            f"sbom-python: no site-packages under {args.venv}. The licences come from the installed "
            "distributions' METADATA, and an environment that was never installed has none — run "
            "`uv sync --frozen` in services/scientific-adapter (or `make security-tools`) first."
        )

    metadatas: dict[tuple[str, str], str] = {}
    for name in os.listdir(sitepackages):
        if not name.endswith(".dist-info"):
            continue
        stem = name[: -len(".dist-info")]
        if "-" not in stem:
            continue
        pkg, _, version = stem.rpartition("-")
        metadatas[(normalize(pkg), version)] = os.path.join(sitepackages, name, "METADATA")

    resolved = unresolved = 0
    for component in doc.get("components") or []:
        key = (normalize(component.get("name", "")), str(component.get("version")))
        path = metadatas.get(key)
        props = component.setdefault("properties", [])
        if path is None or not os.path.isfile(path):
            unresolved += 1
            props.append({
                "name": "post:licence-source",
                "value": (
                    "not resolved: uv.lock locks this distribution for another platform, so it is not "
                    "installed in this environment and carries no METADATA to read a licence from. "
                    "UNVERIFIED by the SBOM — the licence audit is required to name it."
                ),
            })
            continue
        entry, source = licence_of(read_metadata(path))
        if entry is None:
            unresolved += 1
            props.append({
                "name": "post:licence-source",
                "value": f"not resolved: {os.path.relpath(path, args.venv)} has no licence field",
            })
            continue
        component.setdefault("licenses", []).append(entry)
        resolved += 1
        props.append({"name": "post:licence-source", "value": source})

    doc.setdefault("metadata", {}).setdefault("properties", []).append({
        "name": "post:licence-enrichment",
        "value": (
            f"licences read from {resolved} installed dist-info METADATA file(s) under "
            f"{os.path.relpath(sitepackages, os.getcwd())}; {unresolved} component(s) left without "
            "licence data (named individually in their own properties)"
        ),
    })

    with open(args.out, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, indent=2)
        fh.write("\n")
    print(f"     licences: {resolved} from dist-info METADATA, {unresolved} unresolved (named in the document)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
