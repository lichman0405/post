#!/usr/bin/env python3
"""Derived spec version marker — deterministic, not hand-maintained.

The POST spec version is DERIVED: a SHA-256 content digest over the spec
inputs (tasks/tasks.json plus every file under specs/ except the marker
file itself). Same inputs -> same marker, byte for byte; any spec change
moves the digest, and `--check` fails until the marker is regenerated.

Commands:
  --write   regenerate specs/SPEC_VERSION.json from the current inputs
            (the only way the marker may change)
  --check   (default) verify the checked-in marker matches the inputs
  --json    print the derived marker object as JSON (useful with --check)

Exit codes: 0 match / written; 1 mismatch or marker missing; 2 usage error.
Injection (test-only): --root DIR overrides the repository root.
"""
import argparse
import json
import sys
from pathlib import Path

import speclib

USAGE_ERROR = 2


def parse_args(argv):
    p = argparse.ArgumentParser(
        prog="scripts/spec_version.py",
        description="Derive / verify the deterministic spec version marker "
                    "(see scripts/speclib.py derive_spec_version).")
    p.add_argument("--root", default=str(speclib.default_root()),
                   help="repository root (default: parent of scripts/)")
    p.add_argument("--write", action="store_true",
                   help="regenerate specs/SPEC_VERSION.json and exit")
    p.add_argument("--check", action="store_true",
                   help="verify the marker matches the derived digest "
                        "(default when neither --write nor --check given)")
    p.add_argument("--json", action="store_true",
                   help="print the derived marker object as JSON")
    return p.parse_args(argv)


def read_marker(root):
    path = root / speclib.SPEC_VERSION_REL
    if not path.is_file():
        return None, f"marker missing: {speclib.SPEC_VERSION_REL}"
    try:
        return speclib.load_json(path), ""
    except (OSError, json.JSONDecodeError) as exc:
        return None, f"marker unreadable: {speclib.SPEC_VERSION_REL}: {exc}"


def main(argv):
    args = parse_args(argv)
    root = Path(args.root)
    if not root.is_dir():
        print(f"error: root directory not found: {root}", file=sys.stderr)
        return USAGE_ERROR

    derived = speclib.derive_spec_version(root)
    marker_path = root / speclib.SPEC_VERSION_REL

    if args.write:
        marker_path.write_text(
            json.dumps(derived, sort_keys=True, indent=2) + "\n",
            encoding="utf-8")
        print(f"wrote {speclib.SPEC_VERSION_REL} "
              f"({derived['spec_version']}, {derived['input_count']} inputs)")
        return 0

    marker, err = read_marker(root)
    if marker is None:
        if args.json:
            print(json.dumps({"exit_code": 1, "ok": False,
                              "derived": derived, "error": err},
                             sort_keys=True, indent=2))
        else:
            print(f"MISMATCH: {err} — run scripts/spec_version.py --write")
        return 1

    got = marker.get("combined_digest")
    if got != derived["combined_digest"]:
        if args.json:
            print(json.dumps({"exit_code": 1, "ok": False,
                              "derived": derived,
                              "checked_in_digest": got,
                              "error": "digest mismatch — run "
                                       "scripts/spec_version.py --write"},
                             sort_keys=True, indent=2))
        else:
            print(f"MISMATCH: checked-in digest {got} != derived "
                  f"{derived['combined_digest']} — run "
                  f"scripts/spec_version.py --write")
        return 1

    if args.json:
        print(json.dumps({"exit_code": 0, "ok": True, "derived": derived,
                          "checked_in_digest": got},
                         sort_keys=True, indent=2))
    else:
        print(f"marker current: {derived['spec_version']} "
              f"({derived['input_count']} inputs)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
