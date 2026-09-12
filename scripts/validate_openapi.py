#!/usr/bin/env python3
"""OpenAPI contract integrity check (CI gate, T0008).

specs/api/openapi.yaml is the canonical API contract seed (docs/22,
docs/52: OpenAPI-first). The TypeScript client generation task consumes it
later; a broken or internally inconsistent contract must fail CI now, not
when the generator lands. Together with check-schema-drift (JSON schemas)
this closes the schema/contract drift gate of docs/25.

Checks:

  OPENAPI-PARSE       the file parses as YAML into a mapping
  OPENAPI-VERSION     openapi field is a 3.1.x version string
  OPENAPI-INFO        info.title and info.version are non-empty strings
  OPENAPI-PATHS       paths is a non-empty mapping, every key starts with /
  OPENAPI-OPS         every path item declares >= 1 HTTP operation, each
                      with a non-empty responses mapping and parameters
                      carrying name + valid `in`
  OPENAPI-REFS        every internal $ref resolves inside the document
                      (JSON pointer, ~0/~1 unescaped); external refs are
                      rejected (the check is deterministic and offline)
  OPENAPI-OPIDS       operationIds, when present, are unique document-wide
  OPENAPI-SECURITY    every security requirement names a scheme defined in
                      components.securitySchemes (empty lists allowed)

Exit codes: 0 = all passed; 1 = at least one check failed; 2 = usage error.
Injection (test-only): --root DIR overrides the repository root; a fixture
tree under a temp root is validated exactly like the real repo.
"""
import argparse
import sys
from pathlib import Path

import speclib

USAGE_ERROR = 2
HTTP_METHODS = ("get", "put", "post", "delete", "options", "head",
                "patch", "trace")
PARAM_LOCATIONS = ("query", "header", "path", "cookie")


def fail_usage(msg):
    print(f"error: {msg}", file=sys.stderr)
    sys.exit(USAGE_ERROR)


def parse_args(argv):
    p = argparse.ArgumentParser(
        prog="scripts/validate_openapi.py",
        description="Validate the OpenAPI contract in specs/api/openapi.yaml")
    p.add_argument("--root", default=str(speclib.default_root()),
                   help="repository root (default: parent of scripts/)")
    p.add_argument("--json", action="store_true",
                   help="machine-readable output on stdout")
    return p.parse_args(argv)


def add_fail(checks, check_id, name, measured, detail):
    checks.append(speclib.make_check(
        check_id, name, "failed", "file", measured, "coherent", detail))


def walk(node, path=()):
    """Yield (dotted_path, dict, key, value) for every mapping entry in the
    document, so failures can be located precisely."""
    if isinstance(node, dict):
        for k, v in node.items():
            yield path + (str(k),), node, k, v
            yield from walk(v, path + (str(k),))
    elif isinstance(node, list):
        for i, v in enumerate(node):
            yield from walk(v, path + (str(i),))


def resolve_ref(doc, ref):
    """Resolve an internal '#/...' JSON pointer; returns the node or None."""
    if not ref.startswith("#/"):
        return None
    node = doc
    for part in ref[2:].split("/"):
        part = part.replace("~1", "/").replace("~0", "~")
        if isinstance(node, dict) and part in node:
            node = node[part]
        elif isinstance(node, list) and part.isdigit() \
                and int(part) < len(node):
            node = node[int(part)]
        else:
            return None
    return node


