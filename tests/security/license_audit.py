#!/usr/bin/env python3
"""Judge every SBOM component's licence against ops/security/license-allowlist.json.

    python3 tests/security/license_audit.py \
        --dir .sbom --manifest tests/security/key-components.json \
        --allowlist ops/security/license-allowlist.json

This is the licence half of docs/40_OPEN_SOURCE_LICENSES.md:3 ("实际版本与许可证
在 bootstrap/CI 中生成 SBOM 并核对") and of docs/23_SECURITY_PRIVACY.md §11's
"Critical/High 必须修复或有书面 ADR/risk acceptance".

WHAT MAKES IT RED
-----------------
  * a component under a deny-class licence (strong copyleft or a use
    restriction): the row prints package + licence + version. This is the
    finding the audit exists for;
  * a licence in neither the allow table nor the deny list — an unlisted
    licence is a decision nobody has made;
  * a component with no licence data that is not named in `unlicensed`;
  * an SBOM that is missing entirely, or that lists no components.
  * an infra image in docker-compose.yml with no manifest entry recording its
    licence, or a manifest entry whose image is no longer in the compose file.

WHAT IT REPORTS WITHOUT FAILING (and says so, every run)
--------------------------------------------------------
  * components whose licence the SBOM does not carry and whose `unlicensed`
    entry has no witness: they are printed as UNVERIFIED. A licence nobody can
    check is not the same finding as a licence that is fine, and the row must
    not let the two look alike;
  * infra images whose manifest judgement is "needs-adr" — Redis (RSALv2 /
    SSPLv1) and MinIO (AGPL-3.0). Those are service images rather than package
    dependencies, and what to do about a source-available or AGPL service is a
    legal decision for the Supervisor (docs/40 principle 1), not a measurement
    this script can make. They are printed under NEEDS ADR;
  * infra images whose judgement is "unknown" — a service whose licence this
    tree records nowhere. Printed under LICENCE UNVERIFIED, which is a
    different statement from "checked and permissive".

Exit codes: 0 every component judged and nothing denied; 1 a deny, an
unlisted licence, an unlicensed-and-unnamed component, or an unusable SBOM;
2 usage.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

FACES = ("go", "node", "python")
# SPDX allows "A OR B" and "A AND B"; WITH introduces an exception clause. The
# audit judges each operand, so a dual licence passes only if BOTH arms are
# acceptable (the conservative reading).
SPLIT_RE = re.compile(r"\s+(?:OR|AND)\s+")


def expand_witness_path(path: str) -> str:
    """A witness may point into the Go module cache by name. $GOMODCACHE is a
    `go env` setting rather than an exported variable, so it is resolved from
    `go env` when the environment does not already carry it — and the audit
    still fails loudly if the file turns out not to be there."""
    out = os.path.expandvars(path)
    if "$GOMODCACHE" in out:
        try:
            import subprocess
            cache = subprocess.run(["go", "env", "GOMODCACHE"], capture_output=True, text=True,
                                   check=True).stdout.strip()
            out = out.replace("$GOMODCACHE", cache)
        except Exception:
            pass
    return out


def load_module(path: str, name: str):
    import importlib.util
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def expand(tokens: list[str]) -> list[str]:
    out: list[str] = []
    for t in tokens:
        out.extend(p.strip() for p in SPLIT_RE.split(t) if p.strip())
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--dir", default=".sbom", help="where the three CycloneDX documents live")
    ap.add_argument("--manifest", default="tests/security/key-components.json")
    ap.add_argument("--allowlist", default="ops/security/license-allowlist.json")
    ap.add_argument("--id", default="license-audit")
    ap.add_argument("--repo", default=os.getcwd())
    args = ap.parse_args()

    root = os.path.abspath(args.repo)
    here = os.path.dirname(os.path.abspath(__file__))
    sb = load_module(os.path.join(here, "check-sbom.py"), "check_sbom")

    def path_of(p: str) -> str:
        return p if os.path.isabs(p) else os.path.join(root, p)

    def fail(msg: str) -> int:
        print(f"FAIL {args.id}: {msg}", file=sys.stderr)
        return 1

    try:
        with open(path_of(args.allowlist), encoding="utf-8") as fh:
            allow = json.load(fh)
        with open(path_of(args.manifest), encoding="utf-8") as fh:
            manifest = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        return fail(f"a policy file could not be read ({exc})")

    allowed: dict[str, str] = allow.get("allow") or {}
    deny_ids = set(allow.get("deny", {}).get("ids") or [])
    deny_patterns = allow.get("deny", {}).get("name_patterns") or []
    first_party = allow.get("first_party") or []
    unlicensed = allow.get("unlicensed") or []

    def denied(token: str) -> str:
        if token in deny_ids:
            return f"a deny-class licence id ({token})"
        for p in deny_patterns:
            if re.search(p["matches"], token):
                return p.get("why") or p["matches"]
        return ""

    def is_first_party(cid: str) -> str:
        for e in first_party:
            if e["match"] == cid or cid.endswith("/" + e["match"]):
                return e.get("why") or ""
        return ""

    def unlicensed_entry(name: str, version: str) -> dict | None:
        for e in unlicensed:
            if e.get("match") == name and str(e.get("version")) == str(version):
                return e
        return None

    problems: list[str] = []
    denied_lines: list[str] = []
    unverified: list[str] = []
    first_party_seen: list[str] = []
    licence_counts: dict[str, int] = {}
    total = 0

    for face in FACES:
        report = path_of(os.path.join(args.dir, f"{face}.cdx.json"))
        try:
            with open(report, encoding="utf-8") as fh:
                doc = json.load(fh)
        except OSError:
            problems.append(
                f"the {face} SBOM is missing at {os.path.relpath(report, root)}. The licence audit reads "
                "the documents the sbom-* rows produce; a missing one is not 'nothing to report', it is "
                "no inventory to report from."
            )
            continue
        except json.JSONDecodeError as exc:
            problems.append(f"the {face} SBOM at {os.path.relpath(report, root)} is not valid JSON ({exc})")
            continue

        components = doc.get("components") or []
        if not components:
            problems.append(f"the {face} SBOM at {os.path.relpath(report, root)} lists no components")
            continue

        for c in components:
            total += 1
            name = sb.component_id(c) or c.get("name") or "(unnamed)"
            version = str(c.get("version") or "(no version)")
            ids, exprs = sb.licences(c)
            tokens = expand(ids) + expand(exprs)

            if not tokens:
                why = is_first_party(name)
                if why:
                    first_party_seen.append(f"{face}: {name}@{version} — {why}")
                    continue
                entry = unlicensed_entry(name, version)
                if entry is None:
                    problems.append(
                        f"{face}: {name}@{version} carries no licence in the SBOM and is not named in "
                        f"{args.allowlist}. Add the licence to the generator's output if it exists, or "
                        "record the component in `unlicensed` with the reason it cannot be resolved — "
                        "silence is not a judgement."
                    )
                    continue
                wit = entry.get("witness")
                if not wit:
                    unverified.append(f"{face}: {name}@{version} — {entry.get('why', '')[:160]}")
                    continue
                wpath = expand_witness_path(wit["file"])
                try:
                    with open(wpath, "rb") as fh:
                        blob = fh.read()
                except OSError as exc:
                    problems.append(
                        f"{face}: {name}@{version} is recorded as licensed by {wit['file']}, and that file "
                        f"cannot be read ({exc}). The entry claims a verification that can no longer be "
                        "performed — which is the state a baseline entry must never be in."
                    )
                    continue
                head = blob.decode("utf-8", "replace").splitlines()[:5]
                if wit.get("head_matches") and not re.search(wit["head_matches"], head[0] if head else ""):
                    problems.append(
                        f"{face}: {name}@{version} — {wit['file']} no longer starts with "
                        f"{wit['head_matches']!r} (first line: {head[0] if head else '<empty>'!r})"
                    )
                    continue
                if wit.get("sha256") and hashlib.sha256(blob).hexdigest() != wit["sha256"]:
                    problems.append(
                        f"{face}: {name}@{version} — the content of {wit['file']} is not the file this "
                        "entry was written against (sha256 mismatch). The licence claim has to be "
                        "re-verified rather than carried forward."
                    )
                    continue
                licence_counts["(from LICENSE file)"] = licence_counts.get("(from LICENSE file)", 0) + 1
                continue

            for token in tokens:
                licence_counts[token] = licence_counts.get(token, 0) + 1
                if token in allowed:
                    continue
                reason = denied(token)
                if reason:
                    denied_lines.append(f"{face}: {name}@{version} — {token}: {reason}")
                else:
                    problems.append(
                        f"{face}: {name}@{version} is under {token!r}, which is in neither the allow table "
                        f"nor the deny list of {args.allowlist}. An unlisted licence is a decision nobody "
                        "has made; record it (with a reason) or stop depending on it."
                    )

    # Infra images: the services the deployment runs, whose licences are a
    # decision rather than a measurement. Two-way check, so neither a new
    # service nor a moved tag can pass unnoticed.
    infra = [e for e in (manifest.get("components") or []) if e.get("layer") == "infra"]
    compose = path_of("docker-compose.yml")
    compose_images: list[str] = []
    try:
        with open(compose, encoding="utf-8") as fh:
            for line in fh:
                m = re.match(r"\s*image:\s*(\S+)\s*$", line)
                if m:
                    compose_images.append(m.group(1))
    except OSError as exc:
        problems.append(f"docker-compose.yml could not be read ({exc})")
    manifest_images = [e.get("image") for e in infra if e.get("image")]
    needs_adr: list[str] = []
    undecided: list[str] = []
    for e in infra:
        if not e.get("image") or not e.get("licence") or not e.get("judgement"):
            problems.append(
                f"the infra entry {e.get('name')!r} needs `image`, `licence` and `judgement` — the "
                "deployment's services are dependencies whose licence somebody has to decide."
            )
            continue
        if e["judgement"] not in ("allow", "needs-adr", "unknown"):
            problems.append(
                f"the infra entry {e['name']!r} has judgement {e['judgement']!r} (expected allow, "
                "needs-adr or unknown)"
            )
            continue
        if e["judgement"] == "needs-adr":
            needs_adr.append(f"{e['image']} ({e['name']}) — {e['licence']}")
        elif e["judgement"] == "unknown":
            undecided.append(f"{e['image']} ({e['name']}) — {e['licence']}")
    for img in compose_images:
        base = img.split(":")[0]
        if not any(m and m.split(":")[0] == base for m in manifest_images):
            problems.append(
                f"docker-compose.yml runs {img}, and no manifest entry records its licence. A service "
                "image is a dependency: docs/40 principle 1 wants its licence, version, purpose and "
                "alternatives written down."
            )
    for img in manifest_images:
        if img not in compose_images:
            problems.append(
                f"the manifest records the image {img}, which docker-compose.yml no longer runs. The "
                "recorded licence belongs to a version that is not deployed — re-check and update it."
            )

    if problems:
        print(f"{args.id}: {len(problems)} problem(s):", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        print("", file=sys.stderr)
        return fail(f"{total} component(s) audited across {len(FACES)} SBOM(s), {len(problems)} unresolved")

    if denied_lines:
        print(f"{args.id}: {len(denied_lines)} component(s) under a deny-class licence:", file=sys.stderr)
        for d in denied_lines:
            print(f"  DENIED  {d}", file=sys.stderr)
        print("", file=sys.stderr)
        print(
            "docs/23 §11 and docs/40 principle 1: a high-copyleft or use-restricted dependency needs a "
            "written ADR and legal confirmation. That is the Supervisor's decision, and until it exists "
            "this row is red.",
            file=sys.stderr,
        )
        return fail(f"{len(denied_lines)} deny-class component(s); {total} component(s) audited")

    mix = ", ".join(f"{k}={v}" for k, v in sorted(licence_counts.items(), key=lambda kv: (-kv[1], kv[0])))
    print(f"ok   {args.id}: {total} component(s) across {len(FACES)} SBOM(s), every licence judged against {args.allowlist}")
    print(f"     {mix}")
    if first_party_seen:
        print(f"     first-party, not third-party dependencies ({len(first_party_seen)}):")
        for line in first_party_seen:
            print(f"       {line}")
    if unverified:
        print(f"     UNVERIFIED — no licence in the SBOM and no witness to check ({len(unverified)}):")
        for line in unverified:
            print(f"       {line}")
    if needs_adr:
        print(f"     NEEDS ADR — service images under a restrictive licence ({len(needs_adr)}):")
        for line in needs_adr:
            print(f"       {line}")
        print("       these are not package components and this row does not decide them: docs/40 principle 1")
        print("       asks for a written decision by whoever owns the deployment's licensing.")
    if undecided:
        print(f"     LICENCE UNVERIFIED — a service image whose licence nobody has recorded ({len(undecided)}):")
        for line in undecided:
            print(f"       {line}")
    if "LGPL-3.0-or-later" in licence_counts:
        print("     note: LGPL-3.0-or-later is present (weak copyleft, unmodified native library) — allowed")
        print("           with its obligations written in the allowlist, and NOT treated as the GPL case.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