def check_openapi(root):
    checks = []
    path = root / "specs/api/openapi.yaml"
    if not path.is_file():
        add_fail(checks, "OPENAPI-PARSE", "specs/api/openapi.yaml parses",
                 "missing", "specs/api/openapi.yaml not found")
        return checks
    if not speclib.yaml_available():
        add_fail(checks, "OPENAPI-PARSE", "specs/api/openapi.yaml parses",
                 "PyYAML unavailable",
                 "install pyyaml (CI: pip install pyyaml) to parse the "
                 "contract")
        return checks
    try:
        doc = speclib.load_yaml(path)
    except Exception as exc:  # yaml.YAMLError and friends
        add_fail(checks, "OPENAPI-PARSE", "specs/api/openapi.yaml parses",
                 str(exc), "valid YAML mapping")
        return checks
    if not isinstance(doc, dict):
        add_fail(checks, "OPENAPI-PARSE", "specs/api/openapi.yaml parses",
                 f"top-level {type(doc).__name__}", "mapping")
        return checks

    version = doc.get("openapi")
    if isinstance(version, str) and version.startswith("3.1"):
        checks.append(speclib.make_check(
            "OPENAPI-VERSION", "openapi version is 3.1.x", "passed",
            "file", version, "3.1.x", ""))
    else:
        add_fail(checks, "OPENAPI-VERSION", "openapi version is 3.1.x",
                 repr(version), "the generator tasks target 3.1.x")

    info = doc.get("info")
    if isinstance(info, dict) and info.get("title") and info.get("version"):
        checks.append(speclib.make_check(
            "OPENAPI-INFO", "info.title and info.version present",
            "passed", "file",
            f"{info.get('title')} {info.get('version')}",
            "non-empty strings", ""))
    else:
        add_fail(checks, "OPENAPI-INFO", "info.title and info.version present",
                 repr(info), "both are mandatory for a publishable contract")

    paths = doc.get("paths")
    bad_paths = []
    if isinstance(paths, dict) and paths:
        bad_paths = [p for p in paths if not str(p).startswith("/")]
        if not bad_paths:
            checks.append(speclib.make_check(
                "OPENAPI-PATHS", "paths is a non-empty mapping of /-prefixed "
                "paths", "passed", "file", f"{len(paths)} paths",
                "all start with /", ""))
        else:
            add_fail(checks, "OPENAPI-PATHS",
                     "paths is a non-empty mapping of /-prefixed paths",
                     f"bad={bad_paths[:5]}",
                     "path keys must start with /")
    else:
        add_fail(checks, "OPENAPI-PATHS",
                 "paths is a non-empty mapping of /-prefixed paths",
                 f"paths={type(paths).__name__}",
                 "an empty contract would silently pass every generator")

    op_problems = []
    op_ids = []
    ops_seen = 0
    if isinstance(paths, dict):
        for p, item in paths.items():
            if not isinstance(item, dict):
                op_problems.append(f"{p}: not a path item object")
                continue
            methods = [m for m in HTTP_METHODS if m in item]
            if not methods:
                op_problems.append(f"{p}: no HTTP operation declared")
                continue
            for m in methods:
                op = item[m]
                ops_seen += 1
                if not isinstance(op, dict):
                    op_problems.append(f"{p}.{m}: not an operation object")
                    continue
                if "operationId" in op:
                    op_ids.append(op["operationId"])
                responses = op.get("responses")
                if not isinstance(responses, dict) or not responses:
                    op_problems.append(f"{p}.{m}: no responses declared")
                for prm in op.get("parameters") or []:
                    if isinstance(prm, dict) and isinstance(
                            prm.get("$ref"), str):
                        prm = resolve_ref(doc, prm["$ref"]) or {}
                    if not isinstance(prm, dict) or not prm.get("name") \
                            or prm.get("in") not in PARAM_LOCATIONS:
                        op_problems.append(
                            f"{p}.{m}: parameter without name/valid in: {prm}")
    if not op_problems:
        checks.append(speclib.make_check(
            "OPENAPI-OPS",
            "every path has an operation with responses and valid "
            "parameters", "passed", "file",
            f"{ops_seen} operations", "well-formed", ""))
    else:
        add_fail(checks, "OPENAPI-OPS",
                 "every path has an operation with responses and valid "
                 "parameters", f"bad={op_problems[:5]}",
                 "operations without responses/parameters break codegen")

    bad_refs, external_refs = [], []
    for where, node, key, value in walk(doc):
        if key == "$ref":
            if not isinstance(value, str) or not value.startswith("#/"):
                external_refs.append(".".join(where))
            elif resolve_ref(doc, value) is None:
                bad_refs.append(f"{'.'.join(where)} -> {value}")
    if not bad_refs and not external_refs:
        checks.append(speclib.make_check(
            "OPENAPI-REFS", "every $ref resolves inside the document",
            "passed", "file", "all refs resolve",
            "internal JSON pointers only", ""))
    else:
        add_fail(checks, "OPENAPI-REFS",
                 "every $ref resolves inside the document",
                 f"dangling={bad_refs[:5]} external={external_refs[:5]}",
                 "a dangling ref only fails at generator time; external "
                 "refs would make the check network-dependent")

    dup_op_ids = sorted({o for o in op_ids if op_ids.count(o) > 1})
    if not dup_op_ids:
        checks.append(speclib.make_check(
            "OPENAPI-OPIDS", "operationIds are unique document-wide",
            "passed", "file", f"{len(op_ids)} ids", "unique", ""))
    else:
        add_fail(checks, "OPENAPI-OPIDS", "operationIds are unique "
                 "document-wide", f"dup={dup_op_ids[:5]}",
                 "duplicate operationIds collide in generated clients")

    schemes = {}
    comps = doc.get("components")
    if isinstance(comps, dict) and isinstance(comps.get("securitySchemes"),
                                              dict):
        schemes = comps["securitySchemes"]
    unknown_schemes, seen = [], False
    for req in doc.get("security") or []:
        if not isinstance(req, dict):
            unknown_schemes.append(str(req))
            continue
        for s in req:
            seen = True
            if s not in schemes:
                unknown_schemes.append(s)
    if not unknown_schemes and seen:
        checks.append(speclib.make_check(
            "OPENAPI-SECURITY",
            "security requirements name defined schemes",
            "passed", "file", "all schemes defined",
            "components.securitySchemes", ""))
    elif not unknown_schemes:
        checks.append(speclib.make_check(
            "OPENAPI-SECURITY",
            "security requirements name defined schemes",
            "passed", "file", "no security requirements declared",
            "components.securitySchemes", ""))
    else:
        add_fail(checks, "OPENAPI-SECURITY",
                 "security requirements name defined schemes",
                 f"unknown={unknown_schemes[:5]}",
                 "an undefined scheme breaks every client generator")
    return checks


def print_human(checks):
    failed = 0
    for c in checks:
        mark = {"passed": "ok   ", "failed": "FAIL ",
                "unknown": "?    ", "error": "ERR  "}.get(c["status"], "?    ")
        line = f"{mark}{c['id']}: {c['name']}"
        if c["status"] == "failed":
            failed += 1
            line += (f"\n      measured: {c['measured']}"
                     f"\n      detail: {c['detail']}")
        print(line)
    total = len(checks)
    print(f"{failed}/{total} openapi checks failed"
          if failed else f"all {total} openapi checks passed")


def main(argv):
    args = parse_args(argv)
    checks = check_openapi(Path(args.root))
    if args.json:
        import json
        print(json.dumps({"checks": checks}, indent=2))
    else:
        print_human(checks)
    return 1 if any(c["status"] == "failed" for c in checks) else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
